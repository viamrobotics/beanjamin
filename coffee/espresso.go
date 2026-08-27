package coffee

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang/geo/r3"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/module/trace"
)

const (
	shortPause   = 100 * time.Millisecond
	gripperPause = 500 * time.Millisecond
	pourPause    = 3 * time.Second
)

const (
	//filter pose switches
	filterPoseGrinderApproach            = "grinder_approach"
	filterPoseGrinderActivate            = "grinder_activate"
	filterPoseDecafGrinderApproach       = "decaf_grinder_approach"
	filterPoseDecafGrinderActivate       = "decaf_grinder_activate"
	filterPoseTamperApproach             = "tamper_approach"
	filterPoseTamperActivate             = "tamper_activate"
	filterPoseCoffeeApproach             = "coffee_approach"
	filterPoseCoffeeIn                   = "coffee_in"
	filterPoseCoffeeLockedFinal          = "coffee_locked_final"
	filterPoseHome                       = "home"
	filterPoseCloseToCleaning            = "close_to_cleaning"
	filterPoseApproachToCleaningScrapper = "approach_to_cleaning_scrapper"
	filterPoseCleaningScrapperActive     = "cleaning_scrapper_active"
	filterPoseApproachToCleaningBrush    = "approach_to_cleaning_brush"
	filterPoseCleaningBrushActive        = "cleaning_brush_active"
	filterPoseCoffeeShake                = "coffee_shake"

	// Keep-alive purge poses (required only when keepalive is configured). Both
	// are filter-frame poses: the arm holds the portafilter while it presses the
	// machine's 1 CUP button, so the portafilter is what gets positioned.
	filterPosePurgeApproach = "purge_approach"
	filterPosePurgePress    = "purge_press"

	//claw pose switches
	clawPoseCoffeeButtonApproach    = "coffee_button_approach"
	clawPoseCoffeeButtonOn          = "coffee_button_on"
	clawPoseCoffeeButtonOff         = "coffee_button_off"
	clawPoseFilterReleased          = "filter_released"
	clawPoseCoffeeLockedFinal       = "coffee_locked_final"
	clawPoseCupReadyForCoffee       = "cup_ready_for_coffee"
	clawPoseCupUnderMachineApproach = "cup_under_machine_approach"

	// Brew-button claw poses, used when has_separate_brew_buttons is set. That
	// machine has one momentary button per shot size, so each size gets its own
	// standoff and press pose and the press is a straight-in linear poke from
	// directly in front of that button.
	clawPoseEspressoButtonApproach = "espresso_button_approach"
	clawPoseEspressoButtonPress    = "espresso_button_press"
	clawPoseLungoButtonApproach    = "lungo_button_approach"
	clawPoseLungoButtonPress       = "lungo_button_press"

	// iced-coffee claw poses (only required when can_serve_iced is set; the
	// glass itself is vision-detected via the glass observe switch).
	clawPoseIceMachineApproach = "ice_machine_approach" // staged in front of the ice chute
	clawPoseIceMachineDispense = "ice_machine_dispense" // glass held under the chute while the pin pulses
	clawPoseStagingApproach    = "staging_approach"     // above the staging area
	clawPoseStaging            = "staging"              // down in the staging area, ready to release the glass
	clawPosePourApproach       = "pour_approach"        // espresso cup upright above the staged glass
	clawPosePour               = "pour"                 // espresso cup tilted to pour over the ice

	// camera pose switches (extra vantages live on
	// the same switch and are enumerated at runtime).
	camPoseCupObserve = "cup_observe"
)

const (
	// Frame names
	componentFilter = "filter"
	componentClaws  = "coffee-claws-middle"
	gripPoint       = "grip-point"
)

// glassPoseObserve is the home/recovery observe pose on the glass observe
// switch (parallel to camPoseCupObserve on the cup observe switch).
const glassPoseObserve = "glass_observe"

