package coffee

import (
	"context"
	"strings"
	"testing"
	"time"

	"beanjamin/coffee/order"
)

func TestDeliveryOrder_OldestActive(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	if _, err := c.deliveryMachineHeartbeat(map[string]any{"state": "running", "mode": "dock"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	first := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")
	_ = enqueueDelivery(t, c, "lungo", "Bea", "bea@example.com")
	// A pickup order ahead of neither delivery must not surface.
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "espresso", "customer_name": "Cam",
	}); err != nil {
		t.Fatalf("pickup enqueue: %v", err)
	}

	got, err := c.deliveryPending()
	if err != nil {
		t.Fatalf("deliveryPending: %v", err)
	}
	if got["available"] != true || got["order_id"] != first || got["status"] != deliveryMaking {
		t.Fatalf("deliveryPending = %v, want Ada's order still making", got)
	}
	if _, ok := got["pickup_position"]; ok {
		t.Errorf("pickup_position = %v, want it omitted until the drink is served", got["pickup_position"])
	}
	if got["cup_type"] != "cup" || got["customer_email"] != "ada@example.com" {
		t.Errorf("details = %v", got)
	}

	handover, err := c.deliveryHandoverStatus(map[string]any{"order_id": first})
	if err != nil {
		t.Fatalf("handover: %v", err)
	}
	if handover["status"] != deliveryMaking {
		t.Errorf("handover while brewing = %v, want making", handover["status"])
	}
}

func TestDeliveryHandover_SucceedsOnceDockedAndRunning(t *testing.T) {
	c, _ := newTestCoffee(t, &Config{CanServeIced: true})
	running(t, c)
	id := enqueueDelivery(t, c, "iced_coffee", "Ada", "ada@example.com")
	c.delivery.markReady(order.Order{ID: id, Drink: "iced_coffee", PickupPosition: 0}, c.deliveryHeartbeatTimeout())

	// Asking whether to come does not dock, so the handoff has not started.
	look, err := c.deliveryPending()
	if err != nil {
		t.Fatalf("deliveryPending: %v", err)
	}
	if look["status"] != deliveryReady || look["cup_type"] != "glass" || look["pickup_position"] != 0 {
		t.Fatalf("deliveryPending = %v, want ready glass in slot 0", look)
	}
	waiting, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil {
		t.Fatalf("handover: %v", err)
	}
	if waiting["status"] != deliveryReady {
		t.Fatalf("status before dock = %v, want ready", waiting["status"])
	}

	docked, err := c.deliveryMachineReady()
	if err != nil {
		t.Fatalf("deliveryMachineReady: %v", err)
	}
	if docked["status"] != deliveryHandedOver || docked["cup_type"] != "glass" || docked["pickup_position"] != 0 {
		t.Fatalf("docked order = %v, want handed_over glass in slot 0", docked)
	}

	done, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil {
		t.Fatalf("handover: %v", err)
	}
	if done["status"] != deliveryHandedOver {
		t.Fatalf("status = %v, want handed_over", done["status"])
	}

	// The handed-over drink is gone from the dock response.
	empty, err := c.deliveryMachineReady()
	if err != nil {
		t.Fatalf("deliveryMachineReady: %v", err)
	}
	if empty["available"] != false {
		t.Fatalf("deliveryMachineReady after handover = %v, want nothing", empty)
	}
	// And a later poll of the same id still reports success.
	again, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || again["status"] != deliveryHandedOver {
		t.Fatalf("repeat handover = %v, %v", again, err)
	}
}

func TestDeliveryMachineReady_StartsHandoffWhenTheDrinkIsServed(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")

	docked, err := c.deliveryMachineReady()
	if err != nil {
		t.Fatalf("deliveryMachineReady: %v", err)
	}
	if docked["status"] != deliveryMaking {
		t.Fatalf("status at dock = %v, want making", docked["status"])
	}

	c.delivery.markReady(order.Order{ID: id, PickupPosition: 1}, c.deliveryHeartbeatTimeout())
	got, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || got["status"] != deliveryHandedOver {
		t.Fatalf("handover after serve = %v, %v, want handed_over", got, err)
	}
}

