package coffee

// Queue and run control: proceed, clear_queue, reset_world, idle waiting, and
// the cancel path that interrupts an in-flight order and recovers the arm.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.viam.com/rdk/logging"
)

func (s *beanjaminCoffee) proceedQueue() (map[string]any, error) {
	select {
	case s.queue.proceed <- struct{}{}:
		return map[string]any{"status": "resumed"}, nil
	default:
		return nil, errors.New("not currently paused between orders")
	}
}

func (s *beanjaminCoffee) clearQueue() (map[string]any, error) {
	removed := s.queue.Clear()
	s.logger.Infof("cleared %d orders from queue", removed)
	return map[string]any{"status": "cleared", "removed": removed}, nil
}

// resetWorld brings the service back to an idle state from anywhere: cancels a
// running sequence (waiting for it to actually stop), clears any pending and
// recently-completed orders, rebuilds the cached frame system from the service
// (discarding mid-cycle mutations like a portafilter frame reparented to world
// by lockFilterFrame), and releases the cancel-induced queue pause so
// processQueue is ready for new orders. Each step is best-effort and skipped
// when not applicable, so it is safe to call from any state.
func (s *beanjaminCoffee) resetWorld(ctx context.Context) (map[string]any, error) {
	cancelled := s.signalCancel()
	if cancelled {
		if err := s.waitForIdle(ctx, resetCancelWaitTimeout); err != nil {
			return nil, fmt.Errorf("reset_world: %w", err)
		}
	}

	removed := s.queue.Clear()

	// reset_world is an operator's "everything is fine, start over" button.
	// Clear the portafilter state flags so a subsequent cancel doesn't try
	// to run recovery against a state that no longer matches reality.
	s.portafilterInMachine.Store(false)
	s.portafilterHasGrounds.Store(false)

	if err := s.resetFrameSystem(ctx); err != nil {
		return nil, fmt.Errorf("reset_world: %w", err)
	}

	unpaused := false
	if s.paused.Load() {
		select {
		case s.queue.proceed <- struct{}{}:
		default:
			// Buffered slot is full — a proceed signal is already pending and
			// will be consumed by processQueue. Either way, the unpause was
			// requested.
		}
		unpaused = true
	}

	s.logger.Infof("reset_world: cancelled=%v cleared=%d unpaused=%v frame_system_reset=true",
		cancelled, removed, unpaused)
	return map[string]any{
		"status":    "reset",
		"cancelled": cancelled,
		"cleared":   removed,
		"unpaused":  unpaused,
	}, nil
}

// signalCancel interrupts any in-flight motion by cancelling the shared
// cancelCtx and pausing the queue. Returns true if a sequence was running.
// Does not wait for the running goroutine to observe the cancellation.
func (s *beanjaminCoffee) signalCancel() bool {
	if !s.running.Load() {
		return false
	}
	s.paused.Store(true)
	s.mu.Lock()
	s.cancelFunc()
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())
	s.mu.Unlock()
	return true
}

