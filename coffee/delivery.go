package coffee

// Delivery-cart session. The cart polls this service; the coffee machine does
// not push a delivery request. A delivery order stays visible here after the
// brew queue's recent buffer (15s) has forgotten it, until handover succeeds
// or the order fails.
//
// The cart's loop is: delivery_machine_heartbeat, delivery_pending to decide
// whether to come, delivery_machine_ready once docked (that starts the
// handoff), then delivery_handover_status until the status is handed_over or
// failed. making, ready, and retrying are not terminal — the cart keeps polling.

import (
	"fmt"
	"sync"
	"time"

	"beanjamin/coffee/order"
)

const (
	// defaultDeliveryHeartbeatTimeoutSec is how long a cart heartbeat stays
	// valid when delivery_heartbeat_timeout_sec is unset.
	defaultDeliveryHeartbeatTimeoutSec = 60

	// deliveryResultRetention is how long a terminal handover result (handed
	// over or failed) stays queryable by order id.
	deliveryResultRetention = time.Hour
)

// Statuses reported to the cart. making, ready, and retrying are non-terminal.
const (
	deliveryMaking     = "making"
	deliveryReady      = "ready"
	deliveryRetrying   = "retrying"
	deliveryHandedOver = "handed_over"
	deliveryFailed     = "failed"
)

// deliveryOrder is one delivery-fulfillment order tracked for the cart.
type deliveryOrder struct {
	order  order.Order
	status string
	served bool // drink is in a serving slot; pickup_position is meaningful
	// handoffStarted is set by delivery_machine_ready for this order. The
	// handoff does not begin for an order the cart has not named this way.
	handoffStarted bool
	err            string
	updated        time.Time
}

func (o *deliveryOrder) active() bool {
	switch o.status {
	case deliveryMaking, deliveryReady, deliveryRetrying:
		return true
	default:
		return false
	}
}

// deliveryCart is the cart session and the delivery orders waiting on it.
type deliveryCart struct {
	mu sync.Mutex

	orders map[string]*deliveryOrder

	heartSeen  bool
	heartAt    time.Time
	heartState string
	heartMode  string
	heartErr   string

	// now overrides the clock in tests. Nil uses time.Now.
	now func() time.Time
}

func newDeliveryCart() *deliveryCart {
	return &deliveryCart{orders: make(map[string]*deliveryOrder)}
}

func (d *deliveryCart) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (s *beanjaminCoffee) deliveryHeartbeatTimeout() time.Duration {
	sec := float64(defaultDeliveryHeartbeatTimeoutSec)
	if s.cfg != nil {
		sec = orDefault(s.cfg.DeliveryHeartbeatTimeoutSec, sec)
	}
	return time.Duration(sec * float64(time.Second))
}

// deliveryAccepting reports whether a new delivery order may be enqueued.
// Pickup orders do not consult it. No heartbeat yet refuses delivery.
func (s *beanjaminCoffee) deliveryAccepting() bool {
	ok, _ := s.deliveryRefusal()
	return ok
}

// deliveryRefusal reports whether delivery orders may be enqueued and, when
// they may not, the reason a prepare_order rejection should carry.
func (s *beanjaminCoffee) deliveryRefusal() (bool, string) {
	if s.delivery == nil {
		return false, "no delivery-cart heartbeat received"
	}
	return s.delivery.accepting(s.deliveryHeartbeatTimeout())
}

func (d *deliveryCart) accepting(timeout time.Duration) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.acceptingLocked(timeout)
}

func (d *deliveryCart) acceptingLocked(timeout time.Duration) (bool, string) {
	if !d.heartSeen {
		return false, "no delivery-cart heartbeat received"
	}
	if age := d.clock().Sub(d.heartAt); age > timeout {
		return false, fmt.Sprintf("delivery-cart heartbeat is %s old (limit %s)", age.Truncate(time.Second), timeout)
	}
	if d.heartState != "running" {
		return false, fmt.Sprintf("delivery-cart heartbeat state is %q, want \"running\"", d.heartState)
	}
	return true, ""
}

