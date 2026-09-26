package coffee

// prepareDrink: the nine-phase brew cycle for one order.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"beanjamin/coffee/order"
	"beanjamin/coffee/speech"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"
)

func (s *beanjaminCoffee) prepareDrink(ctx context.Context, o order.Order) (err error) {
	drink, customerName := o.Drink, o.DisplayName()
	batchIndex, batchSize := o.BatchIndex, o.BatchSize
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(ctx, "beanjamin::prepareDrink["+drink+"]")
	defer span.End()

	if !s.running.CompareAndSwap(false, true) {
		return errors.New("a sequence is already running")
	}
	defer s.running.Store(false)
	// Capture the step the order errored at before `running` flips false above
	// (LIFO defers: this runs first). Cancel and rewind wait for idle and then
	// mutate currentStep, so reading it any later would race with them.
	defer func() {
		if err != nil {
			step, _ := s.currentStep.Load().(string)
			s.failedStep.Store(step)
		}
	}()

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()
	// Runs before `running` flips false, because a rewind taking the gate
	// afterward starts clearing the very state this inspects. A cancelled
	// cancelCtx means an operator cancel or reset_world, which own the pause.
	defer func() {
		if err != nil && cancelCtx.Err() == nil {
			s.pauseOnFault(logger, err)
		}
	}()

	// Pick up any out-of-band frame-system edits (e.g. portafilter handle geometry
	// changed during calibration) before planning. Guarded so an in-flight held
	// item or locked filter from a prior call is preserved.
	if err := s.refreshFrameSystemIfClean(ctx); err != nil {
		return fmt.Errorf("refresh frame system before brew: %w", err)
	}

	logger.Infof("starting %s preparation (pour_wait=%v)", drink, s.drinkBrewTime(drink))

	if err := s.normalizeGripperAtStart(ctx); err != nil {
		return fmt.Errorf("normalize gripper before brew: %w", err)
	}

	// runPhase publishes the step label, logs the progress line and runs the
	// phase in its own trace span. Keep the label and the "step N/9" line in
	// sync: both surface to the UI, which collapses on the raw label.
	runPhase := func(step, spanName, progress string, fn func(ctx, cancelCtx context.Context) error) error {
		s.setStep(step)
		logger.Info(progress)
		phaseCtx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::"+spanName)
		defer stepSpan.End()
		return fn(phaseCtx, cancelCtx)
	}

	if order.IsDecaf(drink) {
		if err := runPhase(stepGrinding, "grinding_decaf", "step 1/9: grinding decaf coffee", s.grindDecaf); err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "decaf grinder", "decaf_grinds", 1)
	} else {
		if err := runPhase(stepGrinding, "grinding", "step 1/9: grinding coffee", s.grindCoffee); err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "grinder", "regular_grinds", 1)
	}

	if err := runPhase(stepTamping, "tamping", "step 2/9: tamping ground", s.tampGround); err != nil {
		return err
	}
	if err := runPhase(stepLockingPortafilter, "locking_portafilter", "step 3/9: locking portafilter", s.lockPortaFilter); err != nil {
		return err
	}
	if err := runPhase(stepReleasingFilter, "releasing_filter", "step 4/9: releasing filter", s.releaseFilter); err != nil {
		return err
	}
	if err := runPhase(stepPlacingCup, "placing_cup", "step 5/9: placing cup", s.setCupForCoffee); err != nil {
		return err
	}
	// A cup was pulled off the stack; count it now so a later fault still credits real use.
	s.incrementSensorReading(ctx, s.usageSensor, "espresso cups", "espresso_cups_used", 1)

	// The separate-buttons machine doses itself, leaving the arm free mid-pour.
	// Put that idle time to use (brewAndPrep): an iced drink stages its glass, and
	// an espresso or lungo lines the gripper up over the cup, so once the pour ends
	// only the grasp is left — no plan-and-move to wait through. The toggle machine
	// holds the switch throughout, so it stays sequential.
	armFreeDuringPour := s.cfg.HasSeparateBrewButtons
	cupApproached := false

	brewProgress := fmt.Sprintf("step 6/9: brewing %s", drink)
	brewPhase := func(ctx, cancelCtx context.Context) error { return s.brew(ctx, cancelCtx, drink) }
	if armFreeDuringPour {
		if order.IsIced(drink) {
			brewProgress += " while prepping the iced glass"
		}
		brewPhase = func(ctx, cancelCtx context.Context) error {
			if err := s.brewAndPrep(ctx, cancelCtx, drink); err != nil {
				return err
			}
			// brewAndPrep lines the gripper up over the cup for every non-iced drink.
			cupApproached = !order.IsIced(drink)
			return nil
		}
	}
	if err := s.speaker.Say(ctx, speech.AlmostReady()); err != nil {
		logger.Warnf("failed to say almost-ready: %v", err)
	}
	if err := runPhase(stepBrewing, "brewing", brewProgress, brewPhase); err != nil {
		return err
	}
	s.incrementSensorReading(ctx, s.usageSensor, "water", "usage", waterDelta(drink))
	s.incrementSensorReading(ctx, s.usageSensor, "drip tray", "drip_tray_brews", 1)

	var servedSlot int
	serve := func(ctx, cancelCtx context.Context) (err error) {
		switch {
		case order.IsIced(drink) && armFreeDuringPour:
			// Glass already iced and staged during the brew; finish the rest.
			servedSlot, err = s.finishIced(ctx, cancelCtx, order.IsMilk(drink))
		case order.IsIced(drink):
			servedSlot, err = s.serveIced(ctx, cancelCtx, order.IsMilk(drink))
		case cupApproached:
			// Hot drink, gripper already poised over the cup: grasp it and shelf it.
			if err = s.graspBrewedCup(ctx, cancelCtx); err != nil {
				return err
			}
			servedSlot, err = s.placeHeldInServingArea(ctx, cancelCtx, heldFilled)
		default:
			servedSlot, err = s.placeFullCupOnShelf(ctx, cancelCtx)
		}
		return err
	}
	if err := runPhase(stepServing, "serving", "step 6b/9: serving cup", serve); err != nil {
		return err
	}
	// Record where the drink physically landed so a delivery order can report it
	// as the pickup_position.
	o.PickupPosition = servedSlot
	if o.Fulfillment == order.FulfillmentDelivery {
		if err := s.readyForDelivery(ctx, o); err != nil {
			logger.Warnf("failed to announce ready-for-delivery: %v", err)
		}
	} else if err := s.speaker.SayAlways(ctx, speech.DrinkReady(drink, customerName, batchIndex, batchSize)); err != nil {
		logger.Warnf("failed to say drink-ready: %v", err)
	}

	if err := runPhase(stepGrabbingFilter, "grabbing_filter", "step 7/9: grabbing filter", s.grabFilter); err != nil {
		return err
	}
	if err := runPhase(stepUnlockingPortafilter, "unlocking_portafilter", "step 8/9: unlocking portafilter", s.unlockPortaFilter); err != nil {
		return err
	}
	if err := runPhase(stepCleaning, "cleaning", "post: cleaning portafilter", s.cleanPortafilter); err != nil {
		return err
	}
	s.incrementSensorReading(ctx, s.usageSensor, "cleaner", "cleanings", 1)

	s.setStep(stepFinishingUp)
	logger.Infof("step 9/9: moving to home pose")
	if err := s.executeStep(ctx, cancelCtx, Step{PoseName: filterPoseHome, PoseSwitch: s.filterSw}); err != nil {
		return err
	}

	logger.Infof("%s preparation complete", drink)
	return nil
}

