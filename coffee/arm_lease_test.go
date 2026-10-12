package coffee

import (
	"beanjamin/coffee/order"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.viam.com/rdk/testutils/inject"
)

// freshLease resets s.lease to an idle arm with a live cancelCtx, as NewCoffee
// leaves it.
func freshLease(s *beanjaminCoffee) {
	s.lease.cancelCtx, s.lease.cancelFunc = context.WithCancel(context.Background())
	s.lease.holder = ""
	s.lease.paused = false
}

// mustClaim takes the arm for who, standing in for a running sequence. Give it
// back with s.lease.release().
func mustClaim(t *testing.T, s *beanjaminCoffee, who string) {
	t.Helper()
	if s.lease.cancelCtx == nil {
		s.lease.cancelCtx, s.lease.cancelFunc = context.WithCancel(context.Background())
	}
	if _, err := s.lease.claimManual(who); err != nil {
		t.Fatalf("claim(%q): %v", who, err)
	}
}

// TestClaimRefusesWhileHeld: only one sequence holds the arm at a time, and the
// refusal names who has it.
func TestClaimRefusesWhileHeld(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)

	if _, err := s.lease.claimManual("keepalive purge"); err != nil {
		t.Fatalf("first claim should take the free arm: %v", err)
	}
	_, err := s.lease.claimManual("reset_world")
	if !errors.Is(err, errArmBusy) {
		t.Fatalf("second claim err = %v, want errArmBusy", err)
	}
	if !strings.Contains(err.Error(), "keepalive purge") {
		t.Errorf("err = %q, want it to name the holder", err)
	}
	s.lease.release()
	if _, err := s.lease.claimManual("reset_world"); err != nil {
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

	ctx, err := s.lease.claimManual("execute_action open_door")
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
	next, err := s.lease.claimManual("reset_world")
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
		ctx, _ := s.lease.claimManual("reset_world")
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

// TestSignalCancelPausesWhenIdle: cancel and reset_world both start with
// signalCancel, and it pauses the queue even with nothing running, so an idle
// cancel holds back the next order and keepalive purge until proceed. The
// keepalive's own pause check is covered in keepalive_test.go.
func TestSignalCancelPausesWhenIdle(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)
	before := s.lease.cancelCtx

	if s.signalCancel() {
		t.Error("signalCancel on an idle arm should report that nothing was running")
	}
	if !s.lease.isPaused() {
		t.Fatal("signalCancel on an idle arm must still pause the queue")
	}
	if s.lease.cancelCtx != before || before.Err() != nil {
		t.Error("with nothing running there is no context to cancel or replace")
	}
}

// TestPauseBlocksOnlyAutomatedClaims: the order queue and the keepalive wait out
// a pause, troubleshooting does not, and both answers come from the same lock.
func TestPauseBlocksOnlyAutomatedClaims(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)
	s.lease.pause()

	if _, err := s.lease.claimAutomated("order queue"); !errors.Is(err, errQueuePaused) {
		t.Fatalf("automated claim while paused: err = %v, want errQueuePaused", err)
	}
	if s.lease.busy() {
		t.Fatal("a refused claim must not take the arm")
	}
	if _, err := s.lease.claimManual("reset_world"); err != nil {
		t.Fatalf("manual claim while paused: %v, want it allowed", err)
	}
	if _, err := s.lease.claimAutomated("keepalive purge"); !errors.Is(err, errArmBusy) {
		t.Errorf("automated claim while held: err = %v, want errArmBusy", err)
	}
	s.lease.release()

	if !s.lease.unpause() {
		t.Fatal("unpause should report the pause it lifted")
	}
	if _, err := s.lease.claimAutomated("order queue"); err != nil {
		t.Fatalf("automated claim after proceed: %v", err)
	}
	s.lease.release()
}

// TestRunPurgeYieldsToAPause: a purge asked for while paused is refused with
// errQueuePaused, which keepAliveLoop treats as a skip rather than a failure.
func TestRunPurgeYieldsToAPause(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)
	s.lease.pause()

	if err := s.runPurge(context.Background()); !errors.Is(err, errQueuePaused) {
		t.Fatalf("runPurge while paused: err = %v, want errQueuePaused", err)
	}
	if s.lease.busy() {
		t.Error("a refused purge must not hold the arm")
	}
}

// TestGripperActionsClaimTheArm: the gripper DoCommand actions are
// troubleshooting, so they refuse while another sequence holds the arm.
func TestGripperActionsClaimTheArm(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)
	g := inject.NewGripper("g")
	opened := false
	g.OpenFunc = func(context.Context, map[string]any) error { opened = true; return nil }
	s.gripper = g

	mustClaim(t, s, "order queue")
	if _, err := s.handleOpenGripper(context.Background()); !errors.Is(err, errArmBusy) {
		t.Errorf("open_gripper while held: err = %v, want errArmBusy", err)
	}
	if opened {
		t.Error("open_gripper must not move the gripper while another sequence holds the arm")
	}
	s.lease.release()

	if _, err := s.handleOpenGripper(context.Background()); err != nil {
		t.Fatalf("open_gripper on a free arm: %v", err)
	}
	if !opened || s.lease.busy() {
		t.Errorf("open_gripper should run and hand the arm back: opened=%v busy=%v", opened, s.lease.busy())
	}
}

// TestShutdownRefusesLaterClaims: Close cancels whoever holds the arm, and
// nothing can take it afterward.
func TestShutdownRefusesLaterClaims(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	freshLease(s)
	ctx, err := s.lease.claimManual("execute_action open_door")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	s.lease.shutdown()
	if ctx.Err() == nil {
		t.Error("shutdown must cancel the holder's context")
	}
	s.lease.release()

	if _, err := s.lease.claimManual("reset_world"); !errors.Is(err, errServiceClosed) {
		t.Errorf("manual claim after shutdown: err = %v, want errServiceClosed", err)
	}
	if _, err := s.lease.claimAutomated("order queue"); !errors.Is(err, errServiceClosed) {
		t.Errorf("automated claim after shutdown: err = %v, want errServiceClosed", err)
	}
}

// TestPanickingOrderPausesQueue: a panic mid-order is a fault like any other,
// so the queue pauses for by-hand recovery and proceed instead of starting the next order
// from an unknown state.
func TestPanickingOrderPausesQueue(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, nil)
	freshLease(s)
	g := inject.NewGripper("g")
	g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
		panic("gripper driver blew up")
	}
	s.gripper = g

	cancelCtx, err := s.lease.claimAutomated("order queue")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	s.safeExecuteOrder(cancelCtx, order.NewOrder("espresso", "Alice", "", ""))
	s.lease.release()

	if !s.lease.isPaused() {
		t.Error("a panicking order must pause the queue")
	}
}
