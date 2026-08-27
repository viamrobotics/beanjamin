package coffee

// Queue and run control: proceed, clear_queue, reset_world, idle waiting, the
// cancel path that stops an in-flight order, and the rewind path that drives
// the arm back to a clean starting state.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.viam.com/rdk/logging"
)

// proceedQueue releases the pause a cancel left behind so processQueue starts
// the next order.
//
// A cancel-induced pause is the one idle state where the recorded world can
// still hold mid-cycle mutations — a filter frame reparented to world, a held
// cup geometry, a staged glass obstacle — that no longer describe the machine
// the operator has since tidied up by hand or with a rewind. So the frame
// system is rebuilt from the service before the pause is released: after the
// signal lands the queue goroutine may start planning immediately, and cachedFS
// may only be swapped while no sequence owns the arm. The recorded fridge-door
// angle survives the rebuild — only reset_world can assert the door is shut.
//
// When the queue is not paused there is nothing to resume against, so the frame
// system is left alone rather than discarding state a manually-stepped action
// (a held portafilter, a locked filter frame) still depends on.
func (s *beanjaminCoffee) proceedQueue(ctx context.Context) (map[string]any, error) {
	reset := false
	if s.paused.Load() {
		if !s.running.CompareAndSwap(false, true) {
			return nil, errors.New("proceed: a sequence is still running — wait for it to stop, or cancel it first")
		}
		err := s.resetFrameSystem(ctx)
		s.running.Store(false)
		if err != nil {
			return nil, fmt.Errorf("proceed: %w", err)
		}
		reset = true
	}

	select {
	case s.queue.proceed <- struct{}{}:
		s.logger.Infof("proceed: queue resumed, frame_system_reset=%v", reset)
		return map[string]any{"status": "resumed", "frame_system_reset": reset}, nil
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
// by lockFilterFrame), forgets that the fridge door is standing open, and
// releases the cancel-induced queue pause so processQueue is ready for new
// orders. Each step is best-effort and skipped when not applicable, so it is
// safe to call from any state.
//
// Because it declares the world to be exactly as configured, run it only when
// that is true — in particular, shut the fridge door by hand first, or the arm
// will plan straight through a panel the model now believes is closed.
func (s *beanjaminCoffee) resetWorld(ctx context.Context) (map[string]any, error) {
	cancelled := s.signalCancel()
	if cancelled {
		if err := s.waitForIdle(ctx, resetCancelWaitTimeout); err != nil {
			return nil, fmt.Errorf("reset_world: %w", err)
		}
	}

	removed := s.queue.Clear()

	// reset_world is an operator's "everything is fine, start over" button.
	// Clear the portafilter state flags so a subsequent rewind doesn't try
	// to run recovery against a state that no longer matches reality.
	s.portafilterInMachine.Store(false)
	s.portafilterHasGrounds.Store(false)
	// Only an operator can assert the fridge door is physically shut, so this is
	// the one place the recorded angle is cleared — every other rebuild re-applies
	// it rather than pretending a door closed itself.
	s.doorOpenDegs = 0

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
// entry points (cancel, rewind) that run outside the queue goroutine and so
// don't receive the tagged logger as a parameter. Never returns nil.
func (s *beanjaminCoffee) activeOrderLogger() logging.Logger {
	if l := s.activeLogger.Load(); l != nil {
		return *l
	}
	return s.logger
}

// cancel stops the machine and nothing else: it aborts the sequence, halts the
// arm mid-trajectory and pauses the queue. No motion is planned, no state flag
// is cleared and the frame system is left alone, so the recorded world still
// matches the physical one for rewind to act on.
func (s *beanjaminCoffee) cancel(ctx context.Context) (map[string]any, error) {
	cancelled := s.signalCancel()
	logger := s.activeOrderLogger()

	// Stop halts the trajectory where it stands. Never fatal: cancel must go on
	// to wait for the sequence to unwind.
	if cancelled && s.arm != nil {
		if err := s.arm.Stop(ctx, nil); err != nil {
			logger.Warnf("cancel: failed to stop the arm: %v", err)
		}
	}

	if cancelled {
		if err := s.waitForIdle(ctx, resetCancelWaitTimeout); err != nil {
			return nil, fmt.Errorf("cancel: %w", err)
		}
		if err := s.sayAlways(ctx, cancelAnnouncement); err != nil {
			logger.Warnf("cancel: failed to announce cancellation: %v", err)
		}
	}

	s.currentStep.Store("")
	if cancelled {
		logger.Info("cancel: sequence stopped and queue paused — run 'rewind' to recover the arm, then 'proceed'")
	} else {
		logger.Info("cancel: nothing was running")
	}
	return map[string]any{
		"status":    "cancelled",
		"cancelled": cancelled,
		"queue":     queueState(s.paused.Load()),
	}, nil
}

// queueState renders the paused flag for a command response.
func queueState(paused bool) string {
	if paused {
		return "paused"
	}
	return "running"
}

// rewind drives the arm back to the state a brew cycle starts from: empty
// gripper, clean portafilter, filter home in the claws. It stops any running
// sequence first, so it is safe to call at any time. On a failed recovery the
// state flags stay set so a second rewind retries. See README for the cases.
func (s *beanjaminCoffee) rewind(ctx context.Context) (map[string]any, error) {
	cancelled := s.signalCancel()
	if cancelled {
		if err := s.waitForIdle(ctx, resetCancelWaitTimeout); err != nil {
			return nil, fmt.Errorf("rewind: %w", err)
		}
	}

	// Take exclusive ownership of the arm before any recovery motion so
	// other commands (execute_action, prepare_order consumer) can't race.
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("rewind: another sequence is running")
	}
	defer s.running.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	// Rewind runs outside the queue goroutine, so the in-flight order's tagged
	// logger has to be looked up rather than passed in.
	logger := s.activeOrderLogger()

	// Announce up front so anyone standing at the machine hears what is about
	// to move before it moves.
	if err := s.sayAlways(ctx, rewindAnnouncement); err != nil {
		logger.Warnf("rewind: failed to announce recovery: %v", err)
	}

	// Drop the container before recovery, so the motion that follows plans
	// against an empty gripper rather than around an item already let go.
	if err := s.dropHeldContainer(ctx); err != nil {
		return nil, fmt.Errorf("rewind: %w", err)
	}

	recovered := false
	switch {
	case s.portafilterInMachine.Load():
		logger.Infof("rewind: portafilter is in the machine — running recovery (grab → unlock → clean → home)")
		s.setStep(stepRecoveringFilter)
		if err := s.grabFilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("rewind: recovery grab_filter: %w", err)
		}
		s.setStep(stepUnlockingPortafilter)
		if err := s.unlockPortaFilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("rewind: recovery unlock_portafilter: %w", err)
		}
		s.setStep(stepCleaning)
		if err := s.cleanPortafilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("rewind: recovery clean_portafilter: %w", err)
		}
		s.setStep(stepFinishingUp)
		homeStep := Step{PoseName: filterPoseHome, PoseSwitch: s.filterSw}
		if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
			return nil, fmt.Errorf("rewind: recovery home: %w", err)
		}
		s.portafilterInMachine.Store(false)
		recovered = true
	case s.portafilterHasGrounds.Load():
		logger.Infof("rewind: portafilter has grounds — running recovery (clean → home)")
		s.setStep(stepCleaning)
		if err := s.cleanPortafilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("rewind: recovery clean_portafilter: %w", err)
		}
		s.setStep(stepFinishingUp)
		homeStep := Step{PoseName: filterPoseHome, PoseSwitch: s.filterSw}
		if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
			return nil, fmt.Errorf("rewind: recovery home: %w", err)
		}
		// cleanPortafilter already cleared portafilterHasGrounds on success.
		recovered = true
	}

	if err := s.resetFrameSystem(ctx); err != nil {
		return nil, fmt.Errorf("rewind: %w", err)
	}

	s.currentStep.Store("")
	logger.Infof("rewind: cancelled=%v recovered=%v — queue paused, send 'proceed' to resume",
		cancelled, recovered)
	return map[string]any{
		"status":    "rewound",
		"cancelled": cancelled,
		"recovered": recovered,
		"queue":     queueState(s.paused.Load()),
	}, nil
}
