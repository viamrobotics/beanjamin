package coffee

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// levelOrientation is a typical "cup upright" orientation (local Z pointing up).
var levelOrientation = &spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 0}

// Every case here starts on the world Z axis, where the azimuth is meaningless
// and the carry falls back to the chord (minCarrySweepRadiusMm) — so the counts
// are straight-line counts. TestComputeLevelCarryWaypoints_SweepsAroundBase
// covers the spacing of a real, swept carry.
func TestComputeLevelCarryWaypoints_FallbackSegmentCount(t *testing.T) {
	tests := []struct {
		name         string
		start, end   r3.Vector
		spacingMm    float64
		wantWaypoint int // total poses returned (intermediates + final)
	}{
		{
			name:         "650mm at 200mm spacing -> 4 segments, 4 poses",
			start:        r3.Vector{X: 0, Y: 0, Z: 0},
			end:          r3.Vector{X: 650, Y: 0, Z: 0},
			spacingMm:    200,
			wantWaypoint: 4, // ceil(650/200)=4 segments -> 3 intermediate + final
		},
		{
			name:         "exactly 200mm -> 1 segment, only the final pose",
			start:        r3.Vector{X: 0, Y: 0, Z: 0},
			end:          r3.Vector{X: 200, Y: 0, Z: 0},
			spacingMm:    200,
			wantWaypoint: 1,
		},
		{
			name:         "shorter than spacing -> single segment",
			start:        r3.Vector{X: 0, Y: 0, Z: 0},
			end:          r3.Vector{X: 50, Y: 0, Z: 0},
			spacingMm:    200,
			wantWaypoint: 1,
		},
		{
			name:         "just over 400mm -> 3 segments",
			start:        r3.Vector{X: 10, Y: 10, Z: 10},
			end:          r3.Vector{X: 411, Y: 10, Z: 10},
			spacingMm:    200,
			wantWaypoint: 3, // dist 401 -> ceil(401/200)=3 segments -> 2 intermediate + final
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := spatialmath.NewPose(tt.start, levelOrientation)
			end := spatialmath.NewPose(tt.end, levelOrientation)
			poses := computeLevelCarryWaypoints(start, end, tt.spacingMm)
			if len(poses) != tt.wantWaypoint {
				t.Errorf("got %d waypoints, want %d", len(poses), tt.wantWaypoint)
			}
		})
	}
}

func TestComputeLevelCarryWaypoints_FinalIsDestination(t *testing.T) {
	// The final waypoint is the destination pose itself, in both position and
	// orientation, so the carry lands exactly at the configured approach pose.
	// Off-axis endpoints, so this exercises the sweep rather than the fallback:
	// the swept path has to close on the destination as exactly as a lerp does.
	start := spatialmath.NewPose(r3.Vector{X: 420, Y: -260, Z: 500}, levelOrientation)
	endOrient := &spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 90}
	end := spatialmath.NewPose(r3.Vector{X: 180, Y: 540, Z: 500}, endOrient)

	poses := computeLevelCarryWaypoints(start, end, defaultCarryWaypointSpacingMm)

	last := poses[len(poses)-1]
	if dist := last.Point().Sub(end.Point()).Norm(); dist > 0.01 {
		t.Errorf("final waypoint position differs from destination by %.4f mm", dist)
	}
	diff := spatialmath.OrientationBetween(last.Orientation(), end.Orientation())
	if angle := diff.AxisAngles().Theta * 180.0 / math.Pi; angle > 0.1 {
		t.Errorf("final waypoint orientation differs from destination by %.4f°", angle)
	}
}

func TestComputeLevelCarryWaypoints_FallbackOnLineAndOrientationInterpolates(t *testing.T) {
	// A carry that both translates and changes orientation, starting on the world
	// Z axis so the sweep falls back to the chord: every waypoint must sit exactly
	// on the straight line between start and end, and the orientation must rotate
	// monotonically from the start toward the destination. The orientation half
	// holds for a swept carry too — only the position leaves the line.
	start := spatialmath.NewPose(r3.Vector{X: 0, Y: 0, Z: 0}, levelOrientation)
	endOrient := &spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 90}
	end := spatialmath.NewPose(r3.Vector{X: 900, Y: 300, Z: 0}, endOrient)

	poses := computeLevelCarryWaypoints(start, end, defaultCarryWaypointSpacingMm)
	if len(poses) < 2 {
		t.Fatalf("expected multiple waypoints, got %d", len(poses))
	}

	delta := end.Point().Sub(start.Point())
	prevAngle := 0.0
	for i, p := range poses {
		// On the line: cross product of (point-start) with delta is ~zero.
		rel := p.Point().Sub(start.Point())
		if cross := rel.Cross(delta).Norm(); cross > 0.01 {
			t.Errorf("waypoint %d is off the straight line (cross=%.4f)", i, cross)
		}
		// Orientation rotates monotonically away from the start toward the end.
		diff := spatialmath.OrientationBetween(start.Orientation(), p.Orientation())
		angle := diff.AxisAngles().Theta * 180.0 / math.Pi
		if angle < prevAngle-0.01 {
			t.Errorf("waypoint %d orientation not monotonic: %.4f° < previous %.4f°", i, angle, prevAngle)
		}
		prevAngle = angle
	}
	// The final waypoint reaches the destination's 90° twist.
	if math.Abs(prevAngle-90.0) > 0.1 {
		t.Errorf("final orientation rotation %.4f° differs from expected 90°", prevAngle)
	}
}

