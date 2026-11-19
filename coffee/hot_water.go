package coffee

// Hot water: the arm opens and closes the espresso machine's hot water valve by
// turning a 3D-printed lever clamped onto the valve's dial
// (hardware/hot-water-knob). The lever is a hinged obstacle like the fridge
// door: its frame's origin sits on the dial axis, and a turn re-places it about
// that axis as the claws carry it round.

import (
	"context"
	"fmt"
	"math"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

const (
	// frameHotWaterLever is the lever obstacle. Its origin is on the dial axis
	// with local Z along it, pointing the way that opens the valve.
	frameHotWaterLever = "hot-water-lever"
	// frameHotWaterCollar is the clamp round the dial that carries the lever.
	frameHotWaterCollar = "hot-water-collar"

	// defaultHotWaterOpenDegs is how far open_hot_water turns the lever from
	// closed. On the Breville BES920 the valve hisses at ~15°, drips at ~18°
	// and pours a full stream from ~33° of a 90° travel.
	defaultHotWaterOpenDegs = 37.5

	// hotWaterTurnDegsPerStep is the sweep's waypoint spacing. At the ~72 mm
	// grip radius a 5° hop's chord sags 0.07 mm off the arc.
	hotWaterTurnDegsPerStep = 5

	// hotWaterApproachMm is how far beyond the grip, back along the claws'
	// pointing direction (out along the lever, past its tip), the claws open
	// before sliding on, and where they back out to after letting go.
	hotWaterApproachMm = 150
)

// openHotWater grips the hot water lever and turns the valve open, leaving it
// pouring. Registered as the open_hot_water execute_action. The machine stops
// the pour on its own after about 30 seconds, but does not heat while the
// valve stays open, so close_hot_water has to follow.
func (s *beanjaminCoffee) openHotWater(ctx, cancelCtx context.Context) error {
	return s.turnHotWater(ctx, cancelCtx, "open_hot_water", "Pouring hot water", s.hotWaterOpenDegs())
}

// closeHotWater grips the lever where open left it and turns the valve shut.
// Registered as the close_hot_water execute_action.
func (s *beanjaminCoffee) closeHotWater(ctx, cancelCtx context.Context) error {
	return s.turnHotWater(ctx, cancelCtx, "close_hot_water", "Closing hot water", 0)
}

// turnHotWater turns the lever from wherever the scene has it
// (hotWaterOpenDegs) to toDeg. The claws have to be empty, which during a brew
// means the portafilter is locked into the machine and released: the modeled
// filter on a closed gripper cannot be told apart from an empty one, and opening
// the jaws round the lever would drop a real one.
func (s *beanjaminCoffee) turnHotWater(ctx, cancelCtx context.Context, action, stepLabel string, toDeg float64) error {
	if s.cfg.HotWaterLeverGripRelativePose == nil {
		return fmt.Errorf("%s requires hot_water_lever_grip_relative_pose", action)
	}
	sc := s.scene()
	if !sc.filterFrameLocked {
		return fmt.Errorf("%s needs empty claws: lock the portafilter into the machine and release it first (lock_portafilter, release_filter)", action)
	}
	if sc.heldItemAttached || sc.heldMilk != "" {
		return fmt.Errorf("%s needs empty claws, but the gripper is holding something", action)
	}
	if math.Abs(toDeg-sc.hotWaterOpenDegs) < 0.5 {
		s.logger.Infof("%s: the lever already stands at %.1f°; nothing to turn", action, sc.hotWaterOpenDegs)
		return nil
	}
	return s.inChunk(action, func() error { return s.recordHotWaterTurn(ctx, cancelCtx, action, stepLabel, toDeg) })
}

// recordHotWaterTurn is turnHotWater's arm work, the lever's version of
// recordDoorSweep: go to the hot_water_approach claws pose, open the jaws
// beyond the lever's tip, slide straight on, close, carry the lever round its
// axis one waypoint at a time with the lever obstacle re-placed at each angle,
// then let go and back straight out along the lever. The scene keeps the lever
// where the turn left it.
func (s *beanjaminCoffee) recordHotWaterTurn(ctx, cancelCtx context.Context, action, stepLabel string, toDeg float64) error {
	logger := s.logger
	s.setStep(stepLabel)

	before := s.scene()
	fs := before.fs
	inputs := placement{inputs: before.inputs}.fullInputs(fs).ToLinearInputs()
	fromDeg := before.hotWaterOpenDegs
	baseOriginPose, err := hingeClosedOrigin(fs, frameHotWaterLever, fromDeg)
	if err != nil {
		return err
	}
	gripRel := relativePoseToSpatial(s.cfg.HotWaterLeverGripRelativePose)
	collisions := s.filterFakeModeCollisions(hotWaterLeverCollisions())

	gripStart, err := hotWaterGripWorld(fs, inputs, gripRel)
	if err != nil {
		return err
	}
	// Through hot_water_approach first: a free move straight to the standoff
	// from wherever the arm is (the group head, during a brew) can land on the
	// flipped-wrist IK branch, which runs out of reach partway through the turn.
	// From the home side of the machine it lands on the branch the grip was
	// taught on.
	if err := s.executeStep(ctx, cancelCtx, Step{PoseName: clawPoseHotWaterApproach, PoseSwitch: s.clawsSw}); err != nil {
		return fmt.Errorf("move to %s: %w", clawPoseHotWaterApproach, err)
	}
	// Opening at the standoff keeps the wider open-jaw silhouette out of the
	// traverse. The slide-on is linear because the claws/lever pair is exempt
	// from collision checks: unconstrained, the planner could swing the claws
	// through the lever and still call the path clear.
	if err := s.moveToRawPose(ctx,
		&poseData{pose: hotWaterStandoff(gripStart), refFrame: referenceframe.World, componentName: frameGripPoint},
		nil, nil, nil); err != nil {
		return fmt.Errorf("approach the hot water lever: %w", err)
	}
	if s.gripper != nil {
		if err := s.openGripper(ctx); err != nil {
			return fmt.Errorf("open gripper for the hot water lever: %w", err)
		}
	}
	if err := s.moveToRawPose(ctx,
		&poseData{pose: gripStart, refFrame: referenceframe.World, componentName: frameGripPoint},
		defaultApproachConstraint, collisions, s.slowMoveOptions()); err != nil {
		return fmt.Errorf("slide onto the hot water lever: %w", err)
	}
	if s.gripper != nil {
		if err := s.closeGripper(ctx); err != nil {
			return fmt.Errorf("grip the hot water lever: %w", err)
		}
	}

	// The turn: one motion per waypoint, each planned against a frame system
	// with the lever re-placed at that angle, joined into one arm move.
	sweep := computeDoorSweep(fromDeg, toDeg, hotWaterTurnDegsPerStep)
	logger.Infof("%s: turning the lever %.1f°→%.1f° in %d steps", action, fromDeg, toDeg, len(sweep)-1)
	thetas := sweep[1:]
	for i, theta := range thetas {
		turned, err := copyFrameSystem(fs)
		if err != nil {
			return err
		}
		if err := setHingeTheta(turned, frameHotWaterLever, baseOriginPose, theta); err != nil {
			return err
		}
		gripNow, err := hotWaterGripWorld(turned, inputs, gripRel)
		if err != nil {
			return err
		}
		goal := armplanning.NewPlanState(referenceframe.FrameSystemPoses{
			frameGripPoint: referenceframe.NewPoseInFrame(referenceframe.World, gripNow),
		}, nil)
		op := chunkOp{join: i < len(thetas)-1}
		if !op.join {
			// One arm call does not say which waypoint it stopped on, so a failed
			// turn is located from where the arm actually stopped.
			op.recover = func(ctx context.Context, segments [][][]referenceframe.Input) *sceneState {
				return s.hotWaterTurnedTo(ctx, before, baseOriginPose, fromDeg, thetas, segments)
			}
		}
		if err := s.addMotion(ctx, cancelCtx, action, op, func(start referenceframe.FrameSystemInputs) (*motionRequest, error) {
			return &motionRequest{
				name:  action,
				label: action,
				req: &armplanning.PlanRequest{
					FrameSystem: turned,
					Goals:       []*armplanning.PlanState{goal},
					StartState:  armplanning.NewPlanState(nil, start),
					Constraints: buildConstraints(doorSweepConstraint, collisions),
				},
				speed: s.slowMoveOptions(),
			}, nil
		}); err != nil {
			return fmt.Errorf("%s step θ=%.1f: %w", action, theta, err)
		}
	}
	if err := s.editScene(func(sc *sceneState) error {
		sc.hotWaterOpenDegs = toDeg
		return setHingeTheta(sc.fs, frameHotWaterLever, baseOriginPose, toDeg)
	}); err != nil {
		return err
	}

	// Let go, then back straight out along the lever where the turn left it.
	if s.gripper != nil {
		if err := s.openGripper(ctx); err != nil {
			return fmt.Errorf("release the hot water lever: %w", err)
		}
	}
	gripEnd, err := hotWaterGripWorld(s.scene().fs, inputs, gripRel)
	if err != nil {
		return err
	}
	if err := s.moveToRawPose(ctx,
		&poseData{pose: hotWaterStandoff(gripEnd), refFrame: referenceframe.World, componentName: frameGripPoint},
		defaultApproachConstraint, collisions, nil); err != nil {
		return fmt.Errorf("back off the hot water lever: %w", err)
	}
	// Closed again for whatever moves next: the collision tuning assumes it.
	if s.gripper != nil {
		if err := s.closeGripper(ctx); err != nil {
			return fmt.Errorf("close gripper after releasing the hot water lever: %w", err)
		}
	}
	return nil
}

// hotWaterGripWorld is where the grip point closes on the lever as it stands in
// fs: hot_water_lever_grip_relative_pose in the lever's frame.
func hotWaterGripWorld(fs *referenceframe.FrameSystem, inputs *referenceframe.LinearInputs, gripRel spatialmath.Pose) (spatialmath.Pose, error) {
	tf, err := fs.Transform(inputs, referenceframe.NewPoseInFrame(frameHotWaterLever, gripRel), referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("hot water lever grip to world: %w", err)
	}
	return tf.(*referenceframe.PoseInFrame).Pose(), nil
}

// hotWaterStandoff backs grip off hotWaterApproachMm along the claws' pointing
// direction, which runs along the lever toward the dial: out past the tip.
func hotWaterStandoff(grip spatialmath.Pose) spatialmath.Pose {
	return spatialmath.Compose(grip, spatialmath.NewPoseFromPoint(r3.Vector{Z: -hotWaterApproachMm}))
}

// hotWaterTurnedTo returns before with the lever where a failed turn left it,
// located from the arm's joints like doorSweptTo: the claws hold the lever, so
// they say how far it went.
func (s *beanjaminCoffee) hotWaterTurnedTo(
	ctx context.Context, before *sceneState, baseOriginPose spatialmath.Pose, fromDeg float64, thetas []float64, segments [][][]referenceframe.Input,
) *sceneState {
	var positions [][]referenceframe.Input
	var thetaOf []float64
	for i, seg := range segments {
		positions = append(positions, seg...)
		for range seg {
			thetaOf = append(thetaOf, thetas[i])
		}
	}
	reached := fromDeg
	if actual, err := s.arm.CurrentInputs(context.WithoutCancel(ctx)); err != nil {
		s.logger.Warnf("could not read the arm to locate the hot water lever after a failed turn, assuming %.1f°: %v", fromDeg, err)
	} else {
		reached = nearestTheta(actual, positions, thetaOf, fromDeg)
	}
	turned, err := before.clone()
	if err != nil {
		s.logger.Errorf("could not copy the scene to re-place the hot water lever at %.1f° after a failed turn; "+
			"run reset_world before moving again: %v", reached, err)
		return nil
	}
	turned.hotWaterOpenDegs = reached
	if err := setHingeTheta(turned.fs, frameHotWaterLever, baseOriginPose, reached); err != nil {
		s.logger.Errorf("could not re-place the hot water lever model at %.1f° after a failed turn; "+
			"run reset_world before moving again: %v", reached, err)
	}
	return turned
}

// hotWaterOpenDegs returns the configured lever turn, defaulting to
// defaultHotWaterOpenDegs.
func (s *beanjaminCoffee) hotWaterOpenDegs() float64 {
	return orDefault(s.cfg.HotWaterOpenDegs, defaultHotWaterOpenDegs)
}
