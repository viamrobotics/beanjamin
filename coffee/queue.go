package coffee

import (
	"context"
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

		// Drain orders one by one.
		for {
			// Honour a pause before every order, not just after the one that
			// was cancelled or faulted: a cancel that interrupted a manual
			// execute_action or a keepalive purge pauses the queue too, and that
			// pause has to hold the next order back just the same.
			if !s.waitForProceed() {
				return
			}

			order, ok := s.queue.Start()
			if !ok {
				s.logger.Debugf("queue empty, waiting for new orders")
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
			s.safeExecuteOrder(order)
			s.activeLogger.Store(nil)
			// Retire the order into recent with CompletedAt set. The frontend
			// renders recent orders as the green "Ready!" card for
			// order.RecentDisplayDuration before they're pruned by List().
			s.queue.Complete()
			// Reset the service-global step now that no order is current. The
			// completed copy in recent keeps its raw_step for debugging.
			s.currentStep.Store("")
		}
	}
}

// waitForProceed blocks while a cancel or an order fault has the queue paused,
// and reports false when the service is shutting down.
//
// The paused flag is never cleared here. It is the single source of truth that
// proceedQueue, resetWorld, Status and the keepalive loop all read, so only the
// command granting the resume may clear it: a consumer that cleared it on the
// way into the wait would be invisible to a proceed arriving in that window,
// which would then report "queue was not paused", send no signal, and leave
// this goroutine parked with nothing left that can ever wake it — the queue
// silently stops making drinks while Status still reports it idle and unpaused.
//
// The wakeup is re-checked rather than trusted, so a signal parked while
// nothing was waiting cannot release a later pause nobody asked to release.
func (s *beanjaminCoffee) waitForProceed() bool {
	if !s.paused.Load() {
		return true
	}
	logger := s.activeOrderLogger()
	logger.Infof("queue paused — send 'proceed' to resume")
	for s.paused.Load() {
		select {
		case <-s.queue.Proceed():
		case <-s.queueStop:
			return false
		}
	}
	logger.Infof("received 'proceed', resuming queue processing")
	return true
}

// safeExecuteOrder wraps executeQueuedOrder with panic recovery so that a
// single failing order cannot kill the queue-processing goroutine and strand
// every order behind it. Notifies the optional order sensor and queues a clip from each camera via cam storage when configured.
func (s *beanjaminCoffee) safeExecuteOrder(o order.Order) {
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

	// Snapshot the cancel context this order will run under so we can tell an
	// operator cancel from a genuine fault. signalCancel cancels this exact
	// context and then rotates s.cancelCtx to a fresh one, so we must capture
	// it now: prepareDrink reads the same s.cancelCtx (it can't rotate while
	// running is false), and reading s.cancelCtx later would see the fresh,
	// un-cancelled replacement and miss the cancellation. Relying on the error
	// unwrapping to context.Canceled is unreliable — executeStep's cancelCtx
	// branch returns a plain (non-wrapped) error.
	s.mu.Lock()
	orderCancelCtx := s.cancelCtx
	s.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			execErr = fmt.Errorf("panic: %v", r)
			step, _ := s.currentStep.Load().(string)
			s.failedStep.Store(step)
			logger.Errorf("panic while processing order for %s: %v — queue will still save video and order reading",
				o.CustomerName, r)
		}
		failedStep, _ := s.failedStep.Load().(string)
		s.notifyOrderReading(order.Reading{
			Order:      o,
			ExecErr:    execErr,
			FailedStep: failedStep,
			// An operator cancel (or reset_world) interrupts the order by
			// cancelling orderCancelCtx; a genuine fault leaves it un-cancelled.
			// Only meaningful alongside a failure, hence the execErr guard.
			OperatorCancelled: execErr != nil && orderCancelCtx.Err() != nil,
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
		} else {
			s.setSensorReading(ctx, s.usageSensor, "consecutive orders", "successful_consecutive_orders", 0)
		}
		// SaveOrderVideoAsync owns clearing the pending record—only after the save
		// succeeds—so a crash/restart during the post-roll wait stays recoverable.
		s.clips.SaveOrderVideoAsync(o, videoFrom, execErr, s.activeOrderLogger())
		span.End()
	}()
	execErr = s.executeQueuedOrder(ctx, o)
}

// executeQueuedOrder runs a single order: says greeting, brews, says completion.
// A non-nil return means the brew sequence failed; the caller still notifies the sensor and saves video via safeExecuteOrder.
func (s *beanjaminCoffee) executeQueuedOrder(ctx context.Context, order order.Order) error {
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

	if err := s.prepareDrink(ctx, order); err != nil {
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