// waitForIdle polls until s.running flips back to false (meaning the cancelled
// sequence has fully unwound through its defers) or the timeout / ctx expires.
func (s *beanjaminCoffee) waitForIdle(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for s.running.Load() {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for sequence to stop", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

// activeOrderLogger returns the order-scoped logger for the in-flight order
// when one is being processed, otherwise the base service logger. Used by
// entry points (cancel) that run outside the queue goroutine and so don't
// receive the tagged logger as a parameter. Never returns nil.
func (s *beanjaminCoffee) activeOrderLogger() logging.Logger {
	if l := s.activeLogger.Load(); l != nil {
		return *l
	}
	return s.logger
}

// cancel interrupts any running sequence and drives whichever recovery the
// current state requires so the operator does not need a follow-up reset_world:
//   - portafilter locked in the machine (post-releaseFilter, pre-grabFilter):
//     grab → unlock → clean → home.
//   - portafilter held by the arm with grounds in it (post-grindCoffee,
//     pre-cleanPortafilter, and not in the machine): clean → home.
//   - otherwise: no recovery motion (queue paused, frame system reset).
//
// The frame system is rebuilt at the end to discard any lockFilterFrame
// mutation. The queue is left paused with its pending orders intact; send
// 'proceed' to resume. If recovery motion fails, the frame system is left
// untouched and the flags remain set so a subsequent cancel can retry.
//
// Known limitation: a cancel that fires mid-lockPortaFilter (between the
// motion entering the machine and releaseFilter's gripper.Open) may try to
// route the arm away while the bayonet is partially engaged. There is no
// safe automated recovery for that narrow window — the operator must
// intervene manually.
func (s *beanjaminCoffee) cancel(ctx context.Context) (map[string]any, error) {
	cancelled := s.signalCancel()
	if cancelled {
		if err := s.waitForIdle(ctx, resetCancelWaitTimeout); err != nil {
			return nil, fmt.Errorf("cancel: %w", err)
		}
	}

	// Take exclusive ownership of the arm before any recovery motion so
	// other commands (execute_action, prepare_order consumer) can't race.
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("cancel: another sequence is running")
	}
	defer s.running.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	// Tag recovery logs with the in-flight order's ID when an order is still
	// active (cancel runs outside the queue goroutine, so there's no tagged
	// logger passed in); falls back to the base logger on an idle cancel.
	logger := s.activeOrderLogger()

	// Announce the cancellation up front so the operator hears what's
	// happening before any recovery motion begins. Only speak when there
	// is something to actually cancel/recover — silence on a no-op cancel.
	if cancelled || s.portafilterInMachine.Load() || s.portafilterHasGrounds.Load() {
		if err := s.sayAlways(ctx, cancelAnnouncement); err != nil {
			logger.Warnf("cancel: failed to announce cancellation: %v", err)
		}
	}

	// Drop any cup/glass still in the gripper before recovery so the arm starts
	// empty and the frame system stops tracking a container we've let go. The
	// resetFrameSystem below also forgets the geometry, but dropping here both
	// releases the physical object and clears the held-item frame mid-flow, so
	// the recovery motion that follows plans against reality.
	if err := s.dropHeldContainer(ctx); err != nil {
		return nil, fmt.Errorf("cancel: %w", err)
	}

	recovered := false
	switch {
	case s.portafilterInMachine.Load():
		logger.Infof("cancel: portafilter is in the machine — running recovery (grab → unlock → clean → home)")
		s.setStep(stepRecoveringFilter)
		if err := s.grabFilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("cancel: recovery grab_filter: %w", err)
		}
		s.setStep(stepUnlockingPortafilter)
		if err := s.unlockPortaFilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("cancel: recovery unlock_portafilter: %w", err)
		}
		s.setStep(stepCleaning)
		if err := s.cleanPortafilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("cancel: recovery clean_portafilter: %w", err)
		}
		s.setStep(stepFinishingUp)
		homeStep := Step{PoseName: filterPoseHome, Component: componentFilter}
		if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
			return nil, fmt.Errorf("cancel: recovery home: %w", err)
		}
		s.portafilterInMachine.Store(false)
		recovered = true
	case s.portafilterHasGrounds.Load():
		logger.Infof("cancel: portafilter has grounds — running recovery (clean → home)")
		s.setStep(stepCleaning)
		if err := s.cleanPortafilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("cancel: recovery clean_portafilter: %w", err)
		}
		s.setStep(stepFinishingUp)
		homeStep := Step{PoseName: filterPoseHome, Component: componentFilter}
		if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
			return nil, fmt.Errorf("cancel: recovery home: %w", err)
		}
		// cleanPortafilter already cleared portafilterHasGrounds on success.
		recovered = true
	}

	if err := s.resetFrameSystem(ctx); err != nil {
		return nil, fmt.Errorf("cancel: %w", err)
	}

	s.currentStep.Store("")
	logger.Infof("cancel: cancelled=%v recovered=%v — queue paused, send 'proceed' to resume",
		cancelled, recovered)
	return map[string]any{
		"status":    "cancelled",
		"cancelled": cancelled,
		"recovered": recovered,
		"queue":     "paused",
	}, nil
}
