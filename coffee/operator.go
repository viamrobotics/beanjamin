package coffee

// Queue and run control: proceed, clear_queue, reset_world, idle waiting, and
// the cancel path that stops an in-flight order.

import (
	"beanjamin/coffee/speech"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// proceedQueue re-syncs the recorded world with the real one and, when a cancel
// or an order fault left the queue paused, releases the pause so processQueue
// starts the next order.
//
// The rebuild is unconditional: any idle state can hold mid-cycle mutations (a
// reparented filter frame, a held cup geometry, a staged glass) that no longer
// describe a machine an operator has since tidied up, and
// refreshFrameSystemIfClean declines to rebuild precisely because they are
// present. A faulted order pauses the queue for this, but a
// manually-stepped execute_action that fails leaves them behind with the queue
// running. The running gate is required because cachedFS may only be swapped
// while no sequence owns the arm.
//
// It clears the recorded fridge-door angle too: proceed is the operator saying
// the machine has been put right, fridge included. A rebuild cannot shut a real
// door, which is why no rebuild clears the angle on its own — the assertion is
// the operator's, so the response reports the angle forgotten and a door left
// standing is visible before the next plan routes through the panel. It also
// forgets the gripper's modeled contents without opening the gripper, so
// anything still in the jaws has to be taken out by hand first.
func (s *beanjaminCoffee) proceedQueue(ctx context.Context) (map[string]any, error) {
	if _, err := s.lease.claimManual("proceed"); err != nil {
		return nil, fmt.Errorf("proceed: %w — wait for it to stop, or cancel it first", err)
	}
	// Warn before the flag is cleared: forgetting a held item without opening
	// the jaws leaves the arm planning through whatever it is still carrying.
	if s.heldItemAttached {
		s.activeOrderLogger().Warn("proceed: forgetting a held item — if the gripper really is holding something, take it out by hand first")
	}
	if s.heldMilk != "" {
		s.activeOrderLogger().Warnf("proceed: forgetting that the %s milk is out of the fridge — %s", s.heldMilk, s.milkPutBackHint(s.heldMilk))
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
	s.lease.release()
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

	// Clearing the pause IS the resume: the waiting queue and the keepalive
	// loop see it on their next check. unpause reports whether there was a
	// pause, so two proceeds racing cannot both claim to have released it.
	if !s.lease.unpause() {
		s.logger.Info("proceed: frame system rebuilt, queue was not paused")
		return proceedResponse("reset", false, doorOpenDegs), nil
	}

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

// clearQueue drops the backlog of orders still waiting to be made. The order
// currently being brewed is deliberately spared: clear_queue means "stop making
// more drinks", not "abandon the one on the arm right now" — cancel and
// reset_world are the commands that stop a running sequence. Recently-completed
// orders are left alone for the same reason, being drinks already sitting in the
// serving area; they self-prune after order.RecentDisplayDuration.
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
	if _, err := s.lease.claimManual("reset_world"); err != nil {
		return nil, fmt.Errorf("reset_world: another sequence started before reset could take over — try again: %w", err)
	}

	removed := s.queue.Clear()

	// reset_world is an operator's "everything is fine, start over" button.
	// Clear the portafilter state flags so the next fault's log doesn't report
	// state that no longer matches reality.
	s.portafilterInMachine.Store(false)
	s.portafilterHasGrounds.Store(false)
	// Only an operator can assert the fridge door is physically shut, so clearing
	// the recorded angle belongs to the commands that say so — this one and
	// proceed. A rebuild on its own re-applies it rather than pretending a door
	// closed itself.
	s.doorOpenDegs = 0

	err := s.resetFrameSystem(ctx)
	s.lease.release()
	if err != nil {
		return nil, fmt.Errorf("reset_world: %w", err)
	}

	unpaused := s.lease.unpause()

	s.logger.Infof("reset_world: cancelled=%v cleared=%d unpaused=%v frame_system_reset=true",
		cancelled, removed, unpaused)
	return map[string]any{
		"status":    "reset",
		"cancelled": cancelled,
		"cleared":   removed,
		"unpaused":  unpaused,
	}, nil
}

// signalCancel pauses the queue and interrupts any in-flight motion by
// cancelling the shared cancelCtx. Returns true if a sequence was running.
// Does not wait for the running goroutine to observe the cancellation.
//
// The pause happens even when the arm is idle, so a cancel always works as a
// pause button: no new order starts until proceed.
//
// The holder is checked under mu, the lock claim holds, so a sequence cannot
// take the arm between this check and the cancel and end up with the
// replacement context.
func (s *beanjaminCoffee) signalCancel() bool {
	s.lease.mu.Lock()
	defer s.lease.mu.Unlock()
	s.lease.paused = true
	if s.lease.holder == "" {
		return false
	}
	s.lease.cancelFunc()
	s.lease.cancelCtx, s.lease.cancelFunc = context.WithCancel(context.Background())
	return true
}

// waitForIdle polls until the arm is released (meaning the cancelled
// sequence has fully unwound through its defers) or the timeout / ctx expires.
func (s *beanjaminCoffee) waitForIdle(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for s.lease.busy() {
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

// cancel stops the machine and nothing else: it aborts the sequence, halts the
// arm mid-trajectory and pauses the queue. With nothing running it only pauses
// the queue, so no new order starts until proceed. No motion is planned, no state flag
// is cleared and the frame system is left alone, so the recorded world still
// matches the physical one until the operator has put it right and proceeds.
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
		if err := s.speaker.SayAlways(ctx, cancelAnnouncement); err != nil {
			logger.Warnf("cancel: failed to announce cancellation: %v", err)
		}
	}

	s.currentStep.Store("")
	if cancelled {
		logger.Info("cancel: sequence stopped and queue paused — " + recoverByHand)
	} else {
		logger.Info("cancel: nothing was running — queue paused, send 'proceed' to resume")
	}
	return map[string]any{
		"status":    "cancelled",
		"cancelled": cancelled,
		"queue":     queueState(s.lease.isPaused()),
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

	if err := s.speaker.Say(ctx, speech.OrderCancelled(order.Drink, order.DisplayName())); err != nil {
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

// recoverByHand is what every cancel and fault tells the operator to do: there
// is no command that drives the arm back to a clean start, so the machine is
// put right by hand and proceed resumes from there.
const recoverByHand = "put the machine back to its starting state by hand (a clean portafilter held in the claws, nothing else in the gripper, the fridge door shut), then send 'proceed'"

// queueState renders the paused flag for a command response.
func queueState(paused bool) string {
	if paused {
		return "paused"
	}
	return "running"
}
