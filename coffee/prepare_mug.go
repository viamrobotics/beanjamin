package coffee

// The mug drinks' share of the order routine: the hot water drink's own phases
// and the Americano's serve, kept out of prepareDrink so the cup and iced flows
// there stay readable.

import (
	"context"

	"beanjamin/coffee/order"
	"beanjamin/coffee/speech"
)

// phaseRunner is prepareDrink's runPhase: it publishes the step label, logs
// the progress line and runs the phase in its own trace span. The mug routines
// take it so their phases surface the same way.
type phaseRunner func(step, spanName, progress string, fn func(ctx, cancelCtx context.Context) error) error

// actionPhase is the phaseRunner for a mug routine run by execute_action, outside
// an order: it publishes the step label, logs the progress line and runs the
// phase on the action's contexts.
func (s *beanjaminCoffee) actionPhase(ctx, cancelCtx context.Context) phaseRunner {
	return func(step, _, progress string, fn func(ctx, cancelCtx context.Context) error) error {
		s.setStep(step)
		s.logger.Info(progress)
		return fn(ctx, cancelCtx)
	}
}

// prepareHotWater runs the hot water drink. The gripper holds the portafilter
// between orders, so it locks the empty portafilter into the group head and lets
// go before it can pick up a mug, then takes it back at the end. No grinding,
// tamping, brewing or cleaning. Called from prepareDrink after its shared setup,
// so the same defers (failed-step capture, pause on fault, run end) cover it. A
// mug drink always goes to the shelf, never the delivery cart.
func (s *beanjaminCoffee) prepareHotWater(ctx, cancelCtx context.Context, o order.Order, runPhase phaseRunner) error {
	drink := o.Drink
	logger := s.activeOrderLogger()

	// Locking the empty portafilter and releasing it are one chunk, as in
	// prepareDrink.
	if err := s.inChunk("prepare portafilter", func() error {
		if err := runPhase(stepLockingPortafilter, "locking_portafilter", "step 1/8: locking empty portafilter", s.lockPortaFilter); err != nil {
			return err
		}
		return runPhase(stepReleasingFilter, "releasing_filter", "step 2/8: releasing filter", s.releaseFilter)
	}); err != nil {
		return err
	}

	if err := runPhase(stepPlacingMug, "placing_mug_at_water", "step 3/8: placing mug at the hot water", s.placeMugAtWater); err != nil {
		return err
	}
	// A mug was pulled off the shelf; count it now so a later fault still credits real use.
	mugLabel, mugKey := cupCounter(drink)
	s.countUsage(ctx, mugLabel, mugKey, 1)

	addHotWater := func(ctx, cancelCtx context.Context) error { return s.dispenseHotWater(ctx, cancelCtx, drink) }
	if err := runPhase(stepAddingHotWater, "adding_hot_water", "step 4/8: adding hot water", addHotWater); err != nil {
		return err
	}
	s.countUsage(ctx, "water", "water_used", waterDelta(drink))

	serve := func(ctx, cancelCtx context.Context) error {
		_, err := s.grabAndShelve(ctx, cancelCtx, s.regrabMugAtWater)
		return err
	}
	if err := runPhase(stepServing, "serving", "step 5/8: serving mug", serve); err != nil {
		return err
	}
	// Mug drinks are pickup-only. The serve is planned ahead of the arm, so the
	// drink is only ready once the arm has set it down and backed off.
	s.handOffEffect(func() {
		if err := s.speaker.SayAlways(ctx, speech.DrinkReady(o.Label(), o.DisplayName(), o.BatchIndex, o.BatchSize)); err != nil {
			logger.Warnf("failed to say drink-ready: %v", err)
		}
	})

	// Retrieving the portafilter through the move home is one chunk, as in
	// prepareDrink.
	if err := s.inChunk("retrieve portafilter", func() error {
		if err := runPhase(stepGrabbingFilter, "grabbing_filter", "step 6/8: grabbing filter", s.grabFilter); err != nil {
			return err
		}
		if err := runPhase(stepUnlockingPortafilter, "unlocking_portafilter", "step 7/8: unlocking portafilter", s.unlockPortaFilter); err != nil {
			return err
		}
		moveHome := func(ctx, cancelCtx context.Context) error {
			return s.executeStep(ctx, cancelCtx, Step{PoseName: filterPoseHome, PoseSwitch: s.filterSw})
		}
		return runPhase(stepFinishingUp, "moving_home", "step 8/8: moving to home pose", moveHome)
	}); err != nil {
		return err
	}

	logger.Infof("%s preparation complete", drink)
	return nil
}

// serveAmericano finishes an Americano once the lungo has poured into the mug
// under the group head: grasp the mug (approaching it first unless the arm
// already lined up during the pour), slide it to the hot water spout, set it
// down, top it up with hot water, and pick it back up — one chunk of its own,
// the top-up stretch — then hand the held mug to placeDrink to set down.
// Returns where placeDrink put it.
//
// The top-up runs before the placement rather than as its grab, so a slot that
// fails to plan only re-records the placement, never the top-up.
func (s *beanjaminCoffee) serveAmericano(ctx, cancelCtx context.Context, approached bool, placeDrink servePlacement, runPhase phaseRunner) (int, error) {
	if err := s.inChunk(chunkAmericanoTopUp, func() error {
		return s.topUpAmericano(ctx, cancelCtx, approached, runPhase)
	}); err != nil {
		return -1, err
	}
	s.setStep(stepServing)
	// The mug is already in the gripper: no grab.
	return placeDrink(ctx, cancelCtx, nil)
}

// chunkAmericanoTopUp is the chunk the Americano's top-up is planned in.
const chunkAmericanoTopUp = "top up americano"

// topUpAmericano is serveAmericano's top-up stretch: it ends with the topped-up
// mug in the gripper, clear of the spout.
func (s *beanjaminCoffee) topUpAmericano(ctx, cancelCtx context.Context, approached bool, runPhase phaseRunner) error {
	mug := s.mugVessel()
	if !approached {
		if err := s.approachBrewedVessel(ctx, cancelCtx, mug); err != nil {
			return err
		}
	}
	if err := s.graspBrewedInPlace(ctx, cancelCtx, mug); err != nil {
		return err
	}
	if err := s.slideMugToWater(ctx, cancelCtx); err != nil {
		return err
	}
	if err := s.setMugAtWater(ctx, cancelCtx); err != nil {
		return err
	}
	addHotWater := func(ctx, cancelCtx context.Context) error { return s.dispenseHotWater(ctx, cancelCtx, "americano") }
	if err := runPhase(stepAddingHotWater, "adding_hot_water", "step 6c/9: adding hot water", addHotWater); err != nil {
		return err
	}
	return s.regrabMugAtWater(ctx, cancelCtx)
}

// serveHotWater is the serve_hot_water action: the hot water drink's mug
// phases end to end (place a mug at the spout, add hot water, pick it back up
// and serve it). Like serve_iced_coffee it assumes the gripper is free, i.e.
// the portafilter is already out of it.
func (s *beanjaminCoffee) serveHotWater(ctx, cancelCtx context.Context) (int, error) {
	if err := s.placeMugAtWater(ctx, cancelCtx); err != nil {
		return -1, err
	}
	if err := s.dispenseHotWater(ctx, cancelCtx, "hot_water"); err != nil {
		return -1, err
	}
	return s.grabAndShelve(ctx, cancelCtx, s.regrabMugAtWater)
}
