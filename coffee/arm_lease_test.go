package coffee

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// holdArm stands in for a sequence holding the arm — an order, a purge, a
// manual action. The lease is released on cleanup if the test hasn't already.
func holdArm(t *testing.T, s *beanjaminCoffee) (context.Context, func()) {
	t.Helper()
	ctx, release, err := s.lease.tryAcquire("test sequence")
	if err != nil {
		t.Fatalf("holdArm: %v", err)
	}
	t.Cleanup(release)
	return ctx, release
}

// pauseQueue sets the pause the way a cancel or a fault would, without needing
// a holder to interrupt.
func pauseQueue(l *armLease) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.isPaused = true
}

// unwindOnCancel plays a sequence goroutine: it releases the lease once its
// context is cancelled, as a real sequence does after its step returns.
func unwindOnCancel(ctx context.Context, release func()) {
	go func() {
		<-ctx.Done()
		release()
	}()
}

type queueClaim struct {
	ctx     context.Context
	release func(bool)
	ok      bool
}

func startQueueClaim(l *armLease, stop <-chan struct{}) <-chan queueClaim {
	out := make(chan queueClaim, 1)
	go func() {
		ctx, release, ok := l.acquireForQueue(stop)
		out <- queueClaim{ctx, release, ok}
	}()
	return out
}

func assertStillWaiting(t *testing.T, claimed <-chan queueClaim, why string) {
	t.Helper()
	select {
	case c := <-claimed:
		if c.ok {
			c.release(false)
		}
		t.Fatalf("acquireForQueue returned %s", why)
	case <-time.After(50 * time.Millisecond):
	}
}

func awaitClaim(t *testing.T, claimed <-chan queueClaim) queueClaim {
	t.Helper()
	select {
	case c := <-claimed:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("acquireForQueue never returned")
		return queueClaim{}
	}
}

// TestTryAcquireRefusesWhileHeld: the loser gets errArmBusy and changes nothing
// — the holder keeps the arm, its context stays live, and no pause appears.
func TestTryAcquireRefusesWhileHeld(t *testing.T) {
	var l armLease
	ctx, release, err := l.tryAcquire("first")
	if err != nil {
		t.Fatalf("first tryAcquire: %v", err)
	}

	if _, _, err := l.tryAcquire("second"); !errors.Is(err, errArmBusy) {
		t.Fatalf("second tryAcquire err = %v, want errArmBusy", err)
	}
	if got := l.holderName(); got != "first" {
		t.Errorf("holder = %q, want %q", got, "first")
	}
	if ctx.Err() != nil {
		t.Error("a refused claim must not cancel the holder")
	}
	if l.paused() {
		t.Error("a refused claim must not pause the queue")
	}

	release()
	if l.busy() {
		t.Error("the arm should be free after release")
	}
	if !errors.Is(context.Cause(ctx), errLeaseReleased) {
		t.Errorf("cause after release = %v, want errLeaseReleased", context.Cause(ctx))
	}
	if leaseInterrupted(ctx) {
		t.Error("a holder releasing its own lease is not an interruption")
	}
}

// TestReleaseIsIdempotent: a stale release from an earlier holding must not
// free the arm out from under the current holder.
func TestReleaseIsIdempotent(t *testing.T) {
	var l armLease
	_, release1, _ := l.tryAcquire("first")
	release1()
	ctx2, release2, err := l.tryAcquire("second")
	if err != nil {
		t.Fatalf("second tryAcquire: %v", err)
	}
	defer release2()

	release1()
	if got := l.holderName(); got != "second" {
		t.Errorf("holder = %q after a stale release, want %q", got, "second")
	}
	if ctx2.Err() != nil {
		t.Error("a stale release must not cancel the current holder")
	}
}

// TestCancelHolderCancelsAndPauses: a cancel with something running cancels it
// with errOperatorCancel and pauses; a cancel with nothing running does neither.
func TestCancelHolderCancelsAndPauses(t *testing.T) {
	var l armLease
	if _, ok := l.cancelHolder(); ok {
		t.Fatal("cancelHolder reported a holder on a free arm")
	}
	if l.paused() {
		t.Fatal("an idle cancel must not pause the queue")
	}

	ctx, release, _ := l.tryAcquire("seq")
	released, ok := l.cancelHolder()
	if !ok {
		t.Fatal("cancelHolder missed the holder")
	}
	if !errors.Is(context.Cause(ctx), errOperatorCancel) || !leaseInterrupted(ctx) {
		t.Errorf("cause = %v, want errOperatorCancel", context.Cause(ctx))
	}
	if !l.paused() {
		t.Error("cancelling a holder must pause the queue")
	}
	select {
	case <-released:
		t.Fatal("released closed before the holder let go")
	default:
	}
	release()
	if err := waitReleased(context.Background(), released, time.Second); err != nil {
		t.Fatalf("waitReleased: %v", err)
	}
	if !errors.Is(context.Cause(ctx), errOperatorCancel) {
		t.Error("release must not overwrite the operator-cancel cause")
	}
}