// requiredPose pairs a pose name with the switch it must resolve on. Used by
// validateConfiguredPoses.
type requiredPose struct {
	sw       toggleswitch.Switch
	poseName string
}

// requiredPoses returns the set of switch poses that the currently-enabled
// configuration can drive the arm to. The core brew cycle (grind → tamp →
// lock → release → brew → grab → unlock → home) always runs, so its poses are
// always required. Cleaning poses are likewise always included: the
// recovery path in rewind() runs cleanPortafilter whenever the portafilter
// holds grounds, which is the case for every order once grinding starts. Optional features (decaf, iced coffee) contribute their
// poses only when their config flag is set.
func (s *beanjaminCoffee) requiredPoses() []requiredPose {
	poses := []requiredPose{
		// step 1: grind (regular)
		{s.filterSw, filterPoseGrinderApproach},
		{s.filterSw, filterPoseGrinderActivate},
		// step 2: tamp
		{s.filterSw, filterPoseTamperApproach},
		{s.filterSw, filterPoseTamperActivate},
		// step 3: lock portafilter
		{s.filterSw, filterPoseCoffeeApproach},
		{s.filterSw, filterPoseCoffeeIn},
		{s.filterSw, filterPoseCoffeeLockedFinal},
		// step 4: release filter
		{s.clawsSw, clawPoseFilterReleased},
		// step 7: grab filter
		{s.clawsSw, clawPoseCoffeeLockedFinal},
		// step 9: home
		{s.filterSw, filterPoseHome},
		// cleaning (post-brew and rewind recovery)
		{s.filterSw, filterPoseCloseToCleaning},
		{s.filterSw, filterPoseApproachToCleaningScrapper},
		{s.filterSw, filterPoseCleaningScrapperActive},
		{s.filterSw, filterPoseApproachToCleaningBrush},
		{s.filterSw, filterPoseCleaningBrushActive},
	}

	// step 6: brew. Which claw poses exist depends on the machine. On the
	// button machine both shot sizes are required even if only one is ordered
	// today — the drink is only known per-order, so a switch that can't reach
	// the lungo button is misconfigured regardless of what's queued.
	if s.cfg.HasSeparateBrewButtons {
		poses = append(poses,
			requiredPose{s.clawsSw, clawPoseEspressoButtonApproach},
			requiredPose{s.clawsSw, clawPoseEspressoButtonPress},
			requiredPose{s.clawsSw, clawPoseLungoButtonApproach},
			requiredPose{s.clawsSw, clawPoseLungoButtonPress},
		)
	} else {
		poses = append(poses,
			requiredPose{s.clawsSw, clawPoseCoffeeButtonApproach},
			requiredPose{s.clawsSw, clawPoseCoffeeButtonOn},
			requiredPose{s.clawsSw, clawPoseCoffeeButtonOff},
		)
	}

	// step 8: unlock portafilter. The arm only travels to the shake pose when a
	// shake duration is configured.
	if s.cfg.PortafilterShakeSec > 0 {
		poses = append(poses, requiredPose{s.filterSw, filterPoseCoffeeShake})
	}

	if s.cfg.CanServeDecaf {
		poses = append(poses,
			requiredPose{s.filterSw, filterPoseDecafGrinderApproach},
			requiredPose{s.filterSw, filterPoseDecafGrinderActivate},
		)
	}

	// The keep-alive purge holds the 1 CUP button with the portafilter still in
	// the claws. Gated: a machine without keepalive configured never travels to
	// these poses, and requiring them unconditionally would fail construction on
	// every existing machine.
	if s.cfg.KeepAlive != nil {
		poses = append(poses,
			requiredPose{s.filterSw, filterPosePurgeApproach},
			requiredPose{s.filterSw, filterPosePurgePress},
		)
	}

	poses = append(poses,
		requiredPose{s.clawsSw, clawPoseCupUnderMachineApproach},
		requiredPose{s.clawsSw, clawPoseCupReadyForCoffee},
		requiredPose{s.cameraObserveSw, camPoseCupObserve},
	)

	if s.cfg.CanServeIced {
		// serveIcedCoffee dispenses ice, stages the glass, and pours the
		// espresso over the ice (the cup-retrieval poses above always run).
		poses = append(poses,
			requiredPose{s.clawsSw, clawPoseIceMachineApproach},
			requiredPose{s.clawsSw, clawPoseIceMachineDispense},
			requiredPose{s.clawsSw, clawPoseStagingApproach},
			requiredPose{s.clawsSw, clawPoseStaging},
			requiredPose{s.clawsSw, clawPosePourApproach},
			requiredPose{s.clawsSw, clawPosePour},
			requiredPose{s.glassObserveSw, glassPoseObserve},
		)
	}

	return poses
}

