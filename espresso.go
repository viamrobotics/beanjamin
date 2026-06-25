package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/referenceframe"
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

	//claw pose switches
	clawPoseCoffeeButtonApproach    = "coffee_button_approach"
	clawPoseCoffeeButtonOn          = "coffee_button_on"
	clawPoseCoffeeButtonOff         = "coffee_button_off"
	clawPoseFilterReleased          = "filter_released"
	clawPoseCoffeeLockedFinal       = "coffee_locked_final"
	clawPoseCupReadyForCoffee       = "cup_ready_for_coffee"
	clawPoseCupUnderMachineApproach = "cup_under_machine_approach"

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
	// component/frame names accepted by switchForComponent and used as
	// AllowedCollision frame names.
	componentFilter = "filter"
	componentClaws  = "coffee-claws-middle"
	componentCam    = "cam"
	// componentGlassCam routes observe-pose fetches to the dedicated glass
	// observe switch (glass_observe_pose_switcher_name) during dynamic glass
	// pickup. The switch drives the same camera frame as componentCam.
	componentGlassCam = "glass-cam"
)

// glassPoseObserve is the home/recovery observe pose on the glass observe
// switch (parallel to camPoseCupObserve on the cup observe switch).
const glassPoseObserve = "glass_observe"

// requiredPose pairs a switch pose name with the component (pose switch) it
// must resolve on. The component string matches the names accepted by
// switchForComponent ("filter" / "coffee-claws-middle"). Used by
// validateConfiguredPoses.
type requiredPose struct {
	component string
	poseName  string
}

