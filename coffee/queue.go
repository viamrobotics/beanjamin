package coffee

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"

	"beanjamin/coffee/order"
)

// processQueue is the background consumer goroutine. It runs orders from the
// queue one at a time in FIFO order.
func (s *beanjaminCoffee) processQueue() {
	for {
		// Wait for work or shutdown.
		select {
		case <-s.queueStop:
			return
		case <-s.queue.Notify():
		}

		// Drain orders one by one. Len counts only pending orders here, since
		// nothing is current between orders.
		for s.queue.Len() > 0 {
			// Claim the arm before taking the order, so an order never starts
			// while something else holds the arm or the queue is paused. Until
			// the claim the order stays pending: get_queue shows it waiting and
			// cancel_order can still remove it.
			cancelCtx, ok := s.waitForArm()
			if !ok {
				return
			}

			order, ok := s.queue.Start()
			if !ok {
				// cancel_order or clear_queue emptied the backlog while we
				// waited for the arm.
				s.lease.release()
				break
			}

			// Tag every log emitted while this order runs with its ID, then
			// thread the tagged logger down through the whole brew lifecycle.
			orderLogger := s.logger.WithFields("order_id", order.ID)

			remaining := s.queue.Len() - 1 // Len counts the current order; drop it
			orderLogger.Infof("processing order for %s (%s) — %d order(s) waiting behind it",
				order.CustomerName, order.Drink, remaining)

			// Publish the tagged logger so the whole brew lifecycle — and
			// out-of-goroutine entry points like cancel — pick it up via
			// activeOrderLogger().
			s.activeLogger.Store(&orderLogger)
			s.safeExecuteOrder(cancelCtx, order)
			s.activeLogger.Store(nil)
			// Retire the order into recent with CompletedAt set. The frontend
			// renders recent orders as the green "Ready!" card for
			// order.RecentDisplayDuration before they're pruned by List().
			s.queue.Complete()
			// Reset the service-global step now that no order is current. The
			// completed copy in recent keeps its raw_step for debugging.
			s.currentStep.Store("")
			// Released last, so a cancel or rewind waiting for the arm never
			// races the queue's own bookkeeping above.
			s.lease.release()
		}
		s.logger.Debugf("queue empty, waiting for new orders")
	}
}

// queueArmPollInterval is how often a waiting queue asks for the arm again.
const queueArmPollInterval = 100 * time.Millisecond

// waitForArm claims the arm for the next order, asking again every
// queueArmPollInterval while the arm is held or the queue is paused. It
// returns the context the order must run under, or false when the service is
// shutting down.
func (s *beanjaminCoffee) waitForArm() (context.Context, bool) {
	logged := ""
	for {
		cancelCtx, err := s.lease.claimAutomated("order queue")
		if errors.Is(err, errServiceClosed) {
			return nil, false
		}
		if err == nil {
			if logged == "paused" {
				s.logger.Infof("received 'proceed', resuming queue processing")
			}
			return cancelCtx, true
		}
		// Log each reason once, not on every retry.
		reason := "busy"
		if errors.Is(err, errQueuePaused) {
			reason = "paused"
		}
		if reason != logged {
			if reason == "paused" {
				s.logger.Infof("queue paused — send 'proceed' to resume")
			} else {
				s.logger.Infof("next order waits for the arm: %v", err)
			}
			logged = reason
		}
		select {
		case <-s.queueStop:
			return nil, false
		case <-time.After(queueArmPollInterval):
		}
	}
}

// safeExecuteOrder wraps executeQueuedOrder with panic recovery so that a
// single failing order cannot kill the queue-processing goroutine and strand
// every order behind it. Notifies the optional order sensor and queues a clip from each camera via cam storage when configured.
//
// cancelCtx is the context the queue's claim on the arm returned. The order runs
// under it, and a cancelled cancelCtx after a failure is what marks the order as
// an operator cancel rather than a fault.
func (s *beanjaminCoffee) safeExecuteOrder(cancelCtx context.Context, o order.Order) {
	// Runs entirely within the window where processQueue has published the
	// order-scoped logger, so activeOrderLogger() returns the tagged logger
	// here and in everything it calls.
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(context.Background(), "beanjamin::order["+o.ID+"/"+o.Drink+"]")
	videoFrom := time.Now().UTC()
	var execErr error
	startedAt := time.Now()
	// Reset the per-order failure step; prepareDrink stores the label it
	// errored at, and a panic below records currentStep directly.
	s.failedStep.Store("")
	s.clips.WritePendingSave(o, videoFrom, logger)

	defer func() {
		if r := recover(); r != nil {
			execErr = fmt.Errorf("panic: %v", r)
			step, _ := s.currentStep.Load().(string)
			s.failedStep.Store(step)
			// A panic is a fault like any other: pause so the next order waits
			// for rewind → proceed instead of starting from an unknown state.
			// The queue still holds the arm here, so nothing automated can
			// start before the pause lands.
			s.lease.pause()
			logger.Errorf("panic while processing order for %s: %v — queue paused; run 'rewind' to recover the arm, then 'proceed'. "+
				"The queue will still save video and order reading", o.CustomerName, r)
		}
		failedStep, _ := s.failedStep.Load().(string)
		s.notifyOrderReading(order.Reading{
			Order:      o,
			ExecErr:    execErr,
			FailedStep: failedStep,
			// An operator cancel (or reset_world) interrupts the order by
			// cancelling cancelCtx; a genuine fault leaves it un-cancelled.
			// Relying on the error unwrapping to context.Canceled is unreliable,
			// since executeStep's cancel branch returns a plain error. Only
			// meaningful alongside a failure, hence the execErr guard.
			OperatorCancelled: execErr != nil && cancelCtx.Err() != nil,
			TraceID:           traceIDFromContext(ctx),
			Decaf:             order.IsDecaf(o.Drink),
			StartedAt:         startedAt,
			EndedAt:           time.Now(),
		})
		// Consecutive-successful-orders streak: bump on success, reset on any
		// non-successful outcome (fault, panic, or operator cancel). ctx here
		// derives from context.Background() (the brew has finished), so it
		// won't be cancelled. Consumable counters already incremented mid-brew
		// are not rolled back — they reflect real physical use.
		if execErr == nil {
			s.incrementSensorReading(ctx, s.usageSensor, "consecutive orders", "successful_consecutive_orders", 1)
			// Credit the completed drink to the recognized customer's order
			// history so it can later be offered as "the usual". Best-effort:
			// a no-op when the order carries no email or no customer-detector
			// is wired in. Recording only here (not on faults/cancels) keeps
			// history to drinks the machine actually made.
			s.recordOrderHistory(ctx, o)
			s.creditLoyaltyPoint(ctx, o)
		} else {
			s.setSensorReading(ctx, s.usageSensor, "consecutive orders", "successful_consecutive_orders", 0)
		}
		// SaveOrderVideoAsync owns clearing the pending record—only after the save
		// succeeds—so a crash/restart during the post-roll wait stays recoverable.
		s.clips.SaveOrderVideoAsync(o, videoFrom, execErr, s.activeOrderLogger())
		span.End()
	}()
	execErr = s.executeQueuedOrder(ctx, cancelCtx, o)
}