// validateConfiguredPoses checks, for the currently-enabled configuration,
// that every switch pose the service can move to actually resolves on its pose
// switch and is non-zero. A missing pose surfaces as a get_pose_by_name error
// from the switch; an all-zero translation indicates an unset/placeholder pose
// that would silently drive the arm to the base origin. Called once at
// construction so a misconfigured switch fails fast instead of mid-order.
func (s *beanjaminCoffee) validateConfiguredPoses(ctx context.Context) error {
	poses := s.requiredPoses()
	for _, rp := range poses {
		pd, err := s.fetchPose(ctx, rp.sw, rp.poseName)
		if err != nil {
			return fmt.Errorf("pose validation: required pose %q on %q switch: %w", rp.poseName, rp.sw.Name().ShortName(), err)
		}
		if pd.pose.Point() == (r3.Vector{}) {
			return fmt.Errorf("pose validation: required pose %q on %q switch resolves to a zero position — is it configured?", rp.poseName, rp.sw.Name().ShortName())
		}
	}
	s.logger.Infof("pose validation: %d configured pose(s) resolved and non-zero", len(poses))
	return nil
}

// say queues text for the speech service when conversational mode is
// enabled, otherwise no-ops. Use this for status-narrating lines (greetings,
// progress prompts, rejections) that an external orchestrator may want to
// own instead. For lines that must always be spoken regardless of mode
// (e.g. the drink-ready handoff), use sayAlways.
func (s *beanjaminCoffee) say(ctx context.Context, text string) error {
	if !s.cfg.Conversational {
		return nil
	}
	return s.sayAlways(ctx, text)
}

// sayAlways queues text for the speech service via the non-blocking
// say_async DoCommand, regardless of the Conversational config. It
// returns as soon as the text is accepted by the speech service's async
// queue; the audio will be played once any in-flight speech has finished.
// No-op when no speech service is configured.
func (s *beanjaminCoffee) sayAlways(ctx context.Context, text string) error {
	if s.speech == nil {
		return nil
	}
	_, err := s.speech.DoCommand(ctx, map[string]any{
		"say_async": text,
	})
	return err
}

// readyForDelivery handles the cup-handoff moment for delivery-fulfillment
// orders, replacing the pickup drink-ready announcement: it sends the
// delivery_request to the delivery machine and waits for its acknowledgment
// (bounded by deliveryMessageTimeout) before speaking, so the order isn't
// announced as handed off on the strength of a request nobody confirmed.
// The caller sets order.PickupPosition from the serving step.
func (s *beanjaminCoffee) readyForDelivery(ctx context.Context, order Order) error {
	s.notifyDeliveryRequest(ctx, order)
	drink := speakableDrink(order.Drink)
	text := fmt.Sprintf("%s ready for delivery!", drink)
	if order.CustomerName != "" {
		text = fmt.Sprintf("%s for %s, ready for delivery!", drink, order.CustomerName)
	}
	return s.sayAlways(ctx, text)
}

