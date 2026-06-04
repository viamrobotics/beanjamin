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
	clawPoseEmptyCupApproach        = "empty_cup_approach"
	clawPoseEmptyCup                = "empty_cup"
	clawPoseCupReadyForCoffee       = "cup_ready_for_coffee"
	clawPoseCupUnderMachineApproach = "cup_under_machine_approach"
	clawPoseCupObserve              = "cup_observe"
)

const (
	// component/frame names accepted by switchForComponent and used as
	// AllowedCollision frame names.
	componentFilter = "filter"
	componentClaws  = "coffee-claws-middle"
)

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
// starts. Optional features (decaf, cup placement/serving, dynamic pickup)
// contribute their poses only when their config flag is set.
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
		// cleaning (clean_after_use + cancel recovery)
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

	if s.cfg.PlaceCup {
		// setCupForCoffee always seats the cup under the machine.
		poses = append(poses,
			requiredPose{componentClaws, clawPoseCupUnderMachineApproach},
			requiredPose{componentClaws, clawPoseCupReadyForCoffee},
		)
		if s.cfg.DynamicCupPickup {
			// pickCupDynamic observes from cup_observe plus any alternates.
			poses = append(poses, requiredPose{componentClaws, clawPoseCupObserve})
			for _, name := range s.cfg.CupObserveAlternates {
				poses = append(poses, requiredPose{componentClaws, name})
			}
		} else {
			// Static pickup grabs from the fixed empty-cup poses.
			poses = append(poses,
				requiredPose{componentClaws, clawPoseEmptyCupApproach},
				requiredPose{componentClaws, clawPoseEmptyCup},
			)
		}
		// Serving: placeFullCupOnShelf composes world poses at runtime (no
		// named pose to validate); giveFullCupToCustomer hands the cup back
		// via the empty-cup poses.
		if !s.cfg.PlaceCupOnShelf {
			poses = append(poses,
				requiredPose{componentClaws, clawPoseEmptyCupApproach},
				requiredPose{componentClaws, clawPoseEmptyCup},
			)
		}
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

var cupGrabCollisions = []AllowedCollision{
	{Frame1: componentClaws, Frame2: "empty-cup"},
	{Frame1: "gripper:claws", Frame2: "empty-cup"},
}

func (s *beanjaminCoffee) executeAction(ctx context.Context, name string) (map[string]interface{}, error) {
	giveCupFunc := s.giveFullCupToCustomer
	if s.cfg.PlaceCupOnShelf {
		giveCupFunc = s.placeFullCupOnShelf
	}

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
		"give_full_cup_to_customer": giveCupFunc,
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

// waterDelta returns the water-usage increment for a brew: 1.5 for lungo sizes
// (lungo/decaf_lungo), 1 otherwise (espresso/decaf).
func waterDelta(drink string) float64 {
	if isLungoDrink(drink) {
		return 1.5
	}
	return 1
}

func (s *beanjaminCoffee) prepareDrink(ctx context.Context, drink, customerName string, batchIndex, batchSize int) (err error) {
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

	s.logger.Infof("starting %s preparation (place_cup=%t, clean_after_use=%t, brew_time=%v)",
		drink, s.cfg.PlaceCup, s.cfg.CleanAfterUse, brewTime)

	s.setStep(stepGrinding)
	isDecaf := isDecafDrink(drink)
	if isDecaf {
		s.logger.Infof("step 1/9: grinding decaf coffee")
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::grinding_decaf")
		err := s.grindDecaf(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "decaf grinder", "decaf_grinds", 1)
	} else {
		s.logger.Infof("step 1/9: grinding coffee")
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::grinding")
		err := s.grindCoffee(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "grinder", "regular_grinds", 1)
	}

	s.setStep(stepTamping)
	s.logger.Infof("step 2/9: tamping ground")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::tamping")
		err := s.tampGround(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	s.setStep(stepLockingPortafilter)
	s.logger.Infof("step 3/9: locking portafilter")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::locking_portafilter")
		err := s.lockPortaFilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	s.setStep(stepReleasingFilter)
	s.logger.Infof("step 4/9: releasing filter")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::releasing_filter")
		err := s.releaseFilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	if s.cfg.PlaceCup {
		s.setStep(stepPlacingCup)
		s.logger.Infof("step 5/9: placing cup (place_cup=true)")
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::placing_cup")
		err := s.setCupForCoffee(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	} else {
		s.logger.Infof("step 5/9: skipping cup placement (place_cup=false)")
	}

	s.setStep(stepBrewing)
	s.logger.Infof("step 6/9: brewing %s", drink)
	if err := s.say(ctx, pickAlmostReady()); err != nil {
		s.logger.Warnf("failed to say almost-ready: %v", err)
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

	if s.cfg.PlaceCup {
		s.setStep(stepServing)
		s.logger.Infof("step 6b/9: serving cup (place_cup=true, place_cup_on_shelf=%t)", s.cfg.PlaceCupOnShelf)
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::serving")
		var err error
		if s.cfg.PlaceCupOnShelf {
			err = s.placeFullCupOnShelf(ctx, cancelCtx)
		} else {
			err = s.giveFullCupToCustomer(ctx, cancelCtx)
		}
		stepSpan.End()
		if err != nil {
			return err
		}
		if err := s.sayAlways(ctx, pickDrinkReady(drink, customerName, batchIndex, batchSize)); err != nil {
			s.logger.Warnf("failed to say drink-ready: %v", err)
		}
	} else {
		s.logger.Infof("step 6b/9: skipping cup handoff (place_cup=false)")
	}

	s.setStep(stepGrabbingFilter)
	s.logger.Infof("step 7/9: grabbing filter")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::grabbing_filter")
		err := s.grabFilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	s.setStep(stepUnlockingPortafilter)
	s.logger.Infof("step 8/9: unlocking portafilter")
	{
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::unlocking_portafilter")
		err := s.unlockPortaFilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
	}

	if s.cfg.CleanAfterUse {
		s.setStep(stepCleaning)
		s.logger.Infof("post: cleaning portafilter (clean_after_use=true)")
		if !s.cfg.PlaceCup {
			s.logger.Infof("post: waiting for manual cup removal (place_cup=false)")
			if err := s.say(ctx, "Please remove the cup before we start the cleaning process!"); err != nil {
				s.logger.Warnf("failed to say cup-removal prompt: %v", err)
			}
			time.Sleep(10 * time.Second)
		}
		ctx, stepSpan := trace.StartSpan(ctx, "beanjamin::step::cleaning")
		err := s.cleanPortafilter(ctx, cancelCtx)
		stepSpan.End()
		if err != nil {
			return err
		}
		s.incrementSensorReading(ctx, s.usageSensor, "cleaner", "cleanings", 1)
	} else {
		s.logger.Infof("post: skipping cleaning (clean_after_use=false)")
	}

	s.setStep(stepFinishingUp)
	s.logger.Infof("step 9/9: moving to home pose")
	homeStep := Step{PoseName: filterPoseHome, Component: componentFilter}
	if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
		return err
	}

	s.logger.Infof("%s preparation complete", drink)
	return nil
}

func (s *beanjaminCoffee) grindCoffee(ctx, cancelCtx context.Context) error {
	// Mark before any motion: any cancel from here onward must clean the
	// filter before going home, in case the grinder dispensed any grounds.
	s.portafilterHasGrounds.Store(true)
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
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("grind_coffee: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) grindDecaf(ctx, cancelCtx context.Context) error {
	// Mark before any motion: any cancel from here onward must clean the
	// filter before going home, in case the grinder dispensed any grounds.
	s.portafilterHasGrounds.Store(true)
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

	if s.cfg.DynamicCupPickup {
		if err := s.pickCupDynamic(ctx, cancelCtx); err != nil {
			return err
		}
	} else {
		// Static pickup: approach -> open gripper -> grab -> retreat.
		approachStep := Step{PoseName: clawPoseEmptyCupApproach, Component: componentClaws, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
			return fmt.Errorf("set_cup_for_coffee: %w", err)
		}

		if err := s.gripper.Open(ctx, nil); err != nil {
			return fmt.Errorf("set_cup_for_coffee: open gripper: %w", err)
		}
		time.Sleep(gripperPause)

		grabStep := Step{PoseName: clawPoseEmptyCup, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
			return fmt.Errorf("set_cup_for_coffee: %w", err)
		}
		if _, err := s.gripper.Grab(ctx, nil); err != nil {
			return fmt.Errorf("set_cup_for_coffee: grab gripper: %w", err)
		}
		time.Sleep(gripperPause)

		retreatStep := Step{PoseName: clawPoseEmptyCupApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
			return fmt.Errorf("set_cup_for_coffee: %w", err)
		}
	}

	cupPlacementApproach := Step{PoseName: clawPoseCupUnderMachineApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, cupPlacementApproach); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	readyStep := Step{PoseName: clawPoseCupReadyForCoffee, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, readyStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Release the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: open gripper: %w", err)
	}
	// Give time for the gripper to open
	time.Sleep(gripperPause)

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
// drops it on the served-drinks shelf at the tile chosen earlier by
// selectShelfTile. Replaces giveFullCupToCustomer when PlaceCupOnShelf=true.
//
// The grab phase mirrors giveFullCupToCustomer (approach -> open -> linear
// descent + grab -> linear retreat). The placement phase plans to a
// world-frame approach pose above the chosen tile, descends linearly to the
// drop pose (claws-middle = shelfTopZ + shelfDropZOffsetMm), opens the
// gripper to release, then retreats linearly and closes the gripper.
func (s *beanjaminCoffee) placeFullCupOnShelf(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("place_full_cup_on_shelf: no gripper configured")
	}

	pickRaw := s.servedShelfTile.Load()
	pick, ok := pickRaw.(servedShelfTilePick)
	if !ok || !pick.ok {
		return fmt.Errorf("place_full_cup_on_shelf: no shelf tile selected — selectShelfTile must run during pickup observation")
	}

	// 1. Retrieve the brewed cup (mirrors giveFullCupToCustomer).
	approachStep := Step{PoseName: clawPoseCupUnderMachineApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: %w", err)
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: open gripper: %w", err)
	}
	time.Sleep(gripperPause)
	grabStep := Step{PoseName: clawPoseCupReadyForCoffee, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: grab gripper: %w", err)
	}
	time.Sleep(gripperPause)
	retreatStep := Step{PoseName: clawPoseCupUnderMachineApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: %w", err)
	}

	// 2. Compose drop + approach poses in world frame. CupGrabRelativePose is
	// the same relative offset used at pickup (composed onto the detected
	// cup centroid) — composing it onto the placement anchor here keeps the
	// claws-to-cup geometry identical between grab and release, so the cup
	// lands centered on the chosen tile.
	dropAnchor := r3.Vector{
		X: pick.tileWorld.X,
		Y: pick.tileWorld.Y,
		Z: pick.shelfTopZ + shelfDropZOffsetMm,
	}
	dropPose := composeCupPose(dropAnchor, relativePoseToSpatial(s.cfg.CupGrabRelativePose))
	approachPose := composeCupPose(dropAnchor, relativePoseToSpatial(s.cfg.CupApproachRelativePose))

	approachPD := &poseData{pose: approachPose, refFrame: referenceframe.World, componentName: componentClaws}
	dropPD := &poseData{pose: dropPose, refFrame: referenceframe.World, componentName: componentClaws}

	s.logger.Infof("shelf placement: anchor world drop_pose=%v approach_pose=%v",
		dropPose, approachPose)

	// 3. Free planning to the approach pose.
	if err := s.moveToRawPose(ctx, approachPD, nil, nil, nil); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: approach: %w", err)
	}

	// 4. Linear descent to the drop pose.
	if err := s.moveToRawPose(ctx, dropPD, defaultApproachConstraint, nil, nil); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: descend: %w", err)
	}

	// 5. Release the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: open gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// 6. Linear retreat back to the approach pose.
	if err := s.moveToRawPose(ctx, approachPD, defaultApproachConstraint, nil, nil); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: retreat: %w", err)
	}

	// 7. Close the gripper for the next move.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("place_full_cup_on_shelf: close gripper: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

func (s *beanjaminCoffee) giveFullCupToCustomer(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("give_full_cup_to_customer: no gripper configured")
	}

	// Approach the cup under the machine.
	approachStep := Step{PoseName: clawPoseCupUnderMachineApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Open gripper to prepare for grabbing.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: open gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// Move down to the cup and grab it.
	grabStep := Step{PoseName: clawPoseCupReadyForCoffee, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: grab gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// Retreat from the machine.
	retreatStep := Step{PoseName: clawPoseCupUnderMachineApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Move to the customer cup position.
	customerApproachStep := Step{PoseName: clawPoseEmptyCupApproach, Component: componentClaws, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, customerApproachStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}
	placeStep := Step{PoseName: clawPoseEmptyCup, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, placeStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Release the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: open gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// Move away from the cup.
	exitStep := Step{PoseName: clawPoseEmptyCupApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, exitStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Close the gripper after moving away.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: close gripper: %w", err)
	}
	time.Sleep(gripperPause)
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
	if err := s.turnCoffeeButtonOn(ctx, cancelCtx); err != nil {
		return fmt.Errorf("brew_coffee: %w", err)
	}
	s.logger.Infof("waiting %s for coffee to brew", brewTime)
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
)

// grindDurationSec returns the configured or default grind duration in seconds.
func (s *beanjaminCoffee) grindDurationSec() float64 {
	if s.cfg.GrindTimeSec > 0 {
		return s.cfg.GrindTimeSec
	}
	return defaultGrindTimeSec
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
		s.logger.Infof("pivoting from %q to %q", step.PivotFromPose, step.PoseName)
		if err := s.executePivot(ctx, cancelCtx, step); err != nil {
			return err
		}
	} else if step.CircularRadiusMm > 0 {
		s.logger.Infof("circular motion around %q", step.PoseName)
		if err := s.executeCircularMotion(ctx, cancelCtx, step); err != nil {
			return err
		}
	} else {
		s.logger.Infof("moving to %q", step.PoseName)
		if err := s.moveToPose(ctx, step); err != nil {
			return err
		}
	}

	if step.Pause > 0 {
		s.logger.Infof("pausing %s after %q", step.Pause, step.PoseName)
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
