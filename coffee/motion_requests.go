package coffee

// Motion requests: every kind of arm motion — direct move, pivot, circular
// motion, no-spill carry — built as a PlanRequest from where it starts, without
// touching the arm, starting from the frame system inputs it is given.

import (
	"context"
	"fmt"
	"time"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// pivotStartToleranceMm is how far off the authored pivot line the arm may
// stand before a pivot refuses to run.
const pivotStartToleranceMm = 2.0

// motionRequest is one arm motion, built and ready to plan.
type motionRequest struct {
	// name is what the motion moves to, for logs and errors.
	name string
	// label is the motion's kind, which tags its saved plan.
	label string
	req   *armplanning.PlanRequest
	speed *StepMoveOptions
	// repeatFor replays the planned trajectory until this long has passed
	// (circular motion), rather than running it once.
	repeatFor time.Duration
}

// toWorld returns pose, expressed in frame, in the world frame with the frames
// standing at inputs.
func toWorld(
	fs *referenceframe.FrameSystem, inputs referenceframe.FrameSystemInputs, frame string, pose spatialmath.Pose,
) (spatialmath.Pose, error) {
	tf, err := fs.Transform(inputs.ToLinearInputs(), referenceframe.NewPoseInFrame(frame, pose), referenceframe.World)
	if err != nil {
		return nil, err
	}
	return tf.(*referenceframe.PoseInFrame).Pose(), nil
}

// stepMotion builds the request for step's arm motion, starting at start. The
// step's Pause is not part of it.
func (s *beanjaminCoffee) stepMotion(ctx context.Context, step Step, start referenceframe.FrameSystemInputs) (*motionRequest, error) {
	switch {
	case step.PivotFromPose != "":
		return s.pivotMotion(ctx, step, start)
	case step.CircularRadiusMm > 0:
		m, err := s.circularMotion(ctx, step, start)
		if err != nil {
			return nil, fmt.Errorf("circular motion around %q: %w", step.PoseName, err)
		}
		return m, nil
	}
	pd, err := s.resolvePose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return nil, err
	}
	// A filled-container traverse (NoSpill) routes through the level carry so the
	// drink doesn't slosh. The carry adds an orientation constraint in place of a
	// LinearConstraint, but still honors the step's AllowedCollisions and
	// MoveOptions.
	if step.NoSpill {
		m, err := s.carryMotion(step.PoseName, pd, step.AllowedCollisions, step.MoveOptions, start)
		if err != nil {
			return nil, fmt.Errorf("no-spill carry to %q failed: %w", step.PoseName, err)
		}
		return m, nil
	}
	m, err := s.rawMotion(step.PoseName, pd, step.LinearConstraint, step.OrientationToleranceDegs, step.AllowedCollisions, step.MoveOptions, start)
	if err != nil {
		return nil, fmt.Errorf("move to %q failed: %w", step.PoseName, err)
	}
	return m, nil
}

// rawMotion builds a direct move to pd from start. orientTolDegs, when
// positive, bounds the moving frame's orientation along the whole path
// (withPathOrientationConstraint).
func (s *beanjaminCoffee) rawMotion(
	name string,
	pd *poseData,
	lc *StepLinearConstraint,
	orientTolDegs float64,
	allowedCollisions []AllowedCollision,
	speed *StepMoveOptions,
	start referenceframe.FrameSystemInputs,
) (*motionRequest, error) {
	logger := s.activeOrderLogger()
	fs := s.scene().fs

	goalPose, err := toWorld(fs, start, pd.refFrame, pd.pose)
	if err != nil {
		return nil, fmt.Errorf("transform destination to world: %w", err)
	}

	allowedCollisions = s.filterFakeModeCollisions(s.scene().appendHeldItemCollisions(allowedCollisions))
	constraints := withPathOrientationConstraint(buildConstraints(lc, allowedCollisions), orientTolDegs)
	plannerOpts := plannerOptionsForConstraint(lc)
	if lc != nil {
		logger.Infof("applying linear constraint (line=%.1fmm, orient=%.1f°)",
			lc.LineToleranceMm, lc.OrientationToleranceDegs)
	} else {
		logger.Infof("applying %.1fmm collision buffer", plannerOpts.CollisionBufferMM)
	}
	if orientTolDegs > 0 {
		logger.Infof("applying path orientation constraint (±%.1f°)", orientTolDegs)
	}
	if len(allowedCollisions) > 0 {
		logger.Infof("allowing %d collision pair(s)", len(allowedCollisions))
	}

	return &motionRequest{
		name:  name,
		label: "move",
		req: &armplanning.PlanRequest{
			FrameSystem: fs,
			Goals: []*armplanning.PlanState{
				armplanning.NewPlanState(referenceframe.FrameSystemPoses{
					pd.componentName: referenceframe.NewPoseInFrame(referenceframe.World, goalPose),
				}, nil),
			},
			StartState:     armplanning.NewPlanState(nil, start),
			Constraints:    constraints,
			PlannerOptions: plannerOpts,
		},
		speed: speed,
	}, nil
}

