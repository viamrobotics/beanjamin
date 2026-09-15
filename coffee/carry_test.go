package coffee

import (
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

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
