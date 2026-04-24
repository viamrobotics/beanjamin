package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	shortPause   = 100 * time.Millisecond
	gripperPause = 500 * time.Millisecond
)

// say queues text for the speech service via the non-blocking say_async
// DoCommand. It returns as soon as the text is accepted by the speech
// service's async queue; the audio will be played once any in-flight
// speech has finished. It is a no-op when no speech service is configured.
func (s *beanjaminCoffee) say(ctx context.Context, text string) error {
	if s.speech == nil {
		return nil
	}
	_, err := s.speech.DoCommand(ctx, map[string]interface{}{
		"say_async": text,
	})
	return err
}

var coffeeBrewingCollisions = []AllowedCollision{
	{Frame1: "filter", Frame2: "coffee-machine-actuation-area"},
	{Frame1: "portafilter-handle", Frame2: "coffee-machine-actuation-area"},
	{Frame1: "coffee-claws-middle", Frame2: "coffee-machine-actuation-area"},
	{Frame1: "gripper:claws", Frame2: "coffee-machine-actuation-area"},
}

var filterGrabCollisions = []AllowedCollision{
	{Frame1: "coffee-claws-middle", Frame2: "portafilter-handle"},
	{Frame1: "gripper:claws", Frame2: "portafilter-handle"},
	{Frame1: "gripper:case-gripper", Frame2: "portafilter-handle"},
}

var cleaningCollisions = []AllowedCollision{
	{Frame1: "filter", Frame2: "cleaner-top"},
	{Frame1: "portafilter-handle", Frame2: "cleaner-top"},
	{Frame1: "coffee-claws-middle", Frame2: "cleaner-top"},
}

var clawCoffeeButtonCollisions = []AllowedCollision{
	{Frame1: "coffee-claws-middle", Frame2: "coffee-machine-buffer-front"},
	{Frame1: "gripper:claws", Frame2: "coffee-machine-buffer-front"},
}

var cupGrabCollisions = []AllowedCollision{
	{Frame1: "coffee-claws-middle", Frame2: "empty-cup"},
	{Frame1: "gripper:claws", Frame2: "empty-cup"},
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
		"give_full_cup_to_customer": s.giveFullCupToCustomer,
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

func (s *beanjaminCoffee) prepareDrink(ctx context.Context, drink, customerName string) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("a sequence is already running")
	}
	defer s.running.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	brewTime := s.drinkBrewTime(drink)

	s.logger.Infof("starting %s preparation (place_cup=%t, clean_after_use=%t, brew_time=%v)",
		drink, s.cfg.PlaceCup, s.cfg.CleanAfterUse, brewTime)

	s.setStep("Grinding")
	isDecaf := drink == "decaf" || drink == "decaf_lungo"
	if isDecaf {
		s.logger.Infof("step 1/9: grinding decaf coffee")
		if err := s.grindDecaf(ctx, cancelCtx); err != nil {
			return err
		}
	} else {
		s.logger.Infof("step 1/9: grinding coffee")
		if err := s.grindCoffee(ctx, cancelCtx); err != nil {
			return err
		}
	}
	s.setStep("Tamping")
	s.logger.Infof("step 2/9: tamping ground")
	if err := s.tampGround(ctx, cancelCtx); err != nil {
		return err
	}
	s.setStep("Locking portafilter")
	s.logger.Infof("step 3/9: locking portafilter")
	if err := s.lockPortaFilter(ctx, cancelCtx); err != nil {
		return err
	}
	s.setStep("Releasing filter")
	s.logger.Infof("step 4/9: releasing filter")
	if err := s.releaseFilter(ctx, cancelCtx); err != nil {
		return err
	}
	if s.cfg.PlaceCup {
		s.setStep("Placing cup")
		s.logger.Infof("step 5/9: placing cup (place_cup=true)")
		if err := s.setCupForCoffee(ctx, cancelCtx); err != nil {
			return err
		}
	} else {
		s.logger.Infof("step 5/9: skipping cup placement (place_cup=false)")
	}
	s.setStep("Brewing")
	s.logger.Infof("step 6/9: brewing %s", drink)
	if err := s.say(ctx, pickAlmostReady()); err != nil {
		s.logger.Warnf("failed to say almost-ready: %v", err)
	}
	if err := s.brew(ctx, cancelCtx, brewTime); err != nil {
		return err
	}

	if s.cfg.PlaceCup {
		s.setStep("Serving")
		s.logger.Infof("step 6b/9: giving full cup to customer (place_cup=true)")
		if err := s.giveFullCupToCustomer(ctx, cancelCtx); err != nil {
			return err
		}
		if err := s.say(ctx, pickDrinkReady(drink, customerName)); err != nil {
			s.logger.Warnf("failed to say drink-ready: %v", err)
		}
	} else {
		s.logger.Infof("step 6b/9: skipping cup handoff (place_cup=false)")
	}

	s.setStep("Grabbing filter")
	s.logger.Infof("step 7/9: grabbing filter")
	if err := s.grabFilter(ctx, cancelCtx); err != nil {
		return err
	}
	s.setStep("Unlocking portafilter")
	s.logger.Infof("step 8/9: unlocking portafilter")
	if err := s.unlockPortaFilter(ctx, cancelCtx); err != nil {
		return err
	}
	if s.cfg.CleanAfterUse {
		s.setStep("Cleaning")
		s.logger.Infof("post: cleaning portafilter (clean_after_use=true)")
		if !s.cfg.PlaceCup {
			s.logger.Infof("post: waiting for manual cup removal (place_cup=false)")
			if err := s.say(ctx, "Please remove the cup before we start the cleaning process!"); err != nil {
				s.logger.Warnf("failed to say cup-removal prompt: %v", err)
			}
			time.Sleep(10 * time.Second)
		}
		if err := s.cleanPortafilter(ctx, cancelCtx); err != nil {
			return err
		}
	} else {
		s.logger.Infof("post: skipping cleaning (clean_after_use=false)")
	}

	s.setStep("Finishing up")
	s.logger.Infof("step 9/9: moving to home pose")
	homeStep := Step{PoseName: "home", Component: "filter"}
	if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
		return err
	}

	s.logger.Infof("%s preparation complete", drink)
	return nil
}

