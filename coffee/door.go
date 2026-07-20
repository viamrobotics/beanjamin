package coffee

// Fridge-door open: the arm grips the passive handle and pulls the door open
// along its hinge arc. The door is a static obstacle whose root frame origin
// sits on the hinge (verified via the live frame system — see the door-open
// design/plan docs), so rotating that frame about its local Z pivots the panel
// about the hinge. The handle chain (fridge-handle-top → -lower-bar → -ball)
// hangs off the door subtree and rides the rotation. θ is swept in software;
// at each step we re-place the static door obstacle at θ, read the handle
// ball's new world pose, and move the gripper to track it.

import (
	"context"
	"errors"
	"fmt"
	"math"

	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

const (
	// frameFridgeDoor is the static door obstacle whose origin is the hinge.
	frameFridgeDoor = "fridge-door"
	// frameFridgeHandleBall is the default grasp-target knob at the end of the
	// handle chain (fridge-handle-top → -lower-bar → -ball). Overridable via
	// Config.DoorGraspFrameName.
	frameFridgeHandleBall = "fridge-handle-ball"

	// frameGripPoint is the gripper's tool-center frame — the frame commanded to
	// the grasp frame's center and tracked through the swing. Approach, grasp,
	// and retract all derive from the ball frame (door_approach_relative_pose);
	// no poses are authored on the switch for open_door.
	frameGripPoint = "grip-point"
)

// computeDoorSweep returns inclusive absolute-angle waypoints (degrees) from
// closedDeg to openDeg, one every ~degPerStep. The first waypoint is closedDeg
// and the last is exactly openDeg. Direction follows the sign of travel, so it
// also works when openDeg < closedDeg (a future close sweep). Mirrors the
// step-count logic of computePivotPoses (motion.go).
func computeDoorSweep(closedDeg, openDeg, degPerStep float64) []float64 {
	total := math.Abs(openDeg - closedDeg)
	numSteps := max(1, int(math.Round(total/degPerStep)))
	out := make([]float64, numSteps+1)
	for i := 0; i <= numSteps; i++ {
		t := float64(i) / float64(numSteps)
		out[i] = closedDeg + (openDeg-closedDeg)*t
	}
	return out
}

// setDoorTheta re-places the static door obstacle at thetaDeg about its own
// origin (the hinge). It composes Rz(θ) onto the door's original closed
// transform, then reuses the lockFilterFrame maneuver (motion.go): capture the
// door's descendants, remove the door, re-add it rotated, and re-attach the
// descendants (the handle chain) with their local transforms unchanged so they
// ride the swing. The door frame's own geometry (the panel, offset from the
// hinge) is preserved across the swap.
//
// baseDoorPose MUST be the door's original closed parent-relative transform,
// captured once by the caller and passed on every call, so repeated calls stay
// absolute rather than accumulating rotation.
func setDoorTheta(fs *referenceframe.FrameSystem, doorFrameName string, baseDoorPose spatialmath.Pose, thetaDeg float64) error {
	door := fs.Frame(doorFrameName)
	if door == nil {
		return fmt.Errorf("door frame %q not found", doorFrameName)
	}
	parent, err := fs.Parent(door)
	if err != nil {
		return fmt.Errorf("door parent: %w", err)
	}

	// Rotation about the door frame's local Z, applied at the origin (the hinge).
	rz := spatialmath.NewPoseFromOrientation(&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: thetaDeg})
	rotated := spatialmath.Compose(baseDoorPose, rz)

	// Preserve the door frame's own geometry (the panel), if any.
	var geom spatialmath.Geometry
	if geos, gerr := door.Geometries([]referenceframe.Input{}); gerr == nil && geos != nil && len(geos.Geometries()) > 0 {
		geom = geos.Geometries()[0]
	}

	descendants := collectDescendants(fs, doorFrameName)
	fs.RemoveFrame(door)

	var newDoor referenceframe.Frame
	if geom != nil {
		newDoor, err = referenceframe.NewStaticFrameWithGeometry(doorFrameName, rotated, geom)
	} else {
		newDoor, err = referenceframe.NewStaticFrame(doorFrameName, rotated)
	}
	if err != nil {
		return fmt.Errorf("build rotated door frame: %w", err)
	}
	if err := fs.AddFrame(newDoor, parent); err != nil {
		return fmt.Errorf("re-add door frame: %w", err)
	}
	for _, d := range descendants {
		p := fs.Frame(d.parentName)
		if err := fs.AddFrame(d.frame, p); err != nil {
			return fmt.Errorf("re-attach descendant %q under %q: %w", d.frame.Name(), d.parentName, err)
		}
	}
	return nil
}

