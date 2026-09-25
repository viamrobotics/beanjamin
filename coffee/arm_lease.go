package coffee

// Arm ownership: which sequence holds the arm, how to interrupt it, and the
// queue pause that keeps orders off the arm until an operator says so.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// errArmBusy is what tryAcquire returns while another sequence holds the arm.
	errArmBusy = errors.New("a sequence is already running")
	// errOperatorCancel is the cancel cause cancel, rewind and reset_world put on
	// the holder's context, which is how an interrupted order is told apart from
	// a genuine fault.
	errOperatorCancel = errors.New("cancelled by operator")
	// errServiceClosed is the cancel cause Close puts on the holder's context.
	errServiceClosed = errors.New("service closing")
	// errLeaseReleased is the cause a holder's context carries once it has let
	// go of the arm, so nothing can keep using it after the lease has moved on.
	errLeaseReleased = errors.New("arm lease released")
)

// armLease is the single owner of the arm. Every sequence that moves it or
// mutates the motion state (cachedFS and the held/locked/staged flags) holds a
// lease for its whole run, and the lease is the only place that records who
// holds it, the cancel function for that holder's context, and whether the
// queue is paused. Keeping all three under one mutex is what makes a cancel
// impossible to lose: the holder's context is created in the same critical
// section as the claim, so a cancel either finds no holder or cancels exactly
// the context that holder is running under.
//
// The zero value is a free, unpaused lease.
type armLease struct {
	mu     sync.Mutex
	holder string // who holds the arm, for logs and errors; "" when free
	cancel context.CancelCauseFunc
	// released is closed when the current holding ends, so a canceller can wait
	// for the sequence it interrupted without polling. nil when free.
	released chan struct{}
	isPaused bool
	// closed is set by shutdown and refuses every later claim, so a sequence
	// racing Close cannot start after the holder it would follow was cancelled.
	closed bool
	// changed is closed and dropped whenever the arm is released, the pause is
	// lifted or the lease shuts down, waking every acquireForQueue waiting on it. Created lazily by the
	// first waiter, so the zero value needs no constructor.
	changed chan struct{}
}

// tryAcquire claims the arm for who, or fails at once with errArmBusy when
// another sequence holds it (errServiceClosed after shutdown). It ignores the queue pause: operator commands,
// manual actions and the keepalive purge all run while the queue is paused.
//
// The returned context is cancelled when an operator cancels the holder, when
// the service closes, or when release is called. release is safe to call more
// than once.
func (l *armLease) tryAcquire(who string) (context.Context, func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, nil, errServiceClosed
	}
	if l.holder != "" {
		return nil, nil, fmt.Errorf("%w (%s holds the arm)", errArmBusy, l.holder)
	}
	ctx, release := l.claimLocked(who)
	return ctx, func() { release(false) }, nil
}

// acquireForQueue blocks until the arm is free and the queue is not paused,
// then claims it for the order queue. It reports false, holding nothing, once
// stop is closed or the lease has been shut down.
//
// release(true) pauses the queue in the same critical section that frees the
// arm, so no other order can take the arm between a fault and its pause.
func (l *armLease) acquireForQueue(stop <-chan struct{}) (context.Context, func(pause bool), bool) {
	for {
		// Checked first so a closed stop wins over a free arm: a shut-down
		// service must not start another order.
		select {
		case <-stop:
			return nil, nil, false
		default:
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, nil, false
		}
		if l.holder == "" && !l.isPaused {
			ctx, release := l.claimLocked("order queue")
			l.mu.Unlock()
			return ctx, release, true
		}
		if l.changed == nil {
			l.changed = make(chan struct{})
		}
		changed := l.changed
		l.mu.Unlock()

		select {
		case <-changed:
		case <-stop:
			return nil, nil, false
		}
	}
}

// claimLocked records who as the holder under a fresh context. The caller must
// hold l.mu and have checked the arm is free.
func (l *armLease) claimLocked(who string) (context.Context, func(pause bool)) {
	ctx, cancel := context.WithCancelCause(context.Background())
	released := make(chan struct{})
	l.holder, l.cancel, l.released = who, cancel, released
	return ctx, func(pause bool) {
		l.mu.Lock()
		defer l.mu.Unlock()
		// A second release, or one arriving after the holding already ended,
		// must not free the arm out from under the next holder.
		if l.released != released {
			return
		}
		cancel(errLeaseReleased)
		close(released)
		l.holder, l.cancel, l.released = "", nil, nil
		if pause {
			l.isPaused = true
		}
		l.notifyLocked()
	}
}

// cancelHolder cancels the current holder's context with errOperatorCancel and
// pauses the queue, so whatever was interrupted is not followed straight away
// by the next order. It returns a channel closed once that holder releases the
// arm, and false — pausing nothing — when the arm is free.
func (l *armLease) cancelHolder() (<-chan struct{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder == "" {
		return nil, false
	}
	l.isPaused = true
	l.cancel(errOperatorCancel)
	return l.released, true
}

// shutdown cancels the current holder's context with errServiceClosed and
// refuses every later claim. It leaves the pause alone: nothing runs after a
// close for it to hold back.
func (l *armLease) shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.holder != "" {
		l.cancel(errServiceClosed)
	}
	l.notifyLocked()
}

// unpause lifts the queue pause, reporting whether there was one to lift, so
// two callers racing cannot both claim to have released a single pause.
func (l *armLease) unpause() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.isPaused {
		return false
	}
	l.isPaused = false
	l.notifyLocked()
	return true
}

// paused reports whether the queue is held back until an operator proceeds.
func (l *armLease) paused() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.isPaused
}

// busy reports whether any sequence holds the arm.
func (l *armLease) busy() bool {
	return l.holderName() != ""
}

// holderName names the current holder, or "" when the arm is free.
func (l *armLease) holderName() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder
}

func (l *armLease) notifyLocked() {
	if l.changed != nil {
		close(l.changed)
		l.changed = nil
	}
}

// waitReleased blocks until released (from cancelHolder) is closed, or fails
// once timeout passes or ctx ends.
func waitReleased(ctx context.Context, released <-chan struct{}, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-released:
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out after %s waiting for sequence to stop", timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// leaseInterrupted reports whether a lease context was cancelled from outside
// its holder — an operator cancel, rewind or reset_world, or the service
// closing — rather than by the holder releasing it. The cause is the only
// reliable signal: the step that noticed the cancel may return an error that
// does not wrap context.Canceled.
func leaseInterrupted(ctx context.Context) bool {
	cause := context.Cause(ctx)
	return errors.Is(cause, errOperatorCancel) || errors.Is(cause, errServiceClosed)
}