// TestCancelIsNeverLost races a claim against a cancel. Whenever the cancel
// reports it found a holder, that holder's context must already be cancelled —
// the bug this replaces was a cancel landing between the claim and a separate
// context snapshot, cancelling a context the sequence never used.
func TestCancelIsNeverLost(t *testing.T) {
	for i := 0; i < 2000; i++ {
		var l armLease
		var wg sync.WaitGroup
		var ctx context.Context
		var release func()
		var cancelled bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			var err error
			ctx, release, err = l.tryAcquire("seq")
			if err != nil {
				t.Errorf("tryAcquire: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			_, cancelled = l.cancelHolder()
		}()
		wg.Wait()
		if ctx == nil {
			t.FailNow()
		}
		if cancelled && !errors.Is(context.Cause(ctx), errOperatorCancel) {
			t.Fatalf("iteration %d: cancel found the holder but its context has cause %v", i, context.Cause(ctx))
		}
		if !cancelled && ctx.Err() != nil {
			t.Fatalf("iteration %d: cancel found nothing yet the holder's context is cancelled", i)
		}
		if cancelled != l.paused() {
			t.Fatalf("iteration %d: paused = %v, want %v", i, l.paused(), cancelled)
		}
		release()
	}
}

// TestShutdownCancelsWithoutPausing: Close interrupts the holder, and that
// counts as an interruption rather than a fault.
func TestShutdownCancelsWithoutPausing(t *testing.T) {
	var l armLease
	ctx, release, _ := l.tryAcquire("seq")
	defer release()
	l.shutdown()
	if !errors.Is(context.Cause(ctx), errServiceClosed) || !leaseInterrupted(ctx) {
		t.Errorf("cause = %v, want errServiceClosed", context.Cause(ctx))
	}
	if l.paused() {
		t.Error("shutdown must not pause the queue")
	}
	if _, _, err := l.tryAcquire("late"); !errors.Is(err, errServiceClosed) {
		t.Errorf("tryAcquire after shutdown err = %v, want errServiceClosed", err)
	}
}

// TestShutdownWakesAQueueWaiter: a waiter exits on shutdown alone, and a claim
// arriving after it is refused even with the arm free.
func TestShutdownWakesAQueueWaiter(t *testing.T) {
	var l armLease
	pauseQueue(&l)
	claimed := startQueueClaim(&l, nil)
	assertStillWaiting(t, claimed, "while the queue was paused")
	l.shutdown()
	if c := awaitClaim(t, claimed); c.ok {
		t.Fatal("acquireForQueue claimed the arm after shutdown")
	}
	l.unpause()
	if _, _, ok := l.acquireForQueue(nil); ok {
		t.Fatal("acquireForQueue claimed a shut-down lease")
	}
}

// TestAcquireForQueueWaitsForRelease: the queue blocks while another sequence
// holds the arm and takes it as soon as that sequence lets go.
func TestAcquireForQueueWaitsForRelease(t *testing.T) {
	var l armLease
	_, release, _ := l.tryAcquire("purge")
	claimed := startQueueClaim(&l, nil)
	assertStillWaiting(t, claimed, "while another sequence held the arm")

	release()
	c := awaitClaim(t, claimed)
	if !c.ok {
		t.Fatal("acquireForQueue reported shutdown, want the arm")
	}
	if got := l.holderName(); got != "order queue" {
		t.Errorf("holder = %q, want the order queue", got)
	}
	c.release(false)
	if l.busy() || l.paused() {
		t.Errorf("busy=%v paused=%v after release(false), want both false", l.busy(), l.paused())
	}
}

// TestAcquireForQueueWaitsForUnpause: a free arm is not enough while the queue
// is paused, and the pause is never cleared by the waiter — only by unpause.
func TestAcquireForQueueWaitsForUnpause(t *testing.T) {
	var l armLease
	pauseQueue(&l)
	claimed := startQueueClaim(&l, nil)
	assertStillWaiting(t, claimed, "while the queue was paused")
	if !l.paused() {
		t.Fatal("the waiter must leave the pause for proceed to lift")
	}
	if l.busy() {
		t.Fatal("a paused queue must not hold the arm")
	}
	// Manual commands still run while the queue is paused.
	_, release, err := l.tryAcquire("rewind")
	if err != nil {
		t.Fatalf("tryAcquire while paused: %v", err)
	}
	release()
	assertStillWaiting(t, claimed, "on a release while still paused")

	if !l.unpause() {
		t.Fatal("unpause should report the pause it lifted")
	}
	if l.unpause() {
		t.Error("a second unpause must report there was nothing left to lift")
	}
	if c := awaitClaim(t, claimed); !c.ok {
		t.Fatal("acquireForQueue reported shutdown, want the arm")
	} else {
		c.release(false)
	}
}

// TestAcquireForQueueStaysPausedAfterCancel: cancelling the sequence an order
// waits behind pauses the queue, so the order must not start when it unwinds.
func TestAcquireForQueueStaysPausedAfterCancel(t *testing.T) {
	var l armLease
	ctx, release, _ := l.tryAcquire("execute_action")
	unwindOnCancel(ctx, release)
	claimed := startQueueClaim(&l, nil)

	released, ok := l.cancelHolder()
	if !ok {
		t.Fatal("cancelHolder missed the holder")
	}
	if err := waitReleased(context.Background(), released, time.Second); err != nil {
		t.Fatal(err)
	}
	assertStillWaiting(t, claimed, "after the cancel paused the queue")
	l.unpause()
	if c := awaitClaim(t, claimed); c.ok {
		c.release(false)
	}
}

// TestAcquireForQueueExitsOnStop: a waiter leaves on shutdown holding nothing
// and without touching the holder it was waiting behind.
func TestAcquireForQueueExitsOnStop(t *testing.T) {
	var l armLease
	ctx, release, _ := l.tryAcquire("purge")
	defer release()
	stop := make(chan struct{})
	claimed := startQueueClaim(&l, stop)
	assertStillWaiting(t, claimed, "while another sequence held the arm")

	close(stop)
	if c := awaitClaim(t, claimed); c.ok {
		t.Fatal("acquireForQueue claimed the arm, want shutdown")
	}
	if got := l.holderName(); got != "purge" || ctx.Err() != nil {
		t.Errorf("holder = %q ctxErr = %v, want the purge untouched", got, ctx.Err())
	}
}

// TestAcquireForQueueStopWinsOverAFreeArm: once the service is closing, a free
// arm must not start another order.
func TestAcquireForQueueStopWinsOverAFreeArm(t *testing.T) {
	var l armLease
	stop := make(chan struct{})
	close(stop)
	if _, _, ok := l.acquireForQueue(stop); ok {
		t.Fatal("acquireForQueue claimed the arm after stop closed")
	}
	if l.busy() {
		t.Error("a refused queue claim must hold nothing")
	}
}

// TestReleaseWithPausePauses:the queue's fault path pauses in the release, so
// a waiter behind it stays put.
func TestReleaseWithPausePauses(t *testing.T) {
	var l armLease
	_, release, ok := l.acquireForQueue(nil)
	if !ok {
		t.Fatal("acquireForQueue on a free arm failed")
	}
	claimed := startQueueClaim(&l, nil)
	release(true)
	if !l.paused() || l.busy() {
		t.Fatalf("paused=%v busy=%v after release(true), want paused and free", l.paused(), l.busy())
	}
	assertStillWaiting(t, claimed, "after a faulting release paused the queue")
	l.unpause()
	if c := awaitClaim(t, claimed); c.ok {
		c.release(false)
	}
}

// TestWaitReleasedTimesOut covers a sequence that ignores its cancel.
func TestWaitReleasedTimesOut(t *testing.T) {
	var l armLease
	_, release, _ := l.tryAcquire("stuck")
	defer release()
	released, _ := l.cancelHolder()
	if err := waitReleased(context.Background(), released, 20*time.Millisecond); err == nil {
		t.Fatal("waitReleased should time out while the holder hangs on")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitReleased(ctx, released, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitReleased err = %v, want context.Canceled", err)
	}
}

// TestLeaseStress hammers every entry point at once under -race: tryAcquire,
// acquireForQueue, cancelHolder, unpause. The invariant is mutual exclusion —
// never two holders at a time — and that every waiter eventually gets out.
func TestLeaseStress(t *testing.T) {
	var l armLease
	var inside sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	hold := func(release func()) {
		if !inside.TryLock() {
			t.Error("two holders inside the lease at once")
			release()
			return
		}
		inside.Unlock()
		release()
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, release, err := l.tryAcquire("manual"); err == nil {
					hold(release)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, release, ok := l.acquireForQueue(stop)
			if !ok {
				return
			}
			hold(func() { release(false) })
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			l.cancelHolder()
			l.unpause()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	close(stop)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a lease user never got out")
	}
}
