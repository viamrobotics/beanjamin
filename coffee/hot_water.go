package coffee

// Hot water: the arm opens and closes the espresso machine's hot water valve by
// turning a 3D-printed lever clamped onto the valve's dial
// (hardware/hot-water-knob). The collar round the dial is a hinged obstacle like
// the fridge door: its frame's origin sits on the dial axis, the lever obstacle
// is its child, and a turn re-places the collar (and with it the lever) about
// that axis as the claws carry the lever round.

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

const (
	// frameHotWaterLever is the lever obstacle, a child of the collar.
	frameHotWaterLever = "hot-water-lever"
	// frameHotWaterCollar is the clamp round the dial that carries the lever. It
	// turns with the dial, so it is the hinge: its origin is on the dial axis
	// with local Z along it, pointing the way that opens the valve.
	frameHotWaterCollar = "hot-water-collar"
	frameHotWaterHinge  = frameHotWaterCollar

	// defaultHotWaterOpenDegs is how far the arm turns the lever from closed. On
	// Cappuccina's BES920 the stream starts ~28° in and runs fully by ~45° of the
	// valve's 90° travel.
	defaultHotWaterOpenDegs = 45

	// hotWaterTurnDegsPerStep is the sweep's waypoint spacing. At the ~72 mm
	// grip radius a 5° hop's chord sags 0.07 mm off the arc.
	hotWaterTurnDegsPerStep = 5

	// hotWaterApproachMm is how far beyond the grip, back along the claws'
	// pointing direction (out along the lever, past its tip), the claws open
	// before sliding on, and where they back out to after letting go.
	hotWaterApproachMm = 150

	// Pour times. The machine stops a pour on its own after about 30 s, which
	// fills ~3/4 of a mug.
	defaultHotWaterPourSec          = 28
	defaultAmericanoHotWaterPourSec = 15
	maxHotWaterPourSec              = 30
)

// openHotWater grips the hot water lever and turns the valve open, leaving it
// pouring. Registered as the open_hot_water execute_action. The machine stops
// the pour on its own after about 30 seconds, but does not heat while the
// valve stays open, so close_hot_water has to follow.
func (s *beanjaminCoffee) openHotWater(ctx, cancelCtx context.Context) error {
	if s.hotWaterLeverAt(s.hotWaterOpenDegs()) {
		return nil
	}
	return s.hotWaterRoutine(ctx, cancelCtx, "open_hot_water", "Pouring hot water", func(l *hotWaterLever) error {
		return l.turnTo(s.hotWaterOpenDegs())
	})
}

// closeHotWater grips the lever where open left it and turns the valve shut.
// Registered as the close_hot_water execute_action.
func (s *beanjaminCoffee) closeHotWater(ctx, cancelCtx context.Context) error {
	if s.hotWaterLeverAt(0) {
		return nil
	}
	return s.hotWaterRoutine(ctx, cancelCtx, "close_hot_water", "Closing hot water", func(l *hotWaterLever) error {
		return l.turnTo(0)
	})
}

// hotWaterLeverAt reports whether the scene already has the lever at deg, so
// there is nothing to turn.
func (s *beanjaminCoffee) hotWaterLeverAt(deg float64) bool {
	if at := s.scene().hotWaterOpenDegs; math.Abs(at-deg) < 0.5 {
		s.logger.Infof("the hot water lever already stands at %.1f°; nothing to turn", at)
		return true
	}
	return false
}

// pourHotWater grips the lever, turns the valve open, holds it there for pour,
// turns it shut and lets go: the mug drinks' hot water step. Holding the lever
// through the pour saves letting go and re-gripping, and keeps the valve from
// being left open by anything but a fault.
func (s *beanjaminCoffee) pourHotWater(ctx, cancelCtx context.Context, pour time.Duration) error {
	return s.hotWaterRoutine(ctx, cancelCtx, "pour_hot_water", "Pouring hot water", func(l *hotWaterLever) error {
		if err := l.turnTo(s.hotWaterOpenDegs()); err != nil {
			return err
		}
		s.activeOrderLogger().Infof("pour_hot_water: pouring for %s", pour)
		pourCtx, done := mergedCancelContext(ctx, cancelCtx) // cancel stops the wait
		defer done()
		if err := s.handOff(pourCtx, move{name: "pour hot water", sleep: pour}); err != nil {
			return err
		}
		return l.turnTo(0)
	})
}

