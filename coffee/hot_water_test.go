package coffee

import (
	"context"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// Cappuccina's lever as configured: origin on the dial axis with local Z along
// world +Y (the direction that opens the valve), and the grip the arm was
// taught there, 72 mm out along the lever.
var (
	testLeverOrigin = spatialmath.NewPose(r3.Vector{X: 872.5, Y: -1.8, Z: 322.8}, &spatialmath.OrientationVectorDegrees{OY: 1})
	testLeverGrip   = &RelativePose{X: 2.63, Y: 60.33, Z: -10.98, OX: -0.0366, OY: -0.8379, OZ: -0.5446}
)

// buildTestLeverFS builds the lever through the RDK's real frame-system
// construction, so it gets the same "<name>_origin" split as on the machine.
func buildTestLeverFS(t *testing.T) (*referenceframe.FrameSystem, spatialmath.Pose) {
	t.Helper()
	box, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{Y: 94}), r3.Vector{X: 150, Y: 50, Z: 44}, frameHotWaterLever)
	if err != nil {
		t.Fatal(err)
	}
	link := referenceframe.NewLinkInFrame(referenceframe.World, testLeverOrigin, frameHotWaterLever, box)
	fs, err := referenceframe.NewFrameSystem("test", []*referenceframe.FrameSystemPart{{FrameConfig: link}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := hingeBasePose(fs, frameHotWaterLever)
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
// the robot: closed, the taught grip; turned 37.5°, where dialsim's turn ended
// with the valve pouring.
func TestHotWaterGripFollowsTheLever(t *testing.T) {
	fs, base := buildTestLeverFS(t)
	if got, want := gripAt(t, fs).Point(), (r3.Vector{X: 812.17, Y: -12.78, Z: 320.17}); !near(got, want) {
		t.Errorf("closed grip at %v, want %v", got, want)
	}
	if err := setHingeTheta(fs, frameHotWaterLever, base, defaultHotWaterOpenDegs); err != nil {
		t.Fatal(err)
	}
	open := gripAt(t, fs)
	if got, want := open.Point(), (r3.Vector{X: 823.04, Y: -12.78, Z: 357.44}); !near(got, want) {
		t.Errorf("open grip at %v, want %v — the lever turns up about world +Y", got, want)
	}
	ov := open.Orientation().OrientationVectorDegrees()
	if want := (r3.Vector{X: 0.6870, Y: -0.5446, Z: -0.4810}); !near(r3.Vector{X: ov.OX, Y: ov.OY, Z: ov.OZ}.Mul(100), want.Mul(100)) {
		t.Errorf("open grip points %v, want %v — the claws turn with the lever", ov, want)
	}

	// Turning back shut lands on the closed grip, recovered from the turned frame.
	closedBase, err := hingeClosedOrigin(fs, frameHotWaterLever, defaultHotWaterOpenDegs)
	if err != nil {
		t.Fatal(err)
	}
	if !spatialmath.PoseAlmostEqual(closedBase, base) {
		t.Errorf("recovered closed origin %v, want %v", closedBase, base)
	}
	if err := setHingeTheta(fs, frameHotWaterLever, closedBase, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := gripAt(t, fs).Point(), (r3.Vector{X: 812.17, Y: -12.78, Z: 320.17}); !near(got, want) {
		t.Errorf("grip after closing at %v, want %v", got, want)
	}
}

// TestHotWaterStandoffIsOutAlongTheLever: the claws open and back out beyond
// the lever's tip, not beside it.
func TestHotWaterStandoffIsOutAlongTheLever(t *testing.T) {
	fs, _ := buildTestLeverFS(t)
	grip := gripAt(t, fs)
	got := hotWaterStandoff(grip)
	if want := (r3.Vector{X: 686.49, Y: 68.92, Z: 314.68}); !near(got.Point(), want) {
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
