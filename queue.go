package beanjamin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
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
// The first six fields are set at construction time and never change.
// RawStep, StepHistory and CompletedAt are mutated as the order moves
// through the espresso routine; all are guarded by OrderQueue.mu and must
// only be updated through OrderQueue methods.
type Order struct {
	ID           string    `json:"id"`
	Drink        string    `json:"drink"`
	CustomerName string    `json:"customer_name"`
	Greeting     string    `json:"greeting"`
	Completion   string    `json:"completion"`
	EnqueuedAt   time.Time `json:"enqueued_at"`

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
	pending []Order // active queue, FIFO
	recent  []Order // completed orders, append-most-recent-last
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
// queue one at a time in FIFO order. When clean_after_use is disabled and the
// queue is non-empty, it pauses between orders until the operator sends "proceed".
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

			remaining := s.queue.Len() - 1 // excluding the one about to run
			s.logger.Infof("processing order %s for %s (%s) — %d order(s) waiting behind it",
				order.ID, order.CustomerName, order.Drink, remaining)

			s.currentOrderID.Store(order.ID)
			s.safeExecuteOrder(order)
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
				s.logger.Infof("order cancelled — queue paused, send 'proceed' to resume")
				s.paused.Store(true)
				select {
				case <-s.queue.proceed:
					s.logger.Infof("received 'proceed', resuming queue processing")
					s.paused.Store(false)
				case <-s.queueStop:
					s.paused.Store(false)
					return
				}
			}

			// If cleanup is not automatic, pause
			// so the operator can clean up before the next order starts.
			if !s.cfg.CleanAfterUse {
				s.logger.Infof("queue drained — pausing for manual cleanup, send 'proceed' to continue")
				s.paused.Store(true)
				select {
				case <-s.queue.proceed:
					s.logger.Infof("received 'proceed', resuming queue processing")
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
// every order behind it. Notifies the optional order sensor and queues a zoo-cam clip when configured.
func (s *beanjaminCoffee) safeExecuteOrder(order Order) {
	ctx := context.Background()
	videoFrom := time.Now().UTC()
	var execErr error
	startedAt := time.Now()
	defer func() {
		if r := recover(); r != nil {
			execErr = fmt.Errorf("panic: %v", r)
			s.logger.Errorf("panic while processing order %s for %s: %v — queue will still save video and order reading",
				order.ID, order.CustomerName, r)
		}
		s.notifyOrderReading(order, execErr, startedAt, time.Now())
		s.saveOrderVideoAsync(order, videoFrom, execErr)
	}()
	execErr = s.executeQueuedOrder(ctx, order)
}

// executeQueuedOrder runs a single order: says greeting, brews, says completion.
// A non-nil return means the brew sequence failed; the caller still notifies the sensor and saves video via safeExecuteOrder.
func (s *beanjaminCoffee) executeQueuedOrder(ctx context.Context, order Order) error {
	waitTime := time.Since(order.EnqueuedAt).Round(time.Second)
	s.logger.Infof("starting order %s for %s (%s) — waited %s in queue",
		order.ID, order.CustomerName, order.Drink, waitTime)

	if order.Greeting != "" {
		if err := s.say(ctx, order.Greeting); err != nil {
			s.logger.Warnf("failed to say greeting for order %s: %v", order.ID, err)
		}
	}

	if err := s.prepareDrink(ctx, order.Drink, order.CustomerName); err != nil {
		s.logger.Errorf("order %s for %s failed: %v", order.ID, order.CustomerName, err)
		return err
	}

	if order.Completion != "" {
		if err := s.say(ctx, order.Completion); err != nil {
			s.logger.Warnf("failed to say completion for order %s: %v", order.ID, err)
		}
	}

	// Don't reset raw_step here. The order's last raw_step (whatever cleanup
	// label it ended on) stays attached to the completed copy that
	// processQueue is about to move into the recent buffer; the frontend
	// renders any order with completed_at set as the green "Ready!" card
	// regardless of raw_step value.
	s.logger.Infof("order %s complete for %s", order.ID, order.CustomerName)
	return nil
}

func (s *beanjaminCoffee) notifyOrderReading(order Order, execErr error, startedAt, endedAt time.Time) {
	if s.orderSensorSink != nil {
		s.orderSensorSink.pushOrderReading(order, execErr, startedAt, endedAt)
	}
}

// enqueueOrder validates the order and adds it to the queue.
// It returns immediately with the queue position.
func (s *beanjaminCoffee) enqueueOrder(ctx context.Context, orderRaw interface{}) (map[string]interface{}, error) {
	s.logger.Infof("received order request")

	order, ok := orderRaw.(map[string]interface{})
	if !ok {
		s.logger.Warnf("rejected order: invalid payload type %T", orderRaw)
		return nil, fmt.Errorf("prepare_order value must be an object with keys: drink, customer_name, initial_greeting, completion_statement")
	}

	drink, _ := order["drink"].(string)
	customerName, _ := order["customer_name"].(string)
	s.logger.Infof("order request: drink=%q customer=%q", drink, customerName)

	switch drink {
	case "espresso", "lungo":
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

	if initialGreeting == "" {
		initialGreeting = pickGreeting(drink, customerName)
	}

	o := NewOrder(drink, customerName, initialGreeting, completionStatement)
	pos := s.queue.Enqueue(o)

	s.logger.Infof("order %s queued at position %d for %s (queue depth: %d)", o.ID, pos, customerName, pos)

	return map[string]interface{}{
		"status":         "queued",
		"order_id":       o.ID,
		"queue_position": pos,
		"customer_name":  customerName,
	}, nil
}
