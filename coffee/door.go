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
	"fmt"
	"math"
	"time"

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
// hinge arc, leaving it open. Registered as the open_door execute_action.
func (s *beanjaminCoffee) openDoor(ctx, cancelCtx context.Context) error {
	return s.sweepDoor(ctx, cancelCtx, "open_door", "Opening fridge", 0, s.doorOpenAngleDegs())
}

// closeDoor is openDoor in reverse: it grips the handle where the open door
// leaves it and pushes the panel back to the hinge's closed angle. Registered
// as the close_door execute_action.
//
// The frame system always rebuilds with the door at its authored closed
// transform, so a close starts by placing the door obstacle at the open angle —
// otherwise the grasp would be resolved against a panel the world model thinks
// is shut while the real one stands open. This assumes the door is actually at
// door_open_angle_degs, which holds when close_door follows open_door.
func (s *beanjaminCoffee) closeDoor(ctx, cancelCtx context.Context) error {
	return s.sweepDoor(ctx, cancelCtx, "close_door", "Closing fridge", s.doorOpenAngleDegs(), 0)
}

// sweepDoor grips the passive fridge handle and drives the door from fromDeg to
// toDeg along its hinge arc, re-placing the static door obstacle at each swept
// angle so collision-checking stays honest. It then releases and retracts,
// leaving the door at toDeg. Both directions share this body — only the
// endpoints differ, so an open is (0 → open angle) and a close is the reverse.
//
// Runs behind executeAction, which takes the running gate, captures cancelCtx,
// and refreshes the frame system before this runs. The frame system is rebuilt
// on exit (normal or cancel) so the in-place door mutation cannot leak.
func (s *beanjaminCoffee) sweepDoor(ctx, cancelCtx context.Context, action, stepLabel string, fromDeg, toDeg float64) error {
	logger := s.logger

	// Merge both contexts so cancellation from either stops planning/execution.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(cancelCtx, func() { cancel() })
	defer stop()
	defer cancel()

	// Always rebuild the frame system afterward to discard the door mutation.
	defer func() {
		if err := s.resetFrameSystem(ctx); err != nil {
			logger.Warnf("%s: resetFrameSystem failed: %v", action, err)
		}
	}()

	if s.cfg.DoorApproachRelativePose == nil {
		return fmt.Errorf("%s requires door_approach_relative_pose", action)
	}
	s.setStep(stepLabel)

	// 1. Place the door at fromDeg, then resolve the grasp frame's pose there and
	//    derive approach + grasp from it. door_approach_relative_pose is a
	//    RelativePose offset composed onto the grasp frame's center (the door
	//    analog of cup_approach_relative_pose onto a detected cup, via
	//    composeCupPose, but resolved against a live frame). Its orientation is
	//    the grasp orientation, held fixed for the whole sweep so the gripper
	//    never twists off the handle.
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	// Captured before any mutation, so every setDoorTheta stays absolute against
	// the authored closed transform rather than accumulating rotation.
	doorFrame := fs.Frame(frameFridgeDoor)
	if doorFrame == nil {
		return fmt.Errorf("door frame %q not found", frameFridgeDoor)
	}
	baseDoorPose, err := doorFrame.Transform([]referenceframe.Input{})
	if err != nil {
		return fmt.Errorf("door base transform: %w", err)
	}
	if err := setDoorTheta(fs, frameFridgeDoor, baseDoorPose, fromDeg); err != nil {
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

	// Move to the standoff, open the jaws there, then straight to the ball
	// center, then close. Opening at the standoff and not earlier keeps the
	// wider open-gripper silhouette out of the traverse to the fridge.
	if err := s.moveToRawPose(ctx,
		&poseData{pose: approachWorld, refFrame: referenceframe.World, componentName: frameGripPoint},
		nil, nil, nil); err != nil {
		return fmt.Errorf("approach handle: %w", err)
	}
	if s.gripper != nil {
		if err := s.gripper.Open(ctx, nil); err != nil {
			return fmt.Errorf("open gripper for handle grab: %w", err)
		}
		time.Sleep(gripperPause)
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

	// 2. Sweep θ fromDeg→toDeg, re-planning each step with the door repositioned.
	sweep := computeDoorSweep(fromDeg, toDeg, s.doorPivotDegreesPerStep())
	logger.Infof("%s: sweeping %.0f°→%.0f° in %d steps", action, fromDeg, toDeg, len(sweep)-1)

	for _, theta := range sweep[1:] { // skip fromDeg — the door is already there
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
		s.savePlanRequestAndResponse(req, plan, action, err)
		if err != nil {
			return fmt.Errorf("plan %s step θ=%.0f: %w", action, theta, err)
		}
		positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
		if err != nil {
			return fmt.Errorf("frame inputs θ=%.0f: %w", theta, err)
		}
		if err := s.arm.MoveThroughJointPositions(ctx, positions, s.slowMovementMoveOptions(), nil); err != nil {
			return fmt.Errorf("execute %s step θ=%.0f: %w", action, theta, err)
		}
	}

	// 3. Release, then retract to a standoff from the handle where the sweep
	//    left it: the same approach offset resolved against the ball's pose at
	//    toDeg (fs still holds the door at the final θ), so the exit backs off
	//    exactly as the approach came in.
	if s.gripper != nil {
		if err := s.gripper.Open(ctx, nil); err != nil {
			return fmt.Errorf("release handle: %w", err)
		}
	}
	_, retractInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	ballEnd, err := s.ballWorldPose(fs, retractInputs.ToLinearInputs())
	if err != nil {
		return err
	}
	retractWorld := composeCupPose(ballEnd.Point(), approachRel)
	if err := s.moveToRawPose(ctx,
		&poseData{pose: retractWorld, refFrame: referenceframe.World, componentName: frameGripPoint},
		nil, collisions, nil); err != nil {
		return fmt.Errorf("retract: %w", err)
	}
	return nil
}
