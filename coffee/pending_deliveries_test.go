package coffee

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.viam.com/rdk/testutils/inject"

	"beanjamin/coffee/order"
)

// servedDeliveryOrder is a delivery order as prepareDrink hands it to
// readyForDelivery, with the serving slot already filled in.
func servedDeliveryOrder(id string, slot int) order.Order {
	return order.Order{
		ID:             id,
		Drink:          "iced_coffee",
		CustomerName:   "Alice",
		CustomerEmail:  "alice@example.com",
		Fulfillment:    order.FulfillmentDelivery,
		EnqueuedAt:     time.Date(2026, 7, 16, 15, 4, 5, 0, time.UTC),
		PickupPosition: slot,
	}
}

// pendingDeliveries runs get_pending_deliveries through DoCommand and returns
// its entries, checking the count matches.
func pendingDeliveries(t *testing.T, s *beanjaminCoffee) []map[string]any {
	t.Helper()
	res, err := s.DoCommand(context.Background(), map[string]any{"get_pending_deliveries": true})
	if err != nil {
		t.Fatalf("get_pending_deliveries error: %v", err)
	}
	list, ok := res["deliveries"].([]any)
	if !ok {
		t.Fatalf("deliveries = %T, want []any (structpb rejects typed slices)", res["deliveries"])
	}
	// float64 to match the gRPC wire type, as Status does.
	if count, ok := res["count"].(float64); !ok || int(count) != len(list) {
		t.Fatalf("count = %v (%T), want float64(%d)", res["count"], res["count"], len(list))
	}
	out := make([]map[string]any, len(list))
	for i, e := range list {
		out[i] = e.(map[string]any)
	}
	return out
}

func TestReadyForDelivery_RecordsPendingDelivery(t *testing.T) {
	peer := &fakePeer{resp: map[string]any{"received": true}}
	s := newStatusService(t, nil)
	s.deliveryHandler = peer

	before := time.Now().UTC().Truncate(time.Second)
	if err := s.readyForDelivery(context.Background(), servedDeliveryOrder("order-1", 3)); err != nil {
		t.Fatalf("readyForDelivery error: %v", err)
	}
	// The push still goes out.
	if got := peer.commands(); len(got) != 1 {
		t.Fatalf("peer received %d commands, want the delivery_request", len(got))
	}

	got := pendingDeliveries(t, s)
	if len(got) != 1 {
		t.Fatalf("pending deliveries = %v, want one", got)
	}
	e := got[0]
	want := map[string]any{
		"order_id":        "order-1",
		"drink":           "iced_coffee",
		"cup_type":        "glass",
		"customer_name":   "Alice",
		"customer_email":  "alice@example.com",
		"pickup_position": float64(3),
		"order_timestamp": "2026-07-16T15:04:05Z",
		"acknowledged":    true,
	}
	for k, v := range want {
		if e[k] != v {
			t.Errorf("%s = %v (%T), want %v (%T)", k, e[k], e[k], v, v)
		}
	}
	servedAt, err := time.Parse(time.RFC3339, e["served_at"].(string))
	if err != nil || servedAt.Before(before) {
		t.Errorf("served_at = %v, want an RFC3339 time no earlier than %v", e["served_at"], before)
	}
}

// TestReadyForDelivery_UnacknowledgedPushStillRecorded: the pull API exists for
// exactly the drinks the push failed to hand off, so they must be listed too.
func TestReadyForDelivery_UnacknowledgedPushStillRecorded(t *testing.T) {
	cases := []struct {
		name string
		peer *fakePeer
	}{
		{"no handler configured", nil},
		{"peer error", &fakePeer{err: errors.New("peer offline")}},
		{"peer refused", &fakePeer{resp: map[string]any{"received": false}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStatusService(t, nil)
			if tc.peer != nil {
				s.deliveryHandler = tc.peer
			}
			if err := s.readyForDelivery(context.Background(), servedDeliveryOrder("order-1", 0)); err != nil {
				t.Fatalf("readyForDelivery error: %v", err)
			}
			got := pendingDeliveries(t, s)
			if len(got) != 1 {
				t.Fatalf("pending deliveries = %v, want one", got)
			}
			if got[0]["acknowledged"] != false {
				t.Errorf("acknowledged = %v, want false", got[0]["acknowledged"])
			}
		})
	}
}

