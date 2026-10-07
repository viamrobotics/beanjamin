package coffee

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	// errArmBusy is returned by a claim while another sequence holds the arm.
	errArmBusy = errors.New("a sequence is already running")
	// errQueuePaused is returned by an automated claim while the queue is
	// paused after a cancel or a fault.
	errQueuePaused = errors.New("the queue is paused — send 'proceed' to resume")
)

// armLease is the one place that decides who may move the arm. Every field is
// guarded by mu, and every rule below runs inside a single critical section, so
// no caller can observe the state halfway through a change.
//
// There are two kinds of caller:
//
//   - Automated work (the order queue and the keepalive purge) claims with
//     claimAutomated, which also refuses while the queue is paused.
//   - Troubleshooting (execute_action, run_cup_flow, rewind, reset_world,
//     proceed and the gripper actions) claims with claimManual, which ignores
//     the pause so an operator can recover the machine.
//
// Both refuse while someone else holds the arm. cancel stops whoever holds it,
// of either kind, and always pauses the queue, even when the arm is idle.
type armLease struct {
	mu     sync.Mutex
	holder string // who holds the arm, for logs, errors and Status; "" when free
	paused bool   // set by cancel or a fault; only proceed and reset_world clear it

	// cancelCtx is the shared context motion steps watch. cancel fires
	// cancelFunc and replaces both. A claim hands out the current cancelCtx in
	// the same critical section that records the holder, so a cancel can never
	// land between the two and be lost.
	cancelCtx  context.Context
	cancelFunc func()
}

// claimManual takes the arm for a troubleshooting command. It ignores the
// queue pause and fails at once with errArmBusy when the arm is held. The
// returned context is the one the caller's motion must watch for a cancel.
func (l *armLease) claimManual(who string) (context.Context, error) {
	return l.claim(who, false)
}

// claimAutomated takes the arm for the order queue or the keepalive purge. On
// top of claimManual's rule it fails with errQueuePaused while paused.
func (l *armLease) claimAutomated(who string) (context.Context, error) {
	return l.claim(who, true)
}

func (l *armLease) claim(who string, respectPause bool) (context.Context, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != "" {
		return nil, fmt.Errorf("%w (%s holds the arm)", errArmBusy, l.holder)
	}
	if respectPause && l.paused {
		return nil, errQueuePaused
	}
	l.holder = who
	l.ensureCtxLocked()
	return l.cancelCtx, nil
}

// release gives the arm back. Every successful claim must be paired with
// exactly one release.
func (l *armLease) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holder = ""
}

// cancel pauses the queue and, when someone holds the arm, cancels the context
// they are running under and replaces it with a fresh one for the next claim.
// It reports whether anyone was holding the arm. With the arm idle it is just a
// pause button: nothing automated starts again until proceed.
func (l *armLease) cancel() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paused = true
	if l.holder == "" {
		return false
	}
	l.ensureCtxLocked()
	l.cancelFunc()
	l.cancelCtx, l.cancelFunc = context.WithCancel(context.Background())
	return true
}

// pause holds automated work back without interrupting anyone. A faulted order
// calls it while the queue still holds the arm.
func (l *armLease) pause() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paused = true
}

// unpause lifts the pause, reporting whether there was one to lift, so two
// callers racing cannot both claim to have released a single pause.
func (l *armLease) unpause() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	was := l.paused
	l.paused = false
	return was
}

// shutdown cancels whoever holds the arm when the service closes.
func (l *armLease) shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureCtxLocked()
	l.cancelFunc()
}

// ensureCtxLocked creates the shared context on first use, so a zero armLease
// is ready to use. The caller must hold mu.
func (l *armLease) ensureCtxLocked() {
	if l.cancelCtx == nil {
		l.cancelCtx, l.cancelFunc = context.WithCancel(context.Background())
	}
}

// isPaused reports whether automated work is held back until proceed.
func (l *armLease) isPaused() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.paused
}

// holderName names who holds the arm, or "" when it is free.
func (l *armLease) holderName() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder
}

// busy reports whether anyone holds the arm.
func (l *armLease) busy() bool {
	return l.holderName() != ""
}
