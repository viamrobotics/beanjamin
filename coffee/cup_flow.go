package coffee

// Operator troubleshooting and hardware-tuning routines reachable via
// DoCommand, exercising parts of the brew cycle outside a real order.

import (
	"context"
	"errors"
	"fmt"
)

// runCupFlow exercises the full cup-handling path without brewing: for each of
// count iterations it dynamically picks a cup (sweeping observe poses until one
// sees a cup), sets it under the machine, retrieves it, and places it on the
// served shelf at the next round-robin slot. Intended for tuning the observe
// sweep + shelf placement on hardware.
//
// It assumes the portafilter has been physically removed from the claws — the
// flow never touches portafilter state. Each placement advances the shelf-slot
// counter inside placeFullCupOnShelf.
func (s *beanjaminCoffee) runCupFlow(ctx context.Context, count int) (map[string]any, error) {
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

	// Pick up any out-of-band frame-system edits before planning. Guarded so a
	// held item or locked filter from a prior call is preserved.
	if err := s.refreshFrameSystemIfClean(ctx); err != nil {
		return nil, fmt.Errorf("run_cup_flow: refresh frame system: %w", err)
	}

	logger.Infof("run_cup_flow: starting %d iteration(s) (assumes portafilter physically removed)", count)
	for i := 1; i <= count; i++ {
		s.setStep(fmt.Sprintf("Cup flow %d/%d", i, count))
		logger.Infof("run_cup_flow: iteration %d/%d — pick cup + set under machine", i, count)
		if err := s.setCupForCoffee(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("run_cup_flow: iteration %d/%d: pickup: %w", i, count, err)
		}
		logger.Infof("run_cup_flow: iteration %d/%d — retrieve from machine + place on shelf", i, count)
		if _, err := s.placeFullCupOnShelf(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("run_cup_flow: iteration %d/%d: place-on-shelf: %w", i, count, err)
		}
	}

	homeStep := Step{PoseName: "home", PoseSwitch: s.filterSw}
	if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
		return nil, fmt.Errorf("run_cup_flow: home: %w", err)
	}

	logger.Infof("run_cup_flow: complete (%d iteration(s))", count)
	return map[string]any{"status": "complete", "iterations": count}, nil
}