// ballWorldPose returns the grasp frame's (handle ball's) current world pose
// from fs — its point is the grasp target the gripper tracks through the sweep.
func (s *beanjaminCoffee) ballWorldPose(fs *referenceframe.FrameSystem, inputs *referenceframe.LinearInputs) (spatialmath.Pose, error) {
	tf, err := fs.Transform(inputs,
		referenceframe.NewPoseInFrame(s.doorGraspFrameName(), spatialmath.NewZeroPose()),
		referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("grasp frame %q to world: %w", s.doorGraspFrameName(), err)
	}
	return tf.(*referenceframe.PoseInFrame).Pose(), nil
}

// openDoor grips the passive fridge handle and pulls the door open along its
// hinge arc, re-placing the static door obstacle at each swept angle so
// collision-checking stays honest. It then releases and retracts, leaving the
// door open. Registered as an execute_action action, so the executeAction
// wrapper takes the running gate, captures cancelCtx, and refreshes the frame
// system before this runs. The frame system is rebuilt on exit (normal or
// cancel) so the in-place door mutation cannot leak.
func (s *beanjaminCoffee) openDoor(ctx, cancelCtx context.Context) error {
	logger := s.logger

	// Merge both contexts so cancellation from either stops planning/execution.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(cancelCtx, func() { cancel() })
	defer stop()
	defer cancel()

	// Always rebuild the frame system afterward to discard the door mutation.
	defer func() {
		if err := s.resetFrameSystem(ctx); err != nil {
			logger.Warnf("open_door: resetFrameSystem failed: %v", err)
		}
	}()

	if s.cfg.DoorApproachRelativePose == nil {
		return errors.New("open_door requires door_approach_relative_pose")
	}
	s.setStep("Opening fridge")

	// 1. Resolve the grasp frame's (closed) pose, then derive approach + grasp
	//    from it. door_approach_relative_pose is a RelativePose offset composed
	//    onto the grasp frame's center (the door analog of
	//    cup_approach_relative_pose onto a detected cup, via composeCupPose, but
	//    resolved against a live frame). Its orientation is the grasp orientation,
	//    held fixed for the whole open so the gripper never twists off the handle.
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	ballBase, err := s.ballWorldPose(fs, fsInputs.ToLinearInputs())
	if err != nil {
		return err
	}
	approachRel := relativePoseToSpatial(s.cfg.DoorApproachRelativePose)
	approachWorld := composeCupPose(ballBase.Point(), approachRel)
	graspOrient := approachRel.Orientation()
	graspWorld := spatialmath.NewPose(ballBase.Point(), graspOrient)
	collisions := s.filterFakeModeCollisions(doorOpenCollisions(s.doorGraspFrameName()))

	// Move to the standoff, then straight to the ball center, then close.
	if err := s.moveToRawPose(ctx,
		&poseData{pose: approachWorld, refFrame: referenceframe.World, componentName: frameGripPoint},
		nil, nil, nil); err != nil {
		return fmt.Errorf("approach handle: %w", err)
	}
	if err := s.moveToRawPose(ctx,
		&poseData{pose: graspWorld, refFrame: referenceframe.World, componentName: frameGripPoint},
		nil, collisions, nil); err != nil {
		return fmt.Errorf("move to grasp (ball center): %w", err)
	}
	if s.gripper != nil {
		if _, err := s.gripper.Grab(ctx, nil); err != nil {
			return fmt.Errorf("grab handle: %w", err)
		}
	}

	// 2. Door base transform, captured once so each setDoorTheta stays absolute.
	doorFrame := fs.Frame(frameFridgeDoor)
	if doorFrame == nil {
		return fmt.Errorf("door frame %q not found", frameFridgeDoor)
	}
	baseDoorPose, err := doorFrame.Transform([]referenceframe.Input{})
	if err != nil {
		return fmt.Errorf("door base transform: %w", err)
	}

	// 3. Sweep θ closed→open, re-planning each step with the door repositioned.
	sweep := computeDoorSweep(0, s.doorOpenAngleDegs(), s.doorPivotDegreesPerStep())
	logger.Infof("open_door: sweeping %.0f° in %d steps", s.doorOpenAngleDegs(), len(sweep)-1)

	for _, theta := range sweep[1:] { // skip 0° — already there
		if err := setDoorTheta(fs, frameFridgeDoor, baseDoorPose, theta); err != nil {
			return err
		}
		// Fresh joint inputs (the arm moved last step); fs is the mutated cachedFS.
		_, inNow, err := s.currentInputs(ctx)
		if err != nil {
			return err
		}
		linNow := inNow.ToLinearInputs()
		ballNow, err := s.ballWorldPose(fs, linNow)
		if err != nil {
			return err
		}
		// Track only the ball's point through the swing while holding the grasp
		// orientation fixed. The handle knob is spherical, so the grasp doesn't
		// constrain wrist roll; letting grip-point ride the door panel's rotation
		// (Compose with the rigid grasp offset) twisted the wrist off the handle.
		// Commanding a constant tool orientation and following only the point
		// keeps the gripper square to the handle the whole way.
		goalPose := spatialmath.NewPose(ballNow.Point(), graspOrient)
		goal := armplanning.NewPlanState(referenceframe.FrameSystemPoses{
			frameGripPoint: referenceframe.NewPoseInFrame(referenceframe.World, goalPose),
		}, nil)

		req := &armplanning.PlanRequest{
			FrameSystem: fs,
			Goals:       []*armplanning.PlanState{goal},
			StartState:  armplanning.NewPlanState(nil, inNow),
			Constraints: buildConstraints(nil, collisions),
		}
		plan, _, err := armplanning.PlanMotion(ctx, logger, req)
		s.savePlanRequestAndResponse(req, plan, "open_door", err)
		if err != nil {
			return fmt.Errorf("plan open_door step θ=%.0f: %w", theta, err)
		}
		positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
		if err != nil {
			return fmt.Errorf("frame inputs θ=%.0f: %w", theta, err)
		}
		if err := s.arm.MoveThroughJointPositions(ctx, positions, s.slowMovementMoveOptions(), nil); err != nil {
			return fmt.Errorf("execute open_door step θ=%.0f: %w", theta, err)
		}
	}

	// 4. Release, then retract to a standoff from the open handle: the same
	//    approach offset resolved against the ball's OPEN pose (fs still holds the
	//    door at the final θ), so the exit backs off exactly as the approach came
	//    in. Leaves the door open.
	if s.gripper != nil {
		if err := s.gripper.Open(ctx, nil); err != nil {
			return fmt.Errorf("release handle: %w", err)
		}
	}
	_, retractInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	ballOpen, err := s.ballWorldPose(fs, retractInputs.ToLinearInputs())
	if err != nil {
		return err
	}
	retractWorld := composeCupPose(ballOpen.Point(), approachRel)
	if err := s.moveToRawPose(ctx,
		&poseData{pose: retractWorld, refFrame: referenceframe.World, componentName: frameGripPoint},
		nil, collisions, nil); err != nil {
		return fmt.Errorf("retract: %w", err)
	}
	return nil
}
