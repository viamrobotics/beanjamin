package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// say sends text to the speech service via DoCommand. It is a no-op when
// no speech service is configured.
func (s *beanjaminCoffee) say(ctx context.Context, text string) error {
	if s.speech == nil {
		return nil
	}
	_, err := s.speech.DoCommand(ctx, map[string]interface{}{
		"say": text,
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

// prepareOrder is kept for backward compatibility but now delegates to enqueueOrder.
func (s *beanjaminCoffee) prepareOrder(ctx context.Context, orderRaw interface{}) (map[string]interface{}, error) {
	return s.enqueueOrder(ctx, orderRaw)
}

func (s *beanjaminCoffee) executeAction(ctx context.Context, name string) (map[string]interface{}, error) {
	actions := map[string]func(ctx, cancelCtx context.Context) error{
		"grind_coffee":              s.grindCoffee,
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

func (s *beanjaminCoffee) prepareEspresso(ctx context.Context, customerName string) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("a sequence is already running")
	}
	defer s.running.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	s.logger.Infof("starting espresso preparation (place_cup=%t, clean_after_use=%t, brew_time_sec=%v)",
		s.cfg.PlaceCup, s.cfg.CleanAfterUse, s.cfg.BrewTimeSec)

	s.logger.Infof("step 1/7: grinding coffee")
	if err := s.grindCoffee(ctx, cancelCtx); err != nil {
		return err
	}
	s.logger.Infof("step 2/7: tamping ground")
	if err := s.tampGround(ctx, cancelCtx); err != nil {
		return err
	}
	s.logger.Infof("step 3/7: locking portafilter")
	if err := s.lockPortaFilter(ctx, cancelCtx); err != nil {
		return err
	}
	s.logger.Infof("step 4/7: releasing filter")
	if err := s.releaseFilter(ctx, cancelCtx); err != nil {
		return err
	}
	if s.cfg.PlaceCup {
		s.logger.Infof("step 5/7: placing cup (place_cup=true)")
		if err := s.setCupForCoffee(ctx, cancelCtx); err != nil {
			return err
		}
	} else {
		s.logger.Infof("step 5/7: skipping cup placement (place_cup=false)")
	}
	s.logger.Infof("step 6/7: brewing coffee")
	if err := s.say(ctx, pickAlmostReady()); err != nil {
		s.logger.Warnf("failed to say almost-ready: %v", err)
	}
	if err := s.brewCoffee(ctx, cancelCtx); err != nil {
		return err
	}

	if s.cfg.PlaceCup {
		s.logger.Infof("step 6b/7: giving full cup to customer (place_cup=true)")
		if err := s.giveFullCupToCustomer(ctx, cancelCtx); err != nil {
			return err
		}
		if err := s.say(ctx, pickEspressoReady(customerName)); err != nil {
			s.logger.Warnf("failed to say espresso-ready: %v", err)
		}
	} else {
		s.logger.Infof("step 6b/7: skipping cup handoff (place_cup=false)")
	}

	s.logger.Infof("step 7/7: grabbing filter")
	if err := s.grabFilter(ctx, cancelCtx); err != nil {
		return err
	}

	if s.cfg.CleanAfterUse {
		s.logger.Infof("post: cleaning portafilter (clean_after_use=true)")
		if !s.cfg.PlaceCup {
			s.logger.Infof("post: waiting for manual cup removal (place_cup=false)")
			s.say(ctx, "Please remove the cup before we start the cleaning process!")
			time.Sleep(10 * time.Second)
		}
		if err := s.cleanPortafilter(ctx, cancelCtx); err != nil {
			return err
		}
	} else {
		s.logger.Infof("post: skipping cleaning (clean_after_use=false)")
	}

	s.logger.Infof("espresso preparation complete")
	return nil
}

func (s *beanjaminCoffee) grindCoffee(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: "grinder_approach", Component: "filter", PauseSec: 1},
		{PoseName: "grinder_activate", Component: "filter", PauseSec: 1, LinearConstraint: defaultApproachConstraint},
		{PoseName: "grinder_approach", Component: "filter", PauseSec: 10, LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("grind_coffee: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) tampGround(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: "tamper_approach", Component: "filter", PauseSec: 1},
		{PoseName: "tamper_activate", Component: "filter", PauseSec: 5, LinearConstraint: defaultApproachConstraint},
		{PoseName: "tamper_approach", Component: "filter", PauseSec: 1, LinearConstraint: defaultApproachConstraint},
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
		{PoseName: "coffee_approach", Component: "filter", PauseSec: 1},
		{PoseName: "coffee_in", Component: "filter", PauseSec: 1, LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
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
		{PoseName: "coffee_approach", Component: "filter", PauseSec: 1, LinearConstraint: defaultApproachConstraint},
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
	time.Sleep(500 * time.Millisecond)
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
	time.Sleep(500 * time.Millisecond)
	return nil
}

func (s *beanjaminCoffee) setCupForCoffee(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("set_cup_for_coffee: no gripper configured")
	}

	// Approach the empty cup.
	approachStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", PauseSec: 0.5}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Be ready to get to the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: open gripper: %w", err)
	}
	// Give time for the gripper to open
	time.Sleep(500 * time.Millisecond)

	// Move down to the cup and grab it.
	grabStep := Step{PoseName: "empty_cup", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, PauseSec: 0.5}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: grab gripper: %w", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Retreat and move to the coffee position.
	retreatStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, PauseSec: 0.5}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	cupPlacementApproach := Step{PoseName: "cup_under_machine_approach", Component: "coffee-claws-middle", PauseSec: 0.5}
	if err := s.executeStep(ctx, cancelCtx, cupPlacementApproach); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	readyStep := Step{PoseName: "cup_ready_for_coffee", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, PauseSec: 0.1}
	if err := s.executeStep(ctx, cancelCtx, readyStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Release the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: open gripper: %w", err)
	}
	// Give time for the gripper to open
	time.Sleep(500 * time.Millisecond)

	// Move away from the cup.
	exitStep := Step{PoseName: "cup_under_machine_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, PauseSec: 0.1}
	if err := s.executeStep(ctx, cancelCtx, exitStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Close the gripper after moving away.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: close gripper: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}

func (s *beanjaminCoffee) giveFullCupToCustomer(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("give_full_cup_to_customer: no gripper configured")
	}

	// Approach the cup under the machine.
	approachStep := Step{PoseName: "cup_under_machine_approach", Component: "coffee-claws-middle", PauseSec: 0.5}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Open gripper to prepare for grabbing.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: open gripper: %w", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Move down to the cup and grab it.
	grabStep := Step{PoseName: "cup_ready_for_coffee", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, PauseSec: 0.5}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: grab gripper: %w", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Retreat from the machine.
	retreatStep := Step{PoseName: "cup_under_machine_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, PauseSec: 0.5}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Move to the customer cup position.
	customerApproachStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", PauseSec: 0.5}
	if err := s.executeStep(ctx, cancelCtx, customerApproachStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}
	placeStep := Step{PoseName: "empty_cup", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, PauseSec: 0.5}
	if err := s.executeStep(ctx, cancelCtx, placeStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Release the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: open gripper: %w", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Move away from the cup.
	exitStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, PauseSec: 0.1}
	if err := s.executeStep(ctx, cancelCtx, exitStep); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: %w", err)
	}

	// Close the gripper after moving away.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("give_full_cup_to_customer: close gripper: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
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

func (s *beanjaminCoffee) brewCoffee(ctx, cancelCtx context.Context) error {
	if err := s.turnCoffeeButtonOn(ctx, cancelCtx); err != nil {
		return fmt.Errorf("brew_coffee: %w", err)
	}
	brewTime := 8 * time.Second
	if s.cfg.BrewTimeSec > 0 {
		brewTime = time.Duration(s.cfg.BrewTimeSec * float64(time.Second))
	}
	s.logger.Infof("waiting %s for espresso to brew", brewTime)
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

func (s *beanjaminCoffee) cleanPortafilter(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: "close_to_cleaning", Component: "filter"},
		{PoseName: "approach_to_cleaning_scrapper", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, PauseSec: 0.5},
		{PoseName: "cleaning_scrapper_active", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, PauseSec: 2},
		{PoseName: "approach_to_cleaning_scrapper", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, PauseSec: 0.5},
		{PoseName: "approach_to_cleaning_brush", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, PauseSec: 0.5},
		{PoseName: "cleaning_brush_active", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, PauseSec: 2},
		{PoseName: "approach_to_cleaning_brush", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, PauseSec: 0.5},
		{PoseName: "close_to_cleaning", Component: "filter", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, PauseSec: 0.5},
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
	} else {
		s.logger.Infof("moving to %q", step.PoseName)
		if err := s.moveToPose(ctx, step); err != nil {
			return err
		}
	}

	if step.PauseSec > 0 {
		pause := time.Duration(step.PauseSec * float64(time.Second))
		s.logger.Infof("pausing %s after %q", pause, step.PoseName)
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return fmt.Errorf("cancelled during pause after %q: %w", step.PoseName, ctx.Err())
		case <-cancelCtx.Done():
			return fmt.Errorf("cancelled during pause after %q", step.PoseName)
		}
	}
	return nil
}