func TestDeliveryMachineReady_DoesNotStartALaterOrder(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)

	empty, err := c.deliveryMachineReady()
	if err != nil || empty["available"] != false {
		t.Fatalf("deliveryMachineReady = %v, %v, want nothing", empty, err)
	}
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")
	c.delivery.markReady(order.Order{ID: id, PickupPosition: 1}, c.deliveryHeartbeatTimeout())
	waiting, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || waiting["status"] != deliveryReady {
		t.Fatalf("handover = %v, %v, want ready until the cart docks for this order", waiting, err)
	}

	started, err := c.deliveryMachineReady()
	if err != nil || started["order_id"] != id || started["status"] != deliveryHandedOver {
		t.Fatalf("deliveryMachineReady = %v, %v, want this order handed_over", started, err)
	}
}

func TestDeliveryHandover_RetriesUntilHeartbeatRecovers(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	c.delivery.now = func() time.Time { return now }
	if _, err := c.deliveryMachineHeartbeat(map[string]any{
		"state": "running", "mode": "dock", "last_error": nil,
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")
	c.delivery.markReady(order.Order{ID: id, PickupPosition: 2}, c.deliveryHeartbeatTimeout())

	now = now.Add(time.Minute + time.Second)
	if _, err := c.deliveryMachineHeartbeat(map[string]any{
		"state": "stopped", "mode": "dock", "last_error": "bumper",
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	started, err := c.deliveryMachineReady()
	if err != nil {
		t.Fatalf("dock: %v", err)
	}
	if started["status"] != deliveryRetrying {
		t.Fatalf("deliveryMachineReady = %v, want retrying", started["status"])
	}
	retrying, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil {
		t.Fatalf("handover: %v", err)
	}
	if retrying["status"] != deliveryRetrying {
		t.Fatalf("status = %v, want retrying", retrying["status"])
	}
	if errText, _ := retrying["error"].(string); !strings.Contains(errText, "stopped") || !strings.Contains(errText, "bumper") {
		t.Errorf("error = %q, want the state and last_error", errText)
	}
	// The cart keeps polling, and the drink is still the one waiting at the dock.
	still, err := c.deliveryPending()
	if err != nil || still["order_id"] != id || still["status"] != deliveryRetrying {
		t.Fatalf("deliveryPending during retry = %v, %v", still, err)
	}

	now = now.Add(time.Second)
	if _, err := c.deliveryMachineHeartbeat(map[string]any{"state": "running", "mode": "dock"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	done, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || done["status"] != deliveryHandedOver {
		t.Fatalf("handover after recovery = %v, %v", done, err)
	}
}

func TestEnqueueDelivery_RefusesWithoutARunningHeartbeat(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	_, err := c.enqueueOrder(context.Background(), deliveryPayload("espresso", "Ada", "ada@example.com"))
	if err == nil || !strings.Contains(err.Error(), "no delivery-cart heartbeat") {
		t.Fatalf("err = %v, want a missing-heartbeat rejection", err)
	}
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "espresso", "customer_name": "Ada",
	}); err != nil {
		t.Fatalf("pickup should be accepted without a cart, got %v", err)
	}

	now := time.Now()
	c.delivery.now = func() time.Time { return now }
	if _, err := c.deliveryMachineHeartbeat(map[string]any{"state": "error", "last_error": "estop"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	_, err = c.enqueueOrder(context.Background(), deliveryPayload("espresso", "Ada", "ada@example.com"))
	if err == nil || !strings.Contains(err.Error(), "error") {
		t.Fatalf("err = %v, want a non-running rejection", err)
	}

	if _, err := c.deliveryMachineHeartbeat(map[string]any{"state": "running"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	now = now.Add(61 * time.Second)
	_, err = c.enqueueOrder(context.Background(), deliveryPayload("espresso", "Ada", "ada@example.com"))
	if err == nil || !strings.Contains(err.Error(), "old") {
		t.Fatalf("err = %v, want a stale-heartbeat rejection", err)
	}
	if c.queue.Len() != 1 {
		t.Fatalf("queue len = %d, want only the pickup", c.queue.Len())
	}
}

func TestDelivery_CancelAndClearDropTheOrder(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)
	canceled := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")
	kept := enqueueDelivery(t, c, "espresso", "Bea", "bea@example.com")
	if _, err := c.cancelOrder(context.Background(), canceled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	resp, err := c.deliveryHandoverStatus(map[string]any{"order_id": canceled})
	if err != nil {
		t.Fatalf("handover: %v", err)
	}
	if resp["status"] != deliveryFailed || resp["error"] != "canceled" {
		t.Fatalf("canceled handover = %v", resp)
	}
	oldest, err := c.deliveryPending()
	if err != nil || oldest["order_id"] != kept {
		t.Fatalf("oldest after cancel = %v, %v", oldest, err)
	}

	if _, err := c.clearQueue(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared, err := c.deliveryHandoverStatus(map[string]any{"order_id": kept})
	if err != nil || cleared["status"] != deliveryFailed {
		t.Fatalf("cleared handover = %v, %v", cleared, err)
	}
	empty, err := c.deliveryPending()
	if err != nil || empty["available"] != false {
		t.Fatalf("deliveryPending after clear = %v, %v", empty, err)
	}
}

func TestDelivery_FaultAfterServingStaysReady(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")
	c.delivery.markReady(order.Order{ID: id, PickupPosition: 1}, c.deliveryHeartbeatTimeout())
	c.delivery.failIfMaking(id, "cleanup failed")

	got, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || got["status"] != deliveryReady {
		t.Fatalf("handover = %v, %v, want ready", got, err)
	}
	c.delivery.failIfMaking(id, "grind failed")
	// Still ready: failIfMaking only applies before the drink is served.
	// A failure that never reached the slot does fail it.
	other := enqueueDelivery(t, c, "lungo", "Bea", "bea@example.com")
	c.delivery.failIfMaking(other, "grind failed")
	failed, err := c.deliveryHandoverStatus(map[string]any{"order_id": other})
	if err != nil || failed["status"] != deliveryFailed || failed["error"] != "grind failed" {
		t.Fatalf("unserved failure = %v, %v", failed, err)
	}
}

func TestDelivery_LeavingClearsTheDock(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")
	if _, err := c.deliveryMachineReady(); err != nil {
		t.Fatalf("dock: %v", err)
	}
	c.delivery.markReady(order.Order{ID: id, PickupPosition: 1}, c.deliveryHeartbeatTimeout())
	c.delivery.failActive("reset")

	empty, err := c.deliveryPending()
	if err != nil || empty["available"] != false {
		t.Fatalf("after reset = %v, %v", empty, err)
	}
	// reset cleared the dock. A drink that becomes ready afterward waits
	// until the cart calls delivery_machine_ready again. delivery_pending
	// does not start the handoff.
	next := enqueueDelivery(t, c, "espresso", "Bea", "bea@example.com")
	c.delivery.markReady(order.Order{ID: next, PickupPosition: 0}, c.deliveryHeartbeatTimeout())
	look, err := c.deliveryPending()
	if err != nil || look["status"] != deliveryReady {
		t.Fatalf("deliveryPending = %v, %v, want ready", look, err)
	}
	waiting, err := c.deliveryHandoverStatus(map[string]any{"order_id": next})
	if err != nil || waiting["status"] != deliveryReady {
		t.Fatalf("handover without a new dock = %v, %v", waiting, err)
	}
}

func TestDeliveryHeartbeat_RejectsBadPayloads(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	for _, bad := range []any{"running", map[string]any{}, map[string]any{"state": 1}, map[string]any{"state": "running", "mode": 2}} {
		if _, err := c.deliveryMachineHeartbeat(bad); err == nil {
			t.Errorf("expected error for %#v", bad)
		}
	}
	resp, err := c.DoCommand(context.Background(), map[string]any{
		"delivery_machine_heartbeat": map[string]any{"state": "running", "mode": "dock"},
	})
	if err != nil {
		t.Fatalf("DoCommand heartbeat: %v", err)
	}
	if resp["accepting_orders"] != true || resp["mode"] != "dock" {
		t.Fatalf("response = %v", resp)
	}
}

func TestValidate_RejectsNegativeDeliveryHeartbeatTimeout(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DeliveryHeartbeatTimeoutSec = -1
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "delivery_heartbeat_timeout_sec") {
		t.Fatalf("expected delivery_heartbeat_timeout_sec error, got %v", err)
	}
}

func running(t *testing.T, c *beanjaminCoffee) {
	t.Helper()
	if _, err := c.deliveryMachineHeartbeat(map[string]any{"state": "running", "mode": "dock"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func enqueueDelivery(t *testing.T, c *beanjaminCoffee, drink, name, email string) string {
	t.Helper()
	resp, err := c.enqueueOrder(context.Background(), deliveryPayload(drink, name, email))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	id, _ := resp["order_id"].(string)
	if id == "" {
		t.Fatal("enqueue returned no order_id")
	}
	return id
}

func deliveryPayload(drink, name, email string) map[string]any {
	return map[string]any{
		"drink":          drink,
		"customer_name":  name,
		"customer_email": email,
		"fulfillment":    "delivery",
	}
}