// recordOrderHistory credits a completed drink to the customer's history; no-op
// without an email or detector, best-effort otherwise.
func (s *beanjaminCoffee) recordOrderHistory(ctx context.Context, order Order) {
	if s.customerDetector == nil || order.CustomerEmail == "" {
		return
	}
	if _, err := s.customerDetector.DoCommand(ctx, map[string]any{
		"record_order": map[string]any{
			"email": order.CustomerEmail,
			"drink": order.Drink,
		},
	}); err != nil {
		s.activeOrderLogger().Warnf("failed to record order history for %q: %v", order.CustomerEmail, err)
	}
}

// actionFuncs is the execute_action surface for this machine's configuration.
// Split out from executeAction so which actions exist is assertable without a
// constructed service (executeAction refreshes the frame system before
// dispatching, so it needs real dependencies).
//
// The three placement actions report a serving slot (for a delivery order's
// pickup_position), but a standalone execute_action has no order to attach it
// to, so their closures drop it and return only the error.
func (s *beanjaminCoffee) actionFuncs() map[string]func(ctx, cancelCtx context.Context) error {
	actions := map[string]func(ctx, cancelCtx context.Context) error{
		"grind_coffee":       s.grindCoffee,
		"grind_decaf":        s.grindDecaf,
		"tamp_ground":        s.tampGround,
		"lock_portafilter":   s.lockPortaFilter,
		"unlock_portafilter": s.unlockPortaFilter,
		"release_filter":     s.releaseFilter,
		"grab_filter":        s.grabFilter,
		"brew_coffee":        s.brewCoffee,
		"set_cup_for_coffee": s.setCupForCoffee,
		"clean_portafilter":  s.cleanPortafilter,
		"fetch_glass":        s.fetchGlass,               // vision-grab a glass off the shelf
		"pulse_ice_pin":      s.pulseIcePin,              // hardware only, no arm motion
		"dispense_ice":       s.dispenseIce,              // arm to chute + pulse + retreat
		"stage_glass":        s.stageGlass,               // set held glass down, release
		"grab_brewed_cup":    s.grabBrewedCupFromMachine, // retrieve cup from under machine
		"pour_espresso":      s.pourEspresso,             // pour held cup over staged glass
		"grab_staged_glass":  s.grabStagedGlass,          // re-grab the staged glass
		"give_full_cup_to_customer": func(ctx, cancelCtx context.Context) error {
			_, err := s.placeFullCupOnShelf(ctx, cancelCtx)
			return err
		},
		"place_held": func(ctx, cancelCtx context.Context) error { // place held vessel in serving area
			_, err := s.placeHeldInServingArea(ctx, cancelCtx)
			return err
		},
		"serve_iced_coffee": func(ctx, cancelCtx context.Context) error { // full sequence end-to-end
			_, err := s.serveIcedCoffee(ctx, cancelCtx)
			return err
		},
		"open_door":  s.openDoor,  // grip the fridge handle and swing it open
		"close_door": s.closeDoor, // grip the open door's handle and swing it shut
	}

	// Only register the button actions this machine actually has, so the
	// unknown-action error lists a set the operator can really run.
	if s.cfg.HasSeparateBrewButtons {
		actions["press_espresso_button"] = s.pressEspressoButton
		actions["press_lungo_button"] = s.pressLungoButton
		actions["brew_lungo"] = s.brewLungo
		// One keep-alive purge on demand. Gated on the machine, not on keepalive
		// being configured, so the purge poses can be verified before the loop is
		// switched on — and never offered on the toggle machine, where holding the
		// switch would pour an uncontrolled dose.
		actions["keepalive_purge"] = s.purge
	} else {
		actions["turn_coffee_button_on"] = s.turnCoffeeButtonOn
		actions["turn_coffee_button_off"] = s.turnCoffeeButtonOff
	}

	return actions
}

