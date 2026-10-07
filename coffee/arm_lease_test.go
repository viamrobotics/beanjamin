package coffee

import (
	"context"
	"testing"
	"time"
)

// freshLease resets s.lease to an idle arm with a live cancelCtx, as NewCoffee
// leaves it.
func freshLease(s *beanjaminCoffee) {
	s.lease.cancelCtx, s.lease.cancelFunc = context.WithCancel(context.Background())
	s.lease.running.Store(false)
	s.lease.paused.Store(false)
}

// TestClaimRefusesWhileHeld: only one sequence holds the arm at a time.
func TestClaimRefusesWhileHeld(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)

	if _, ok := s.lease.claim(); !ok {
		t.Fatal("first claim should take the free arm")
	}
	if _, ok := s.lease.claim(); ok {
		t.Fatal("second claim should be refused while the arm is held")
	}
	s.lease.running.Store(false)
	if _, ok := s.lease.claim(); !ok {
		t.Fatal("claim should succeed again once the arm is released")
	}
}

// TestClaimHandsBackTheContextACancelFires: the context a claim returns is the
// one a later cancel cancels, and the next claim gets the fresh replacement.
func TestClaimHandsBackTheContextACancelFires(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)

	ctx, ok := s.lease.claim()
	if !ok {
		t.Fatal("claim should take the free arm")
	}
	if !s.signalCancel() {
		t.Fatal("signalCancel should report the running sequence")
	}
	if ctx.Err() == nil {
		t.Fatal("the claimed context must be cancelled")
	}

	s.lease.running.Store(false)
	next, ok := s.lease.claim()
	if !ok {
		t.Fatal("claim after the cancel should take the free arm")
	}
	if next.Err() != nil {
		t.Error("the next claim must get the fresh, live context")
	}
}

// TestClaimWaitsForTheCancelLock pins the lost-cancel fix. signalCancel checks
// running and swaps cancelCtx while holding mu, so claim must not take the arm
// until it holds mu too. Before the fix, claim set running first and read
// cancelCtx afterward, and a cancel in that gap fired the old context while the
// claimer picked up the fresh one and kept moving the arm.
func TestClaimWaitsForTheCancelLock(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)
	want := s.lease.cancelCtx

	// Hold the lock a cancel would use, then start a claim.
	s.lease.mu.Lock()
	got := make(chan context.Context, 1)
	go func() {
		ctx, _ := s.lease.claim()
		got <- ctx
	}()

	time.Sleep(50 * time.Millisecond)
	tookArm := s.lease.running.Load()
	s.lease.mu.Unlock()
	if tookArm {
		t.Fatal("claim took the arm without holding mu — a cancel could land before it hands back cancelCtx")
	}

	select {
	case ctx := <-got:
		if ctx != want {
			t.Error("claim should hand back the cancelCtx current at the moment it took the arm")
		}
	case <-time.After(time.Second):
		t.Fatal("claim never finished after the lock was released")
	}
}
