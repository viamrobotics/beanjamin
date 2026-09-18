package coffee

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.viam.com/rdk/module/trace"
)

// RecentDisplayDuration is how long a completed order stays visible in
// List() / Status() before it is pruned. The frontend renders these as
// "Ready!" green cards, identical to the in-flight cleanup state.
const RecentDisplayDuration = 15 * time.Second

// Valid values for Order.Fulfillment.
const (
	FulfillmentPickup   = "pickup"
	FulfillmentDelivery = "delivery"
)

// StepEntry records the start of a single processing step for an order.
type StepEntry struct {
	Step      string    `json:"step"`
	StartedAt time.Time `json:"started_at"`
}

// Order represents a customer coffee order in the queue.
//
// The identity/greeting fields are fixed once the order is enqueued (most by
// NewOrder; CustomerEmail by enqueueOrder) and never change. RawStep,
// StepHistory and CompletedAt are mutated as the order moves through the
// espresso routine; all are guarded by OrderQueue.mu and must only be updated
// through OrderQueue methods.
type Order struct {
	ID           string `json:"id"`
	Drink        string `json:"drink"`
	CustomerName string `json:"customer_name"`
	// CustomerRealName is the name the customer actually gave, before the kiosk
	// misspelled it into CustomerName. It is the identity key for per-customer
	// aggregation: CustomerName is re-misspelled on every order, so counting on
	// it splits one customer across as many rows as they have placed orders.
	// Empty for callers that never misspell (voice, operator) — the order
	// sensor falls back to CustomerName for those.
	CustomerRealName string `json:"customer_real_name,omitempty"`
	// CustomerEmail identifies the recognized customer, to credit their history.
	CustomerEmail string `json:"customer_email,omitempty"`
	Greeting      string `json:"greeting"`
	Completion    string `json:"completion"`
	// Fulfillment is how the customer receives the drink: FulfillmentPickup
	// (default) or FulfillmentDelivery. Set at enqueue time; immutable afterward.
	Fulfillment string    `json:"fulfillment"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
	// PickupPosition is the 0-based serving-area slot the finished drink was
	// placed in, returned by the serving step (placeFullCupOnShelf /
	// serveIcedCoffee) and set on prepareDrink's in-flight copy, then carried
	// into the delivery_request; the queue's copy never has it.
	PickupPosition int `json:"pickup_position,omitempty"`

	// BatchIndex / BatchSize identify this order's slot within a multi-drink
	// batch (1-based, e.g. "2 of 3"). Both zero for single orders. Set at
	// enqueue time; immutable afterward. Used by the drink-ready announcement
	// to call out batch position so customers can track progress.
	BatchIndex int `json:"batch_index,omitempty"`
	BatchSize  int `json:"batch_size,omitempty"`

	RawStep     string      `json:"raw_step"`
	StepHistory []StepEntry `json:"step_history"`
	// CompletedAt is set by Complete() when the espresso routine finishes.
	// The order then sits in OrderQueue.recent for RecentDisplayDuration
	// before being pruned. Zero value means the order is still pending.
	CompletedAt time.Time `json:"completed_at"`
}

// OrderQueue is a thread-safe order queue. An order moves through three
// stages, and which stage it is in is the queue's own structure rather than
// something callers have to infer:
//
//	pending → current → recent
//
// pending is the FIFO backlog still waiting to be made, current is the single
// order being made right now, and recent is a short-lived buffer of completed
// orders so the webapp can render a "Ready!" card without diffing polls. The
// whole lifecycle is owned by the backend.
type OrderQueue struct {
	mu      sync.Mutex
	pending []Order       // backlog still waiting to be made, FIFO
	current *Order        // the order being made right now; nil when idle
	recent  []Order       // completed orders, append-most-recent-last
	notify  chan struct{} // buffered(1), poked on enqueue to wake consumer
	proceed chan struct{} // buffered(1), operator signal to resume after inter-order pause
}

// NewOrderQueue creates a new empty order queue.
func NewOrderQueue() *OrderQueue {
	return &OrderQueue{
		notify:  make(chan struct{}, 1),
		proceed: make(chan struct{}, 1),
	}
}

// Enqueue adds an order to the back of the backlog and returns its 1-based
// position among the orders still to be made, counting the one on the arm.
// Position 1 therefore means "nothing ahead of you, this starts next".
//
// The in-flight order has to count: callers gate the spoken "Order received"
// acknowledgement on a position above 1, so leaving it out would meet a
// customer who ordered mid-brew with silence.
func (q *OrderQueue) Enqueue(order Order) int {
	q.mu.Lock()
	q.pending = append(q.pending, order)
	pos := len(q.pending)
	if q.current != nil {
		pos++
	}
	q.mu.Unlock()

	// Non-blocking poke to wake consumer.
	select {
	case q.notify <- struct{}{}:
	default:
	}

	return pos
}

// Peek returns the order at the front of the backlog — the one Start picks up
// next — without removing it. It never returns the in-flight order; Current
// does that.
func (q *OrderQueue) Peek() (Order, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return Order{}, false
	}
	return copyOrder(q.pending[0]), true
}

// Start moves the front of the backlog into the current slot and returns it,
// reporting false when the backlog is empty.
//
// The queue has a single consumer (processQueue), which pairs every successful
// Start with a Complete; starting while an order is already current would
// abandon that order. The returned copy is deep so the consumer can hold it for
// the length of the brew while SetCurrentStep keeps appending to the queue's
// own StepHistory.
func (q *OrderQueue) Start() (Order, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return Order{}, false
	}
	o := q.pending[0]
	q.pending = append(q.pending[:0], q.pending[1:]...)
	q.current = &o
	return copyOrder(o), true
}

// Current returns the order being made right now, or false when idle.
func (q *OrderQueue) Current() (Order, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == nil {
		return Order{}, false
	}
	return copyOrder(*q.current), true
}

// CurrentID returns the ID of the order being made right now, or "" when idle.
func (q *OrderQueue) CurrentID() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == nil {
		return ""
	}
	return q.current.ID
}

// Complete retires the current order into the recent buffer with
// CompletedAt = time.Now(), leaving the queue idle. This is the canonical
// "the espresso routine finished" transition. No-op when nothing is current.
func (q *OrderQueue) Complete() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == nil {
		return
	}
	o := *q.current
	o.CompletedAt = time.Now()
	q.current = nil
	q.recent = append(q.recent, o)
}

// Len returns how many orders still need to be made: the backlog plus the one
// on the arm. Recently-completed orders do NOT count toward depth.
func (q *OrderQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.pending)
	if q.current != nil {
		n++
	}
	return n
}

// List returns a snapshot of all visible orders in render order: recently
// completed first (most-recent first), then the order on the arm, then the
// backlog in FIFO — top to bottom as the UI reads it. Expired entries in recent
// (CompletedAt older than RecentDisplayDuration) are pruned in the same call.
// StepHistory is deep-copied so callers can read the snapshot without racing
// against concurrent SetCurrentStep updates.
func (q *OrderQueue) List() []Order {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Prune expired recent entries in place.
	cutoff := time.Now().Add(-RecentDisplayDuration)
	kept := q.recent[:0]
	for _, o := range q.recent {
		if o.CompletedAt.After(cutoff) {
			kept = append(kept, o)
		}
	}
	q.recent = kept

	out := make([]Order, 0, len(q.recent)+1+len(q.pending))
	// Recent orders rendered most-recent-first (top of the UI).
	for i := len(q.recent) - 1; i >= 0; i-- {
		out = append(out, copyOrder(q.recent[i]))
	}
	if q.current != nil {
		out = append(out, copyOrder(*q.current))
	}
	for _, o := range q.pending {
		out = append(out, copyOrder(o))
	}
	return out
}

// copyOrder returns a value-copy of o with StepHistory deep-copied.
func copyOrder(o Order) Order {
	out := o
	if o.StepHistory != nil {
		out.StepHistory = make([]StepEntry, len(o.StepHistory))
		copy(out.StepHistory, o.StepHistory)
	}
	return out
}

// SetCurrentStep records a step transition on the order being made right now,
// updating RawStep and appending to StepHistory.
//
// No-op when the queue is idle: a step published with no order on the arm — a
// keep-alive purge, say — belongs to no order and surfaces through Status only.
func (q *OrderQueue) SetCurrentStep(rawStep string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == nil {
		return
	}
	q.current.RawStep = rawStep
	q.current.StepHistory = append(q.current.StepHistory, StepEntry{
		Step:      rawStep,
		StartedAt: time.Now(),
	})
}

// Clear empties every stage — backlog, the order on the arm, and the recent
// buffer — returning the total removed. This is the full wipe behind
// reset_world, which has already cancelled the sequence running the current
// order. clear_queue wants ClearPending instead.
func (q *OrderQueue) Clear() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.pending) + len(q.recent)
	if q.current != nil {
		n++
	}
	q.pending = nil
	q.current = nil
	q.recent = nil
	return n
}

// ClearPending drops the backlog. It reports how many orders were removed and
// the ID of the order left running ("" when idle), both read under one lock so
// a caller can describe what it spared without racing the brew finishing.
//
// The order on the arm and the recent buffer are structurally out of reach,
// which is the point: clear_queue means "make no more drinks", and the drink
// already being made has to survive it — staying visible in List, still taking
// SetCurrentStep updates, and still completing into recent as a "Ready!" card.
func (q *OrderQueue) ClearPending() (removed int, currentID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	removed = len(q.pending)
	q.pending = nil
	if q.current != nil {
		currentID = q.current.ID
	}
	return removed, currentID
}

// NewOrder creates an Order with a generated UUID and current timestamp.
// Fulfillment defaults to pickup; callers override it before enqueueing.
func NewOrder(drink, customerName, greeting, completion string) Order {
	return Order{
		ID:           uuid.New().String(),
		Drink:        drink,
		CustomerName: customerName,
		Greeting:     greeting,
		Completion:   completion,
		Fulfillment:  FulfillmentPickup,
		EnqueuedAt:   time.Now(),
	}
}

// processQueue is the background consumer goroutine. It runs orders from the
// queue one at a time in FIFO order.
func (s *beanjaminCoffee) processQueue() {
	for {
		// Wait for work or shutdown.
		select {
		case <-s.queueStop:
			return
		case <-s.queue.notify:
		}

		// Drain orders one by one.
		for {
			// Honour a cancel-induced pause before every order, not just after
			// the one that was cancelled: a cancel that interrupted a manual
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
			// RecentDisplayDuration before they're pruned by List().
			s.queue.Complete()
			// Reset the service-global step now that no order is current. The
			// completed copy in recent keeps its raw_step for debugging.
			s.currentStep.Store("")
		}
	}
}

// waitForProceed blocks while an operator cancel has the queue paused, and
// reports false when the service is shutting down.
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
	logger.Infof("queue paused by a cancel — send 'proceed' to resume")
	for s.paused.Load() {
		select {
		case <-s.queue.proceed:
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
func (s *beanjaminCoffee) safeExecuteOrder(order Order) {
	// Runs entirely within the window where processQueue has published the
	// order-scoped logger, so activeOrderLogger() returns the tagged logger
	// here and in everything it calls.
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(context.Background(), "beanjamin::order["+order.ID+"/"+order.Drink+"]")
	videoFrom := time.Now().UTC()
	var execErr error
	startedAt := time.Now()
	// Reset the per-order failure step; prepareDrink stores the label it
	// errored at, and a panic below records currentStep directly.
	s.failedStep.Store("")
	s.writePendingSave(order, videoFrom)

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
				order.CustomerName, r)
		}
		failedStep, _ := s.failedStep.Load().(string)
		s.notifyOrderReading(orderReading{
			order:      order,
			execErr:    execErr,
			failedStep: failedStep,
			// An operator cancel (or reset_world) interrupts the order by
			// cancelling orderCancelCtx; a genuine fault leaves it un-cancelled.
			// Only meaningful alongside a failure, hence the execErr guard.
			operatorCancelled: execErr != nil && orderCancelCtx.Err() != nil,
			traceID:           traceIDFromContext(ctx),
			decaf:             isDecafDrink(order.Drink),
			startedAt:         startedAt,
			endedAt:           time.Now(),
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
			s.recordOrderHistory(ctx, order)
		} else {
			s.setSensorReading(ctx, s.usageSensor, "consecutive orders", "successful_consecutive_orders", 0)
		}
		// saveOrderVideoAsync owns clearing the pending record—only after the save
		// succeeds—so a crash/restart during the post-roll wait stays recoverable.
		s.saveOrderVideoAsync(order, videoFrom, execErr)
		span.End()
	}()
	execErr = s.executeQueuedOrder(ctx, order)
}

// executeQueuedOrder runs a single order: says greeting, brews, says completion.
// A non-nil return means the brew sequence failed; the caller still notifies the sensor and saves video via safeExecuteOrder.
func (s *beanjaminCoffee) executeQueuedOrder(ctx context.Context, order Order) error {
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(ctx, "beanjamin::executeQueuedOrder")
	defer span.End()
	waitTime := time.Since(order.EnqueuedAt).Round(time.Second)
	logger.Infof("starting order for %s (%s) — waited %s in queue",
		order.CustomerName, order.Drink, waitTime)

	if order.Greeting != "" {
		if err := s.say(ctx, order.Greeting); err != nil {
			logger.Warnf("failed to say greeting: %v", err)
		}
	}

	if err := s.prepareDrink(ctx, order); err != nil {
		logger.Errorf("order for %s failed: %v", order.CustomerName, err)
		return err
	}

	if order.Completion != "" {
		if err := s.say(ctx, order.Completion); err != nil {
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

func (s *beanjaminCoffee) notifyOrderReading(r orderReading) {
	if s.orderSensorSink != nil {
		s.orderSensorSink.pushOrderReading(r)
	}
	// Best-effort Slack alert on any non-successful attempt (faults + operator
	// cancels). No-op when no slack_notifier_name is configured.
	s.notifyOrderFailureSlack(r)
	// Red LED flash + snarky spoken line on genuine faults only.
	s.reactToOrderFailure(r)
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

// enqueueOrder validates the order and adds it to the queue.
// It returns immediately with the queue position. When the optional
// "count" field is > 1, N identical orders are enqueued back-to-back
// (each with its own UUID) and the per-order "Order received" line is
// replaced with a single consolidated batch announcement.
func (s *beanjaminCoffee) enqueueOrder(ctx context.Context, orderRaw any) (map[string]any, error) {
	s.logger.Infof("received order request")

	order, ok := orderRaw.(map[string]any)
	if !ok {
		s.logger.Warnf("rejected order: invalid payload type %T", orderRaw)
		return nil, fmt.Errorf("prepare_order value must be an object with keys: drink, customer_name, initial_greeting, completion_statement, count, fulfillment")
	}

	drink, _ := order["drink"].(string)
	customerName, _ := order["customer_name"].(string)
	// Trimmed because this is an aggregation key: " Vijay" and "Vijay" must not
	// become two customers.
	customerRealName, _ := order["customer_real_name"].(string)
	customerRealName = strings.TrimSpace(customerRealName)
	customerEmail, _ := order["customer_email"].(string)
	s.logger.Infof("order request: drink=%q customer=%q", drink, customerName)

	switch drink {
	case "espresso", "lungo":
	case "decaf", "decaf_lungo":
		if !s.cfg.CanServeDecaf {
			s.logger.Infof("rejected decaf order %q from %s (can_serve_decaf=false)", drink, customerName)
			msg := pickUnsupportedDrink(drink)
			if err := s.say(ctx, msg); err != nil {
				s.logger.Warnf("failed to say rejection: %v", err)
			}
			return nil, fmt.Errorf("unsupported drink %q: %s", drink, msg)
		}
	case "iced_coffee":
		if !s.cfg.CanServeIced {
			s.logger.Infof("rejected iced order %q from %s (can_serve_iced=false)", drink, customerName)
			msg := pickUnsupportedDrink(drink)
			if err := s.say(ctx, msg); err != nil {
				s.logger.Warnf("failed to say rejection: %v", err)
			}
			return nil, fmt.Errorf("unsupported drink %q: %s", drink, msg)
		}
	case "iced_latte":
		// can_serve_iced_latte implies can_serve_iced (Validate rejects it
		// otherwise), so the one flag is the whole gate.
		if !s.cfg.CanServeIcedLatte {
			s.logger.Infof("rejected iced latte order %q from %s (can_serve_iced_latte=false)", drink, customerName)
			msg := pickUnsupportedDrink(drink)
			if err := s.say(ctx, msg); err != nil {
				s.logger.Warnf("failed to say rejection: %v", err)
			}
			return nil, fmt.Errorf("unsupported drink %q: %s", drink, msg)
		}
	default:
		s.logger.Infof("rejected order for unsupported drink %q from %s", drink, customerName)
		msg := pickUnsupportedDrink(drink)
		if err := s.say(ctx, msg); err != nil {
			s.logger.Warnf("failed to say rejection: %v", err)
		}
		return nil, fmt.Errorf("unsupported drink %q: %s", drink, msg)
	}

	initialGreeting, _ := order["initial_greeting"].(string)
	completionStatement, _ := order["completion_statement"].(string)

	fulfillment, err := parseFulfillment(order["fulfillment"])
	if err != nil {
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}
	// Delivery orders must be attributable: the delivery bot identifies the
	// recipient by email, so an anonymous delivery has nowhere to go. Pickup
	// (the default) stays open to anonymous walk-ups.
	if fulfillment == FulfillmentDelivery && customerEmail == "" {
		err := fmt.Errorf("delivery orders require a customer_email")
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}

	count, err := s.parseOrderCount(order["count"])
	if err != nil {
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}

	ids := make([]string, 0, count)
	var firstPos int
	for i := 0; i < count; i++ {
		// For single orders, auto-pick a brew-start greeting if one wasn't
		// supplied. For batches we deliberately leave Greeting empty: the
		// consolidated batch announcement at submission already covered
		// "we got your order", so executeQueuedOrder speaking another
		// "let me whip up your espresso!" before each of N cups is just
		// noise. (executeQueuedOrder skips the speech when Greeting is "".)
		greeting := initialGreeting
		if greeting == "" && count == 1 {
			greeting = pickGreeting(drink, customerName)
		}
		o := NewOrder(drink, customerName, greeting, completionStatement)
		o.CustomerRealName = customerRealName
		o.CustomerEmail = customerEmail
		o.Fulfillment = fulfillment
		if count > 1 {
			o.BatchIndex = i + 1
			o.BatchSize = count
		}
		pos := s.queue.Enqueue(o)
		if i == 0 {
			firstPos = pos
		}
		ids = append(ids, o.ID)
		s.logger.Infof("order %s queued at position %d for %s (batch %d/%d)",
			o.ID, pos, customerName, i+1, count)
	}

	// Single-order path keeps the original "Order received…" announcement
	// gated on pos > 1. Batch path replaces it with one consolidated
	// pickOrderReceivedBatch line so we don't fire N-1 blocking TTS calls
	// back-to-back.
	switch {
	case count == 1 && firstPos > 1:
		if err := s.say(ctx, pickOrderReceived(drink, customerName)); err != nil {
			s.logger.Warnf("failed to announce order %s: %v", ids[0], err)
		}
	case count > 1:
		if err := s.say(ctx, pickOrderReceivedBatch(drink, customerName, count)); err != nil {
			s.logger.Warnf("failed to announce batch: %v", err)
		}
	}

	return map[string]any{
		"status":         "queued",
		"order_id":       ids[0],
		"queue_position": firstPos,
		"customer_name":  customerName,
		"order_ids":      ids,
		"count":          count,
	}, nil
}

// parseFulfillment validates the optional "fulfillment" field on a
// prepare_order payload. Absent/nil/empty → pickup. Anything other than
// "pickup" or "delivery" → error. Caller should reject before any enqueue.
func parseFulfillment(v any) (string, error) {
	if v == nil {
		return FulfillmentPickup, nil
	}
	f, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("fulfillment must be a string, got %T", v)
	}
	switch f {
	case "":
		return FulfillmentPickup, nil
	case FulfillmentPickup, FulfillmentDelivery:
		return f, nil
	default:
		return "", fmt.Errorf("fulfillment must be %q or %q, got %q", FulfillmentPickup, FulfillmentDelivery, f)
	}
}

// parseOrderCount validates and coerces the optional "count" field on a
// prepare_order payload. Absent/nil → 1. Non-numeric, fractional,
// out-of-range values → error. Caller should reject before any enqueue.
func (s *beanjaminCoffee) parseOrderCount(v any) (int, error) {
	if v == nil {
		return 1, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("count must be a number, got %T", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, fmt.Errorf("count must be a whole number, got %v", f)
	}
	if f < 1 {
		return 0, fmt.Errorf("count must be >= 1, got %v", f)
	}
	limit := s.maxBatchSize()
	if f > float64(limit) {
		return 0, fmt.Errorf("count must be <= %d, got %v", limit, f)
	}
	return int(f), nil
}
