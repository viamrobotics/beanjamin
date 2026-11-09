package coffee

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/testutils/inject"
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

// A scene edit made while moves are in flight is applied where the run has
// planned the arm to be, without waiting for the arm to get there.
func TestSceneEditUsesThePlannedArm(t *testing.T) {
	fs := clawsRevoluteFS(t)
	s := heldGeomService(t, fs)
	s.cfg.ArmName = "j0"
	s.arm = &inject.Arm{CurrentInputsFunc: func(context.Context) ([]referenceframe.Input, error) {
		t.Error("the arm was read although the run had planned where it will be")
		return []referenceframe.Input{0}, nil
	}}
	end := s.startRun(context.Background(), context.Background(), "order", true)
	release := make(chan struct{})
	_ = s.handOff(context.Background(), blocking(release))

	planned := []referenceframe.Input{math.Pi / 2}
	s.run.planned = s.run.planned.withArm("j0", planned)
	// With j0 at 90° the claws stand at (0, 100) rather than (100, 0).
	worldCenter := r3.Vector{X: 0, Y: 100, Z: 60}
	if err := s.attachDetectedGeometry(context.Background(), pickupLabelCup, testBox(t, spatialmath.NewPoseFromPoint(worldCenter))); err != nil {
		t.Fatal(err)
	}
	if s.run.executed.Load().heldItemAttached {
		t.Error("the held item reached the executed scene while the arm was still moving")
	}
	inputs := referenceframe.NewZeroInputs(fs)
	inputs["j0"] = planned
	requireVecEqual(t, heldItemWorldCenter(t, s.scene().fs, inputs), worldCenter, 1e-6)

	close(release)
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if !s.settledScene.heldItemAttached {
		t.Error("the held item is missing from the settled scene")
	}
}

// Without a planned arm position, expectedInputs reads the arm.
func TestExpectedInputsReadsTheArmWhenUnplanned(t *testing.T) {
	s := heldGeomService(t, clawsRevoluteFS(t))
	s.cfg.ArmName = "j0"
	live := []referenceframe.Input{0.3}
	s.arm = &inject.Arm{CurrentInputsFunc: func(context.Context) ([]referenceframe.Input, error) { return live, nil }}
	got, err := s.expectedInputs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got["j0"], live) {
		t.Errorf("expectedInputs j0 = %v, want the arm's %v", got["j0"], live)
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
