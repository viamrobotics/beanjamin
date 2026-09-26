// Package order holds the coffee service's order model: the Order itself, the
// queue orders wait in, the prepare_order request decoding, the drink catalog,
// and the per-attempt Reading handed to the order sensor. It depends on nothing
// in the coffee service, so the service and its peripherals can share it.
package order

import (
	"time"

	"github.com/google/uuid"
)

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
// NewOrder; CustomerEmail by the coffee service's enqueueOrder) and never
// change. RawStep, StepHistory and CompletedAt are mutated as the order moves
// through the espresso routine; all are guarded by Queue.mu and must only be
// updated through Queue methods.
type Order struct {
	ID    string `json:"id"`
	Drink string `json:"drink"`
	// CustomerName is the name the customer gave; per-customer aggregation
	// groups on it.
	CustomerName string `json:"customer_name"`
	// ModifiedCustomerName is the misspelling shown for this order, re-rolled
	// each time, so it can't double as the aggregation key. Read it through
	// DisplayName. Empty for callers that don't misspell.
	ModifiedCustomerName string `json:"modified_customer_name,omitempty"`
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
	// The order then sits in Queue.recent for RecentDisplayDuration
	// before being pruned. Zero value means the order is still pending.
	CompletedAt time.Time `json:"completed_at"`
}

// DisplayName is the name to show and speak: the misspelling when there is one,
// otherwise the name the customer gave. Not stable across a customer's orders.
func (o Order) DisplayName() string {
	if o.ModifiedCustomerName != "" {
		return o.ModifiedCustomerName
	}
	return o.CustomerName
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

// copyOrder returns a value-copy of o with StepHistory deep-copied.
func copyOrder(o Order) Order {
	out := o
	if o.StepHistory != nil {
		out.StepHistory = make([]StepEntry, len(o.StepHistory))
		copy(out.StepHistory, o.StepHistory)
	}
	return out
}