// hotWaterRoutine grips the lever, runs turns, and lets go, all in one chunk.
// The claws have to be empty, which during a brew means the portafilter is
// locked into the machine and released: the modeled filter on a closed gripper
// cannot be told apart from an empty one, and opening the jaws round the lever
// would drop a real one.
func (s *beanjaminCoffee) hotWaterRoutine(ctx, cancelCtx context.Context, action, stepLabel string, turns func(*hotWaterLever) error) error {
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
	return s.inChunk(action, func() error {
		s.setStep(stepLabel)
		l, err := s.gripHotWaterLever(ctx, cancelCtx, action)
		if err != nil {
			return err
		}
		if err := turns(l); err != nil {
			return err
		}
		return l.release()
	})
}

// hotWaterLever is the claws closed on the lever, for turning it.
type hotWaterLever struct {
	s              *beanjaminCoffee
	ctx, cancelCtx context.Context
	action         string
	inputs         *referenceframe.LinearInputs
	gripRel        spatialmath.Pose
	baseOriginPose spatialmath.Pose
	collisions     []AllowedCollision
}

// gripHotWaterLever goes to the hot_water_approach claws pose, opens the jaws
// beyond the lever's tip, slides straight on and closes them on the lever.
func (s *beanjaminCoffee) gripHotWaterLever(ctx, cancelCtx context.Context, action string) (*hotWaterLever, error) {
	sc := s.scene()
	fs := sc.fs
	l := &hotWaterLever{
		s: s, ctx: ctx, cancelCtx: cancelCtx, action: action,
		inputs:     placement{inputs: sc.inputs}.fullInputs(fs).ToLinearInputs(),
		gripRel:    relativePoseToSpatial(s.cfg.HotWaterLeverGripRelativePose),
		collisions: s.filterFakeModeCollisions(hotWaterLeverCollisions()),
	}
	var err error
	if l.baseOriginPose, err = hingeClosedOrigin(fs, frameHotWaterHinge, sc.hotWaterOpenDegs); err != nil {
		return nil, err
	}
	grip, err := hotWaterGripWorld(fs, l.inputs, l.gripRel)
	if err != nil {
		return nil, err
	}
	// Through hot_water_approach first: a free move straight to the standoff
	// from wherever the arm is (the group head, during a brew) can land on the
	// flipped-wrist IK branch, which runs out of reach partway through the turn.
	// From the home side of the machine it lands on the branch the grip was
	// taught on.
	if err := s.executeStep(ctx, cancelCtx, Step{PoseName: clawPoseHotWaterApproach, PoseSwitch: s.clawsSw}); err != nil {
		return nil, fmt.Errorf("move to %s: %w", clawPoseHotWaterApproach, err)
	}
	// Opening at the standoff keeps the wider open-jaw silhouette out of the
	// traverse. The slide-on is linear because the claws/lever pair is exempt
	// from collision checks: unconstrained, the planner could swing the claws
	// through the lever and still call the path clear.
	if err := s.moveToRawPose(ctx,
		&poseData{pose: hotWaterStandoff(grip), refFrame: referenceframe.World, componentName: frameGripPoint},
		nil, nil, nil); err != nil {
		return nil, fmt.Errorf("approach the hot water lever: %w", err)
	}
	if s.gripper != nil {
		if err := s.openGripper(ctx); err != nil {
			return nil, fmt.Errorf("open gripper for the hot water lever: %w", err)
		}
	}
	if err := s.moveToRawPose(ctx,
		&poseData{pose: grip, refFrame: referenceframe.World, componentName: frameGripPoint},
		defaultApproachConstraint, l.collisions, s.slowMoveOptions()); err != nil {
		return nil, fmt.Errorf("slide onto the hot water lever: %w", err)
	}
	if s.gripper != nil {
		if err := s.closeGripper(ctx); err != nil {
			return nil, fmt.Errorf("grip the hot water lever: %w", err)
		}
	}
	return l, nil
}

