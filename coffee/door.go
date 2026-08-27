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

// doorOriginFrameName is the frame setDoorTheta actually rotates. The RDK splits
// every frame-system part in two: "<name>_origin" carries the part's offset from
// its parent AND its collision geometry (a tailGeometryStaticFrame), while
// "<name>" is a child model frame that, for a geometry-only part like the door,
// is a zero static frame holding nothing. Rotating "<name>" therefore swings the
// handle subtree hanging off it but leaves the door panel obstacle frozen at its
// closed pose — the world model claims the fridge is shut while the arm pulls it
// open. Rotating "<name>_origin" moves the panel and the handle chain together.
// Same convention lockFilterFrame relies on (motion.go).
func doorOriginFrameName(doorFrameName string) string { return doorFrameName + "_origin" }

// setDoorTheta re-places the door at thetaDeg about the hinge. It composes Rz(θ)
// onto the door origin frame's original closed transform, then reuses the
// lockFilterFrame maneuver (motion.go): capture descendants, remove the frame,
// re-add it rotated, and re-attach the descendants (the model frame and the
// handle chain below it) with their local transforms unchanged so they ride the
// swing. Because Rz is composed onto the END of the origin transform, the pivot
// is the origin frame's endpoint — the hinge.
//
// baseOriginPose MUST be the door origin frame's original closed parent-relative
// transform, captured once by the caller and passed on every call, so repeated
// calls stay absolute rather than accumulating rotation.
func setDoorTheta(fs *referenceframe.FrameSystem, doorFrameName string, baseOriginPose spatialmath.Pose, thetaDeg float64) error {
	originName := doorOriginFrameName(doorFrameName)
	origin := fs.Frame(originName)
	if origin == nil {
		return fmt.Errorf("door origin frame %q not found", originName)
	}
	parent, err := fs.Parent(origin)
	if err != nil {
		return fmt.Errorf("door origin parent: %w", err)
	}

	rz := spatialmath.NewPoseFromOrientation(&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: thetaDeg})
	rotated := spatialmath.Compose(baseOriginPose, rz)

	// Re-express the panel geometry for the rotated transform. Both frame types
	// this sees report their geometry already composed with their own transform,
	// i.e. in parent coordinates: a tailGeometryStaticFrame by definition, and the
	// plain static replacement built below because it deliberately stores geometry
	// that way (the frame system skips a frame's own transform when resolving
	// GeometriesInFrame, so parent coordinates are what land it in the right
	// world position — the pattern lockFilterFrame uses).
	//
	// Undo the frame's CURRENT transform, not baseOriginPose, to recover the
	// hinge-local panel. Using the base would be correct only on the first call;
	// from the second onward the frame already carries the previous θ and the
	// rotation would compound, walking the panel away from the handle over a sweep.
	var geom spatialmath.Geometry
	current, err := origin.Transform([]referenceframe.Input{})
	if err != nil {
		return fmt.Errorf("door origin current transform: %w", err)
	}
	if geos, gerr := origin.Geometries([]referenceframe.Input{}); gerr == nil && geos != nil && len(geos.Geometries()) > 0 {
		local := geos.Geometries()[0].Transform(spatialmath.PoseInverse(current))
		geom = local.Transform(rotated)
	}

	descendants := collectDescendants(fs, originName)
	fs.RemoveFrame(origin)

	var newOrigin referenceframe.Frame
	if geom != nil {
		newOrigin, err = referenceframe.NewStaticFrameWithGeometry(originName, rotated, geom)
	} else {
		newOrigin, err = referenceframe.NewStaticFrame(originName, rotated)
	}
	if err != nil {
		return fmt.Errorf("build rotated door frame: %w", err)
	}
	if err := fs.AddFrame(newOrigin, parent); err != nil {
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

// doorBasePose returns the fridge door's authored shut transform — the absolute
// reference every setDoorTheta composes against. Valid only on a frame system
// that has not been swung yet (a fresh rebuild); sweepDoor recovers it from the
// live frame instead, which may already carry an angle.
func doorBasePose(fs *referenceframe.FrameSystem) (spatialmath.Pose, error) {
	name := doorOriginFrameName(frameFridgeDoor)
	f := fs.Frame(name)
	if f == nil {
		return nil, fmt.Errorf("door origin frame %q not found", name)
	}
	return f.Transform([]referenceframe.Input{})
}

// openDoor grips the passive fridge handle and pulls the door open along its
// hinge arc, leaving it open. Registered as the open_door execute_action.
func (s *beanjaminCoffee) openDoor(ctx, cancelCtx context.Context) error {
	return s.sweepDoor(ctx, cancelCtx, "open_door", "Opening fridge", s.doorOpenAngleDegs())
}

// closeDoor is openDoor in reverse: it grips the handle where the open door left
// it and pushes the panel back shut. Registered as the close_door execute_action.
func (s *beanjaminCoffee) closeDoor(ctx, cancelCtx context.Context) error {
	return s.sweepDoor(ctx, cancelCtx, "close_door", "Closing fridge", 0)
}

// doorThetaFromArm maps the arm's live joint configuration back to the sweep
// angle it belongs to, by finding the nearest configuration in the trajectory that
// was executed. thetaOf must be parallel to positions.
//
// A concatenated sweep is one arm call, so an abort no longer reports which
// waypoint it stopped on — but the gripper is rigidly holding the handle, so the
// joints do say how far the panel actually travelled. Guessing the target instead
// would leave the world model claiming a physical obstacle is somewhere it is not.
// Falls back to fallbackDeg (the sweep's start) if the arm cannot be read.
func (s *beanjaminCoffee) doorThetaFromArm(ctx context.Context, positions [][]referenceframe.Input, thetaOf []float64, fallbackDeg float64) float64 {
	// Reached on an aborted move, so ctx is usually already cancelled.
	actual, err := s.arm.CurrentInputs(context.WithoutCancel(ctx))
	if err != nil {
		s.logger.Warnf("could not read arm to locate the door after a failed sweep, assuming θ=%.0f: %v", fallbackDeg, err)
		return fallbackDeg
	}
	return nearestTheta(actual, positions, thetaOf, fallbackDeg)
}

// nearestTheta returns thetaOf[i] for the positions[i] closest to actual in joint
// space, or fallbackDeg if there is nothing to match against.
func nearestTheta(actual []referenceframe.Input, positions [][]referenceframe.Input, thetaOf []float64, fallbackDeg float64) float64 {
	best, bestDist := fallbackDeg, math.Inf(1)
	for i, p := range positions {
		if d := referenceframe.InputsL2Distance(actual, p); d < bestDist {
			best, bestDist = thetaOf[i], d
		}
	}
	return best
}

// sweepDoor grips the passive fridge handle and drives the door from wherever it
// currently stands (s.doorOpenDegs) to toDeg along its hinge arc, re-placing the
// door obstacle at each swept angle so collision-checking stays honest. Both
// directions share this body — only the target differs, so an open is
// (current → open angle) and a close is (current → 0).
//
// The whole arc is planned before the arm moves and executed as one trajectory:
// per-waypoint execution made the swing visibly clunky, since each waypoint hop
// was its own MoveThroughJointPositions and the arm accelerated and stopped ~9
// times across a 90° sweep.
//
// The modeled door is left where the sweep left it, and s.doorOpenDegs records
// it. Snapping the model shut on the way out would be a lie: opening a door does
// not close it, and every later plan would route the arm through a panel that is
// really standing open. Only reset_world clears that record. The cost is that a
// half-finished sweep leaves the model at the last angle actually reached, which
// is the honest answer — the operator, not this function, knows where the door
// ended up.
//
// Runs behind executeAction, which takes the running gate, captures cancelCtx,
// and refreshes the frame system before this runs.
func (s *beanjaminCoffee) sweepDoor(ctx, cancelCtx context.Context, action, stepLabel string, toDeg float64) error {
	logger := s.logger

	// Merge both contexts so cancellation from either stops planning/execution.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(cancelCtx, func() { cancel() })
	defer stop()
	defer cancel()

	if s.cfg.DoorApproachRelativePose == nil {
		return fmt.Errorf("%s requires door_approach_relative_pose", action)
	}
	s.setStep(stepLabel)

	// 1. Resolve the grasp frame's pose where the door currently stands, then
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
	// The frame already carries doorOpenDegs, so back that rotation out to recover
	// the authored shut transform. Every setDoorTheta composes against that
	// absolute reference, so a sweep cannot accumulate rotation.
	fromDeg := s.doorOpenDegs
	originName := doorOriginFrameName(frameFridgeDoor)
	originFrame := fs.Frame(originName)
	if originFrame == nil {
		return fmt.Errorf("door origin frame %q not found", originName)
	}
	currentOriginPose, err := originFrame.Transform([]referenceframe.Input{})
	if err != nil {
		return fmt.Errorf("door base transform: %w", err)
	}
	baseOriginPose := spatialmath.Compose(currentOriginPose,
		spatialmath.PoseInverse(spatialmath.NewPoseFromOrientation(
			&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: fromDeg})))

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
		time.Sleep(gripperPause)
	}

	// 2. Plan the whole θ fromDeg→toDeg sweep, then execute the concatenated
	//    trajectory in one arm call. Each waypoint still gets its own plan against
	//    the door re-placed at that θ — a PlanRequest carries one frame system, so
	//    a single multi-goal plan (what executePivot does) could only be
	//    collision-checked against one door angle, and the door is the obstacle
	//    that moves. Chaining each plan's end configuration into the next start
	//    state (withArmInputs, the same way cup_pickup chains approach into grab)
	//    keeps that per-θ honesty while producing one continuous trajectory, so the
	//    arm no longer decelerates to a stop at every waypoint.
	sweep := computeDoorSweep(fromDeg, toDeg, s.doorPivotDegreesPerStep())
	logger.Infof("%s: planning %.0f°→%.0f° in %d steps", action, fromDeg, toDeg, len(sweep)-1)

	// Read the arm once, here: the sweep starts from the grasp configuration, not
	// the pre-approach one fsInputs holds. From then on planInputs advances through
	// the plans rather than being re-read, because the arm does not move until the
	// whole sweep is planned — live joints would be the same start state for every
	// waypoint and each plan would jump back to the handle's closed position.
	_, planInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	var positions [][]referenceframe.Input
	// thetaOf[i] is the door angle positions[i] belongs to, for locating the panel
	// again if execution aborts partway.
	var thetaOf []float64

	for _, theta := range sweep[1:] { // skip fromDeg — the door is already there
		if err := setDoorTheta(fs, frameFridgeDoor, baseOriginPose, theta); err != nil {
			return err
		}
		ballNow, err := s.ballWorldPose(fs, planInputs.ToLinearInputs())
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
			StartState:  armplanning.NewPlanState(nil, planInputs),
			Constraints: buildConstraints(nil, collisions),
		}
		plan, _, err := armplanning.PlanMotion(ctx, logger, req)
		s.savePlanRequestAndResponse(req, plan, action, err)
		if err != nil {
			return fmt.Errorf("plan %s step θ=%.0f: %w", action, theta, err)
		}
		stepPositions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
		if err != nil {
			return fmt.Errorf("frame inputs θ=%.0f: %w", theta, err)
		}
		if len(stepPositions) == 0 {
			return fmt.Errorf("%s step θ=%.0f planned an empty trajectory", action, theta)
		}
		positions = append(positions, stepPositions...)
		for range stepPositions {
			thetaOf = append(thetaOf, theta)
		}
		planInputs = s.withArmInputs(planInputs, stepPositions[len(stepPositions)-1])
	}

	logger.Infof("%s: executing %d concatenated waypoints", action, len(positions))
	if err := s.arm.MoveThroughJointPositions(ctx, positions, s.slowMovementMoveOptions(), nil); err != nil {
		// One arm call no longer reports which waypoint it died on, so recover the
		// angle from where the arm actually stopped rather than assuming the target.
		// Both fs and doorOpenDegs have to land on it: fs is the cached frame system
		// every later plan is checked against, and doorOpenDegs is what a rebuild
		// re-applies.
		reached := s.doorThetaFromArm(ctx, positions, thetaOf, fromDeg)
		s.doorOpenDegs = reached
		if serr := setDoorTheta(fs, frameFridgeDoor, baseOriginPose, reached); serr != nil {
			logger.Errorf("could not re-place the door model at θ=%.0f after a failed sweep; "+
				"run reset_world before moving again: %v", reached, serr)
		}
		return fmt.Errorf("execute %s sweep: %w", action, err)
	}
	s.doorOpenDegs = toDeg

	// 3. Release, retract to a standoff from the handle where the sweep left it
	//    (the same approach offset resolved against the ball's pose at toDeg — fs
	//    still holds the door at the final θ — so the exit backs off exactly as the
	//    approach came in), then close the jaws for whatever moves next.
	if s.gripper != nil {
		if err := s.gripper.Open(ctx, nil); err != nil {
			return fmt.Errorf("release handle: %w", err)
		}
		// The jaws have to clear the handle before the retract pulls away, or the
		// arm backs off still clamped and wrenches the door against its hinge.
		time.Sleep(gripperPause)
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
	// An open gripper has a wider collision silhouette than the allowed-collision
	// tuning assumes, and nothing downstream of a standalone open_door/close_door
	// normalizes it (normalizeGripperAtStart only guards a brew cycle). Leaving the
	// jaws open here also defeats step 1's own standoff rule on the next sweep:
	// close_door would traverse to the fridge wide open.
	if s.gripper != nil {
		if _, err := s.gripper.Grab(ctx, nil); err != nil {
			return fmt.Errorf("close gripper after releasing handle: %w", err)
		}
		time.Sleep(gripperPause)
	}
	return nil
}
