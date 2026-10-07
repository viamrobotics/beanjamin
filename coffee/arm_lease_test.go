package coffee

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// freshLease resets s.lease to an idle arm with a live cancelCtx, as NewCoffee
// leaves it.
func freshLease(s *beanjaminCoffee) {
	s.lease.cancelCtx, s.lease.cancelFunc = context.WithCancel(context.Background())
	s.lease.holder = ""
	s.lease.paused.Store(false)
}

// mustClaim takes the arm for who, standing in for a running sequence. Give it
// back with s.lease.release().
func mustClaim(t *testing.T, s *beanjaminCoffee, who string) {
	t.Helper()
	if s.lease.cancelCtx == nil {
		s.lease.cancelCtx, s.lease.cancelFunc = context.WithCancel(context.Background())
	}
	if _, err := s.lease.claim(who); err != nil {
		t.Fatalf("claim(%q): %v", who, err)
	}
}

// TestClaimRefusesWhileHeld: only one sequence holds the arm at a time, and the
// refusal names who has it.
func TestClaimRefusesWhileHeld(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)

	if _, err := s.lease.claim("keepalive purge"); err != nil {
		t.Fatalf("first claim should take the free arm: %v", err)
	}
	_, err := s.lease.claim("rewind")
	if !errors.Is(err, errArmBusy) {
		t.Fatalf("second claim err = %v, want errArmBusy", err)
	}
	if !strings.Contains(err.Error(), "keepalive purge") {
		t.Errorf("err = %q, want it to name the holder", err)
	}
	s.lease.release()
	if _, err := s.lease.claim("rewind"); err != nil {
		t.Fatalf("claim should succeed again once the arm is released: %v", err)
	}
}

// TestStatusNamesTheArmHolder: get_queue reports who has the arm, and an empty
// arm_holder when it is free.
func TestStatusNamesTheArmHolder(t *testing.T) {
	s := newStatusService(t, &Config{})
	mustClaim(t, s, "execute_action open_door")
	resp, err := s.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if resp["arm_holder"] != "execute_action open_door" || resp["is_busy"] != true {
		t.Errorf("arm_holder = %v, is_busy = %v; want the holder's name and true", resp["arm_holder"], resp["is_busy"])
	}

	s.lease.release()
	resp, err = s.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if resp["arm_holder"] != "" || resp["is_busy"] != false {
		t.Errorf("arm_holder = %v, is_busy = %v; want empty and false", resp["arm_holder"], resp["is_busy"])
	}
}

// TestClaimHandsBackTheContextACancelFires: the context a claim returns is the
// one a later cancel cancels, and the next claim gets the fresh replacement.
func TestClaimHandsBackTheContextACancelFires(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)

	ctx, err := s.lease.claim("execute_action open_door")
	if err != nil {
		t.Fatalf("claim should take the free arm: %v", err)
	}
	if !s.signalCancel() {
		t.Fatal("signalCancel should report the running sequence")
	}
	if ctx.Err() == nil {
		t.Fatal("the claimed context must be cancelled")
	}

	s.lease.release()
	next, err := s.lease.claim("rewind")
	if err != nil {
		t.Fatalf("claim after the cancel should take the free arm: %v", err)
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
		ctx, _ := s.lease.claim("rewind")
		got <- ctx
	}()

	time.Sleep(50 * time.Millisecond)
	tookArm := s.lease.holder != "" // read under the mu this test holds
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
