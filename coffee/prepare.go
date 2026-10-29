package coffee

// prepareDrink: the nine-phase brew cycle for one order.

import (
	"context"
	"fmt"
	"strings"

	"beanjamin/coffee/order"
	"beanjamin/coffee/speech"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"
)

// prepareDrink runs the brew cycle for one order. The order queue holds the arm
// for the whole call and passes the context its claim returned as cancelCtx.
func (s *beanjaminCoffee) prepareDrink(ctx, cancelCtx context.Context, o order.Order) (err error) {
	drink, customerName := o.Drink, o.DisplayName()
	batchIndex, batchSize := o.BatchIndex, o.BatchSize
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(ctx, "beanjamin::prepareDrink["+drink+"]")
	defer span.End()

	// Capture the step the order errored at while the queue still holds the arm.
	// Cancel and reset_world wait for the arm to be released and then mutate
	// currentStep, so reading it any later would race with them.
	defer func() {
		if err != nil {
			step, _ := s.currentStep.Load().(string)
			s.failedStep.Store(step)
		}
	}()

	// Runs while the queue still holds the arm, because an operator command
	// claiming it afterward can clear the very state this inspects. A cancelled
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

	// Registered last so it settles before the defers above read err.
	endFlow := s.startFlow(ctx, cancelCtx)
	defer func() {
		if flowErr := endFlow(); err == nil {
			err = flowErr
		}
	}()

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

	beans := o.Beans
	if beans == "" {
		beans = order.BeansRegular
	}
	grind := func(ctx, cancelCtx context.Context) error { return s.grindBeans(ctx, cancelCtx, beans) }
	grindSpan := "grinding"
	if beans != order.BeansRegular {
		grindSpan += "_" + beans
	}
	if err := runPhase(stepGrinding, grindSpan, "step 1/9: grinding "+order.DrinkLabel("coffee", beans), grind); err != nil {
		return err
	}
	s.incrementSensorReading(ctx, s.usageSensor, order.DrinkLabel("grinder", beans), grindCounter(beans), 1)

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

	// A delivery drink headed for the cart needs its slot found while the
	// gripper is still empty, since the observe poses tilt the camera down at
	// the cart. A hot drink does it during the pour instead of lining up over the
	// cup; everything else does it just before the drink is grabbed.
	toCart := o.Fulfillment == order.FulfillmentDelivery && s.deliverySlotsVision != nil
	var cartSlot deliverySlot
	cartSlotFound, cartLocated := false, false
	locateCart := func(ctx, cancelCtx context.Context) (err error) {
		cartSlot, cartSlotFound, err = s.locateCartSlot(ctx, cancelCtx, o.ID)
		cartLocated = true
		return err
	}
	hotPrep := s.approachBrewedCup
	if toCart {
		hotPrep = locateCart
	}

	brewProgress := fmt.Sprintf("step 6/9: brewing %s", drink)
	brewPhase := func(ctx, cancelCtx context.Context) error { return s.brew(ctx, cancelCtx, drink) }
	if armFreeDuringPour {
		if order.IsIced(drink) {
			brewProgress += " while prepping the iced glass"
		}
		brewPhase = func(ctx, cancelCtx context.Context) error {
			if err := s.brewAndPrep(ctx, cancelCtx, drink, hotPrep); err != nil {
				return err
			}
			cupApproached = !order.IsIced(drink) && !toCart
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

	// Every iced latte gets the first milk in milk_options.
	milk := ""
	if order.IsMilk(drink) {
		milk = s.firstMilk()
	}
	var servedSlot int
	inCart := false
	// placeDrink grabs the filled drink and sets it down: in the docked cart for
	// a delivery order when cart placement is configured, else on the shelf.
	placeDrink := func(ctx, cancelCtx context.Context, grab grabDrink) (int, error) {
		if toCart && !cartLocated {
			if err := locateCart(ctx, cancelCtx); err != nil {
				return -1, err
			}
		}
		if err := grab(ctx, cancelCtx); err != nil {
			return -1, err
		}
		if cartSlotFound {
			placed, err := s.placeHeldInCart(ctx, cancelCtx, cartSlot)
			if placed {
				// Recorded before err is looked at: a fault after the release must
				// not fail a drink that is already in the cart.
				inCart = true
				s.delivery.markHandedOver(o.ID, cartSlot.tagID)
				return cartSlot.tagID, err
			}
			if err != nil {
				return -1, err
			}
		}
		return s.placeHeldInServingArea(ctx, cancelCtx, heldFilled)
	}
	serve := func(ctx, cancelCtx context.Context) (err error) {
		switch {
		case order.IsIced(drink) && armFreeDuringPour:
			// Glass already iced and staged during the brew; finish the rest.
			servedSlot, err = s.finishIced(ctx, cancelCtx, milk, placeDrink)
		case order.IsIced(drink):
			servedSlot, err = s.serveIced(ctx, cancelCtx, milk, placeDrink)
		case cupApproached:
			// Hot drink, gripper already poised over the cup: grasp it and serve it.
			servedSlot, err = placeDrink(ctx, cancelCtx, s.graspBrewedCup)
		default:
			servedSlot, err = placeDrink(ctx, cancelCtx, s.grabBrewedCupFromMachine)
		}
		return err
	}
	if err := runPhase(stepServing, "serving", "step 6b/9: serving cup", serve); err != nil {
		return err
	}
	// Record where the drink physically landed so a delivery order can report it
	// as the pickup_position: a shelf slot, or the cart slot's tag id.
	o.PickupPosition = servedSlot
	if o.Fulfillment == order.FulfillmentDelivery {
		if !inCart {
			s.delivery.markReady(o, s.deliveryHeartbeatTimeout())
		}
		if err := s.readyForDelivery(ctx, o); err != nil {
			logger.Warnf("failed to announce ready-for-delivery: %v", err)
		}
	} else if err := s.speaker.SayAlways(ctx, speech.DrinkReady(o.Label(), customerName, batchIndex, batchSize)); err != nil {
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
// for the same by-hand recovery and proceed a cancel gets. The recorded state
// cannot be trusted to show everything a fault left behind — a cup knocked over
// or a half-finished pour is never modeled — so the operator looks before any
// further order runs. The log names whatever mid-cycle state is recorded, since
// that is what the operator has to put right. Must be called while holding the arm, like
// strandedState.
func (s *beanjaminCoffee) pauseOnFault(logger logging.Logger, fault error) {
	s.lease.pause()
	left := "no mid-cycle state recorded"
	if stranded := s.strandedState(); len(stranded) > 0 {
		left = "state left behind: " + strings.Join(stranded, ", ")
	}
	logger.Errorf("order faulted (%v), %s — queue paused; %s", fault, left, recoverByHand)
}

// strandedState names the mid-cycle state still recorded for the machine: a
// portafilter left in the group head or holding grounds, a filter frame locked
// to world, an item modeled in the gripper, a glass staged as an obstacle, or a
// fridge door modeled open. Empty means the recorded world is the one a brew
// cycle starts from. The non-atomic fields are owned by the arm holder, so
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
