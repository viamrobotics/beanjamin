package beanjamin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Order represents a customer coffee order in the queue.
type Order struct {
	ID           string    `json:"id"`
	Drink        string    `json:"drink"`
	CustomerName string    `json:"customer_name"`
	Greeting     string    `json:"greeting"`
	Completion   string    `json:"completion"`
	EnqueuedAt   time.Time `json:"enqueued_at"`
}

// OrderQueue is a thread-safe FIFO order queue.
type OrderQueue struct {
	mu      sync.Mutex
	orders  []Order
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

// Enqueue adds an order to the back of the queue and returns its 1-based position.
func (q *OrderQueue) Enqueue(order Order) int {
	q.mu.Lock()
	q.orders = append(q.orders, order)
	pos := len(q.orders)
	q.mu.Unlock()

	// Non-blocking poke to wake consumer.
	select {
	case q.notify <- struct{}{}:
	default:
	}

	return pos
}

// Peek returns the front order without removing it.
func (q *OrderQueue) Peek() (Order, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.orders) == 0 {
		return Order{}, false
	}
	return q.orders[0], true
}

// Dequeue removes and returns the front order.
func (q *OrderQueue) Dequeue() (Order, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.orders) == 0 {
		return Order{}, false
	}
	order := q.orders[0]
	q.orders = q.orders[1:]
	return order, true
}

// Len returns the number of orders in the queue.
func (q *OrderQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.orders)
}

// List returns a copy of all orders in the queue.
func (q *OrderQueue) List() []Order {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Order, len(q.orders))
	copy(out, q.orders)
	return out
}

// Clear removes all orders from the queue and returns how many were removed.
func (q *OrderQueue) Clear() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.orders)
	q.orders = nil
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

			s.safeExecuteOrder(order)
			s.queue.Dequeue()

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
// every order behind it. It always queues a zoo-cam clip for the attempt (success, error, or panic).
func (s *beanjaminCoffee) safeExecuteOrder(order Order) {
	ctx := context.Background()
	videoFrom := time.Now().UTC()
	var execErr error
	defer func() {
		if r := recover(); r != nil {
			execErr = fmt.Errorf("panic: %v", r)
			s.logger.Errorf("panic while processing order %s for %s: %v — queue will still save video",
				order.ID, order.CustomerName, r)
		}
		s.saveOrderVideo(order, videoFrom, execErr)
	}()
	execErr = s.executeQueuedOrder(ctx, order)
}

// executeQueuedOrder runs a single order: says greeting, brews, says completion.
// A non-nil return means the brew sequence failed; the caller still saves video via safeExecuteOrder.
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

	s.logger.Infof("order %s complete for %s", order.ID, order.CustomerName)
	return nil
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
