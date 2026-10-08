package order

import (
	"sync"
	"time"
)

// PendingDeliveryTTL is how long a served delivery order stays on the ledger
// without being collected. Long enough to cover a rover that was out on
// another errand or briefly offline; past it the drink has gone cold, or been
// taken by hand, and the slot almost certainly reused anyway.
const PendingDeliveryTTL = 30 * time.Minute

// PendingDelivery is one delivery order whose drink is sitting in the serving
// area waiting for the rover.
type PendingDelivery struct {
	OrderID string
	Drink   string
	// CupType is the container label ("cup" or "glass"), as the
	// delivery_request reports it.
	CupType       string
	CustomerName  string
	CustomerEmail string
	// PickupPosition is the 0-based serving-area slot the drink was placed in.
	PickupPosition int
	// OrderTimestamp is when the order was enqueued.
	OrderTimestamp time.Time
	// ServedAt is when the drink landed in the serving area; Record stamps it.
	ServedAt time.Time
	// Acknowledged is whether the delivery_request push got received: true.
	Acknowledged bool
}

// Deliveries is the ledger of delivery orders that have been served but not
// yet collected, so a rover that missed the push can still find the drink by
// polling. Entries leave it when the rover reports the drink collected, when
// something else is served into the same slot (that cup is gone), when they
// outlive PendingDeliveryTTL, or on Clear.
//
// The zero value is an empty ledger ready to use, and it is safe for
// concurrent use.
type Deliveries struct {
	mu      sync.Mutex
	pending []PendingDelivery // served order, oldest first
	// now is the clock, swappable in tests; nil means time.Now.
	now func() time.Time
}

func (d *Deliveries) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// prune drops entries older than PendingDeliveryTTL. Caller holds mu.
func (d *Deliveries) prune() {
	cutoff := d.clock().Add(-PendingDeliveryTTL)
	kept := d.pending[:0]
	for _, p := range d.pending {
		if p.ServedAt.After(cutoff) {
			kept = append(kept, p)
		}
	}
	d.pending = kept
}

// Record adds p to the ledger, stamping its ServedAt. A second record for the
// same order replaces the first rather than listing the drink twice.
func (d *Deliveries) Record(p PendingDelivery) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	p.ServedAt = d.clock()
	d.removeLocked(func(e PendingDelivery) bool { return e.OrderID == p.OrderID })
	d.pending = append(d.pending, p)
}

// Acknowledge marks the order's push as received. Reports false when the order
// is not on the ledger, such as when it was already collected.
func (d *Deliveries) Acknowledge(orderID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	for i := range d.pending {
		if d.pending[i].OrderID == orderID {
			d.pending[i].Acknowledged = true
			return true
		}
	}
	return false
}

// List returns the pending deliveries, oldest first.
func (d *Deliveries) List() []PendingDelivery {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	out := make([]PendingDelivery, len(d.pending))
	copy(out, d.pending)
	return out
}

// Collect removes the order from the ledger, reporting whether it was there.
// Collecting an order twice, or one that never was pending, reports false.
func (d *Deliveries) Collect(orderID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	return len(d.removeLocked(func(e PendingDelivery) bool { return e.OrderID == orderID })) > 0
}

// ClearSlot removes and returns any entries recorded at the given slot. A new
// item served into the slot means whatever stood there is gone.
func (d *Deliveries) ClearSlot(slot int) []PendingDelivery {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	return d.removeLocked(func(e PendingDelivery) bool { return e.PickupPosition == slot })
}

// Clear empties the ledger and returns how many entries it dropped, counting
// only those that had not already expired.
func (d *Deliveries) Clear() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	n := len(d.pending)
	d.pending = nil
	return n
}

// removeLocked drops the entries match selects and returns them. Caller holds mu.
func (d *Deliveries) removeLocked(match func(PendingDelivery) bool) []PendingDelivery {
	var removed []PendingDelivery
	kept := d.pending[:0]
	for _, p := range d.pending {
		if match(p) {
			removed = append(removed, p)
		} else {
			kept = append(kept, p)
		}
	}
	d.pending = kept
	return removed
}