// gripperFS builds the machine's gripper chain with held-item attached at an
// identity offset — the shape it has when the geometry happens to be grasped
// world-aligned. Use gripperFSWithHeldItem for a container-rotated frame.
func gripperFS(t *testing.T, gripperPose spatialmath.Pose) *referenceframe.FrameSystem {
	t.Helper()
	return gripperFSWithHeldItem(t, gripperPose, spatialmath.NewZeroPose())
}

// gripperFSWithHeldItem builds world -> gripper -> {grip-point,
// coffee-claws-middle -> held-item}, mirroring the real machine: grip-point sits
// 115mm out along the gripper's Z and the claws 90mm, so held-item is 25mm short
// of grip-point along the tool axis. gripperPose places the gripper in the world;
// heldItemPose is the transform the container geometry gives the held-item frame
// (heldItemFramePose).
func gripperFSWithHeldItem(t *testing.T, gripperPose, heldItemPose spatialmath.Pose) *referenceframe.FrameSystem {
	t.Helper()
	fs := referenceframe.NewEmptyFrameSystem("test")
	add := func(name string, pose spatialmath.Pose, parent referenceframe.Frame) referenceframe.Frame {
		f, err := referenceframe.NewStaticFrame(name, pose)
		if err != nil {
			t.Fatalf("new %s frame: %v", name, err)
		}
		if err := fs.AddFrame(f, parent); err != nil {
			t.Fatalf("add %s frame: %v", name, err)
		}
		return f
	}
	gripper := add("gripper", gripperPose, fs.World())
	add(gripPoint, spatialmath.NewPoseFromPoint(r3.Vector{Z: 115}), gripper)
	claws := add(componentClaws, spatialmath.NewPoseFromPoint(r3.Vector{Z: 90}), gripper)
	add(heldItemFrameName, heldItemPose, claws)
	return fs
}

// The pour poses are authored for grip-point, but a no-spill carry commands the
// held-item frame. held-item sits 25mm short of grip-point along the tool axis,
// so the goal must be shifted by that offset — otherwise grip-point overshoots
// the authored pose by 25mm and executePivot refuses to pivot (max 2mm).
func TestCarryGoalForMoveFrame_ShiftsByHeldItemOffset(t *testing.T) {
	// Gripper level, tool axis (its local Z) pointing along world +X.
	gripperPose := spatialmath.NewPose(
		r3.Vector{X: 100, Y: -300, Z: 240},
		&spatialmath.OrientationVectorDegrees{OX: 1, Theta: -180},
	)
	fs := gripperFS(t, gripperPose)
	inputs := referenceframe.NewZeroInputs(fs).ToLinearInputs()

	authored := spatialmath.NewPose(
		r3.Vector{X: 160, Y: -300, Z: 240},
		&spatialmath.OrientationVectorDegrees{OX: 1, Theta: -180},
	)
	dest := &poseData{pose: authored, refFrame: referenceframe.World, componentName: gripPoint}

	goal, err := carryGoalForMoveFrame(fs, inputs, dest, heldItemFrameName)
	if err != nil {
		t.Fatalf("carryGoalForMoveFrame: %v", err)
	}

	// The tool axis points along world +X, so held-item must stop 25mm short of
	// the authored grip-point pose.
	want := r3.Vector{X: 135, Y: -300, Z: 240}
	if got := goal.Point(); got.Sub(want).Norm() > 1e-6 {
		t.Errorf("goal = %v, want %v", got, want)
	}
}

// With nothing held the carry commands grip-point itself, so the authored pose
// is already the goal and no shift may be applied.
func TestCarryGoalForMoveFrame_NoShiftWhenFrameMatches(t *testing.T) {
	fs := gripperFS(t, spatialmath.NewZeroPose())
	inputs := referenceframe.NewZeroInputs(fs).ToLinearInputs()

	authored := spatialmath.NewPose(
		r3.Vector{X: 160, Y: -300, Z: 240},
		&spatialmath.OrientationVectorDegrees{OX: 1, Theta: -180},
	)
	dest := &poseData{pose: authored, refFrame: referenceframe.World, componentName: gripPoint}

	goal, err := carryGoalForMoveFrame(fs, inputs, dest, gripPoint)
	if err != nil {
		t.Fatalf("carryGoalForMoveFrame: %v", err)
	}
	if !spatialmath.PoseAlmostEqual(goal, authored) {
		t.Errorf("goal = %v, want the authored pose %v", goal, authored)
	}
}