// pauseOnFault pauses the queue after a genuine fault, so the next order waits
// for the same rewind → proceed recovery a cancel gets. The recorded state
// cannot be trusted to show everything a fault left behind — a cup knocked over
// or a half-finished pour is never modeled — so the operator looks before any
// further order runs. The log names whatever mid-cycle state is recorded, since
// that is what rewind has to undo. Must be called while holding the running
// gate, like strandedState.
func (s *beanjaminCoffee) pauseOnFault(logger logging.Logger, fault error) {
	s.paused.Store(true)
	left := "no mid-cycle state recorded"
	if stranded := s.strandedState(); len(stranded) > 0 {
		left = "state left behind: " + strings.Join(stranded, ", ")
	}
	logger.Errorf("order faulted (%v), %s — queue paused; "+
		"run 'rewind' to recover the arm (and shut the fridge by hand if it is open), then 'proceed'",
		fault, left)
}

// strandedState names the mid-cycle state still recorded for the machine: a
// portafilter left in the group head or holding grounds, a filter frame locked
// to world, an item modeled in the gripper, a glass staged as an obstacle, or a
// fridge door modeled open. Empty means the recorded world is the one a brew
// cycle starts from. The non-atomic fields are owned by the running gate, so
// the caller must hold it.
func (s *beanjaminCoffee) strandedState() []string {
	var stranded []string
	if s.portafilterInMachine.Load() {
		stranded = append(stranded, "portafilter in machine")
	}
	if s.portafilterHasGrounds.Load() {
		stranded = append(stranded, "grounds in portafilter")
	}
	if s.filterFrameLocked {
		stranded = append(stranded, "filter frame locked")
	}
	if s.heldItemAttached {
		stranded = append(stranded, "item in gripper")
	}
	if s.stagedGlassPlaced {
		stranded = append(stranded, "glass staged")
	}
	if s.doorOpenDegs != 0 {
		stranded = append(stranded, fmt.Sprintf("fridge door open %.0f°", s.doorOpenDegs))
	}
	return stranded
}