func (s *beanjaminCoffee) executeAction(ctx context.Context, name string) (map[string]any, error) {
	actions := s.actionFuncs()

	action, ok := actions[name]
	if !ok {
		names := make([]string, 0, len(actions))
		for k := range actions {
			names = append(names, k)
		}
		return nil, fmt.Errorf("unknown action %q, available actions: %v", name, names)
	}

	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("a sequence is already running")
	}
	defer s.running.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	// Pick up any out-of-band frame-system edits before planning. Guarded so a
	// held item or locked filter established by a prior action call (manual
	// step-by-step sequences span separate DoCommands) is preserved.
	if err := s.refreshFrameSystemIfClean(ctx); err != nil {
		return nil, fmt.Errorf("refresh frame system before action %q: %w", name, err)
	}

	s.logger.Infof("executing action %q", name)

	if err := action(ctx, cancelCtx); err != nil {
		return nil, err
	}

	s.logger.Infof("action %q complete", name)
	return map[string]any{"status": "complete", "action": name}, nil
}

// isDecafDrink reports whether the drink uses the decaf grinding path.
func isDecafDrink(drink string) bool {
	return drink == "decaf" || drink == "decaf_lungo"
}

// isLungoDrink reports whether the drink is a lungo-size pour, matching the
// lungo cases in drinkBrewTime.
func isLungoDrink(drink string) bool {
	return drink == "lungo" || drink == "decaf_lungo"
}

// isIcedDrink reports whether the drink uses the iced-coffee serving path
// (fetch glass -> dispense ice -> pour espresso over ice) instead of handing
// the espresso cup to the customer. It brews espresso like any other drink.
func isIcedDrink(drink string) bool {
	return drink == "iced_coffee"
}

// waterDelta returns the water-usage increment for a brew: 1.5 for lungo sizes
// (lungo/decaf_lungo), 1 otherwise (espresso/decaf).
func waterDelta(drink string) float64 {
	if isLungoDrink(drink) {
		return 1.5
	}
	return 1
}

func (s *beanjaminCoffee) prepareDrink(ctx context.Context, order Order) (err error) {
	drink, customerName := order.Drink, order.CustomerName
	batchIndex, batchSize := order.BatchIndex, order.BatchSize
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

	s.setStep(stepGrinding)
	isDecaf := isDecafDrink(drink)
	if isDecaf {
		logger.Infof("step 1/9: grinding decaf coffee")
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::grinding_decaf")
		err := s.grindDecaf(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "decaf grinder", "decaf_grinds", 1)
	} else {
		logger.Infof("step 1/9: grinding coffee")
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::grinding")
		err := s.grindCoffee(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "grinder", "regular_grinds", 1)
	}

	s.setStep(stepTamping)
	logger.Infof("step 2/9: tamping ground")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::tamping")
		err := s.tampGround(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	s.setStep(stepLockingPortafilter)
	logger.Infof("step 3/9: locking portafilter")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::locking_portafilter")
		err := s.lockPortaFilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	s.setStep(stepReleasingFilter)
	logger.Infof("step 4/9: releasing filter")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::releasing_filter")
		err := s.releaseFilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	s.setStep(stepPlacingCup)
	logger.Infof("step 5/9: placing cup")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::placing_cup")
		err := s.setCupForCoffee(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	s.setStep(stepBrewing)
	logger.Infof("step 6/9: brewing %s", drink)
	if err := s.say(ctx, pickAlmostReady()); err != nil {
		logger.Warnf("failed to say almost-ready: %v", err)
	}
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::brewing")
		err := s.brew(ctx, cancelCtx, drink)
		stepSpan.End()
		if err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "water", "usage", waterDelta(drink))
		s.incrementSensorReading(ctx, s.usageSensor, "drip tray", "drip_tray_brews", 1)
		// The pour reset the machine's own idle timer, so the keep-alive loop's
		// clock restarts here. Keyed off the brew rather than the order: a shot
		// that poured and then failed at serving still counts as machine use.
		s.recordMachineActivity()
	}

	s.setStep(stepServing)
	logger.Infof("step 6b/9: serving cup")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::serving")
		var servedSlot int
		var err error
		if isIcedDrink(drink) {
			servedSlot, err = s.serveIcedCoffee(ctx, cancelCtx)
		} else {
			servedSlot, err = s.placeFullCupOnShelf(ctx, cancelCtx)
		}
		stepSpan.End()
		if err != nil {
			return err
		}
		// Record where the drink physically landed so a delivery order can
		// report it as the pickup_position.
		order.PickupPosition = servedSlot
		if order.Fulfillment == FulfillmentDelivery {
			if err := s.readyForDelivery(ctx, order); err != nil {
				logger.Warnf("failed to announce ready-for-delivery: %v", err)
			}
		} else {
			if err := s.sayAlways(ctx, pickDrinkReady(drink, customerName, batchIndex, batchSize)); err != nil {
				logger.Warnf("failed to say drink-ready: %v", err)
			}
		}
	}

	s.setStep(stepGrabbingFilter)
	logger.Infof("step 7/9: grabbing filter")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::grabbing_filter")
		err := s.grabFilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	s.setStep(stepUnlockingPortafilter)
	logger.Infof("step 8/9: unlocking portafilter")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::unlocking_portafilter")
		err := s.unlockPortaFilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	s.setStep(stepCleaning)
	logger.Infof("post: cleaning portafilter")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::cleaning")
		err := s.cleanPortafilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "cleaner", "cleanings", 1)
	}

	s.setStep(stepFinishingUp)
	logger.Infof("step 9/9: moving to home pose")
	homeStep := Step{PoseName: filterPoseHome, PoseSwitch: s.filterSw}
	if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
		return err
	}

	logger.Infof("%s preparation complete", drink)
	return nil
}