// The held-item frame is rotated onto the container's axes, so it is no longer
// co-oriented with grip-point. Converting the authored destination must undo that
// rotation as well as the 25mm offset: placing the gripper so held-item lands
// exactly on the returned goal has to put grip-point back on the authored pose.
func TestCarryGoalForMoveFrame_UndoesHeldItemRotation(t *testing.T) {
	// A grasp whose container axis is nowhere near the tool axis, as on the real
	// machine where the cup hangs crosswise in the claws.
	heldItemPose := spatialmath.NewPoseFromOrientation(
		&spatialmath.OrientationVectorDegrees{OX: 1, Theta: 25},
	)
	gripperPose := spatialmath.NewPose(
		r3.Vector{X: 100, Y: -300, Z: 240},
		&spatialmath.OrientationVectorDegrees{OY: 1, Theta: -180},
	)
	fs := gripperFSWithHeldItem(t, gripperPose, heldItemPose)
	inputs := referenceframe.NewZeroInputs(fs).ToLinearInputs()

	authored := spatialmath.NewPose(
		r3.Vector{X: 160, Y: -120, Z: 240},
		&spatialmath.OrientationVectorDegrees{OY: 1, Theta: -180},
	)
	dest := &poseData{pose: authored, refFrame: referenceframe.World, componentName: gripPoint}

	goal, err := carryGoalForMoveFrame(fs, inputs, dest, heldItemFrameName)
	if err != nil {
		t.Fatalf("carryGoalForMoveFrame: %v", err)
	}

	// Where the gripper has to be for held-item to sit on the goal.
	heldInGripper, err := fs.Transform(inputs,
		referenceframe.NewPoseInFrame(heldItemFrameName, spatialmath.NewZeroPose()), "gripper")
	if err != nil {
		t.Fatalf("transform held-item into gripper: %v", err)
	}
	moved := gripperFSWithHeldItem(t,
		spatialmath.Compose(goal, spatialmath.PoseInverse(heldInGripper.(*referenceframe.PoseInFrame).Pose())),
		heldItemPose)

	gp, err := moved.Transform(referenceframe.NewZeroInputs(moved).ToLinearInputs(),
		referenceframe.NewPoseInFrame(gripPoint, spatialmath.NewZeroPose()), referenceframe.World)
	if err != nil {
		t.Fatalf("transform grip-point to world: %v", err)
	}
	if got := gp.(*referenceframe.PoseInFrame).Pose(); !spatialmath.PoseAlmostEqual(got, authored) {
		t.Errorf("grip-point landed at %v, want the authored pose %v", got, authored)
	}
}

// The default spacing decides how many shaped goals a carry gets. A 600mm
// traverse — roughly the serving-area placement — is broken into 6 segments, so
// consecutive goals sit 100mm apart. Starts on the Z axis, so this measures the
// chord; a swept carry counts the same way against its own arc length.
func TestComputeLevelCarryWaypoints_FallbackDefaultSpacing(t *testing.T) {
	start := spatialmath.NewPose(r3.Vector{}, levelOrientation)
	end := spatialmath.NewPose(r3.Vector{X: 600}, levelOrientation)

	poses := computeLevelCarryWaypoints(start, end, defaultCarryWaypointSpacingMm)
	if len(poses) != 6 {
		t.Fatalf("got %d waypoints over 600mm, want 6", len(poses))
	}
	for i, p := range poses {
		if want := 100.0 * float64(i+1); math.Abs(p.Point().X-want) > 1e-6 {
			t.Errorf("waypoint %d at X=%g, want %g", i, p.Point().X, want)
		}
	}
}

