package beanjamin

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.viam.com/rdk/module/trace"
)

// RecentDisplayDuration is how long a completed order stays visible in
// List() / Status() before it is pruned. The frontend renders these as
// "Ready!" green cards, identical to the in-flight cleanup state.
const RecentDisplayDuration = 15 * time.Second

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
	// CustomerEmail identifies the recognized customer, to credit their history.
	CustomerEmail string    `json:"customer_email,omitempty"`
	Greeting      string    `json:"greeting"`
	Completion    string    `json:"completion"`
	EnqueuedAt    time.Time `json:"enqueued_at"`

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

// OrderQueue is a thread-safe FIFO order queue with a short-lived buffer
// of recently-completed orders. The completed buffer exists so the webapp
// can render a "Ready!" card without having to diff polls — the lifecycle
// is fully owned by the backend.
type OrderQueue struct {
	mu      sync.Mutex
	pending []Order       // active queue, FIFO
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

// Enqueue adds an order to the back of the pending queue and returns its
// 1-based position within pending.
func (q *OrderQueue) Enqueue(order Order) int {
	q.mu.Lock()
	q.pending = append(q.pending, order)
	pos := len(q.pending)
	q.mu.Unlock()

	// Non-blocking poke to wake consumer.
	select {
	case q.notify <- struct{}{}:
	default:
	}

	return pos
}

// Peek returns the front pending order without removing it.
func (q *OrderQueue) Peek() (Order, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return Order{}, false
	}
	return q.pending[0], true
}

// Complete marks the order matching id as completed. It removes the order
// from pending and appends it to recent with CompletedAt = time.Now(). This
// is the canonical "the espresso routine finished" transition — there is no
// separate Dequeue.
func (q *OrderQueue) Complete(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.pending {
		if q.pending[i].ID != id {
			continue
		}
		o := q.pending[i]
		o.CompletedAt = time.Now()
		q.pending = append(q.pending[:i], q.pending[i+1:]...)
		q.recent = append(q.recent, o)
		return
	}
}

// Len returns the number of pending orders. Recently-completed orders do
// NOT count toward depth — the depth reported to clients is "how many
// orders still need to be made".
func (q *OrderQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// List returns a snapshot of all visible orders, in render order:
// recently-completed orders first (most-recent first), then pending in FIFO.
// Expired entries in recent (CompletedAt older than RecentDisplayDuration)
// are pruned in the same call. StepHistory is deep-copied so callers can
// read the snapshot without racing against concurrent SetStep updates.
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

	out := make([]Order, 0, len(q.recent)+len(q.pending))
	// Recent orders rendered most-recent-first (top of the UI).
	for i := len(q.recent) - 1; i >= 0; i-- {
		out = append(out, copyOrder(q.recent[i]))
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

// SetStep records a step transition for the order matching id. It updates
// RawStep and appends to StepHistory. Searches both pending and recent so
// late-arriving step updates after Complete are still attributed correctly.
// No-op if no order matches.
func (q *OrderQueue) SetStep(id, rawStep string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if setStepIn(q.pending, id, rawStep) {
		return
	}
	setStepIn(q.recent, id, rawStep)
}

func setStepIn(orders []Order, id, rawStep string) bool {
	for i := range orders {
		if orders[i].ID != id {
			continue
		}
		orders[i].RawStep = rawStep
		orders[i].StepHistory = append(orders[i].StepHistory, StepEntry{
			Step:      rawStep,
			StartedAt: time.Now(),
		})
		return true
	}
	return false
}

// Clear removes all pending and recent orders, returning the total removed.
func (q *OrderQueue) Clear() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.pending) + len(q.recent)
	q.pending = nil
	q.recent = nil
	return n
}

// NewOrder creates an Order with a generated UUID and current timestamp.
func NewOrder(drink, customerName, greeting, completion string) Order {
	return Order{
		ID:           uuid.New().String(),
		Drink:        drink,
		CustomerName: customerName,
		Greeting:     greeting,
		Completion:   completion,
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
			order, ok := s.queue.Peek()
			if !ok {
				s.logger.Debugf("queue empty, waiting for new orders")
				break
			}

			// Tag every log emitted while this order runs with its ID, then
			// thread the tagged logger down through the whole brew lifecycle.
			orderLogger := s.logger.WithFields("order_id", order.ID)

			remaining := s.queue.Len() - 1 // excluding the one about to run
			orderLogger.Infof("processing order for %s (%s) — %d order(s) waiting behind it",
				order.CustomerName, order.Drink, remaining)

			s.currentOrderID.Store(order.ID)
			// Publish the tagged logger so the whole brew lifecycle — and
			// out-of-goroutine entry points like cancel — pick it up via
			// activeOrderLogger().
			s.activeLogger.Store(&orderLogger)
			s.safeExecuteOrder(order)
			s.activeLogger.Store(nil)
			s.currentOrderID.Store("")
			// Move the order from pending to recent with CompletedAt set.
			// The frontend renders recent orders as the green "Ready!" card
			// for RecentDisplayDuration before they're pruned by List().
			s.queue.Complete(order.ID)
			// Reset the service-global step now that the order is no longer
			// pending. The completed copy in recent keeps its raw_step for
			// debugging.
			s.currentStep.Store("")

			// If the operator cancelled the running order, pause so no new
			// orders start until they explicitly send 'proceed'.
			if s.paused.Swap(false) {
				orderLogger.Infof("order cancelled — queue paused, send 'proceed' to resume")
				s.paused.Store(true)
				select {
				case <-s.queue.proceed:
					orderLogger.Infof("received 'proceed', resuming queue processing")
					s.paused.Store(false)
				case <-s.queueStop:
					s.paused.Store(false)
					return
				}
			}
		}
	}
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

	if err := s.prepareDrink(ctx, order.Drink, order.CustomerName, order.BatchIndex, order.BatchSize); err != nil {
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
func (s *beanjaminCoffee) enqueueOrder(ctx context.Context, orderRaw interface{}) (map[string]interface{}, error) {
	s.logger.Infof("received order request")

	order, ok := orderRaw.(map[string]interface{})
	if !ok {
		s.logger.Warnf("rejected order: invalid payload type %T", orderRaw)
		return nil, fmt.Errorf("prepare_order value must be an object with keys: drink, customer_name, initial_greeting, completion_statement, count")
	}

	drink, _ := order["drink"].(string)
	customerName, _ := order["customer_name"].(string)
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
		o.CustomerEmail = customerEmail
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

	return map[string]interface{}{
		"status":         "queued",
		"order_id":       ids[0],
		"queue_position": firstPos,
		"customer_name":  customerName,
		"order_ids":      ids,
		"count":          count,
	}, nil
}

// parseOrderCount validates and coerces the optional "count" field on a
// prepare_order payload. Absent/nil → 1. Non-numeric, fractional,
// out-of-range values → error. Caller should reject before any enqueue.
func (s *beanjaminCoffee) parseOrderCount(v interface{}) (int, error) {
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
