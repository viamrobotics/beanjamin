package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (s *beanjaminCoffee) prepareOrder(ctx context.Context, orderRaw interface{}) (map[string]interface{}, error) {
	order, ok := orderRaw.(map[string]interface{})
	if !ok {
		return nil, errors.New("prepare_order value must be an object with keys: drink, customer_name, initial_greeting, completion_statement")
	}

	drink, _ := order["drink"].(string)
	if drink != "espresso" {
		return nil, fmt.Errorf("unsupported drink %q, only \"espresso\" is supported", drink)
	}
	customerName, _ := order["customer_name"].(string)
	initialGreeting, _ := order["initial_greeting"].(string)
	completionStatement, _ := order["completion_statement"].(string)

	if initialGreeting == "" {
		initialGreeting = pickGreeting(customerName)
	}

	s.logger.Infof("prepare_order: %s – %s", customerName, initialGreeting)

	if s.speech != nil {
		if _, err := s.speech.Say(ctx, initialGreeting, true); err != nil {
			s.logger.Warnf("failed to say greeting: %v", err)
		}
	}

	if err := s.prepareEspresso(ctx); err != nil {
		return nil, err
	}

	if customerName != "" && s.speech != nil {
		msg := pickAlmostReady(customerName)
		if _, err := s.speech.Say(ctx, msg, true); err != nil {
			s.logger.Warnf("failed to say almost-ready: %v", err)
		}
	}

	s.logger.Infof("prepare_order complete: %s – %s", customerName, completionStatement)

	return map[string]interface{}{
		"status":               "complete",
		"customer_name":        customerName,
		"initial_greeting":     initialGreeting,
		"completion_statement": completionStatement,
	}, nil
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
		{PoseName: "grinder_activate", PauseSec: 1},
		{PoseName: "grinder_approach", PauseSec: 10},
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
		{PoseName: "tamper_activate", PauseSec: 5},
		{PoseName: "tamper_approach", PauseSec: 1},
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
		{PoseName: "coffee_in", PauseSec: 1},
		{PoseName: "coffee_locked_mid", PauseSec: 5},
		{PoseName: "coffee_locked_final", PauseSec: 5},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("lock_porta_filter: %w", err)
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

	s.logger.Infof("moving to %q", step.PoseName)

	_, err := s.sw.DoCommand(ctx, map[string]interface{}{
		"set_position_by_name": step.PoseName,
	})
	if err != nil {
		return fmt.Errorf("move to %q failed: %w", step.PoseName, err)
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