// worldGoals turns poses authored in refFrame into plan states commanding
// componentName, each transformed into the world frame.
func worldGoals(
	fs *referenceframe.FrameSystem,
	inputs referenceframe.FrameSystemInputs,
	refFrame, componentName string,
	poses []spatialmath.Pose,
) ([]*armplanning.PlanState, error) {
	goals := make([]*armplanning.PlanState, 0, len(poses))
	for _, pose := range poses {
		world, err := toWorld(fs, inputs, refFrame, pose)
		if err != nil {
			return nil, fmt.Errorf("transform waypoint to world: %w", err)
		}
		goals = append(goals, armplanning.NewPlanState(
			referenceframe.FrameSystemPoses{componentName: referenceframe.NewPoseInFrame(referenceframe.World, world)}, nil,
		))
	}
	return goals, nil
}

// pivotMotion builds a pivot: interpolated waypoints from step.PivotFromPose to
// step.PoseName, planned as one multi-goal trajectory so the arm never stops
// partway through the arc. The two poses may sit at different points: the
// position then moves in a straight line while the orientation rotates, in
// lockstep (e.g. raising a bottle as it tilts to pour); identical points give a
// pure rotation in place.
func (s *beanjaminCoffee) pivotMotion(ctx context.Context, step Step, start referenceframe.FrameSystemInputs) (*motionRequest, error) {
	logger := s.activeOrderLogger()
	fs := s.scene().fs

	startPD, err := s.resolvePose(ctx, step.PoseSwitch, step.PivotFromPose)
	if err != nil {
		return nil, fmt.Errorf("pivot start: %w", err)
	}
	endPD, err := s.resolvePose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return nil, fmt.Errorf("pivot end: %w", err)
	}
	if startPD.componentName != endPD.componentName {
		return nil, fmt.Errorf("pivot %q → %q: component mismatch (%q vs %q)",
			step.PivotFromPose, step.PoseName, startPD.componentName, endPD.componentName)
	}

	// Seed the arc from where the arm stands rather than from the authored start
	// pose. Each segment's first waypoint is dropped on the assumption we're
	// already standing on it, so an arm that ended the previous step off the
	// authored pose would get one coarse planner segment to climb back onto the
	// arc — precisely the free-form rotation the fine waypoints exist to prevent,
	// and with a portafilter engaged in the bayonet while it happens. The
	// position is snapped onto the authored start→end line so the pivot stays on
	// it even if the arm has drifted a fraction of a mm.
	actual, err := s.pivotStartPose(fs, start, startPD)
	if err != nil {
		return nil, fmt.Errorf("pivot %q: current pose of %q: %w", step.PivotFromPose, startPD.componentName, err)
	}
	seed, dist := pivotSeedPoint(startPD.pose.Point(), endPD.pose.Point(), actual.Point())
	if dist > pivotStartToleranceMm {
		return nil, fmt.Errorf("pivot %q → %q: arm is %.2f mm off the pivot line (max %.1f mm) — refusing to pivot about the wrong point",
			step.PivotFromPose, step.PoseName, dist, pivotStartToleranceMm)
	}
	fromPose := spatialmath.NewPose(seed, actual.Orientation())

	// Rotate straight onto the goal, or past it and back when the step asks for
	// an overshoot. Both segments are planned together and run as one
	// trajectory, so the arm never stops at the overshot pose.
	targets := []spatialmath.Pose{endPD.pose}
	if step.PivotExtraDegrees != 0 {
		// Axis from the authored pair, not fromPose: the arm's drift should seed
		// where the arc starts, never tilt the axis it turns about.
		over, err := pivotOvershootPose(startPD.pose, endPD.pose, step.PivotExtraDegrees)
		if err != nil {
			return nil, fmt.Errorf("pivot %q → %q: %w", step.PivotFromPose, step.PoseName, err)
		}
		targets = []spatialmath.Pose{over, endPD.pose}
	}

	var poses []spatialmath.Pose
	segStart := fromPose
	for _, target := range targets {
		seg := computePivotPoses(logger, segStart, target, step.PivotDegreesPerStep)
		poses = append(poses, seg[1:]...) // seg[0] is where the previous segment left us
		segStart = target
	}
	logger.Infof("pivot %q → %q: %d waypoints (%.1f°/step, %.1f° overshoot)",
		step.PivotFromPose, step.PoseName, len(poses), step.PivotDegreesPerStep, step.PivotExtraDegrees)

	goals, err := worldGoals(fs, start, startPD.refFrame, startPD.componentName, poses)
	if err != nil {
		return nil, err
	}

	return &motionRequest{
		name:  step.PoseName,
		label: "pivot",
		req: &armplanning.PlanRequest{
			FrameSystem: fs,
			Goals:       goals,
			StartState:  armplanning.NewPlanState(nil, start),
			Constraints: buildConstraints(step.LinearConstraint, s.filterFakeModeCollisions(s.scene().appendHeldItemCollisions(step.AllowedCollisions))),
		},
		speed: step.MoveOptions,
	}, nil
}