const (
	defaultEspressoBrewTime = 8 * time.Second
	defaultLungoBrewTime    = 15 * time.Second
	defaultGrindTimeSec     = 7.5
	// defaultButtonPressHoldSec is the claw dwell on a brew button when
	// button_press_hold_sec is unset.
	defaultButtonPressHoldSec = 0.5
	// defaultIceDispenseSec is how long the ice pin is held HIGH when
	// ice_dispense_sec is unset.
	defaultIceDispenseSec = 5.0
)

// runSteps executes each step in order, wrapping the first failure with label
// (e.g. "tamp_ground") so the caller's error identifies the failed phase.
func (s *beanjaminCoffee) runSteps(ctx, cancelCtx context.Context, label string, steps ...Step) error {
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) executeStep(ctx, cancelCtx context.Context, step Step) error {
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(ctx, "beanjamin::executeStep::"+step.PoseName)
	defer span.End()

	select {
	case <-ctx.Done():
		return fmt.Errorf("cancelled before %q: %w", step.PoseName, ctx.Err())
	case <-cancelCtx.Done():
		return fmt.Errorf("cancelled before %q", step.PoseName)
	default:
	}

	if step.PivotFromPose != "" {
		logger.Infof("pivoting from %q to %q", step.PivotFromPose, step.PoseName)
		if err := s.executePivot(ctx, cancelCtx, step); err != nil {
			return err
		}
	} else if step.CircularRadiusMm > 0 {
		logger.Infof("circular motion around %q", step.PoseName)
		if err := s.executeCircularMotion(ctx, cancelCtx, step); err != nil {
			return err
		}
	} else {
		logger.Infof("moving to %q", step.PoseName)
		if err := s.moveToPose(ctx, cancelCtx, step); err != nil {
			return err
		}
	}

	if step.Pause > 0 {
		logger.Infof("pausing %s after %q", step.Pause, step.PoseName)
		select {
		case <-time.After(step.Pause):
		case <-ctx.Done():
			return fmt.Errorf("cancelled during pause after %q: %w", step.PoseName, ctx.Err())
		case <-cancelCtx.Done():
			return fmt.Errorf("cancelled during pause after %q", step.PoseName)
		}
	}
	return nil
}
