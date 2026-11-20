package coffee

// The mug drinks' arm routines: carry a mug between the group head and the hot
// water spout, stage it there while hot water is added, and pick it back up.
// Gated by can_serve_mug_drinks. The order phases that call them are in
// prepare_mug.go.

import (
	"context"
	"fmt"
)

// stagedMugFrameName is the World-parented obstacle frame for a mug set down at
// the hot water spout while the arm is away operating the hot water switch.
const stagedMugFrameName = "staged-mug"

// slideMugToWater carries the full mug from under the group head to
// mug_ready_for_water, under the hot water outlet, along a straight line,
// at the spillage-prevention speed because the mug is full.
func (s *beanjaminCoffee) slideMugToWater(ctx, cancelCtx context.Context) error {
	step := Step{PoseName: clawPoseMugReadyForWater, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint,
		MoveOptions: s.spillagePreventionMoveOptions(), AllowedCollisions: s.scene().heldItemSurfaceCollisions(heldItemMachineCollisions)}
	if err := s.executeStep(ctx, cancelCtx, step); err != nil {
		return fmt.Errorf("slide_mug_to_water: %w", err)
	}
	return nil
}

// setMugAtWater sets the held mug down at the spout and backs the arm away,
// leaving the mug as a static world obstacle (staged-mug) so the hot water
// moves route around it. The reverse is regrabMugAtWater.
func (s *beanjaminCoffee) setMugAtWater(ctx, cancelCtx context.Context) error {
	if err := s.openGripper(ctx); err != nil {
		return fmt.Errorf("set_mug_at_water: %w", err)
	}
	// The mug is set down; keep it as a static world obstacle so the hot water
	// moves route around it.
	if err := s.stageMugAsObstacle(ctx); err != nil {
		return fmt.Errorf("set_mug_at_water: %w", err)
	}
	// The gripper is empty, so the arm's default speed.
	exitStep := Step{PoseName: clawPoseMugWaterApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint}
	if err := s.executeStep(ctx, cancelCtx, exitStep); err != nil {
		return fmt.Errorf("set_mug_at_water: %w", err)
	}
	if err := s.closeGripper(ctx); err != nil {
		return fmt.Errorf("set_mug_at_water: %w", err)
	}
	return nil
}

// dispenseHotWater pours hot water into the mug staged at the spout: the arm
// grips the valve's lever, turns it open, holds it for the drink's pour time,
// turns it shut and lets go (pourHotWater, hot_water.go). An americano tops up
// a lungo already in the mug; hot water fills an empty one.
//
// It runs while the order's moves are being recorded, not executed, so the pour
// wait is a queued move, and it starts and ends with the arm clear of the mug.
func (s *beanjaminCoffee) dispenseHotWater(ctx, cancelCtx context.Context, drink string) error {
	if err := s.pourHotWater(ctx, cancelCtx, s.hotWaterPourTime(drink)); err != nil {
		return fmt.Errorf("dispense_hot_water: %w", err)
	}
	return nil
}

// regrabMugAtWater picks the staged mug back up and carries it clear of the
// spout: approach, open, slow linear descent onto the mug, grab, then a slow
// linear retreat with the mug in hand. The reverse of setMugAtWater.
func (s *beanjaminCoffee) regrabMugAtWater(ctx, cancelCtx context.Context) error {
	approachStep := Step{PoseName: clawPoseMugWaterApproach, PoseSwitch: s.clawsSw}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("regrab_mug_at_water: %w", err)
	}
	if err := s.openGripper(ctx); err != nil {
		return fmt.Errorf("regrab_mug_at_water: %w", err)
	}
	// Descend onto the mug; it stays a world obstacle, but the jaws may touch it
	// for this step. Slow, to settle onto the handle-side grasp gently.
	grabStep := Step{PoseName: clawPoseMugReadyForWater, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause,
		MoveOptions: s.slowMoveOptions(), AllowedCollisions: s.scene().stagedGrabCollisions(stagedMug)}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("regrab_mug_at_water: %w", err)
	}
	if err := s.grabAndVerifyHolding(ctx, "mug at water"); err != nil {
		return fmt.Errorf("regrab_mug_at_water: grab gripper: %w", err)
	}
	// The mug is back in the gripper: move it from the world obstacle to a held item.
	if err := s.regrabStagedMug(ctx); err != nil {
		s.activeOrderLogger().Warnf("regrab_mug_at_water: reattach mug geometry failed, continuing untracked: %v", err)
	}
	retreatStep := Step{PoseName: clawPoseMugWaterApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint,
		MoveOptions: s.spillagePreventionMoveOptions(), AllowedCollisions: s.scene().heldItemSurfaceCollisions(heldItemMachineCollisions)}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("regrab_mug_at_water: %w", err)
	}
	return nil
}

// placeMugAtWater is the hot-water-alone path: vision-grab an empty mug off the
// shelf and set it straight down at the spout.
func (s *beanjaminCoffee) placeMugAtWater(ctx, cancelCtx context.Context) error {
	if err := s.pickMugDynamic(ctx, cancelCtx); err != nil {
		return err
	}
	approachStep := Step{PoseName: clawPoseMugWaterApproach, PoseSwitch: s.clawsSw}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("place_mug_at_water: %w", err)
	}
	// The mug is empty, so the arm's default speed.
	readyStep := Step{PoseName: clawPoseMugReadyForWater, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause,
		AllowedCollisions: s.scene().heldItemSurfaceCollisions(heldItemMachineCollisions)}
	if err := s.executeStep(ctx, cancelCtx, readyStep); err != nil {
		return fmt.Errorf("place_mug_at_water: %w", err)
	}
	return s.setMugAtWater(ctx, cancelCtx)
}

// stageMugAsObstacle is setMugAtWater's release path: it turns the held mug into
// the static staged-mug obstacle. No-op when nothing is attached.
func (s *beanjaminCoffee) stageMugAsObstacle(ctx context.Context) error {
	return s.stageAsObstacle(ctx, stagedMug)
}

// regrabStagedMug moves the staged mug from the world back into the gripper once
// the jaws have closed on it, modeling the grasp from where the mug stands.
func (s *beanjaminCoffee) regrabStagedMug(ctx context.Context) error {
	return s.regrabStaged(ctx, stagedMug)
}
