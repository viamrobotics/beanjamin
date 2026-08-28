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

// buildTestDoorFS constructs the fridge subtree through the RDK's real
// frame-system construction path, so the two-frame split every part gets is
// reproduced exactly as it is on the machine: "<name>_origin" carries the offset
// from the parent AND the collision geometry, and "<name>" is a child model
// frame that, for a geometry-only part, holds nothing. Hand-attaching geometry
// to a single frame instead does NOT match the machine, and is what hid the bug
// where the handle swept but the panel obstacle stayed frozen shut.
//
// Returns the frame system and the door origin frame's base transform, which is
// what setDoorTheta expects as its absolute reference.
func buildTestDoorFS(t *testing.T) (*referenceframe.FrameSystem, spatialmath.Pose) {
	t.Helper()

	// Panel geometry offset -300 in Y from the hinge (like the real -235 offset).
	box, err := spatialmath.NewBox(
		spatialmath.NewPoseFromPoint(r3.Vector{X: 0, Y: -300, Z: 0}),
		r3.Vector{X: 45, Y: 600, Z: 500}, "panel")
	if err != nil {
		t.Fatal(err)
	}
	// Door root at (500,0,0), identity orientation: origin == hinge.
	doorLink := referenceframe.NewLinkInFrame(referenceframe.World,
		spatialmath.NewPoseFromPoint(r3.Vector{X: 500, Y: 0, Z: 0}), "door", box)
	// Handle ball 300mm out along the door's -Y, as a child of the door.
	ballLink := referenceframe.NewLinkInFrame("door",
		spatialmath.NewPoseFromPoint(r3.Vector{X: 0, Y: -300, Z: 0}), "ball", nil)

	fs, err := referenceframe.NewFrameSystem("test",
		[]*referenceframe.FrameSystemPart{{FrameConfig: doorLink}, {FrameConfig: ballLink}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := fs.Frame("door_origin").Transform([]referenceframe.Input{})
	if err != nil {
		t.Fatal(err)
	}
	return fs, base
}

// panelWorldCenter returns the world-space center of the door panel geometry,
// resolved the way the planner does: read the geometry off the frame it lives
// on, then let the frame system lift it to world.
func panelWorldCenter(t *testing.T, fs *referenceframe.FrameSystem) r3.Vector {
	t.Helper()
	f := fs.Frame("door_origin")
	if f == nil {
		t.Fatal("door_origin not in frame system")
	}
	g, err := f.Geometries([]referenceframe.Input{})
	if err != nil {
		t.Fatal(err)
	}
	geos := g.Geometries()
	if len(geos) != 1 {
		t.Fatalf("door_origin carries %d geometries, want 1", len(geos))
	}
	tf, err := fs.Transform(referenceframe.NewZeroInputs(fs).ToLinearInputs(),
		referenceframe.NewGeometriesInFrame("door_origin", geos), referenceframe.World)
	if err != nil {
		t.Fatal(err)
	}
	return tf.(*referenceframe.GeometriesInFrame).Geometries()[0].Pose().Point()
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

// TestSetDoorTheta_PanelGeometrySweeps is the regression test for the reported
// bug: on the machine the handle swept but the panel obstacle stayed put, so
// every plan was collision-checked against a door frozen shut. The panel must
// ride the same arc as the ball.
func TestSetDoorTheta_PanelGeometrySweeps(t *testing.T) {
	fs, base := buildTestDoorFS(t)

	// The RDK puts part geometry on "<name>_origin", never on the model frame.
	// If this stops holding, setDoorTheta is rotating the wrong frame again.
	modelGeos, err := fs.Frame("door").Geometries([]referenceframe.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(modelGeos.Geometries()); n != 0 {
		t.Errorf("model frame \"door\" carries %d geometries; the RDK should put them on door_origin", n)
	}

	if before := panelWorldCenter(t, fs); before.Sub(r3.Vector{X: 500, Y: -300, Z: 0}).Norm() > 0.5 {
		t.Fatalf("closed panel center = %v, want ~(500,-300,0)", before)
	}

	if err := setDoorTheta(fs, "door", base, 90); err != nil {
		t.Fatal(err)
	}

	// Panel local (0,-300,0) rotated +90° about Z -> (300,0,0), + hinge (500,0,0):
	// the same arc the ball sweeps.
	if got := panelWorldCenter(t, fs); got.Sub(r3.Vector{X: 800, Y: 0, Z: 0}).Norm() > 0.5 {
		t.Errorf("panel world center = %v after 90° swing, want ~(800,0,0)", got)
	}
}

// TestSetDoorTheta_CloseSweepReturnsToClosed walks the whole close sweep the way
// sweepDoor does — open angle first, then every waypoint down to 0 — and checks
// the ball lands back where the closed door put it. This is what makes a close
// safe: each setDoorTheta is absolute against the authored closed transform, so
// stepping 90°→0° cannot accumulate residual rotation and leave the world model
// believing the door is ajar after it has physically shut.
func TestSetDoorTheta_CloseSweepReturnsToClosed(t *testing.T) {
	fs, base := buildTestDoorFS(t)
	closedBall := worldPoint(t, fs, "ball")
	closedPanel := panelWorldCenter(t, fs)

	sweep := computeDoorSweep(90, 0, 10)
	for _, theta := range sweep {
		if err := setDoorTheta(fs, "door", base, theta); err != nil {
			t.Fatalf("theta %v: %v", theta, err)
		}
	}

	// The sweep's first waypoint must have actually opened the door, or the
	// "returns to closed" assertions below would pass on a door that never moved.
	if closedBall.Sub(r3.Vector{X: 500, Y: -300, Z: 0}).Norm() > 0.5 {
		t.Fatalf("closed ball = %v, want ~(500,-300,0)", closedBall)
	}
	if got := worldPoint(t, fs, "ball"); got.Sub(closedBall).Norm() > 0.5 {
		t.Errorf("ball after close sweep = %v, want closed position %v", got, closedBall)
	}
	// sweepDoor's deferred undo restores θ=0 in place rather than rebuilding the
	// frame system, so returning to the authored closed pose has to be exact —
	// otherwise the stale door leaks into whatever action runs next.
	if got := panelWorldCenter(t, fs); got.Sub(closedPanel).Norm() > 0.5 {
		t.Errorf("panel after close sweep = %v, want closed position %v", got, closedPanel)
	}
}

// TestRecoverBasePoseFromSweptDoor pins the arithmetic sweepDoor uses to start a
// sweep on a door that is already open. The modeled door is left standing open
// between actions (a rebuild re-applies doorOpenDegs), so the frame no longer
// carries the authored shut transform that setDoorTheta needs as its absolute
// reference — it has to be recovered by backing the known angle out. Get this
// wrong and every subsequent sweep composes onto a rotated base and the door
// walks away from the hinge.
func TestRecoverBasePoseFromSweptDoor(t *testing.T) {
	fs, base := buildTestDoorFS(t)
	closedBall := worldPoint(t, fs, "ball")
	closedPanel := panelWorldCenter(t, fs)

	const openDegs = 75
	if err := setDoorTheta(fs, "door", base, openDegs); err != nil {
		t.Fatal(err)
	}
	openBall := worldPoint(t, fs, "ball")
	if openBall.Sub(closedBall).Norm() < 1 {
		t.Fatal("door did not open; the recovery below would be vacuous")
	}

	// What sweepDoor does: read the live (already-swung) transform, back out the
	// recorded angle, and treat the result as the absolute base.
	current, err := fs.Frame("door_origin").Transform([]referenceframe.Input{})
	if err != nil {
		t.Fatal(err)
	}
	recovered := spatialmath.Compose(current, spatialmath.PoseInverse(
		spatialmath.NewPoseFromOrientation(&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: openDegs})))
	if !spatialmath.PoseAlmostEqual(recovered, base) {
		t.Errorf("recovered base = %v, want authored base %v", recovered, base)
	}

	// Closing from the open position using the recovered base must land exactly
	// back on the authored shut pose — panel and handle together.
	if err := setDoorTheta(fs, "door", recovered, 0); err != nil {
		t.Fatal(err)
	}
	if got := worldPoint(t, fs, "ball"); got.Sub(closedBall).Norm() > 0.5 {
		t.Errorf("ball after close = %v, want %v", got, closedBall)
	}
	if got := panelWorldCenter(t, fs); got.Sub(closedPanel).Norm() > 0.5 {
		t.Errorf("panel after close = %v, want %v", got, closedPanel)
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

// TestNearestTheta pins the mid-sweep abort recovery. The sweep is now one arm
// call, so a failure does not say which waypoint it stopped on; the gripper is
// holding the handle, so the arm's joints do. Getting this wrong leaves the world
// model believing the panel is at an angle it is not, and the next plan routes the
// arm through it.
func TestNearestTheta(t *testing.T) {
	inputs := func(v float64) []referenceframe.Input {
		return []referenceframe.Input{v, 2 * v}
	}
	// Two waypoints' worth of trajectory: θ=10 then θ=20.
	positions := [][]referenceframe.Input{inputs(0), inputs(1), inputs(2), inputs(3)}
	thetaOf := []float64{10, 10, 20, 20}

	for _, tc := range []struct {
		name   string
		actual []referenceframe.Input
		want   float64
	}{
		{"stopped inside the first waypoint", inputs(0.9), 10},
		{"stopped inside the second", inputs(2.1), 20},
		{"ran to the end", inputs(3), 20},
		{"never left the start", inputs(0), 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nearestTheta(tc.actual, positions, thetaOf, -1); got != tc.want {
				t.Errorf("nearestTheta = %v, want %v", got, tc.want)
			}
		})
	}

	// Nothing planned (or nothing executed) must fall back, not report 0° — a
	// spurious 0 would claim the door is shut.
	if got := nearestTheta(inputs(0), nil, nil, 75); got != 75 {
		t.Errorf("empty trajectory = %v, want fallback 75", got)
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

// TestDoorGraspYawRatio pins that 0 and negative ratios survive as configured
// values — they are real settings (hold the orientation fixed / counter-rotate),
// and the orDefault helper the other door tunables use would swallow both.
func TestDoorGraspYawRatio(t *testing.T) {
	if got := (&beanjaminCoffee{cfg: &Config{}}).doorGraspYawRatio(); got != defaultDoorGraspYawRatio {
		t.Errorf("default yaw ratio = %v, want %v", got, defaultDoorGraspYawRatio)
	}
	for _, want := range []float64{0, -1, 0.5} {
		if got := (&beanjaminCoffee{cfg: &Config{DoorGraspYawRatio: &want}}).doorGraspYawRatio(); got != want {
			t.Errorf("configured yaw ratio = %v, want %v", got, want)
		}
	}
}

// TestYawAboutWorldZ checks the rotation lands in the world frame and not the
// tool's own: a tool pointing straight down — where a tool-frame roll would come
// out mirrored — must still yaw the way the door does, and the standoff
// translation must swing with it.
func TestYawAboutWorldZ(t *testing.T) {
	// +X offset, tool pointing straight down.
	base := spatialmath.NewPose(r3.Vector{X: 100}, &spatialmath.OrientationVectorDegrees{OZ: -1})

	if got := yawAboutWorldZ(base, 0); got.Point().Sub(base.Point()).Norm() > 1e-6 {
		t.Errorf("yaw of 0 moved the point to %v", got.Point())
	}

	// +90° about world Z sends +X to +Y and leaves the down-pointing axis alone.
	got := yawAboutWorldZ(base, 90)
	if got.Point().Sub(r3.Vector{Y: 100}).Norm() > 1e-6 {
		t.Errorf("yawed point = %v, want (0,100,0)", got.Point())
	}
	if ov := got.Orientation().OrientationVectorDegrees(); math.Abs(ov.OZ+1) > 1e-6 {
		t.Errorf("yawed OZ = %v, want -1 (still pointing down)", ov.OZ)
	}

	// The opposite sign must mirror it — the counter-rotating setting.
	if back := yawAboutWorldZ(base, -90); back.Point().Sub(r3.Vector{Y: -100}).Norm() > 1e-6 {
		t.Errorf("counter-yawed point = %v, want (0,-100,0)", back.Point())
	}
}

// TestDoorSweepLinearConstraint pins the tolerance policy. The default has to
// clear the chord-vs-arc divergence (~1.9mm at the default step), a partially
// configured constraint must fill only the missing half, and both-negative is
// the explicit opt-out — zero would be an unsatisfiable constraint, not an
// absent one.
func TestDoorSweepLinearConstraint(t *testing.T) {
	got := (&beanjaminCoffee{cfg: &Config{}}).doorSweepLinearConstraint()
	if got == nil {
		t.Fatal("default constraint = nil, want the default tolerances")
	}
	if got.LineToleranceMm != defaultDoorSweepLineMm || got.OrientationToleranceDegs != defaultDoorSweepOrientDegs {
		t.Errorf("default = %+v, want %v/%v", got, defaultDoorSweepLineMm, defaultDoorSweepOrientDegs)
	}
	if got.LineToleranceMm <= 1.9 {
		t.Errorf("default line tolerance %v does not clear the chord-vs-arc divergence", got.LineToleranceMm)
	}

	partial := &Config{DoorSweepLinearConstraint: &StepLinearConstraint{LineToleranceMm: 12}}
	got = (&beanjaminCoffee{cfg: partial}).doorSweepLinearConstraint()
	if got.LineToleranceMm != 12 {
		t.Errorf("configured line tolerance = %v, want 12", got.LineToleranceMm)
	}
	if got.OrientationToleranceDegs != defaultDoorSweepOrientDegs {
		t.Errorf("unset orientation tolerance = %v, want the default %v", got.OrientationToleranceDegs, defaultDoorSweepOrientDegs)
	}

	off := &Config{DoorSweepLinearConstraint: &StepLinearConstraint{LineToleranceMm: -1, OrientationToleranceDegs: -1}}
	if got := (&beanjaminCoffee{cfg: off}).doorSweepLinearConstraint(); got != nil {
		t.Errorf("both-negative = %+v, want nil (unconstrained sweep)", got)
	}
}

func TestDoorPlanAttempts(t *testing.T) {
	if got := (&beanjaminCoffee{cfg: &Config{}}).doorPlanAttempts(); got != defaultDoorPlanAttempts {
		t.Errorf("default attempts = %d, want %d", got, defaultDoorPlanAttempts)
	}
	if got := (&beanjaminCoffee{cfg: &Config{DoorPlanAttempts: 1}}).doorPlanAttempts(); got != 1 {
		t.Errorf("configured attempts = %d, want 1", got)
	}
}