// deliveryMachineHeartbeat records a cart heartbeat. Only state gates new
// delivery orders; mode and last_error are stored and echoed.
func (s *beanjaminCoffee) deliveryMachineHeartbeat(v any) (map[string]any, error) {
	if s.delivery == nil {
		s.delivery = newDeliveryCart()
	}
	body, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("delivery_machine_heartbeat value must be an object, got %T", v)
	}
	state, ok := body["state"].(string)
	if !ok || state == "" {
		return nil, fmt.Errorf("delivery_machine_heartbeat.state must be a non-empty string, got %T", body["state"])
	}
	mode, err := optionalString(body, "mode")
	if err != nil {
		return nil, err
	}
	lastErr, err := optionalString(body, "last_error")
	if err != nil {
		return nil, err
	}

	accepting, reason := s.delivery.recordHeartbeat(state, mode, lastErr, s.deliveryHeartbeatTimeout())
	resp := map[string]any{
		"ok":               true,
		"accepting_orders": accepting,
		"state":            state,
		"mode":             mode,
	}
	if lastErr != "" {
		resp["last_error"] = lastErr
	}
	if !accepting {
		resp["reason"] = reason
	}
	s.logger.Infof("delivery cart heartbeat state=%q mode=%q accepting=%v", state, mode, accepting)
	return resp, nil
}

func optionalString(body map[string]any, key string) (string, error) {
	v, ok := body[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("delivery_machine_heartbeat.%s must be a string, got %T", key, v)
	}
	return s, nil
}

func (d *deliveryCart) recordHeartbeat(state, mode, lastErr string, timeout time.Duration) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.heartSeen = true
	d.heartAt = d.clock()
	d.heartState = state
	d.heartMode = mode
	d.heartErr = lastErr
	return d.acceptingLocked(timeout)
}

// track registers a delivery order as still being made. Pickup orders and a
// nil cart are ignored.
func (d *deliveryCart) track(o order.Order) {
	if d == nil || o.Fulfillment != order.FulfillmentDelivery {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.orders[o.ID]; exists {
		return
	}
	d.orders[o.ID] = &deliveryOrder{
		order:   o,
		status:  deliveryMaking,
		updated: d.clock(),
	}
}

// markReady records that the drink is in a serving slot. If the cart already
// started this order's handoff with delivery_machine_ready, the attempt runs
// now. A later brew fault (cleanup, homing) must not fail an order that
// already reached the slot.
func (d *deliveryCart) markReady(o order.Order, timeout time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	rec := d.orders[o.ID]
	if rec == nil || !rec.active() {
		return
	}
	rec.order.PickupPosition = o.PickupPosition
	rec.served = true
	rec.status = deliveryReady
	rec.err = ""
	rec.updated = d.clock()
	d.settleLocked(rec, timeout)
}

// failIfMaking marks an order failed only while it is still being made, so a
// fault after the drink is in the slot leaves it ready for the cart.
func (d *deliveryCart) failIfMaking(id, reason string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	rec := d.orders[id]
	if rec == nil || rec.status != deliveryMaking {
		return
	}
	d.failLocked(rec, reason)
}

// failActive fails every order the cart could still be waiting on. reset_world
// uses it: the operator has declared the machine clear of those drinks.
func (d *deliveryCart) failActive(reason string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, rec := range d.orders {
		if rec.active() {
			d.failLocked(rec, reason)
		}
	}
}

func (d *deliveryCart) failLocked(rec *deliveryOrder, reason string) {
	rec.status = deliveryFailed
	rec.err = reason
	rec.updated = d.clock()
}

// deliveryPending answers "is there a delivery to come get?" with the oldest
// order still being made or waiting to be handed over. It does not dock the
// cart and does not start a handoff.
func (s *beanjaminCoffee) deliveryPending() (map[string]any, error) {
	if s.delivery == nil {
		return map[string]any{"available": false}, nil
	}
	return s.delivery.pending(), nil
}

func (d *deliveryCart) pending() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked()
	return d.snapshotLocked(d.oldestLocked())
}

// deliveryMachineReady is the cart reporting that it has docked and is ready
// for a handoff. That call starts the handoff for the oldest active delivery
// order. A drink already in the slot is attempted immediately. A drink still
// being made keeps the handoff open and is attempted when it reaches the slot.
// An empty result starts nothing, so a later order waits for another call.
func (s *beanjaminCoffee) deliveryMachineReady() (map[string]any, error) {
	if s.delivery == nil {
		return map[string]any{"available": false}, nil
	}
	return s.delivery.dock(s.deliveryHeartbeatTimeout()), nil
}