func TestDeliveryCollected(t *testing.T) {
	ctx := context.Background()
	s := newStatusService(t, nil)
	s.recordPendingDelivery(servedDeliveryOrder("order-1", 0))
	s.recordPendingDelivery(servedDeliveryOrder("order-2", 1))

	res, err := s.DoCommand(ctx, map[string]any{"delivery_collected": "order-1"})
	if err != nil {
		t.Fatalf("delivery_collected error: %v", err)
	}
	if res["collected"] != true || res["order_id"] != "order-1" {
		t.Errorf("resp = %v, want collected=true order_id=order-1", res)
	}
	if got := pendingDeliveries(t, s); len(got) != 1 || got[0]["order_id"] != "order-2" {
		t.Errorf("pending after collecting order-1 = %v, want only order-2", got)
	}

	// A poller retrying after a lost response, or naming an order that was
	// never pending, gets collected: false and no error.
	for _, id := range []string{"order-1", "never-served"} {
		res, err := s.DoCommand(ctx, map[string]any{"delivery_collected": id})
		if err != nil {
			t.Fatalf("delivery_collected(%q) error: %v", id, err)
		}
		if res["collected"] != false || res["order_id"] != id {
			t.Errorf("delivery_collected(%q) = %v, want collected=false", id, res)
		}
	}
}

func TestDeliveryCollected_RejectsBadID(t *testing.T) {
	s := newStatusService(t, nil)
	for _, bad := range []any{"", 42.0, true, nil, map[string]any{"order_id": "x"}} {
		if _, err := s.DoCommand(context.Background(), map[string]any{"delivery_collected": bad}); err == nil {
			t.Errorf("delivery_collected(%v) should error", bad)
		}
	}
}

func TestGetPendingDeliveries_EmptyAndOldestFirst(t *testing.T) {
	s := newStatusService(t, nil)
	if got := pendingDeliveries(t, s); len(got) != 0 {
		t.Fatalf("pending on a fresh service = %v, want none", got)
	}
	s.recordPendingDelivery(servedDeliveryOrder("first", 0))
	s.recordPendingDelivery(servedDeliveryOrder("second", 1))
	got := pendingDeliveries(t, s)
	if len(got) != 2 || got[0]["order_id"] != "first" || got[1]["order_id"] != "second" {
		t.Errorf("pending = %v, want first then second", got)
	}
}

// TestReleaseServingSlotDropsPendingDelivery: serving anything into a slot means
// the delivery recorded there is gone, while the other slots keep theirs.
func TestReleaseServingSlotDropsPendingDelivery(t *testing.T) {
	s := newStatusService(t, nil)
	s.recordPendingDelivery(servedDeliveryOrder("at-slot-2", 2))
	s.recordPendingDelivery(servedDeliveryOrder("at-slot-3", 3))

	s.releaseServingSlot(2)

	got := pendingDeliveries(t, s)
	if len(got) != 1 || got[0]["order_id"] != "at-slot-3" {
		t.Errorf("pending after serving into slot 2 = %v, want only at-slot-3", got)
	}
}

// TestPendingDeliveriesSurviveClearQueueAndCancel: those commands are about
// orders not yet made and arm motion; a drink on the shelf is still there.
func TestPendingDeliveriesSurviveClearQueueAndCancel(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestCoffee(t, nil)
	s.arm = inject.NewArm("arm")
	s.lease.cancelCtx, s.lease.cancelFunc = context.WithCancel(context.Background())
	s.recordPendingDelivery(servedDeliveryOrder("order-1", 0))

	if _, err := s.DoCommand(ctx, map[string]any{"clear_queue": true}); err != nil {
		t.Fatalf("clear_queue error: %v", err)
	}
	if _, err := s.DoCommand(ctx, map[string]any{"cancel": true}); err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	if got := pendingDeliveries(t, s); len(got) != 1 {
		t.Errorf("pending after clear_queue and cancel = %v, want order-1 still listed", got)
	}
}

func TestResetWorldClearsPendingDeliveries(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, nil)
	s.lease.cancelCtx, s.lease.cancelFunc = context.WithCancel(context.Background())
	s.recordPendingDelivery(servedDeliveryOrder("order-1", 0))

	resp, err := s.resetWorld(context.Background())
	if err != nil {
		t.Fatalf("reset_world error: %v", err)
	}
	if resp["pending_deliveries_cleared"] != 1 {
		t.Errorf("pending_deliveries_cleared = %v, want 1", resp["pending_deliveries_cleared"])
	}
	if got := pendingDeliveries(t, s); len(got) != 0 {
		t.Errorf("pending after reset_world = %v, want none", got)
	}
}