// pivotStartPose returns where pd's component stands, in pd's reference frame,
// as a pivot begins: read off start, or, when start leaves the arm out because
// the motions recorded ahead of the pivot in a chunk are not planned yet, placed
// by the goal the last of them reaches (goalPlacement).
func (s *beanjaminCoffee) pivotStartPose(
	fs *referenceframe.FrameSystem, start referenceframe.FrameSystemInputs, pd *poseData,
) (spatialmath.Pose, error) {
	at := placement{inputs: start}
	if _, known := start[s.cfg.ArmName]; !known {
		goalAt, ok := s.goalPlacement(start)
		if !ok {
			return nil, errUnplaceable
		}
		at = goalAt
	}
	tf, err := at.transform(fs, referenceframe.NewPoseInFrame(pd.componentName, spatialmath.NewZeroPose()), pd.refFrame)
	if err != nil {
		return nil, err
	}
	return tf.(*referenceframe.PoseInFrame).Pose(), nil
}

// circularMotion builds one revolution of circular waypoints around
// step.PoseName. The revolution is planned once and replayed until
// step.CircularDurationSec has passed.
func (s *beanjaminCoffee) circularMotion(ctx context.Context, step Step, start referenceframe.FrameSystemInputs) (*motionRequest, error) {
	logger := s.activeOrderLogger()
	fs := s.scene().fs

	centerPD, err := s.resolvePose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return nil, fmt.Errorf("circular center: %w", err)
	}

	pointsPerRev := step.CircularPointsPerRev
	if pointsPerRev < 4 {
		pointsPerRev = 8
	}

	poses := computeCircularPoses(centerPD.pose, step.CircularRadiusMm, pointsPerRev)
	logger.Infof("circular motion around %q: radius=%.1fmm, %d pts/rev",
		step.PoseName, step.CircularRadiusMm, pointsPerRev)

	goals, err := worldGoals(fs, start, centerPD.refFrame, centerPD.componentName, poses)
	if err != nil {
		return nil, err
	}

	return &motionRequest{
		name:  step.PoseName,
		label: "circular",
		req: &armplanning.PlanRequest{
			FrameSystem: fs,
			Goals:       goals,
			StartState:  armplanning.NewPlanState(nil, start),
			Constraints: buildConstraints(step.LinearConstraint, s.filterFakeModeCollisions(s.scene().appendHeldItemCollisions(step.AllowedCollisions))),
		},
		speed:     step.MoveOptions,
		repeatFor: time.Duration(step.CircularDurationSec * float64(time.Second)),
	}, nil
}

