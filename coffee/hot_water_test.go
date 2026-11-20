package coffee

import (
	"context"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// Cappuccina's collar and lever as configured: the collar's origin on the dial
// axis with local Z along world +Y (the direction that opens the valve), the
// lever its child, and the grip the arm was taught on the re-clamped lever.
var (
	testHingeOrigin = spatialmath.NewPose(r3.Vector{X: 872.5, Y: -21, Z: 322.8}, &spatialmath.OrientationVectorDegrees{OY: 1})
	testLeverGrip   = &RelativePose{X: 10, Y: 63.9, Z: 13.2, OX: -0.1384, OY: -0.8272, OZ: -0.5446, Theta: -0.01}
)

// buildTestLeverFS builds the collar and lever through the RDK's real
// frame-system construction, so they get the same "<name>_origin" split as on
// the machine.
func buildTestLeverFS(t *testing.T) (*referenceframe.FrameSystem, spatialmath.Pose) {
	t.Helper()
	collarBox, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{X: 9.4}), r3.Vector{X: 74, Y: 74, Z: 40}, frameHotWaterCollar)
	if err != nil {
		t.Fatal(err)
	}
	leverBox, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 150, Y: 50, Z: 44}, frameHotWaterLever)
	if err != nil {
		t.Fatal(err)
	}
	collar := referenceframe.NewLinkInFrame(referenceframe.World, testHingeOrigin, frameHotWaterCollar, collarBox)
	lever := referenceframe.NewLinkInFrame(frameHotWaterCollar, spatialmath.NewPose(r3.Vector{X: 11.3, Y: 93.3, Z: 19.2},
		&spatialmath.OrientationVectorDegrees{OX: -0.9882, OY: 0.1522, OZ: 0.0201, Theta: -123.18}), frameHotWaterLever, leverBox)
	fs, err := referenceframe.NewFrameSystem("test", []*referenceframe.FrameSystemPart{{FrameConfig: collar}, {FrameConfig: lever}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := hingeBasePose(fs, frameHotWaterHinge)
	if err != nil {
		t.Fatal(err)
	}
	return fs, base
}

func gripAt(t *testing.T, fs *referenceframe.FrameSystem) spatialmath.Pose {
	t.Helper()
	p, err := hotWaterGripWorld(fs, referenceframe.NewZeroInputs(fs).ToLinearInputs(), relativePoseToSpatial(testLeverGrip))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func near(a, b r3.Vector) bool { return a.Distance(b) < 0.1 }

// TestHotWaterGripFollowsTheLever checks the grip against the poses measured on
// the robot: closed, the taught grip; turned 45°, where dialsim's turn ended
// with the valve pouring fully. The lever turns with the collar.
func TestHotWaterGripFollowsTheLever(t *testing.T) {
	fs, base := buildTestLeverFS(t)
	closed := r3.Vector{X: 808.6, Y: -7.8, Z: 312.8}
	if got := gripAt(t, fs).Point(); !near(got, closed) {
		t.Errorf("closed grip at %v, want %v", got, closed)
	}
	leverBefore := worldPoint(t, fs, frameHotWaterLever)
	if err := setHingeTheta(fs, frameHotWaterHinge, base, defaultHotWaterOpenDegs); err != nil {
		t.Fatal(err)
	}
	open := gripAt(t, fs)
	if got, want := open.Point(), (r3.Vector{X: 820.24, Y: -7.8, Z: 360.91}); !near(got, want) {
		t.Errorf("open grip at %v, want %v — the lever turns up about world +Y", got, want)
	}
	if got := worldPoint(t, fs, frameHotWaterLever); got.Z <= leverBefore.Z+20 {
		t.Errorf("the lever obstacle stayed at %v after the turn (was %v): it must turn with the collar", got, leverBefore)
	}

	// Turning back shut lands on the closed grip, recovered from the turned frame.
	closedBase, err := hingeClosedOrigin(fs, frameHotWaterHinge, defaultHotWaterOpenDegs)
	if err != nil {
		t.Fatal(err)
	}
	if !spatialmath.PoseAlmostEqual(closedBase, base) {
		t.Errorf("recovered closed origin %v, want %v", closedBase, base)
	}
	if err := setHingeTheta(fs, frameHotWaterHinge, closedBase, 0); err != nil {
		t.Fatal(err)
	}
	if got := gripAt(t, fs).Point(); !near(got, closed) {
		t.Errorf("grip after closing at %v, want %v", got, closed)
	}
}

// TestHotWaterStandoffIsOutAlongTheLever: the claws open and back out beyond
// the lever's tip, not beside it.
func TestHotWaterStandoffIsOutAlongTheLever(t *testing.T) {
	fs, _ := buildTestLeverFS(t)
	grip := gripAt(t, fs)
	got := hotWaterStandoff(grip)
	ov := grip.Orientation().OrientationVectorDegrees()
	if want := grip.Point().Sub(r3.Vector{X: ov.OX, Y: ov.OY, Z: ov.OZ}.Normalize().Mul(hotWaterApproachMm)); !near(got.Point(), want) {
		t.Errorf("standoff at %v, want %v", got.Point(), want)
	}
	if !spatialmath.OrientationAlmostEqual(got.Orientation(), grip.Orientation()) {
		t.Error("the standoff should keep the grip orientation, so the slide-on is straight")
	}
}

func TestHotWaterOpenDegs(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	if got := s.hotWaterOpenDegs(); got != defaultHotWaterOpenDegs {
		t.Errorf("default = %v, want %v", got, defaultHotWaterOpenDegs)
	}
	s.cfg.HotWaterOpenDegs = 30
	if got := s.hotWaterOpenDegs(); got != 30 {
		t.Errorf("configured = %v, want 30", got)
	}
}

// TestHotWaterActionsNeedTheGripConfig: the actions are only offered once the
// lever is configured.
func TestHotWaterActionsNeedTheGripConfig(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	if _, ok := s.actionFuncs()["open_hot_water"]; ok {
		t.Error("open_hot_water offered without hot_water_lever_grip_relative_pose")
	}
	s.cfg.HotWaterLeverGripRelativePose = testLeverGrip
	for _, name := range []string{"open_hot_water", "close_hot_water"} {
		if _, ok := s.actionFuncs()[name]; !ok {
			t.Errorf("%s not offered with the lever configured", name)
		}
	}
}

// TestHotWaterRefusesWithTheFilterOnTheClaws: opening the jaws round the lever
// would drop a portafilter still in them, and a closed gripper reads the same
// on the filter handle as empty, so only a filter locked in the machine counts.
func TestHotWaterRefusesWithTheFilterOnTheClaws(t *testing.T) {
	fs, _ := buildTestLeverFS(t)
	s := &beanjaminCoffee{cfg: &Config{HotWaterLeverGripRelativePose: testLeverGrip}, settledScene: &sceneState{fs: fs}}
	err := s.openHotWater(context.Background(), context.Background())
	if err == nil || !strings.Contains(err.Error(), "lock_portafilter") {
		t.Errorf("open_hot_water with the filter on the claws: err = %v, want a refusal naming lock_portafilter", err)
	}
}

// TestHotWaterSkipsATurnToWhereTheLeverIs: close on a closed lever moves
// nothing (the service has no arm here, so any motion would panic).
func TestHotWaterSkipsATurnToWhereTheLeverIs(t *testing.T) {
	fs, _ := buildTestLeverFS(t)
	s, _ := newTestCoffee(t, &Config{HotWaterLeverGripRelativePose: testLeverGrip})
	s.settledScene = &sceneState{fs: fs, filterFrameLocked: true}
	if err := s.closeHotWater(context.Background(), context.Background()); err != nil {
		t.Errorf("close_hot_water on a closed lever: %v, want nil", err)
	}
}

// TestProceedClearsTheHotWaterLeverAngle mirrors the fridge-door test: proceed
// asserts the valve has been shut by hand, and says so.
func TestProceedClearsTheHotWaterLeverAngle(t *testing.T) {
	s, _ := coffeeWithFridge(t)
	s.scene().hotWaterOpenDegs = 37.5

	resp, err := s.proceedQueue(context.Background())
	if err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	if got, _ := resp["hot_water_lever_cleared_degs"].(float64); got != 37.5 {
		t.Errorf("hot_water_lever_cleared_degs = %v, want 37.5", resp["hot_water_lever_cleared_degs"])
	}
	if s.scene().hotWaterOpenDegs != 0 {
		t.Errorf("hotWaterOpenDegs = %v, want 0", s.scene().hotWaterOpenDegs)
	}
}

// TestHotWaterApproachPoseRequiredWithTheLever: the approach pose is validated
// at startup once the lever is configured, and not before.
func TestHotWaterApproachPoseRequiredWithTheLever(t *testing.T) {
	has := func(s *beanjaminCoffee) bool {
		for _, rp := range s.requiredPoses() {
			if rp.poseName == clawPoseHotWaterApproach {
				return true
			}
		}
		return false
	}
	s := &beanjaminCoffee{cfg: &Config{}}
	if has(s) {
		t.Error("hot_water_approach required without the lever configured")
	}
	s.cfg.HotWaterLeverGripRelativePose = testLeverGrip
	if !has(s) {
		t.Error("hot_water_approach not required with the lever configured")
	}
}
