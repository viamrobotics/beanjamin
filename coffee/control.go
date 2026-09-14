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

// proceedQueue re-syncs the recorded world with the real one and, when a cancel
// left the queue paused, releases the pause so processQueue starts the next
// order.
//
// The rebuild is unconditional: any idle state can hold mid-cycle mutations (a
// reparented filter frame, a held cup geometry, a staged glass) that no longer
// describe a machine an operator has since tidied up. An order that fails on its
// own leaves them behind without pausing the queue, and refreshFrameSystemIfClean
// declines to rebuild precisely because they are present — so without this every
// order after a failure would inherit the stale world. The running gate is
// required because cachedFS may only be swapped while no sequence owns the arm.
//
// It does not clear the recorded fridge-door angle (only reset_world may assert
// the door is shut), so the response reports the angle left standing rather than
// letting frame_system_reset imply the fridge went back to shut with everything
// else. It also forgets the gripper's modeled contents without opening the
// gripper — an operator holding something manually wants rewind, which
// physically lets go.
func (s *beanjaminCoffee) proceedQueue(ctx context.Context) (map[string]any, error) {
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("proceed: a sequence is still running — wait for it to stop, or cancel it first")
	}
	// Warn before the flag is cleared: forgetting a held item without opening
	// the jaws leaves the arm planning through whatever it is still carrying.
	if s.heldItemAttached {
		s.activeOrderLogger().Warn("proceed: forgetting a held item — if the gripper really is holding something, cancel and rewind instead so it lets go first")
	}
	// Read before the arm is handed back, alongside every other mutation flag:
	// doorOpenDegs belongs to the motion goroutine and the running gate is what
	// makes reading it here safe.
	doorOpenDegs := s.doorOpenDegs
	err := s.resetFrameSystem(ctx)
	s.running.Store(false)
	if err != nil {
		return nil, fmt.Errorf("proceed: %w", err)
	}
	// A rebuild does not shut a real door, so the angle is deliberately kept.
	// Say so out loud: otherwise "frame system rebuilt" reads as a full reset
	// and the retained fridge is invisible until a later plan routes through it.
	if doorOpenDegs != 0 {
		s.logger.Warnf("proceed: the fridge door stays modeled open at %.0f° — a rebuild does not shut a real door. "+
			"If you closed it by hand, run reset_world instead; that is the only command that clears the angle", doorOpenDegs)
	}

	// Clearing the flag IS the resume, and it happens here rather than in the
	// queue goroutine so that it lands exactly when the resume is granted.
	// Compare-and-swap so two proceeds racing cannot both claim to have released
	// a single pause.
	if !s.paused.CompareAndSwap(true, false) {
		s.logger.Info("proceed: frame system rebuilt, queue was not paused")
		return proceedResponse("reset", false, doorOpenDegs), nil
	}
	wakeQueue(s.queue)

	s.logger.Info("proceed: frame system rebuilt, queue resumed")
	return proceedResponse("resumed", true, doorOpenDegs), nil
}

// proceedResponse renders a proceed result, carrying the fridge-door angle the
// rebuild kept so an operator can see what "reset" did not cover. Omitted when
// the door is shut, so the field's presence alone flags a door left standing.
func proceedResponse(status string, resumed bool, doorOpenDegs float64) map[string]any {
	resp := map[string]any{
		"status":             status,
		"resumed":            resumed,
		"frame_system_reset": true,
	}
	if doorOpenDegs != 0 {
		resp["fridge_door_open_degs"] = doorOpenDegs
	}
	return resp
}

// wakeQueue nudges a consumer parked in waitForProceed. The flag the caller has
// just cleared is what actually releases the queue; this only saves a parked
// goroutine from sleeping until the next order arrives, and is a no-op when
// nothing is parked — a cancel that interrupted a manual action or a keepalive
// purge pauses the queue with no consumer waiting. A token that goes unclaimed
// is harmless: waitForProceed re-checks the flag after every wakeup rather than
// treating one as permission to run.
func wakeQueue(q *OrderQueue) {
	select {
	case q.proceed <- struct{}{}:
	default:
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

	unpaused := s.paused.CompareAndSwap(true, false)
	if unpaused {
		wakeQueue(s.queue)
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

	// Both recovery paths end the same way.
	cleanAndHome := func() error {
		s.setStep(stepCleaning)
		if err := s.cleanPortafilter(ctx, cancelCtx); err != nil {
			return fmt.Errorf("recovery clean_portafilter: %w", err)
		}
		s.setStep(stepFinishingUp)
		if err := s.executeStep(ctx, cancelCtx, Step{PoseName: filterPoseHome, PoseSwitch: s.filterSw}); err != nil {
			return fmt.Errorf("recovery home: %w", err)
		}
		return nil
	}

	recovered := true
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
		if err := cleanAndHome(); err != nil {
			return nil, fmt.Errorf("rewind: %w", err)
		}
		s.portafilterInMachine.Store(false)
	case s.portafilterHasGrounds.Load():
		logger.Infof("rewind: portafilter has grounds — running recovery (clean → home)")
		if err := cleanAndHome(); err != nil {
			return nil, fmt.Errorf("rewind: %w", err)
		}
		// cleanPortafilter already cleared portafilterHasGrounds on success.
	default:
		recovered = false
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
