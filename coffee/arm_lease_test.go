package coffee

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.viam.com/rdk/testutils/inject"
)

// holdArm claims the arm as a manual sequence named who, standing in for a
// sequence that is running, and returns its release.
func holdArm(t *testing.T, s *beanjaminCoffee, who string) func() {
	t.Helper()
	if _, err := s.lease.claimManual(who); err != nil {
		t.Fatalf("holdArm(%q): %v", who, err)
	}
	return s.lease.release
}

// TestClaimNamesTheHolder: a refused claim says who has the arm, and Status
// reports it, so it is never just "busy".
func TestClaimNamesTheHolder(t *testing.T) {
	var l armLease
	if _, err := l.claimManual("keepalive purge"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := l.claimManual("rewind")
	if !errors.Is(err, errArmBusy) {
		t.Fatalf("second claim err = %v, want errArmBusy", err)
	}
	if !strings.Contains(err.Error(), "keepalive purge") {
		t.Errorf("err = %q, want it to name the holder", err)
	}
	if got := l.holderName(); got != "keepalive purge" {
		t.Errorf("holderName = %q, want keepalive purge", got)
	}

	l.release()
	if l.busy() {
		t.Error("release should free the arm")
	}
}

// TestPauseBlocksOnlyAutomatedClaims: orders and the keepalive wait out a
// pause, troubleshooting does not.
func TestPauseBlocksOnlyAutomatedClaims(t *testing.T) {
	var l armLease
	l.pause()

	if _, err := l.claimAutomated("order queue"); !errors.Is(err, errQueuePaused) {
		t.Fatalf("automated claim while paused: err = %v, want errQueuePaused", err)
	}
	if l.busy() {
		t.Fatal("a refused claim must not take the arm")
	}
	if _, err := l.claimManual("rewind"); err != nil {
		t.Fatalf("manual claim while paused: %v, want it allowed", err)
	}
	l.release()
}

// TestClaimedContextSeesTheCancel: the context a claim hands out is the one a
// cancel fires, because both happen under the same lock. The lost-cancel bug
// was a sequence picking up the replacement context instead.
func TestClaimedContextSeesTheCancel(t *testing.T) {
	var l armLease
	ctx, err := l.claimManual("execute_action open_door")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !l.cancel() {
		t.Fatal("cancel should report the holder it interrupted")
	}
	if ctx.Err() == nil {
		t.Fatal("the holder's context must be cancelled")
	}
	l.release()

	next, err := l.claimManual("rewind")
	if err != nil {
		t.Fatalf("claim after cancel: %v", err)
	}
	if next.Err() != nil {
		t.Error("the next holder must get a fresh, live context")
	}
	l.release()
}

// TestCancelWhileIdlePauses: with nothing running, cancel is a pause button, so
// nothing automated starts until proceed.
func TestCancelWhileIdlePauses(t *testing.T) {
	var l armLease
	if l.cancel() {
		t.Error("cancel on an idle arm should report that nothing was running")
	}
	if !l.isPaused() {
		t.Fatal("cancel on an idle arm must still pause the queue")
	}
	if _, err := l.claimAutomated("order queue"); !errors.Is(err, errQueuePaused) {
		t.Errorf("automated claim after an idle cancel: err = %v, want errQueuePaused", err)
	}
}

// TestUnpauseReportsOnce: two proceeds racing cannot both claim the resume.
func TestUnpauseReportsOnce(t *testing.T) {
	var l armLease
	l.pause()
	if !l.unpause() {
		t.Error("first unpause should report a pause lifted")
	}
	if l.unpause() {
		t.Error("second unpause should report nothing to lift")
	}
}

// TestCancelCommandWhileIdlePausesQueue drives the cancel DoCommand path with
// the arm idle.
func TestCancelCommandWhileIdlePausesQueue(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	resp, err := s.cancel(context.Background())
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if resp["cancelled"] != false || resp["queue"] != "paused" {
		t.Errorf("cancel response = %v, want cancelled=false queue=paused", resp)
	}
}

// TestGripperActionsClaimTheArm: the gripper DoCommand actions are
// troubleshooting, so they refuse while another sequence holds the arm.
func TestGripperActionsClaimTheArm(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	g := inject.NewGripper("g")
	opened := false
	g.OpenFunc = func(context.Context, map[string]any) error { opened = true; return nil }
	s.gripper = g

	release := holdArm(t, s, "order queue")
	if _, err := s.handleOpenGripper(context.Background()); !errors.Is(err, errArmBusy) {
		t.Errorf("open_gripper while held: err = %v, want errArmBusy", err)
	}
	if opened {
		t.Error("open_gripper must not move the gripper while another sequence holds the arm")
	}
	release()

	if _, err := s.handleOpenGripper(context.Background()); err != nil {
		t.Fatalf("open_gripper on a free arm: %v", err)
	}
	if !opened || s.lease.busy() {
		t.Errorf("open_gripper should run and hand the arm back: opened=%v busy=%v", opened, s.lease.busy())
	}
}