// The point of the sweep: a chord between two points at similar reach cuts in
// toward the base, and the arm is standing at the machine. These are the recorded
// glass placement's endpoints, whose chord passes 115mm nearer the Z axis than
// either endpoint does. Every swept waypoint must stay within the endpoints' own
// radius band, land exactly on the destination, and respect the spacing.
func TestComputeLevelCarryWaypoints_SweepsAroundBase(t *testing.T) {
	startPt := r3.Vector{X: 275, Y: -300, Z: 230}
	endPt := r3.Vector{X: 320, Y: 490, Z: 351.4}
	start := spatialmath.NewPose(startPt, levelOrientation)
	end := spatialmath.NewPose(endPt, &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 90})

	poses := computeLevelCarryWaypoints(start, end, defaultCarryWaypointSpacingMm)
	if len(poses) < 2 {
		t.Fatalf("expected multiple waypoints, got %d", len(poses))
	}

	rAt := func(v r3.Vector) float64 { return math.Hypot(v.X, v.Y) }
	lo, hi := math.Min(rAt(startPt), rAt(endPt)), math.Max(rAt(startPt), rAt(endPt))

	prev := startPt
	for i, p := range poses {
		if r := rAt(p.Point()); r < lo-1e-6 || r > hi+1e-6 {
			t.Errorf("waypoint %d sits %.1fmm from the Z axis, outside the endpoint band %.1f..%.1f",
				i, r, lo, hi)
		}
		if step := p.Point().Sub(prev).Norm(); step > defaultCarryWaypointSpacingMm+1e-6 {
			t.Errorf("waypoint %d is %.1fmm from the previous one, over the %.0fmm spacing",
				i, step, defaultCarryWaypointSpacingMm)
		}
		prev = p.Point()
	}

	if d := poses[len(poses)-1].Point().Sub(endPt).Norm(); d > 1e-6 {
		t.Errorf("final waypoint misses the destination by %.6f mm", d)
	}

	// The straight line really does cut inside the band the sweep holds to —
	// otherwise this test would pass for the wrong reason.
	mid := startPt.Add(endPt.Sub(startPt).Mul(0.5))
	if rAt(mid) >= lo {
		t.Fatalf("fixture is not representative: the chord midpoint sits %.1fmm out, not inside %.1f",
			rAt(mid), lo)
	}
}

// Two points on the same azimuth leave nothing to sweep: interpolating radius and
// height along one half-plane is the straight line, so no special case is needed.
func TestComputeLevelCarryWaypoints_EqualAzimuthIsStraight(t *testing.T) {
	startPt := r3.Vector{X: 400, Y: 0, Z: 100}
	endPt := r3.Vector{X: 600, Y: 0, Z: 300}
	poses := computeLevelCarryWaypoints(
		spatialmath.NewPose(startPt, levelOrientation),
		spatialmath.NewPose(endPt, levelOrientation), defaultCarryWaypointSpacingMm)

	delta := endPt.Sub(startPt)
	for i, p := range poses {
		if cross := p.Point().Sub(startPt).Cross(delta).Norm(); cross > 1e-6 {
			t.Errorf("waypoint %d left the straight line (cross=%.6f)", i, cross)
		}
	}
}

// The sweep has to take the short way round, because the orientation slerp does.
// Crossing the ±180° azimuth seam is where a naive subtraction sends the
// container the long way while the cup spins the other.
func TestCarrySweepPoint_TakesShortWayRound(t *testing.T) {
	// 170° to -170°: 20° forwards across the seam, not 340° backwards.
	r := 500.0
	startPt := r3.Vector{X: r * math.Cos(math.Pi*170/180), Y: r * math.Sin(math.Pi*170/180)}
	endPt := r3.Vector{X: r * math.Cos(-math.Pi*170/180), Y: r * math.Sin(-math.Pi*170/180)}

	mid := carrySweepPoint(startPt, endPt, 0.5)
	if deg := math.Abs(math.Atan2(mid.Y, mid.X) * 180 / math.Pi); math.Abs(deg-180) > 1e-6 {
		t.Errorf("midpoint azimuth %.4f°, want ±180° (the short way across the seam)", deg)
	}
	if got := math.Hypot(mid.X, mid.Y); math.Abs(got-r) > 1e-6 {
		t.Errorf("midpoint radius %.4f, want %.1f", got, r)
	}
}

// The carry's path orientation bound has to survive whatever buildConstraints
// produced — including the nil it returns when there is nothing else to carry.
func TestWithNoSpillOrientationConstraint(t *testing.T) {
	got := withNoSpillOrientationConstraint(buildConstraints(nil, nil))
	if got == nil {
		t.Fatalf("expected constraints to be allocated when buildConstraints returns nil")
	}
	if len(got.OrientationConstraint) != 1 {
		t.Fatalf("got %d orientation constraints, want 1", len(got.OrientationConstraint))
	}
	if deg := got.OrientationConstraint[0].OrientationToleranceDegs; deg != noSpillOrientationToleranceDegs {
		t.Errorf("tolerance = %g, want %g", deg, noSpillOrientationToleranceDegs)
	}

	// The allowed collisions the carry injects must not be dropped.
	acs := []AllowedCollision{{Frame1: heldItemFrameName, Frame2: componentClaws}}
	got = withNoSpillOrientationConstraint(buildConstraints(nil, acs))
	if len(got.OrientationConstraint) != 1 {
		t.Fatalf("got %d orientation constraints, want 1", len(got.OrientationConstraint))
	}
	if len(got.CollisionSpecification) != 1 || len(got.CollisionSpecification[0].Allows) != 1 {
		t.Errorf("allowed collisions were dropped: %+v", got.CollisionSpecification)
	}
}
