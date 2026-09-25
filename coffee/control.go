package coffee

// Queue and run control: proceed, clear_queue, reset_world, idle waiting, the
// cancel path that stops an in-flight order, and the rewind path that drives
// the arm back to a clean starting state.

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// It clears the recorded fridge-door angle too: proceed is the operator saying
// the machine has been put right, fridge included. A rebuild cannot shut a real
// door, which is why no rebuild clears the angle on its own — the assertion is
// the operator's, so the response reports the angle forgotten and a door left
// standing is visible before the next plan routes through the panel. It also
// forgets the gripper's modeled contents without opening the gripper — an
// operator holding something manually wants rewind, which physically lets go.
func (s *beanjaminCoffee) proceedQueue(ctx context.Context) (map[string]any, error) {
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("proceed: a sequence is still running — wait for it to stop, or cancel it first")
	}
	// Warn before the flag is cleared: forgetting a held item without opening
	// the jaws leaves the arm planning through whatever it is still carrying.
	if s.heldItemAttached {
		s.activeOrderLogger().Warn("proceed: forgetting a held item — if the gripper really is holding something, cancel and rewind instead so it lets go first")
	}
	// Clear the recorded door angle before the rebuild, so resetFrameSystem has
	// nothing to re-apply and the door lands at its authored shut transform with
	// everything else. Ordering is load-bearing: cleared afterward, the rebuilt
	// frame system would still be carrying the swing.
	doorOpenDegs := s.doorOpenDegs
	s.doorOpenDegs = 0
	err := s.resetFrameSystem(ctx)
	if err != nil {
		// A failed proceed asserts nothing: cachedFS still holds the swung door,
		// so the record has to keep matching it. Zeroed here, the next rebuild
		// would quietly shut a door this proceed never got to vouch for.
		s.doorOpenDegs = doorOpenDegs
	}
	s.running.Store(false)
	if err != nil {
		return nil, fmt.Errorf("proceed: %w", err)
	}
	// Same warning the held item gets, and for the same reason: proceed asserts
	// the world is as configured, and the one thing it cannot check is whether
	// the operator really did shut the door.
	if doorOpenDegs != 0 {
		s.logger.Warnf("proceed: forgetting a fridge door recorded open at %.0f° — if it is not actually shut, "+
			"the next plan will route the arm straight through the panel", doorOpenDegs)
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

// proceedResponse renders a proceed result, reporting the fridge-door angle the
// rebuild forgot so an operator can see that the model now claims a shut door.
// Omitted when the door was already shut, so the field's presence alone flags
// the assertion proceed just made about the physical world.
func proceedResponse(status string, resumed bool, doorClearedDegs float64) map[string]any {
	resp := map[string]any{
		"status":             status,
		"resumed":            resumed,
		"frame_system_reset": true,
	}
	if doorClearedDegs != 0 {
		resp["fridge_door_cleared_degs"] = doorClearedDegs
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

// clearQueue drops the backlog of orders still waiting to be made. The order
// currently being brewed is deliberately spared: clear_queue means "stop making
// more drinks", not "abandon the one on the arm right now" — cancel and
// reset_world are the commands that stop a running sequence. Recently-completed
// orders are left alone for the same reason, being drinks already sitting in the
// serving area; they self-prune after RecentDisplayDuration.
func (s *beanjaminCoffee) clearQueue() (map[string]any, error) {
	removed, currentID := s.queue.ClearPending()
	s.logger.Infof("cleared %d pending orders from queue (in-flight order kept: %v)", removed, currentID != "")
	resp := map[string]any{"status": "cleared", "removed": removed, "kept_current": currentID != ""}
	if currentID != "" {
		resp["kept_current_order_id"] = currentID
	}
	return resp, nil
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
// Everything between the cancel and the unpause runs holding the running gate:
// the rebuild swaps cachedFS and the mutation flags, which only the gate holder
// may touch, and without it a keepalive purge or execute_action could start
// planning against a half-swapped world. The gate is released before the
// unpause, as in proceed, so the order the resume lets through can claim it.
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

	// A sequence can start after the cancel check — once the cancelled one has
	// unwound, or when there was nothing running to cancel — so the arm has to
	// be claimed, not assumed.
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("reset_world: another sequence started before reset could take over — try again")
	}

	removed := s.queue.Clear()

	// reset_world is an operator's "everything is fine, start over" button.
	// Clear the portafilter state flags so a subsequent rewind doesn't try
	// to run recovery against a state that no longer matches reality.
	s.portafilterInMachine.Store(false)
	s.portafilterHasGrounds.Store(false)
	// Only an operator can assert the fridge door is physically shut, so clearing
	// the recorded angle belongs to the commands that say so — this one and
	// proceed. A rebuild on its own re-applies it rather than pretending a door
	// closed itself.
	s.doorOpenDegs = 0

	err := s.resetFrameSystem(ctx)
	s.running.Store(false)
	if err != nil {
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

// cancelOrder drops a single order out of the backlog — clearQueue narrowed to
// one order, and sparing the drink on the arm for the same reason. It touches
// nothing else: the machine keeps making whatever it is making, the queue is
// not paused, and the orders behind the cancelled one move up. Stopping the
// order already in flight is `cancel`'s job; Remove refuses it rather than
// reporting a miss, because that drink is real and someone is waiting on it.
func (s *beanjaminCoffee) cancelOrder(ctx context.Context, v any) (map[string]any, error) {
	id, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("cancel_order value must be an order ID string, got %T", v)
	}
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("cancel_order requires a non-empty order ID")
	}

	order, err := s.queue.Remove(id)
	if err != nil {
		return nil, fmt.Errorf("cancel_order %s: %w", id, err)
	}

	remaining := s.queue.Len()
	s.logger.Infof("cancel_order: dropped queued order %s (%s for %s) — %d order(s) still queued",
		order.ID, order.Drink, order.CustomerName, remaining)

	if err := s.say(ctx, pickOrderCancelled(order.Drink, order.DisplayName())); err != nil {
		s.logger.Warnf("cancel_order: failed to announce cancellation: %v", err)
	}

	return map[string]any{
		"status":        "cancelled",
		"order_id":      order.ID,
		"customer_name": order.CustomerName,
		"drink":         order.Drink,
		"remaining":     float64(remaining),
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
