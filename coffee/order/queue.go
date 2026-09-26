package order

import (
	"errors"
	"sync"
	"time"
)

// RecentDisplayDuration is how long a completed order stays visible in
// List() / Status() before it is pruned. The frontend renders these as
// "Ready!" green cards, identical to the in-flight cleanup state.
const RecentDisplayDuration = 15 * time.Second

// Queue is a thread-safe order queue. An order moves through three
// stages, and which stage it is in is the queue's own structure rather than
// something callers have to infer:
//
//	pending → current → recent
//
// pending is the FIFO backlog still waiting to be made, current is the single
// order being made right now, and recent is a short-lived buffer of completed
// orders so the webapp can render a "Ready!" card without diffing polls. The
// whole lifecycle is owned by the backend.
type Queue struct {
	mu      sync.Mutex
	pending []Order       // backlog still waiting to be made, FIFO
	current *Order        // the order being made right now; nil when idle
	recent  []Order       // completed orders, append-most-recent-last
	notify  chan struct{} // buffered(1), poked on enqueue to wake consumer
	proceed chan struct{} // buffered(1), operator signal to resume after inter-order pause
}

// NewQueue creates a new empty order queue.
func NewQueue() *Queue {
	return &Queue{
		notify:  make(chan struct{}, 1),
		proceed: make(chan struct{}, 1),
	}
}

// Notify returns the channel Enqueue pokes to wake the consumer. It holds at
// most one pending wakeup, so a consumer should drain the backlog on each receive.
func (q *Queue) Notify() <-chan struct{} {
	return q.notify
}

// Proceed returns the channel WakeProceed signals on, for a consumer parked
// while the queue is paused.
func (q *Queue) Proceed() <-chan struct{} {
	return q.proceed
}

// WakeProceed nudges a consumer parked on Proceed. The paused flag the caller
// has just cleared is what actually releases the queue; this only saves a
// parked goroutine from sleeping until the next order arrives, and is a no-op
// when nothing is parked — a cancel that interrupted a manual action or a
// keepalive purge pauses the queue with no consumer waiting. A token that goes
// unclaimed is harmless: the consumer re-checks the flag after every wakeup
// rather than treating one as permission to run.
func (q *Queue) WakeProceed() {
	select {
	case q.proceed <- struct{}{}:
	default:
	}
}

// Enqueue adds an order to the back of the backlog and returns its 1-based
// position among the orders still to be made, counting the one on the arm.
// Position 1 therefore means "nothing ahead of you, this starts next".
//
// The in-flight order has to count: callers gate the spoken "Order received"
// acknowledgement on a position above 1, so leaving it out would meet a
// customer who ordered mid-brew with silence.
func (q *Queue) Enqueue(order Order) int {
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

// Start moves the front of the backlog into the current slot and returns it,
// reporting false when the backlog is empty.
//
// The queue has a single consumer (the coffee service's processQueue), which
// pairs every successful Start with a Complete; starting while an order is
// already current would abandon that order. The returned copy is deep so the
// consumer can hold it for the length of the brew while SetCurrentStep keeps
// appending to the queue's own StepHistory.
func (q *Queue) Start() (Order, bool) {
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

// CurrentID returns the ID of the order being made right now, or "" when idle.
func (q *Queue) CurrentID() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == nil {
		return ""
	}
	return q.current.ID
}

// Errors returned by Remove, distinguishing the three ways a cancel can miss.
var (
	ErrNotQueued = errors.New("no such order in the queue")
	ErrInFlight  = errors.New("order is already being made — use 'cancel' to stop the machine")
	ErrCompleted = errors.New("order has already been made")
)

// Remove drops one still-waiting order out of the backlog and returns it,
// closing the gap so the orders behind it move up.
//
// The order on the arm is out of reach by construction — it sits in the
// current slot, not in pending — which is the same guarantee ClearPending
// leans on. A cancel aimed at it is refused with ErrInFlight rather than
// quietly missing, because removing it would leave the consumer brewing a
// drink the queue no longer accounts for; `cancel` is what stops that one.
func (q *Queue) Remove(id string) (Order, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.pending {
		if q.pending[i].ID != id {
			continue
		}
		o := q.pending[i]
		q.pending = append(q.pending[:i], q.pending[i+1:]...)
		return o, nil
	}
	if q.current != nil && q.current.ID == id {
		return Order{}, ErrInFlight
	}
	for i := range q.recent {
		if q.recent[i].ID == id {
			return Order{}, ErrCompleted
		}
	}
	return Order{}, ErrNotQueued
}

// Complete retires the current order into the recent buffer with
// CompletedAt = time.Now(), leaving the queue idle. This is the canonical
// "the espresso routine finished" transition. No-op when nothing is current.
func (q *Queue) Complete() {
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
func (q *Queue) Len() int {
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
func (q *Queue) List() []Order {
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

// SetCurrentStep records a step transition on the order being made right now,
// updating RawStep and appending to StepHistory.
//
// No-op when the queue is idle: a step published with no order on the arm — a
// keep-alive purge, say — belongs to no order and surfaces through Status only.
func (q *Queue) SetCurrentStep(rawStep string) {
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
func (q *Queue) Clear() int {
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
func (q *Queue) ClearPending() (removed int, currentID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	removed = len(q.pending)
	q.pending = nil
	if q.current != nil {
		currentID = q.current.ID
	}
	return removed, currentID
}