// turnTo carries the gripped lever from where the scene has it to toDeg: one
// motion per waypoint, each planned against a frame system with the collar and
// lever re-placed at that angle, joined into one arm move. The scene keeps the
// lever where the turn left it.
func (l *hotWaterLever) turnTo(toDeg float64) error {
	s := l.s
	before := s.scene()
	fromDeg := before.hotWaterOpenDegs
	if math.Abs(toDeg-fromDeg) < 0.5 {
		return nil
	}
	sweep := computeDoorSweep(fromDeg, toDeg, hotWaterTurnDegsPerStep)
	s.activeOrderLogger().Infof("%s: turning the lever %.1f°→%.1f° in %d steps", l.action, fromDeg, toDeg, len(sweep)-1)
	thetas := sweep[1:]
	for i, theta := range thetas {
		turned, err := copyFrameSystem(before.fs)
		if err != nil {
			return err
		}
		if err := setHingeTheta(turned, frameHotWaterHinge, l.baseOriginPose, theta); err != nil {
			return err
		}
		gripNow, err := hotWaterGripWorld(turned, l.inputs, l.gripRel)
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
				return s.hotWaterTurnedTo(ctx, before, l.baseOriginPose, fromDeg, thetas, segments)
			}
		}
		if err := s.addMotion(l.ctx, l.cancelCtx, l.action, op, func(start referenceframe.FrameSystemInputs) (*motionRequest, error) {
			return &motionRequest{
				name:  l.action,
				label: l.action,
				req: &armplanning.PlanRequest{
					FrameSystem: turned,
					Goals:       []*armplanning.PlanState{goal},
					StartState:  armplanning.NewPlanState(nil, start),
					Constraints: buildConstraints(doorSweepConstraint, l.collisions),
				},
				speed: s.slowMoveOptions(),
			}, nil
		}); err != nil {
			return fmt.Errorf("%s step θ=%.1f: %w", l.action, theta, err)
		}
	}
	return s.editScene(func(sc *sceneState) error {
		sc.hotWaterOpenDegs = toDeg
		return setHingeTheta(sc.fs, frameHotWaterHinge, l.baseOriginPose, toDeg)
	})
}

// release lets go of the lever, backs straight out along it where the turns
// left it, and closes the jaws again for whatever moves next: the collision
// tuning assumes a closed gripper.
func (l *hotWaterLever) release() error {
	s := l.s
	if s.gripper != nil {
		if err := s.openGripper(l.ctx); err != nil {
			return fmt.Errorf("release the hot water lever: %w", err)
		}
	}
	grip, err := hotWaterGripWorld(s.scene().fs, l.inputs, l.gripRel)
	if err != nil {
		return err
	}
	if err := s.moveToRawPose(l.ctx,
		&poseData{pose: hotWaterStandoff(grip), refFrame: referenceframe.World, componentName: frameGripPoint},
		defaultApproachConstraint, l.collisions, nil); err != nil {
		return fmt.Errorf("back off the hot water lever: %w", err)
	}
	if s.gripper != nil {
		if err := s.closeGripper(l.ctx); err != nil {
			return fmt.Errorf("close gripper after releasing the hot water lever: %w", err)
		}
	}
	return nil
}

// hotWaterGripWorld is where the grip point closes on the lever as it stands in
// fs: hot_water_lever_grip_relative_pose in the hinge (collar) frame.
func hotWaterGripWorld(fs *referenceframe.FrameSystem, inputs *referenceframe.LinearInputs, gripRel spatialmath.Pose) (spatialmath.Pose, error) {
	tf, err := fs.Transform(inputs, referenceframe.NewPoseInFrame(frameHotWaterHinge, gripRel), referenceframe.World)
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
	if err := setHingeTheta(turned.fs, frameHotWaterHinge, baseOriginPose, reached); err != nil {
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

// hotWaterPourTime is how long drink holds the valve open: an americano tops
// up a lungo, hot water fills the mug.
func (s *beanjaminCoffee) hotWaterPourTime(drink string) time.Duration {
	sec := orDefault(s.cfg.HotWaterPourSec, defaultHotWaterPourSec)
	if drink == "americano" {
		sec = orDefault(s.cfg.AmericanoHotWaterPourSec, defaultAmericanoHotWaterPourSec)
	}
	return time.Duration(sec * float64(time.Second))
}
