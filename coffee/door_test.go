package coffee

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

func TestComputeDoorSweep_StepCountAndEndpoints(t *testing.T) {
	got := computeDoorSweep(0, 90, 10) // round(90/10)=9 steps -> 10 waypoints
	if len(got) != 10 {
		t.Fatalf("len = %d, want 10", len(got))
	}
	if got[0] != 0 {
		t.Errorf("first = %v, want 0", got[0])
	}
	if math.Abs(got[len(got)-1]-90) > 1e-9 {
		t.Errorf("last = %v, want 90", got[len(got)-1])
	}
}

func TestComputeDoorSweep_Monotonic(t *testing.T) {
	got := computeDoorSweep(0, 90, 15)
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("not increasing at %d: %v then %v", i, got[i-1], got[i])
		}
	}
}

func TestComputeDoorSweep_Reverse(t *testing.T) {
	got := computeDoorSweep(90, 0, 10) // a close sweep
	if got[0] != 90 || math.Abs(got[len(got)-1]) > 1e-9 {
		t.Errorf("reverse sweep endpoints = (%v..%v), want (90..0)", got[0], got[len(got)-1])
	}
}

// buildTestDoorFS constructs a minimal frame system with a door whose origin is
// the hinge, a geometry panel offset from the hinge, and a handle-ball child
// hanging off the panel edge — mirroring the real fridge subtree.
func buildTestDoorFS(t *testing.T) (*referenceframe.FrameSystem, spatialmath.Pose) {
	t.Helper()
	fs := referenceframe.NewEmptyFrameSystem("test")

	// Door root at (500,0,0), identity orientation: origin == hinge.
	doorPose := spatialmath.NewPoseFromPoint(r3.Vector{X: 500, Y: 0, Z: 0})
	// Panel geometry offset -300 in Y from the hinge (like the real -235 offset).
	box, err := spatialmath.NewBox(
		spatialmath.NewPoseFromPoint(r3.Vector{X: 0, Y: -300, Z: 0}),
		r3.Vector{X: 45, Y: 600, Z: 500}, "panel")
	if err != nil {
		t.Fatal(err)
	}
	door, err := referenceframe.NewStaticFrameWithGeometry("door", doorPose, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.AddFrame(door, fs.World()); err != nil {
		t.Fatal(err)
	}

	// Handle ball 300mm out along the door's -Y, as a (grand)child of the door.
	ball, err := referenceframe.NewStaticFrame("ball",
		spatialmath.NewPoseFromPoint(r3.Vector{X: 0, Y: -300, Z: 0}))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.AddFrame(ball, door); err != nil {
		t.Fatal(err)
	}
	return fs, doorPose
}

func worldPoint(t *testing.T, fs *referenceframe.FrameSystem, frame string) r3.Vector {
	t.Helper()
	tf, err := fs.Transform(referenceframe.NewZeroInputs(fs).ToLinearInputs(),
		referenceframe.NewPoseInFrame(frame, spatialmath.NewZeroPose()),
		referenceframe.World)
	if err != nil {
		t.Fatal(err)
	}
	return tf.(*referenceframe.PoseInFrame).Pose().Point()
}

func TestSetDoorTheta_BallSweepsArc(t *testing.T) {
	fs, base := buildTestDoorFS(t)

	if err := setDoorTheta(fs, "door", base, 90); err != nil {
		t.Fatal(err)
	}

	// Ball local (0,-300,0) rotated +90° about Z -> (300,0,0), + hinge (500,0,0).
	got := worldPoint(t, fs, "ball")
	want := r3.Vector{X: 800, Y: 0, Z: 0}
	if got.Sub(want).Norm() > 0.5 {
		t.Errorf("ball world = %v, want ~%v", got, want)
	}

	// Hinge (door origin) must be unchanged — pure rotation about the origin.
	if origin := worldPoint(t, fs, "door"); origin.Sub(r3.Vector{X: 500}).Norm() > 0.01 {
		t.Errorf("door origin moved to %v, want (500,0,0)", origin)
	}
}

func TestSetDoorTheta_PanelGeometrySweeps(t *testing.T) {
	fs, base := buildTestDoorFS(t)
	if err := setDoorTheta(fs, "door", base, 90); err != nil {
		t.Fatal(err)
	}

	// 1. The door geometry must survive the remove/re-add with its local offset
	//    intact (Frame.Geometries returns local coords).
	geos, err := fs.Frame("door").Geometries([]referenceframe.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if len(geos.Geometries()) == 0 {
		t.Fatal("door geometry lost across setDoorTheta")
	}
	localCenter := geos.Geometries()[0].Pose()
	if localCenter.Point().Sub(r3.Vector{X: 0, Y: -300, Z: 0}).Norm() > 0.01 {
		t.Errorf("panel local offset = %v, want (0,-300,0)", localCenter.Point())
	}

	// 2. That preserved local point must ride the rotation to world (800,0,0),
	//    the same arc as the ball — via the PoseInFrame path (frame→world).
	tf, err := fs.Transform(referenceframe.NewZeroInputs(fs).ToLinearInputs(),
		referenceframe.NewPoseInFrame("door", localCenter),
		referenceframe.World)
	if err != nil {
		t.Fatal(err)
	}
	world := tf.(*referenceframe.PoseInFrame).Pose().Point()
	if world.Sub(r3.Vector{X: 800, Y: 0, Z: 0}).Norm() > 0.5 {
		t.Errorf("panel world center = %v, want ~(800,0,0)", world)
	}
}

// TestGraspTracksBallPointFixedOrientation pins the contract openDoor uses
// through the swing: the grip-point goal tracks the ball's *point* but keeps the
// grasp orientation fixed. The handle knob is spherical, so the grasp doesn't
// constrain wrist roll; letting the gripper ride the ball's rotation twisted the
// wrist off the handle, so the goal orientation must stay the grasp orientation
// regardless of how far the ball's own frame has rotated.
func TestGraspTracksBallPointFixedOrientation(t *testing.T) {
	// The fixed grasp orientation (what approachRel.Orientation() supplies).
	graspOrient := &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 45}

	// The ball sweeps: its point moves to (100,0,0) and its own frame rotates
	// +90° about Z as the door panel turns.
	ballNow := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 0, Z: 0},
		&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 90})

	goalPose := spatialmath.NewPose(ballNow.Point(), graspOrient)

	// 1. The goal tracks the ball's point exactly.
	if goalPose.Point().Sub(ballNow.Point()).Norm() > 0.5 {
		t.Errorf("goal point = %v, want ball point %v", goalPose.Point(), ballNow.Point())
	}
	// 2. The goal orientation is the fixed grasp orientation...
	if !spatialmath.OrientationAlmostEqual(goalPose.Orientation(), graspOrient) {
		t.Errorf("goal orientation = %v, want fixed grasp orientation %v",
			goalPose.Orientation(), graspOrient)
	}
	// 3. ...and did NOT follow the ball's rotation — the whole point of the fix.
	if spatialmath.OrientationAlmostEqual(goalPose.Orientation(), ballNow.Orientation()) {
		t.Error("goal orientation followed the ball's rotation; it must stay fixed to the grasp")
	}
}

