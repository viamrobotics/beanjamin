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

func TestComputeLevelCarryWaypoints_SegmentCount(t *testing.T) {
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
	// The final waypoint is the destination pose itself (the t=1 interpolation
	// endpoint), in both position and orientation, so the carry lands exactly at
	// the configured approach pose.
	start := spatialmath.NewPose(r3.Vector{X: 0, Y: 0, Z: 500}, levelOrientation)
	endOrient := &spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 90}
	end := spatialmath.NewPose(r3.Vector{X: 600, Y: 0, Z: 500}, endOrient)

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

func TestComputeLevelCarryWaypoints_OnLineAndOrientationInterpolates(t *testing.T) {
	// A carry that both translates and changes orientation: every waypoint must
	// sit exactly on the straight line between start and end (position is linearly
	// interpolated), and the orientation must rotate monotonically from the start
	// toward the destination.
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

// gripperFS builds world -> gripper -> {grip-point, coffee-claws-middle ->
// held-item}, mirroring the real machine: grip-point sits 115mm out along the
// gripper's Z, the claws 90mm, and held-item is attached to the claws with an
// identity offset. gripperPose places the gripper in the world.
func gripperFS(t *testing.T, gripperPose spatialmath.Pose) *referenceframe.FrameSystem {
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
	add(heldItemFrameName, spatialmath.NewZeroPose(), claws)
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