// carryMotion builds a level carry of the held container from start to dest,
// free-planning the path but holding the container's orientation within
// noSpillOrientationToleranceDegs of the direct start-to-goal reorientation for
// the whole traverse — so the drink doesn't slosh.
//
// The goal commands the held-item frame rather than the gripper, because the
// orientation bound only bounds the drink's tilt when expressed about the
// container's own axis. That frame is neither coincident nor co-oriented with
// the one dest is authored for, so dest is converted into it
// (carryGoalForMoveFrame); with nothing attached it falls back to the gripper
// frame and the conversion is a no-op.
func (s *beanjaminCoffee) carryMotion(
	name string, dest *poseData, allowedCollisions []AllowedCollision, speed *StepMoveOptions, start referenceframe.FrameSystemInputs,
) (*motionRequest, error) {
	logger := s.activeOrderLogger()
	fs := s.scene().fs

	// dest targets its own component (grip-point for a switch pose, held-item
	// for a pour pose). When an item is held, move the held-item frame: it is
	// the container that must stay level.
	moveFrame := gripPoint
	if s.scene().heldItemAttached {
		moveFrame = heldItemFrameName
	}

	destPose, err := carryGoalForMoveFrame(fs, start, dest, moveFrame)
	if err != nil {
		return nil, err
	}

	logger.Infof("no-spill carry: moving %q to %q (path±%.0f°, buffer: %.1fmm)",
		moveFrame, name, noSpillOrientationToleranceDegs, freeMoveCollisionBufferMM)

	return &motionRequest{
		name:  name,
		label: "carry",
		req: &armplanning.PlanRequest{
			FrameSystem: fs,
			Goals: []*armplanning.PlanState{armplanning.NewPlanState(referenceframe.FrameSystemPoses{
				moveFrame: referenceframe.NewPoseInFrame(referenceframe.World, destPose),
			}, nil)},
			StartState: armplanning.NewPlanState(nil, start),
			Constraints: withNoSpillOrientationConstraint(
				buildConstraints(nil, s.filterFakeModeCollisions(s.scene().appendHeldItemCollisions(allowedCollisions)))),
			PlannerOptions: freeMovePlannerOptions(),
		},
		speed: speed,
	}, nil
}

// carryGoalForMoveFrame converts dest — a pose authored for dest.componentName —
// into the world pose moveFrame must reach for that component to land on dest.
// Returns dest's world pose unchanged when moveFrame is the authored component.
//
// The two frames are rigidly linked but not co-oriented: held-item sits on the
// grip point but is rotated onto the container's axes (heldItemFramePose).
// Commanding the container straight at a grip-point goal leaves the gripper
// mis-rotated. The conversion goes through the frame system rather than assuming
// the frames share an origin, so it holds for any rigid offset between them.
func carryGoalForMoveFrame(
	fs *referenceframe.FrameSystem,
	inputs referenceframe.FrameSystemInputs,
	dest *poseData,
	moveFrame string,
) (spatialmath.Pose, error) {
	destPose, err := toWorld(fs, inputs, dest.refFrame, dest.pose)
	if err != nil {
		return nil, fmt.Errorf("transform carry destination to world: %w", err)
	}
	if moveFrame == dest.componentName {
		return destPose, nil
	}
	// moveFrame expressed in the authored component's frame — constant, since both
	// hang rigidly off the gripper.
	offTF, err := fs.Transform(referenceframe.NewZeroInputs(fs).ToLinearInputs(),
		referenceframe.NewPoseInFrame(moveFrame, spatialmath.NewZeroPose()), dest.componentName)
	if err != nil {
		return nil, fmt.Errorf("transform %q into %q: %w", moveFrame, dest.componentName, err)
	}
	return spatialmath.Compose(destPose, offTF.(*referenceframe.PoseInFrame).Pose()), nil
}

// runMotion plans m on its own and runs it on the arm.
func (s *beanjaminCoffee) runMotion(ctx context.Context, m *motionRequest) error {
	positions, err := s.planTrajectory(ctx, m.req, m.label)
	if err != nil {
		return err
	}
	return s.runPositions(ctx, m.name, positions, buildMoveOptions(m.speed), m.repeatFor)
}

// runPositions moves the arm through positions, replaying them until repeatFor
// has passed when it is positive.
func (s *beanjaminCoffee) runPositions(
	ctx context.Context, name string, positions [][]referenceframe.Input, speed *arm.MoveOptions, repeatFor time.Duration,
) error {
	if repeatFor <= 0 {
		return s.arm.MoveThroughJointPositions(ctx, positions, speed, nil)
	}
	deadline := time.Now().Add(repeatFor)
	for rev := 0; time.Now().Before(deadline); rev++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled during circular motion around %q: %w", name, ctx.Err())
		default:
		}
		s.activeOrderLogger().Debugf("circular revolution %d around %q", rev+1, name)
		if err := s.arm.MoveThroughJointPositions(ctx, positions, speed, nil); err != nil {
			return fmt.Errorf("execute circular revolution %d around %q: %w", rev+1, name, err)
		}
	}
	return nil
}
