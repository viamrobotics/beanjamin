package coffee

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// sceneCoffee returns a service whose settled scene has an empty frame system.
func sceneCoffee(t *testing.T) *beanjaminCoffee {
	t.Helper()
	s, _ := newTestCoffee(t, nil)
	s.settledScene = &sceneState{fs: referenceframe.NewEmptyFrameSystem("test")}
	return s
}

// setMilk publishes a scene edit recording milk as out of the fridge.
func setMilk(t *testing.T, s *beanjaminCoffee, milk string) {
	t.Helper()
	if err := s.editScene(func(sc *sceneState) error {
		sc.heldMilk = milk
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A scene edit made while moves are in flight reaches the executed scene only
// once those moves have run.
func TestExecutedSceneFollowsTheArm(t *testing.T) {
	s := sceneCoffee(t)
	end := s.startRun(context.Background(), context.Background(), "order", true)
	release := make(chan struct{})
	_ = s.handOff(context.Background(), blocking(release))

	setMilk(t, s, "oat")
	if got := s.scene().heldMilk; got != "oat" {
		t.Errorf("planned heldMilk = %q, want oat as soon as it is edited", got)
	}
	if got := s.run.executed.Load().heldMilk; got != "" {
		t.Errorf("executed heldMilk = %q while the arm is still moving, want empty", got)
	}
	close(release)
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if got := s.settledScene.heldMilk; got != "oat" {
		t.Errorf("settled heldMilk = %q, want oat once the run has ended", got)
	}
}

// After a failed move the planned scene goes back to what the arm really did,
// and the run leaves that behind.
func TestFailedMoveRewindsThePlannedScene(t *testing.T) {
	s := sceneCoffee(t)
	boom := errors.New("gripper faulted")
	s.gripper = failingGripper(boom)
	end := s.startRun(context.Background(), context.Background(), "order", true)

	_ = s.openGripper(context.Background())
	setMilk(t, s, "oat")
	if err := s.settle(); !errors.Is(err, boom) {
		t.Fatalf("settle = %v, want the gripper failure", err)
	}
	if got := s.scene().heldMilk; got != "" {
		t.Errorf("planned heldMilk = %q after the failure, want the edit that never ran gone", got)
	}
	_ = end()
	if got := s.settledScene.heldMilk; got != "" {
		t.Errorf("settled heldMilk = %q, want empty", got)
	}
}

// A run that does not plan ahead executes each move as it is handed off, so its
// executed scene keeps up with the planned one.
func TestDirectRunExecutesAsItHandsOff(t *testing.T) {
	s := sceneCoffee(t)
	end := s.startDirectRun("execute_action")
	var r recorder
	if err := s.handOff(context.Background(), r.move("a")); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("ran %v, want a to run immediately", got)
	}
	setMilk(t, s, "oat")
	if got := s.run.executed.Load().heldMilk; got != "oat" {
		t.Errorf("executed heldMilk = %q, want oat", got)
	}
	end()
	if s.run != nil {
		t.Error("end should clear the run")
	}
	if got := s.settledScene.heldMilk; got != "oat" {
		t.Errorf("settled heldMilk = %q, want oat", got)
	}
}

// A published scene is never changed: an edit works on a copy of its frame
// system.
func TestEditSceneLeavesThePublishedSceneAlone(t *testing.T) {
	s := sceneCoffee(t)
	before := s.scene()
	frame, err := referenceframe.NewStaticFrame("obstacle", spatialmath.NewZeroPose())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.editScene(func(sc *sceneState) error { return sc.fs.AddFrame(frame, sc.fs.World()) }); err != nil {
		t.Fatal(err)
	}
	if before.fs.Frame("obstacle") != nil {
		t.Error("the edit changed the scene it was made from")
	}
	if s.scene().fs.Frame("obstacle") == nil {
		t.Error("the edited scene is missing the added frame")
	}
}

// A failed edit publishes nothing.
func TestFailedEditPublishesNothing(t *testing.T) {
	s := sceneCoffee(t)
	before := s.scene()
	boom := errors.New("boom")
	if err := s.editScene(func(sc *sceneState) error {
		sc.heldMilk = "oat"
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("editScene = %v, want boom", err)
	}
	if s.scene() != before {
		t.Error("a failed edit replaced the scene")
	}
}

// The run plans each direct move from where the last one leaves the arm, and
// forgets that at a stopping point, where the arm is read again.
func TestPlannedArmInputsResetAtSettle(t *testing.T) {
	s := sceneCoffee(t)
	end := s.startRun(context.Background(), context.Background(), "order", true)
	defer func() { _ = end() }()
	joints := []referenceframe.Input{1, 2, 3}
	s.run.planned = s.run.planned.withArm("arm", joints)
	if got := s.scene().armInputs("arm"); !slices.Equal(got, joints) {
		t.Fatalf("planned arm = %v, want %v", got, joints)
	}
	if err := s.settle(); err != nil {
		t.Fatal(err)
	}
	if got := s.scene().armInputs("arm"); got != nil {
		t.Errorf("planned arm = %v after settle, want nil", got)
	}
}
