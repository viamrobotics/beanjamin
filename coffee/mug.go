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

// hotWaterRoutineWired says whether dispenseHotWater is the real routine. While
// it is false, a hot_water order serves an empty mug and an americano is just a
// lungo, both reported as success, so can_serve_mug_drinks must stay off for
// customers. Flip it when the real routine replaces the stub.
const hotWaterRoutineWired = false

// dispenseHotWater is the hot water routine: a stub until the real one is wired.
//
// The contract for the real routine. It runs while the order's moves are being
// recorded, not executed: the arm is still somewhere behind, working through
// what was handed off before. So:
//   - its arm moves must be ordinary steps (executeStep and friends), which
//     queue in order behind everything already handed off;
//   - any wait (the pour time) must be a queued move, handOff(ctx, move{name:
//     ..., sleep: d}), like the brew wait in brew_steps.go, never a sleep here;
//   - any direct I/O (a board pin, a DoCommand) must go through handOff (as a
//     move's run) or handOffEffect, so it happens when the arm reaches that
//     point, not when the planner does;
//   - or, if it needs each step's result right away, it runs inside
//     syncRegion(), which waits for the arm to catch up first, at the cost of
//     planning ahead.
//
// It must start and end with the arm clear of the mug, since the mug stands
// staged at the spout throughout. It receives the drink id so it can tell a
// top-off (americano, the mug already holds espresso) from a full mug (hot
// water alone).
func (s *beanjaminCoffee) dispenseHotWater(ctx, cancelCtx context.Context, drink string) error {
	logger := s.activeOrderLogger()
	// Handed off so the warning lands when the arm gets here, once.
	s.handOffEffect(func() {
		logger.Warnf("dispense_hot_water: no hot water routine wired yet (drink %s); skipping", drink)
	})
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