// requiredPoses returns the set of switch poses that the currently-enabled
// configuration can drive the arm to. The core brew cycle (grind → tamp →
// lock → release → brew → grab → unlock → home) always runs, so its poses are
// always required. Cleaning poses are likewise always included: the
// cancel-recovery path in cancel() runs cleanPortafilter whenever the
// portafilter holds grounds, which is the case for every order once grinding
// starts. Optional features (decaf, iced coffee) contribute their
// poses only when their config flag is set.
func (s *beanjaminCoffee) requiredPoses() []requiredPose {
	poses := []requiredPose{
		// step 1: grind (regular)
		{componentFilter, filterPoseGrinderApproach},
		{componentFilter, filterPoseGrinderActivate},
		// step 2: tamp
		{componentFilter, filterPoseTamperApproach},
		{componentFilter, filterPoseTamperActivate},
		// step 3: lock portafilter
		{componentFilter, filterPoseCoffeeApproach},
		{componentFilter, filterPoseCoffeeIn},
		{componentFilter, filterPoseCoffeeLockedFinal},
		// step 4: release filter
		{componentClaws, clawPoseFilterReleased},
		// step 6: brew (coffee button on/off)
		{componentClaws, clawPoseCoffeeButtonApproach},
		{componentClaws, clawPoseCoffeeButtonOn},
		{componentClaws, clawPoseCoffeeButtonOff},
		// step 7: grab filter
		{componentClaws, clawPoseCoffeeLockedFinal},
		// step 8: unlock portafilter (adds the shake pose to the lock poses)
		{componentFilter, filterPoseCoffeeShake},
		// step 9: home
		{componentFilter, filterPoseHome},
		// cleaning (post-brew and cancel recovery)
		{componentFilter, filterPoseCloseToCleaning},
		{componentFilter, filterPoseApproachToCleaningScrapper},
		{componentFilter, filterPoseCleaningScrapperActive},
		{componentFilter, filterPoseApproachToCleaningBrush},
		{componentFilter, filterPoseCleaningBrushActive},
	}

	if s.cfg.CanServeDecaf {
		poses = append(poses,
			requiredPose{componentFilter, filterPoseDecafGrinderApproach},
			requiredPose{componentFilter, filterPoseDecafGrinderActivate},
		)
	}

	poses = append(poses,
		requiredPose{componentClaws, clawPoseCupUnderMachineApproach},
		requiredPose{componentClaws, clawPoseCupReadyForCoffee},
		requiredPose{componentCam, camPoseCupObserve},
	)

	if s.cfg.CanServeIced {
		// serveIcedCoffee dispenses ice, stages the glass, and pours the
		// espresso over the ice (the cup-retrieval poses above always run).
		poses = append(poses,
			requiredPose{componentClaws, clawPoseIceMachineApproach},
			requiredPose{componentClaws, clawPoseIceMachineDispense},
			requiredPose{componentClaws, clawPoseStagingApproach},
			requiredPose{componentClaws, clawPoseStaging},
			requiredPose{componentClaws, clawPosePourApproach},
			requiredPose{componentClaws, clawPosePour},
			requiredPose{componentGlassCam, glassPoseObserve},
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
		pd, err := s.fetchPose(ctx, rp.component, rp.poseName)
		if err != nil {
			return fmt.Errorf("pose validation: required pose %q on %q switch: %w", rp.poseName, rp.component, err)
		}
		if pd.pose.Point() == (r3.Vector{}) {
			return fmt.Errorf("pose validation: required pose %q on %q switch resolves to a zero position — is it configured?", rp.poseName, rp.component)
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
	_, err := s.speech.DoCommand(ctx, map[string]interface{}{
		"say_async": text,
	})
	return err
}

// recordOrderHistory credits a completed drink to the customer's history; no-op
// without an email or detector, best-effort otherwise.
func (s *beanjaminCoffee) recordOrderHistory(ctx context.Context, order Order) {
	if s.customerDetector == nil || order.CustomerEmail == "" {
		return
	}
	if _, err := s.customerDetector.DoCommand(ctx, map[string]interface{}{
		"record_order": map[string]interface{}{
			"email": order.CustomerEmail,
			"drink": order.Drink,
		},
	}); err != nil {
		s.activeOrderLogger().Warnf("failed to record order history for %q: %v", order.CustomerEmail, err)
	}
}

var coffeeBrewingCollisions = []AllowedCollision{
	{Frame1: componentFilter, Frame2: "coffee-machine-actuation-area"},
	{Frame1: "portafilter-handle", Frame2: "coffee-machine-actuation-area"},
	{Frame1: componentClaws, Frame2: "coffee-machine-actuation-area"},
	{Frame1: "gripper:claws", Frame2: "coffee-machine-actuation-area"},
}

var filterGrabCollisions = []AllowedCollision{
	{Frame1: componentClaws, Frame2: "portafilter-handle"},
	{Frame1: "gripper:claws", Frame2: "portafilter-handle"},
	{Frame1: "gripper:case-gripper", Frame2: "portafilter-handle"},
}

var cleaningCollisions = []AllowedCollision{
	{Frame1: componentFilter, Frame2: "cleaner-top"},
	{Frame1: "portafilter-handle", Frame2: "cleaner-top"},
	{Frame1: componentClaws, Frame2: "cleaner-top"},
}

var clawCoffeeButtonCollisions = []AllowedCollision{
	{Frame1: componentClaws, Frame2: "coffee-machine-buffer-front"},
	{Frame1: "gripper:claws", Frame2: "coffee-machine-buffer-front"},
}

// Held-item surface collisions (track_held_geometry). When a cup/glass geometry
// is attached to the gripper, the held item must be allowed to approach the
// modeled surfaces it legitimately gets close to during a contact phase — the
// same allowances the bare claws already carry. The gripper-overlap pairs
// (heldItemSelfCollisions) are auto-injected for every held move; these cover
// the per-surface phases and are applied via heldItemSurfaceCollisions so they
// only take effect while an item is actually attached.
var heldItemMachineCollisions = []AllowedCollision{
	{Frame1: heldItemFrameName, Frame2: "coffee-machine-base"},
}

var heldItemServingAreaCollisions = []AllowedCollision{
	{Frame1: heldItemFrameName, Frame2: servingAreaFrameName},
}

func (s *beanjaminCoffee) executeAction(ctx context.Context, name string) (map[string]interface{}, error) {
	actions := map[string]func(ctx, cancelCtx context.Context) error{
		"grind_coffee":              s.grindCoffee,
		"grind_decaf":               s.grindDecaf,
		"tamp_ground":               s.tampGround,
		"lock_portafilter":          s.lockPortaFilter,
		"unlock_portafilter":        s.unlockPortaFilter,
		"release_filter":            s.releaseFilter,
		"grab_filter":               s.grabFilter,
		"turn_coffee_button_on":     s.turnCoffeeButtonOn,
		"turn_coffee_button_off":    s.turnCoffeeButtonOff,
		"brew_coffee":               s.brewCoffee,
		"set_cup_for_coffee":        s.setCupForCoffee,
		"give_full_cup_to_customer": s.placeFullCupOnShelf,
		"clean_portafilter":         s.cleanPortafilter,
	}

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

	s.logger.Infof("executing action %q", name)

	if err := action(ctx, cancelCtx); err != nil {
		return nil, err
	}

	s.logger.Infof("action %q complete", name)
	return map[string]interface{}{"status": "complete", "action": name}, nil
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

func (s *beanjaminCoffee) prepareDrink(ctx context.Context, drink, customerName string, batchIndex, batchSize int) (err error) {
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(ctx, "beanjamin::prepareDrink["+drink+"]")
	defer span.End()

	if !s.running.CompareAndSwap(false, true) {
		return errors.New("a sequence is already running")
	}
	defer s.running.Store(false)
	// Capture the step the order errored at before `running` flips false above
	// (LIFO defers: this runs first). Cancel recovery waits for idle and then
	// mutates currentStep, so reading it any later would race with recovery.
	defer func() {
		if err != nil {
			step, _ := s.currentStep.Load().(string)
			s.failedStep.Store(step)
		}
	}()

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	brewTime := s.drinkBrewTime(drink)

	logger.Infof("starting %s preparation (brew_time=%v)", drink, brewTime)

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
		err := s.brew(ctx, cancelCtx, brewTime)
		stepSpan.End()
		if err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "water", "usage", waterDelta(drink))
	}

	s.setStep(stepServing)
	logger.Infof("step 6b/9: serving cup")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::serving")
		var err error
		if isIcedDrink(drink) {
			err = s.serveIcedCoffee(ctx, cancelCtx)
		} else {
			err = s.placeFullCupOnShelf(ctx, cancelCtx)
		}
		stepSpan.End()
		if err != nil {
			return err
		}
		if err := s.sayAlways(ctx, pickDrinkReady(drink, customerName, batchIndex, batchSize)); err != nil {
			logger.Warnf("failed to say drink-ready: %v", err)
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
	homeStep := Step{PoseName: filterPoseHome, Component: componentFilter}
	if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
		return err
	}

	logger.Infof("%s preparation complete", drink)
	return nil
}

func (s *beanjaminCoffee) grindCoffee(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: filterPoseGrinderApproach, Component: componentFilter, Pause: shortPause},
		{PoseName: filterPoseGrinderActivate, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseGrinderApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		// Circle under the grinder chute to distribute grounds evenly while the grinder dispenses.
		{PoseName: filterPoseGrinderApproach, Component: componentFilter,
			CircularRadiusMm: 8, CircularDurationSec: s.grindDurationSec(), CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		// Mark grounds only as we reach the activate pose: the approach move
		// keeps the filter clean, and the grinder dispenses once it's under the
		// chute. From here onward any cancel must clean the filter before home.
		if step.PoseName == filterPoseGrinderActivate {
			s.portafilterHasGrounds.Store(true)
		}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("grind_coffee: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) grindDecaf(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: filterPoseDecafGrinderApproach, Component: componentFilter, Pause: shortPause},
		{PoseName: filterPoseDecafGrinderActivate, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseDecafGrinderApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		// Circle under the decaf grinder chute to distribute grounds evenly while the grinder dispenses.
		{PoseName: filterPoseDecafGrinderApproach, Component: componentFilter,
			CircularRadiusMm: 8, CircularDurationSec: s.grindDurationSec(), CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		// Mark grounds only as we reach the activate pose: the approach move
		// keeps the filter clean, and the grinder dispenses once it's under the
		// chute. From here onward any cancel must clean the filter before home.
		if step.PoseName == filterPoseDecafGrinderActivate {
			s.portafilterHasGrounds.Store(true)
		}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("grind_decaf: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) tampGround(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: filterPoseTamperApproach, Component: componentFilter, Pause: shortPause},
		{PoseName: filterPoseTamperActivate, Component: componentFilter, Pause: 3000 * time.Millisecond, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseTamperApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("tamp_ground: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) lockPortaFilter(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: filterPoseCoffeeApproach, Component: componentFilter, Pause: shortPause},
		{PoseName: filterPoseCoffeeIn, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: filterPoseCoffeeLockedFinal, Component: componentFilter, PivotFromPose: filterPoseCoffeeIn, PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("lock_portafilter: %w", err)
		}
	}
	if err := s.lockFilterFrame(ctx); err != nil {
		return fmt.Errorf("lock filter frame: %w", err)
	}
	return nil
}

func (s *beanjaminCoffee) unlockPortaFilter(ctx, cancelCtx context.Context) error {
	if err := s.unlockFilterFrame(ctx); err != nil {
		return fmt.Errorf("unlock filter frame: %w", err)
	}
	steps := []Step{
		{PoseName: filterPoseCoffeeIn, Component: componentFilter, PivotFromPose: filterPoseCoffeeLockedFinal, PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: filterPoseCoffeeShake, Component: componentFilter, AllowedCollisions: coffeeBrewingCollisions, LinearConstraint: defaultApproachConstraint},
		// Shake the filter laterally to dislodge the puck.
		{PoseName: filterPoseCoffeeShake, Component: componentFilter,
			CircularRadiusMm: 4, CircularDurationSec: s.cfg.PortafilterShakeSec, CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: filterPoseCoffeeApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("unlock_portafilter: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) releaseFilter(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("release_filter: no gripper configured")
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("release_filter: open gripper: %w", err)
	}
	// Bayonet now holds the filter; arm is committed to leaving it behind.
	// Set the flag before motion so a mid-move cancel still triggers recovery.
	s.portafilterInMachine.Store(true)
	step := Step{PoseName: clawPoseFilterReleased, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterGrabCollisions}
	if err := s.executeStep(ctx, cancelCtx, step); err != nil {
		return fmt.Errorf("release_filter: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("release_filter: grab gripper: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

func (s *beanjaminCoffee) grabFilter(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("grab_filter: no gripper configured")
	}

	approachStep := Step{PoseName: clawPoseFilterReleased, Component: componentClaws}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("grab_filter: %w", err)
	}

	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("grab_filter: open gripper: %w", err)
	}

	alignStep := Step{PoseName: clawPoseCoffeeLockedFinal, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterGrabCollisions}
	if err := s.executeStep(ctx, cancelCtx, alignStep); err != nil {
		return fmt.Errorf("grab_filter: %w", err)
	}

	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("grab_filter: grab gripper: %w", err)
	}
	// Filter is firmly back in the claws; cancel no longer needs to recover.
	s.portafilterInMachine.Store(false)
	time.Sleep(gripperPause)
	return nil
}

func (s *beanjaminCoffee) setCupForCoffee(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("set_cup_for_coffee: no gripper configured")
	}

	if err := s.pickCupDynamic(ctx, cancelCtx); err != nil {
		return err
	}

	cupPlacementApproach := Step{PoseName: clawPoseCupUnderMachineApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, cupPlacementApproach); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	readyStep := Step{PoseName: clawPoseCupReadyForCoffee, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause, AllowedCollisions: s.heldItemSurfaceCollisions(heldItemMachineCollisions)}
	if err := s.executeStep(ctx, cancelCtx, readyStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Release the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: open gripper: %w", err)
	}
	// Give time for the gripper to open
	time.Sleep(gripperPause)
	// Cup is released under the machine; it no longer travels with the gripper.
	s.detachHeldGeometry()

	// Move away from the cup.
	exitStep := Step{PoseName: clawPoseCupUnderMachineApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, exitStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Close the gripper after moving away.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: close gripper: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

// placeFullCupOnShelf retrieves the brewed cup from cup_ready_for_coffee and
// drops it on the serving-area shelf at the next round-robin slot.
func (s *beanjaminCoffee) placeFullCupOnShelf(ctx, cancelCtx context.Context) error {
	if err := s.grabBrewedCupFromMachine(ctx, cancelCtx); err != nil {
		return err
	}
	return s.placeHeldInServingArea(ctx, cancelCtx)
}

// placeHeldInServingArea drops the item currently held by the gripper into the
// serving area: it walks the serving-area slots in round-robin order starting
// from servingAreaSlotCounter and drops the item in the first slot it can reach
// (tryDropCupInSlot), skipping any whose approach or descent cannot be planned.
// On success the counter advances to the slot after the one used, so the next
// placement starts there. The caller must already be holding the item; shared
// by placeFullCupOnShelf (cups) and serveIcedCoffee (the empty espresso cup and
// the iced glass).
func (s *beanjaminCoffee) placeHeldInServingArea(ctx, cancelCtx context.Context) error {
	logger := s.activeOrderLogger()
	if s.gripper == nil {
		return fmt.Errorf("place_in_serving_area: no gripper configured")
	}

	slots, shelfTopZ, err := s.servingAreaSlots(ctx)
	if err != nil {
		return fmt.Errorf("place_in_serving_area: %w", err)
	}

	n := len(slots)
	start := s.servingAreaSlotCounter.Load()
	var lastErr error
	for off := 0; off < n; off++ {
		idx := slotIndex(start+uint64(off), n)
		logger.Infof("place_in_serving_area: trying slot %d/%d", idx+1, n)
		err := s.tryDropCupInSlot(ctx, slots[idx], shelfTopZ)
		if err == nil {
			// Next placement starts at the slot after the one just used.
			s.servingAreaSlotCounter.Store(start + uint64(off) + 1)
			logger.Infof("place_in_serving_area: placed item in slot %d/%d", idx+1, n)
			return nil
		}
		lastErr = err

		// Operator cancel always wins.
		if ctx.Err() != nil || cancelCtx.Err() != nil {
			return fmt.Errorf("place_in_serving_area: cancelled: %w", err)
		}

		// Only planning failures (item still held, arm unmoved) are skippable.
		// Anything else — execution error, or any failure after the item was
		// released — bubbles up.
		if !errors.Is(err, errMotionPlanning) {
			return fmt.Errorf("place_in_serving_area: %w", err)
		}
		logger.Warnf("place_in_serving_area: slot %d/%d unreachable — trying next slot: %v", idx+1, n, err)
	}
	return fmt.Errorf("place_in_serving_area: all %d serving-area slot(s) unreachable; last error: %w", n, lastErr)
}

// tryDropCupInSlot drops the held cup at one serving-area slot: free-plan to the
// approach pose above the slot, descend linearly to the drop pose
// (claws-middle = shelfTopZ + shelfDropZOffsetMm), release, then retreat
// linearly and close the gripper.
//
// CupGrabRelativePose is the same relative offset used at pickup (composed onto
// the detected cup centroid) — composing it onto the placement anchor here
// keeps the claws-to-cup geometry identical between grab and release, so the
// cup lands centered on the slot.
//
// Returned errors split like tryGrabCup so placeFullCupOnShelf can react via
// errors.Is:
//   - wraps errMotionPlanning → the approach or descent could not be planned;
//     the cup is still held and the arm has not committed to the slot, so the
//     caller can try the next slot.
//   - anything else → an execution error, or any failure after the cup was
//     released; bubble up (do not try another slot with an empty gripper).
func (s *beanjaminCoffee) tryDropCupInSlot(ctx context.Context, tileWorld r3.Vector, shelfTopZ float64) error {
	logger := s.activeOrderLogger()
	dropAnchor := r3.Vector{
		X: tileWorld.X,
		Y: tileWorld.Y,
		Z: shelfTopZ + shelfDropZOffsetMm,
	}
	dropPose := composeCupPose(dropAnchor, relativePoseToSpatial(s.cfg.CupGrabRelativePose))
	approachPose := composeCupPose(dropAnchor, relativePoseToSpatial(s.cfg.CupApproachRelativePose))

	approachPD := &poseData{pose: approachPose, refFrame: referenceframe.World, componentName: componentClaws}
	dropPD := &poseData{pose: dropPose, refFrame: referenceframe.World, componentName: componentClaws}
	logger.Infof("shelf placement: slot (x=%.1f, y=%.1f) drop_pose=%v approach_pose=%v",
		tileWorld.X, tileWorld.Y, dropPose, approachPose)

	// 1. Carry the held cup to the approach pose above the slot. With
	// no_spill_carry set, step through level-pinned waypoints (carryHeldLevel)
	// so the drink doesn't slosh on the long traverse; otherwise free-plan
	// straight there. Both wrap planning failures in errMotionPlanning, so on
	// failure the arm has not moved and the cup is still held — the caller can
	// try the next slot.
	carry := func() error { return s.moveToRawPose(ctx, approachPD, nil, nil, nil) }
	if s.cfg.NoSpillCarry {
		carry = func() error { return s.carryHeldLevel(ctx, approachPD) }
	}
	if err := carry(); err != nil {
		return fmt.Errorf("approach slot (x=%.1f, y=%.1f): %w", tileWorld.X, tileWorld.Y, err)
	}

	// The cup is held during the approach and descent, so allow its geometry to
	// approach the shelf surface (no-op when tracking is off / nothing attached).
	shelfCollisions := s.heldItemSurfaceCollisions(heldItemServingAreaCollisions)

	// 2. Linear descent to the drop pose. A planning failure leaves the arm at
	// the approach pose still holding the cup — caller can try the next slot.
	if err := s.moveToRawPose(ctx, dropPD, defaultApproachConstraint, shelfCollisions, nil); err != nil {
		return fmt.Errorf("descend into slot (x=%.1f, y=%.1f): %w", tileWorld.X, tileWorld.Y, err)
	}

	// 3. Release the cup. Past this point the cup is committed to this slot;
	// the steps below strip any errMotionPlanning chain (%v) so the caller does
	// not retry another slot with an empty gripper.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("open gripper to release cup: %w", err)
	}
	time.Sleep(gripperPause)
	// Cup is released onto the shelf; it no longer travels with the gripper.
	s.detachHeldGeometry()

	// 4. Linear retreat back to the approach pose.
	if err := s.moveToRawPose(ctx, approachPD, defaultApproachConstraint, nil, nil); err != nil {
		return fmt.Errorf("retreat after releasing cup (slot x=%.1f, y=%.1f): %v", tileWorld.X, tileWorld.Y, err)
	}

	// 5. Close the gripper for the next move.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("close gripper after release: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

// runCupFlow exercises the full cup-handling path without brewing: for each of
// count iterations it dynamically picks a cup (sweeping observe poses until one
// sees a cup), sets it under the machine, retrieves it, and places it on the
// served shelf at the next round-robin slot. Intended for tuning the observe
// sweep + shelf placement on hardware.
//
// It assumes the portafilter has been physically removed from the claws — the
// flow never touches portafilter state. Each placement advances the shelf-slot
// counter inside placeFullCupOnShelf.
func (s *beanjaminCoffee) runCupFlow(ctx context.Context, count int) (map[string]interface{}, error) {
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("a sequence is already running")
	}
	defer s.running.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	// Not tied to a queued order, so there is no order ID to tag — use the
	// base service logger.
	logger := s.logger

	logger.Infof("run_cup_flow: starting %d iteration(s) (assumes portafilter physically removed)", count)
	for i := 1; i <= count; i++ {
		s.setStep(fmt.Sprintf("Cup flow %d/%d", i, count))
		logger.Infof("run_cup_flow: iteration %d/%d — pick cup + set under machine", i, count)
		if err := s.setCupForCoffee(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("run_cup_flow: iteration %d/%d: pickup: %w", i, count, err)
		}
		logger.Infof("run_cup_flow: iteration %d/%d — retrieve from machine + place on shelf", i, count)
		if err := s.placeFullCupOnShelf(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("run_cup_flow: iteration %d/%d: place-on-shelf: %w", i, count, err)
		}
	}

	homeStep := Step{PoseName: "home", Component: "filter"}
	if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
		return nil, fmt.Errorf("run_cup_flow: home: %w", err)
	}

	logger.Infof("run_cup_flow: complete (%d iteration(s))", count)
	return map[string]interface{}{"status": "complete", "iterations": count}, nil
}

// serveIcedCoffee finishes an iced_coffee order after the espresso has brewed
// into the cup under the machine. It fetches a separate glass, dispenses ice
// into it via the board pin, sets the glass down in the staging area, retrieves
// the espresso cup, and pours the espresso over the ice. Both finished items
// then go into the serving area at the next round-robin slots: the empty
// espresso cup first, then the iced glass (re-grabbed from staging).
func (s *beanjaminCoffee) serveIcedCoffee(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("serve_iced_coffee: no gripper configured")
	}
	if s.iceBoard == nil {
		return fmt.Errorf("serve_iced_coffee: no ice board configured (set ice_board_name)")
	}

	// 1. Fetch the glass off the top shelf.
	if err := s.fetchGlass(ctx, cancelCtx); err != nil {
		return err
	}
	// 2. Carry the glass to the ice machine and dispense ice.
	if err := s.dispenseIce(ctx, cancelCtx); err != nil {
		return err
	}
	// 3. Set the glass down in the staging area to free the gripper.
	if err := s.stageGlass(ctx, cancelCtx); err != nil {
		return err
	}
	// 4. Retrieve the brewed espresso cup from the machine.
	if err := s.grabBrewedCupFromMachine(ctx, cancelCtx); err != nil {
		return err
	}
	// 5. Pour the espresso over the ice in the staged glass.
	if err := s.pourEspresso(ctx, cancelCtx); err != nil {
		return err
	}
	// 6. Place the now-empty espresso cup in the serving area (round-robin).
	if err := s.placeHeldInServingArea(ctx, cancelCtx); err != nil {
		return err
	}
	// 7. Re-grab the iced glass from the staging area.
	if err := s.grabStagedGlass(ctx, cancelCtx); err != nil {
		return err
	}
	// 8. Place the iced glass in the serving area (next round-robin slot).
	return s.placeHeldInServingArea(ctx, cancelCtx)
}

// grabBrewedCupFromMachine retrieves the brewed cup from under the machine:
// approach -> open gripper -> linear descent + grab -> linear retreat. On return
// the cup is held by the gripper and the arm sits at cup_under_machine_approach.
func (s *beanjaminCoffee) grabBrewedCupFromMachine(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: no gripper configured")
	}
	approachStep := Step{PoseName: clawPoseCupUnderMachineApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: %w", err)
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: open gripper: %w", err)
	}
	time.Sleep(gripperPause)
	grabStep := Step{PoseName: clawPoseCupReadyForCoffee, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: %w", err)
	}
	if err := s.grabAndVerifyHolding(ctx); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: grab gripper: %w", err)
	}
	// The cup was tracked at pickup and released under the machine; restore its
	// geometry now that it's back in the gripper so the retreat routes around it.
	// grabAndVerifyHolding only returns nil on a confirmed grab, so this never
	// reattaches onto empty jaws.
	if err := s.reattachGeometry(pickupLabelCup); err != nil {
		s.activeOrderLogger().Warnf("grab_brewed_cup_from_machine: reattach cup geometry failed, continuing untracked: %v", err)
	}
	retreatStep := Step{PoseName: clawPoseCupUnderMachineApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause, AllowedCollisions: s.heldItemSurfaceCollisions(heldItemMachineCollisions)}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: %w", err)
	}
	return nil
}

// grabStagedGlass picks the iced glass back up from the staging area: approach
// -> open -> linear descent + grab -> linear retreat, leaving the glass held by
// the gripper. The reverse of stageGlass.
func (s *beanjaminCoffee) grabStagedGlass(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("grab_staged_glass: no gripper configured")
	}
	approachStep := Step{PoseName: clawPoseStagingApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("grab_staged_glass: %w", err)
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("grab_staged_glass: open gripper: %w", err)
	}
	time.Sleep(gripperPause)
	grabStep := Step{PoseName: clawPoseStaging, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("grab_staged_glass: %w", err)
	}
	if err := s.grabAndVerifyHolding(ctx); err != nil {
		return fmt.Errorf("grab_staged_glass: grab gripper: %w", err)
	}
	// The glass was tracked at pickup and set down in staging; restore its
	// geometry now that it's back in the gripper. grabAndVerifyHolding only
	// returns nil on a confirmed grab, so this never reattaches onto empty jaws.
	if err := s.reattachGeometry(pickupLabelGlass); err != nil {
		s.activeOrderLogger().Warnf("grab_staged_glass: reattach glass geometry failed, continuing untracked: %v", err)
	}
	retreatStep := Step{PoseName: clawPoseStagingApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("grab_staged_glass: %w", err)
	}
	return nil
}

// fetchGlass vision-detects an iced-coffee glass off the top shelf and grabs it,
// leaving it held by the gripper (see pickGlassDynamic). Iced coffee always uses
// vision glass pickup — there is no static fallback (can_serve_iced requires
// dynamic_glass_pickup).
func (s *beanjaminCoffee) fetchGlass(ctx, cancelCtx context.Context) error {
	if err := s.pickGlassDynamic(ctx, cancelCtx); err != nil {
		return fmt.Errorf("fetch_glass: %w", err)
	}
	return nil
}

// dispenseIce carries the held glass to the ice machine, holds it under the
// chute, pulses the ice pin HIGH for iceDispenseSec, then retreats. The pin is
// always driven back LOW — including on cancel — so the ice machine can't be
// left running.
func (s *beanjaminCoffee) dispenseIce(ctx, cancelCtx context.Context) error {
	logger := s.activeOrderLogger()
	approachStep := Step{PoseName: clawPoseIceMachineApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("dispense_ice: %w", err)
	}
	dispenseStep := Step{PoseName: clawPoseIceMachineDispense, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, dispenseStep); err != nil {
		return fmt.Errorf("dispense_ice: %w", err)
	}

	pinName := s.icePinName()
	pin, err := s.iceBoard.GPIOPinByName(pinName)
	if err != nil {
		return fmt.Errorf("dispense_ice: get pin %q: %w", pinName, err)
	}
	dwell := time.Duration(s.iceDispenseSec() * float64(time.Second))
	logger.Infof("dispensing ice: pin %q HIGH for %s", pinName, dwell)
	if err := pin.Set(ctx, true, nil); err != nil {
		return fmt.Errorf("dispense_ice: set pin %q high: %w", pinName, err)
	}
	// Drive the pin LOW with a fresh context so the write still lands if ctx is
	// already cancelled.
	stop := func() error {
		if err := pin.Set(context.Background(), false, nil); err != nil {
			return fmt.Errorf("dispense_ice: set pin %q low: %w", pinName, err)
		}
		return nil
	}
	select {
	case <-time.After(dwell):
	case <-ctx.Done():
		_ = stop()
		return fmt.Errorf("dispense_ice: cancelled during dispense: %w", ctx.Err())
	case <-cancelCtx.Done():
		_ = stop()
		return fmt.Errorf("dispense_ice: cancelled during dispense")
	}
	if err := stop(); err != nil {
		return err
	}

	retreatStep := Step{PoseName: clawPoseIceMachineApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("dispense_ice: %w", err)
	}
	return nil
}

// stageGlass sets the held glass down in the staging area and releases it,
// freeing the gripper to retrieve the espresso cup and pour; the glass is
// re-grabbed afterward (grabStagedGlass) and placed in the serving area.
func (s *beanjaminCoffee) stageGlass(ctx, cancelCtx context.Context) error {
	approachStep := Step{PoseName: clawPoseStagingApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("stage_glass: %w", err)
	}
	placeStep := Step{PoseName: clawPoseStaging, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, placeStep); err != nil {
		return fmt.Errorf("stage_glass: %w", err)
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("stage_glass: open gripper: %w", err)
	}
	time.Sleep(gripperPause)
	// Glass is set down in the staging area; it no longer travels with the gripper.
	s.detachHeldGeometry()
	exitStep := Step{PoseName: clawPoseStagingApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, exitStep); err != nil {
		return fmt.Errorf("stage_glass: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("stage_glass: close gripper: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

// pourEspresso moves the held espresso cup above the staged glass and tilts it
// to pour the espresso over the ice (the tilt geometry lives in the pour pose),
// dwells so the cup drains, then returns it upright before moving away.
func (s *beanjaminCoffee) pourEspresso(ctx, cancelCtx context.Context) error {
	approachStep := Step{PoseName: clawPosePourApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("pour_espresso: %w", err)
	}
	pourStep := Step{PoseName: clawPosePour, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: pourPause}
	if err := s.executeStep(ctx, cancelCtx, pourStep); err != nil {
		return fmt.Errorf("pour_espresso: %w", err)
	}
	uprightStep := Step{PoseName: clawPosePourApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, uprightStep); err != nil {
		return fmt.Errorf("pour_espresso: %w", err)
	}
	return nil
}

func (s *beanjaminCoffee) turnCoffeeButtonOn(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: clawPoseCoffeeButtonApproach, Component: componentClaws},
		{PoseName: clawPoseCoffeeButtonOn, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("turn_coffee_button_on: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) turnCoffeeButtonOff(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: clawPoseCoffeeButtonOff, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
		{PoseName: clawPoseCoffeeButtonApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("turn_coffee_button_off: %w", err)
		}
	}
	return nil
}

// brewCoffee is the execute_action entry point — uses the espresso default brew time.
func (s *beanjaminCoffee) brewCoffee(ctx, cancelCtx context.Context) error {
	return s.brew(ctx, cancelCtx, s.drinkBrewTime("espresso"))
}

// brew presses the coffee button, waits for the given duration, then releases.
func (s *beanjaminCoffee) brew(ctx, cancelCtx context.Context, brewTime time.Duration) error {
	logger := s.activeOrderLogger()
	if err := s.turnCoffeeButtonOn(ctx, cancelCtx); err != nil {
		return fmt.Errorf("brew_coffee: %w", err)
	}
	logger.Infof("waiting %s for coffee to brew", brewTime)
	select {
	case <-time.After(brewTime):
	case <-ctx.Done():
		return fmt.Errorf("brew_coffee: cancelled during brew wait: %w", ctx.Err())
	case <-cancelCtx.Done():
		return fmt.Errorf("brew_coffee: cancelled during brew wait")
	}
	if err := s.turnCoffeeButtonOff(ctx, cancelCtx); err != nil {
		return fmt.Errorf("brew_coffee: %w", err)
	}
	return nil
}

const (
	defaultEspressoBrewTime = 8 * time.Second
	defaultLungoBrewTime    = 15 * time.Second
	defaultGrindTimeSec     = 7.5
	// defaultIceDispenseSec is how long the ice pin is held HIGH when
	// ice_dispense_sec is unset.
	defaultIceDispenseSec = 5.0
)

// grindDurationSec returns the configured or default grind duration in seconds.
func (s *beanjaminCoffee) grindDurationSec() float64 {
	if s.cfg.GrindTimeSec > 0 {
		return s.cfg.GrindTimeSec
	}
	return defaultGrindTimeSec
}

// iceDispenseSec returns the configured or default ice-dispense duration in seconds.
func (s *beanjaminCoffee) iceDispenseSec() float64 {
	if s.cfg.IceDispenseSec > 0 {
		return s.cfg.IceDispenseSec
	}
	return defaultIceDispenseSec
}

// icePinName returns the ice-machine board pin name. Validate requires it to be
// set whenever can_serve_iced is enabled, which is the only path that reaches a
// dispense, so it is always non-empty here.
func (s *beanjaminCoffee) icePinName() string {
	return s.cfg.IceDispensePinName
}

// drinkBrewTime returns the configured or default brew duration for the given drink.
func (s *beanjaminCoffee) drinkBrewTime(drink string) time.Duration {
	switch drink {
	case "lungo", "decaf_lungo":
		if s.cfg.LungoBrewTimeSec > 0 {
			return time.Duration(s.cfg.LungoBrewTimeSec * float64(time.Second))
		}
		return defaultLungoBrewTime
	default:
		if s.cfg.BrewTimeSec > 0 {
			return time.Duration(s.cfg.BrewTimeSec * float64(time.Second))
		}
		return defaultEspressoBrewTime
	}
}

func (s *beanjaminCoffee) cleanPortafilter(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: filterPoseCloseToCleaning, Component: componentFilter},
		{PoseName: filterPoseApproachToCleaningScrapper, Component: componentFilter, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: filterPoseCleaningScrapperActive, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		{PoseName: filterPoseCleaningScrapperActive, Component: componentFilter, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		{PoseName: filterPoseApproachToCleaningScrapper, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: filterPoseApproachToCleaningBrush, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: filterPoseCleaningBrushActive, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		{PoseName: filterPoseCleaningBrushActive, Component: componentFilter, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		{PoseName: filterPoseApproachToCleaningBrush, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: filterPoseCloseToCleaning, Component: componentFilter, AllowedCollisions: cleaningCollisions, Pause: shortPause},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("clean_portafilter: %w", err)
		}
	}
	s.portafilterHasGrounds.Store(false)
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
		if err := s.moveToPose(ctx, step); err != nil {
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