func TestDoorGetters_Defaults(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	if got := s.doorOpenAngleDegs(); got != 90 {
		t.Errorf("doorOpenAngleDegs default = %v, want 90", got)
	}
	if got := s.doorPivotDegreesPerStep(); got != 10 {
		t.Errorf("doorPivotDegreesPerStep default = %v, want 10", got)
	}
}

func TestDoorGetters_Configured(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{DoorOpenAngleDegs: 75, DoorPivotDegreesPerStep: 5}}
	if got := s.doorOpenAngleDegs(); got != 75 {
		t.Errorf("doorOpenAngleDegs = %v, want 75", got)
	}
	if got := s.doorPivotDegreesPerStep(); got != 5 {
		t.Errorf("doorPivotDegreesPerStep = %v, want 5", got)
	}
}

// TestDoorApproachFromBall pins how approach/grasp derive from the ball frame:
// grasp sits at the ball center with the relative pose's orientation; approach
// is offset from the ball center by the relative pose's translation.
func TestDoorApproachFromBall(t *testing.T) {
	ballPoint := r3.Vector{X: 200, Y: 50, Z: 400}
	rel := &RelativePose{X: 0, Y: -120, Z: 0, OX: 0, OY: 0, OZ: 1, Theta: 30}
	relSpatial := relativePoseToSpatial(rel)

	grasp := spatialmath.NewPose(ballPoint, relSpatial.Orientation())
	if grasp.Point().Sub(ballPoint).Norm() > 0.01 {
		t.Errorf("grasp point = %v, want ball center %v", grasp.Point(), ballPoint)
	}

	approach := composeCupPose(ballPoint, relSpatial)
	wantApproach := r3.Vector{X: 200, Y: -70, Z: 400} // ball + (0,-120,0)
	if approach.Point().Sub(wantApproach).Norm() > 0.01 {
		t.Errorf("approach point = %v, want %v", approach.Point(), wantApproach)
	}
}

func TestDoorGraspFrameName(t *testing.T) {
	if got := (&beanjaminCoffee{cfg: &Config{}}).doorGraspFrameName(); got != frameFridgeHandleBall {
		t.Errorf("default grasp frame = %q, want %q", got, frameFridgeHandleBall)
	}
	if got := (&beanjaminCoffee{cfg: &Config{DoorGraspFrameName: "custom-knob"}}).doorGraspFrameName(); got != "custom-knob" {
		t.Errorf("configured grasp frame = %q, want custom-knob", got)
	}
}
