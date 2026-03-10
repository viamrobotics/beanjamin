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
	{Frame1: "coffee-claws-middle", Frame2: "coffee-machine-actuation-area"},
	{Frame1: "gripper:claws", Frame2: "coffee-machine-actuation-area"},
}

func (s *beanjaminCoffee) prepareOrder(ctx context.Context, orderRaw interface{}) (map[string]interface{}, error) {
	order, ok := orderRaw.(map[string]interface{})
	if !ok {
		return nil, errors.New("prepare_order value must be an object with keys: drink, customer_name, initial_greeting, completion_statement")
	}

	drink, _ := order["drink"].(string)
	if drink != "espresso" {
		msg := pickUnsupportedDrink(drink)
		if err := s.say(ctx, msg); err != nil {
			s.logger.Warnf("failed to say rejection: %v", err)
		}
		return nil, fmt.Errorf("unsupported drink %q: %s", drink, msg)
	}
	customerName, _ := order["customer_name"].(string)
	initialGreeting, _ := order["initial_greeting"].(string)
	completionStatement, _ := order["completion_statement"].(string)

	if initialGreeting == "" {
		initialGreeting = pickGreeting("")
	}

	s.logger.Infof("prepare_order: %s – %s", customerName, initialGreeting)

	if err := s.say(ctx, initialGreeting); err != nil {
		s.logger.Warnf("failed to say greeting: %v", err)
	}

	if err := s.prepareEspresso(ctx); err != nil {
		return nil, err
	}

	msg := completionStatement
	if msg == "" {
		msg = pickAlmostReady()
	}
	if err := s.say(ctx, msg); err != nil {
		s.logger.Warnf("failed to say almost-ready: %v", err)
	}

	callout := "Espresso for " + customerName
	if err := s.say(ctx, callout); err != nil {
		s.logger.Warnf("failed to say callout: %v", err)
	}

	s.logger.Infof("prepare_order complete: %s – %s", customerName, completionStatement)

	return map[string]interface{}{
		"status":               "complete",
		"customer_name":        customerName,
		"initial_greeting":     initialGreeting,
		"completion_statement": completionStatement,
	}, nil
}

func (s *beanjaminCoffee) executeAction(ctx context.Context, name string) (map[string]interface{}, error) {
	actions := map[string]func(ctx, cancelCtx context.Context) error{
		"grind_coffee":        s.grindCoffee,
		"tamp_ground":         s.tampGround,
		"lock_portafilter":   s.lockPortaFilter,
		"unlock_portafilter": s.unlockPortaFilter,
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

func (s *beanjaminCoffee) prepareEspresso(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("a sequence is already running")
	}
	defer s.running.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	s.logger.Infof("starting espresso preparation")

	if err := s.grindCoffee(ctx, cancelCtx); err != nil {
		return err
	}
	if err := s.tampGround(ctx, cancelCtx); err != nil {
		return err
	}
	if err := s.lockPortaFilter(ctx, cancelCtx); err != nil {
		return err
	}

	s.logger.Infof("espresso preparation complete")
	return nil
}

func (s *beanjaminCoffee) grindCoffee(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: "grinder_approach", PauseSec: 1},
		{PoseName: "grinder_activate", PauseSec: 1, LinearConstraint: defaultApproachConstraint},
		{PoseName: "grinder_approach", PauseSec: 10, LinearConstraint: defaultApproachConstraint},
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
		{PoseName: "tamper_approach", PauseSec: 1},
		{PoseName: "tamper_activate", PauseSec: 5, LinearConstraint: defaultApproachConstraint},
		{PoseName: "tamper_approach", PauseSec: 1, LinearConstraint: defaultApproachConstraint},
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
		{PoseName: "coffee_approach", PauseSec: 1},
		{PoseName: "coffee_in", PauseSec: 1, LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: "coffee_locked_final", PivotFromPose: "coffee_in", PivotDegreesPerStep: 5,
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
		{PoseName: "coffee_in", PivotFromPose: "coffee_locked_final", PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: "coffee_approach", PauseSec: 1, LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("unlock_portafilter: %w", err)
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