func (s *beanjaminCoffee) grindCoffee(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: "grinder_approach", Component: "filter", Pause: shortPause},
		{PoseName: "grinder_activate", Component: "filter", Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: "grinder_approach", Component: "filter", Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		// Circle under the grinder chute to distribute grounds evenly while the grinder dispenses.
		{PoseName: "grinder_approach", Component: "filter",
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
	steps := []Step{
		{PoseName: "decaf_grinder_approach", Component: "filter", Pause: shortPause},
		{PoseName: "decaf_grinder_activate", Component: "filter", Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: "decaf_grinder_approach", Component: "filter", Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		// Circle under the decaf grinder chute to distribute grounds evenly while the grinder dispenses.
		{PoseName: "decaf_grinder_approach", Component: "filter",
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
		{PoseName: "tamper_approach", Component: "filter", Pause: shortPause},
		{PoseName: "tamper_activate", Component: "filter", Pause: 3000 * time.Millisecond, LinearConstraint: defaultApproachConstraint},
		{PoseName: "tamper_approach", Component: "filter", Pause: shortPause, LinearConstraint: defaultApproachConstraint},
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
		{PoseName: "coffee_approach", Component: "filter", Pause: shortPause},
		{PoseName: "coffee_in", Component: "filter", Pause: shortPause, LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: "coffee_locked_final", Component: "filter", PivotFromPose: "coffee_in", PivotDegreesPerStep: 5,
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
		{PoseName: "coffee_in", Component: "filter", PivotFromPose: "coffee_locked_final", PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: "coffee_shake", Component: "filter", AllowedCollisions: coffeeBrewingCollisions, LinearConstraint: defaultApproachConstraint},
		// Shake the filter laterally to dislodge the puck.
		{PoseName: "coffee_shake", Component: "filter",
			CircularRadiusMm: 4, CircularDurationSec: s.cfg.PortafilterShakeSec, CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: "coffee_approach", Component: "filter", Pause: shortPause, LinearConstraint: defaultApproachConstraint},
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
	step := Step{PoseName: "filter_released", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterGrabCollisions}
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

	approachStep := Step{PoseName: "filter_released", Component: "coffee-claws-middle"}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("grab_filter: %w", err)
	}

	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("grab_filter: open gripper: %w", err)
	}

	alignStep := Step{PoseName: "coffee_locked_final", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterGrabCollisions}
	if err := s.executeStep(ctx, cancelCtx, alignStep); err != nil {
		return fmt.Errorf("grab_filter: %w", err)
	}

	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("grab_filter: grab gripper: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

func (s *beanjaminCoffee) setCupForCoffee(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("set_cup_for_coffee: no gripper configured")
	}

	// Approach the empty cup.
	approachStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Be ready to get to the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: open gripper: %w", err)
	}
	// Give time for the gripper to open
	time.Sleep(gripperPause)

	// Move down to the cup and grab it.
	grabStep := Step{PoseName: "empty_cup", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: grab gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// Retreat and move to the coffee position.
	retreatStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	cupPlacementApproach := Step{PoseName: "cup_under_machine_approach", Component: "coffee-claws-middle", Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, cupPlacementApproach); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	readyStep := Step{PoseName: "cup_ready_for_coffee", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, Pause: shortPause}
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
	exitStep := Step{PoseName: "cup_under_machine_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, Pause: shortPause}
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

func (s *beanjaminCoffee) giveFullCupToCustomer(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("give_full_cup_to_customer: no gripper configured")
	}

	// Approach the cup under the machine.
	approachStep := Step{PoseName: "cup_under_machine_approach", Component: "coffee-claws-middle", Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Open gripper to prepare for grabbing.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: open gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// Move down to the cup and grab it.
	grabStep := Step{PoseName: "cup_ready_for_coffee", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: grab gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// Retreat from the machine.
	retreatStep := Step{PoseName: "cup_under_machine_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Move to the customer cup position.
	customerApproachStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, customerApproachStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}
	placeStep := Step{PoseName: "empty_cup", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, placeStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Release the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: open gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// Move away from the cup.
	exitStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
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
		{PoseName: "coffee_button_approach", Component: "coffee-claws-middle"},
		{PoseName: "coffee_button_on", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
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
		{PoseName: "coffee_button_off", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
		{PoseName: "coffee_button_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
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
		{PoseName: "close_to_cleaning", Component: "filter"},
		{PoseName: "approach_to_cleaning_scrapper", Component: "filter", AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: "cleaning_scrapper_active", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		{PoseName: "cleaning_scrapper_active", Component: "filter", AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		{PoseName: "approach_to_cleaning_scrapper", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: "approach_to_cleaning_brush", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: "cleaning_brush_active", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		{PoseName: "cleaning_brush_active", Component: "filter", AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		{PoseName: "approach_to_cleaning_brush", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: "approach_to_cleaning_scrapper", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: "close_to_cleaning", Component: "filter", AllowedCollisions: cleaningCollisions, Pause: shortPause},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("clean_portafilter: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) executeStep(ctx, cancelCtx context.Context, step Step) error {
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