func (d *deliveryCart) dock(timeout time.Duration) map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked()
	rec := d.oldestLocked()
	if rec != nil {
		rec.handoffStarted = true
		d.settleLocked(rec, timeout)
	}
	return d.snapshotLocked(rec)
}

func (d *deliveryCart) snapshotLocked(rec *deliveryOrder) map[string]any {
	if rec == nil {
		return map[string]any{"available": false}
	}
	return rec.snapshot()
}

func (d *deliveryCart) oldestLocked() *deliveryOrder {
	var best *deliveryOrder
	for _, rec := range d.orders {
		if !rec.active() {
			continue
		}
		if best == nil || rec.order.EnqueuedAt.Before(best.order.EnqueuedAt) ||
			(rec.order.EnqueuedAt.Equal(best.order.EnqueuedAt) && rec.order.ID < best.order.ID) {
			best = rec
		}
	}
	return best
}

func (rec *deliveryOrder) snapshot() map[string]any {
	resp := map[string]any{
		"available":       true,
		"order_id":        rec.order.ID,
		"status":          rec.status,
		"drink":           rec.order.Drink,
		"customer_name":   rec.order.CustomerName,
		"customer_email":  rec.order.CustomerEmail,
		"cup_type":        deliveryCupType(rec.order.Drink),
		"order_timestamp": rec.order.EnqueuedAt.UTC().Format(time.RFC3339),
	}
	if rec.served {
		resp["pickup_position"] = rec.order.PickupPosition
	}
	return resp
}

func deliveryCupType(drink string) string {
	if order.IsIced(drink) {
		return pickupLabelGlass
	}
	return pickupLabelCup
}

// deliveryHandoverStatus is the cart's poll for one order. The handoff starts
// only in delivery_machine_ready. This poll reports that attempt and repeats
// it while the status is retrying. Terminal statuses are handed_over and failed.
func (s *beanjaminCoffee) deliveryHandoverStatus(v any) (map[string]any, error) {
	body, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("delivery_handover_status value must be an object, got %T", v)
	}
	id, _ := body["order_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("delivery_handover_status.order_id must be a non-empty string")
	}
	if s.delivery == nil {
		return nil, fmt.Errorf("no delivery order %s", id)
	}
	resp, err := s.delivery.handover(id, s.deliveryHeartbeatTimeout())
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (d *deliveryCart) handover(id string, timeout time.Duration) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked()
	rec := d.orders[id]
	if rec == nil {
		return nil, fmt.Errorf("no delivery order %s", id)
	}
	d.settleLocked(rec, timeout)
	return rec.handoverSnapshot(), nil
}

// settleLocked attempts a handoff the cart already started with
// delivery_machine_ready. A fresh running heartbeat marks the order handed
// over. Any other heartbeat marks it retrying. A drink that is not in the
// slot yet stays making; markReady attempts it once the slot is known.
func (d *deliveryCart) settleLocked(rec *deliveryOrder, timeout time.Duration) {
	if !rec.handoffStarted {
		return
	}
	if rec.status != deliveryReady && rec.status != deliveryRetrying {
		return
	}
	if ok, reason := d.acceptingLocked(timeout); !ok {
		rec.status = deliveryRetrying
		rec.err = reason
		if d.heartErr != "" {
			rec.err = reason + ": " + d.heartErr
		}
		rec.updated = d.clock()
		return
	}
	rec.status = deliveryHandedOver
	rec.err = ""
	rec.updated = d.clock()
}

func (rec *deliveryOrder) handoverSnapshot() map[string]any {
	resp := map[string]any{
		"order_id": rec.order.ID,
		"status":   rec.status,
	}
	if rec.err != "" {
		resp["error"] = rec.err
	}
	return resp
}

func (d *deliveryCart) pruneLocked() {
	cutoff := d.clock().Add(-deliveryResultRetention)
	for id, rec := range d.orders {
		if !rec.active() && rec.updated.Before(cutoff) {
			delete(d.orders, id)
		}
	}
}