// executeQueuedOrder runs a single order: says greeting, brews, says completion.
// A non-nil return means the brew sequence failed; the caller still notifies the sensor and saves video via safeExecuteOrder.
func (s *beanjaminCoffee) executeQueuedOrder(ctx, cancelCtx context.Context, order order.Order) error {
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(ctx, "beanjamin::executeQueuedOrder")
	defer span.End()
	waitTime := time.Since(order.EnqueuedAt).Round(time.Second)
	logger.Infof("starting order for %s (%s) — waited %s in queue",
		order.CustomerName, order.Drink, waitTime)

	if order.Greeting != "" {
		if err := s.speaker.Say(ctx, order.Greeting); err != nil {
			logger.Warnf("failed to say greeting: %v", err)
		}
	}

	if err := s.prepareDrink(ctx, cancelCtx, order); err != nil {
		logger.Errorf("order for %s failed: %v", order.CustomerName, err)
		return err
	}

	if order.Completion != "" {
		if err := s.speaker.Say(ctx, order.Completion); err != nil {
			logger.Warnf("failed to say completion: %v", err)
		}
	}

	// Don't reset raw_step here. The order's last raw_step (whatever cleanup
	// label it ended on) stays attached to the completed copy that
	// processQueue is about to move into the recent buffer; the frontend
	// renders any order with completed_at set as the green "Ready!" card
	// regardless of raw_step value.
	logger.Infof("order complete for %s", order.CustomerName)
	return nil
}

// orderSensorSink is what the coffee service needs from the
// viam:beanjamin:order-sensor component: somewhere to push each order attempt.
type orderSensorSink interface {
	PushOrderReading(r order.Reading)
}

func (s *beanjaminCoffee) notifyOrderReading(r order.Reading) {
	if s.orderSensorSink != nil {
		s.orderSensorSink.PushOrderReading(r)
	}
	// Best-effort Slack alert on any non-successful attempt (faults + operator
	// cancels). No-op when no slack_notifier_name is configured.
	s.notifyOrderFailureSlack(r)
	// Red LED flash + snarky spoken line on genuine faults only.
	s.faultAlarm.ReactToOrderFailure(r)
}

// traceIDFromContext returns the OTel trace ID for ctx, or "" if there is no
// valid span. Recorded on order readings so a failure links to its full trace.
func traceIDFromContext(ctx context.Context) string {
	sc := trace.FromContext(ctx).SpanContext()
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// recordOrderHistory credits a completed drink to the customer's history; no-op
// without an email or detector, best-effort otherwise.
func (s *beanjaminCoffee) recordOrderHistory(ctx context.Context, order order.Order) {
	if s.customerDetector == nil || order.CustomerEmail == "" {
		return
	}
	if _, err := s.customerDetector.DoCommand(ctx, map[string]any{
		"record_order": map[string]any{
			"email": order.CustomerEmail,
			"drink": order.Drink,
		},
	}); err != nil {
		s.activeOrderLogger().Warnf("failed to record order history for %q: %v", order.CustomerEmail, err)
	}
}

// creditLoyaltyPoint gives a completed drink's customer their point on the CRM.
// The order ID makes the credit idempotent there. Best-effort: a miss is logged
// with what an operator needs to backfill it through the CRM's credit_points.
func (s *beanjaminCoffee) creditLoyaltyPoint(ctx context.Context, o order.Order) {
	if s.crm == nil || o.CustomerEmail == "" {
		return
	}
	if _, err := s.crm.DoCommand(ctx, map[string]any{
		"credit_points": map[string]any{
			"email":    o.CustomerEmail,
			"points":   1.0,
			"reason":   "order",
			"order_id": o.ID,
		},
	}); err != nil {
		s.activeOrderLogger().Warnf("failed to credit a loyalty point to %q for order %s: %v", o.CustomerEmail, o.ID, err)
	}
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
