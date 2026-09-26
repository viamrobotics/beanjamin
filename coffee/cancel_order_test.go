package coffee

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestQueue_Remove_DropsWaitingOrderAndKeepsFIFO(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("lungo", "Bob", "", "")
	c := NewOrder("espresso", "Carol", "", "")
	q.Enqueue(a)
	q.Enqueue(b)
	q.Enqueue(c)

	got, err := q.Remove(b.ID)
	if err != nil {
		t.Fatalf("Remove(b) error: %v", err)
	}
	if got.ID != b.ID || got.CustomerName != "Bob" {
		t.Errorf("removed order = %+v, want Bob's", got)
	}
	if q.Len() != 2 {
		t.Errorf("Len = %d, want 2", q.Len())
	}
	list := q.List()
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != c.ID {
		t.Errorf("remaining orders = %+v, want Alice then Carol", list)
	}
}

// The order on the arm lives in the current slot, not the backlog, so Remove
// cannot reach it by accident. It still has to say so rather than report a
// miss: the drink is real, and `cancel` is what stops it.
func TestQueue_Remove_RefusesTheOrderOnTheArm(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("lungo", "Bob", "", "")
	q.Enqueue(a)
	q.Enqueue(b)

	started, ok := q.Start()
	if !ok || started.ID != a.ID {
		t.Fatalf("Start = %+v, %v; want Alice's order", started, ok)
	}

	if _, err := q.Remove(a.ID); !errors.Is(err, errOrderInFlight) {
		t.Errorf("Remove(current) error = %v, want errOrderInFlight", err)
	}
	if q.Len() != 2 {
		t.Errorf("Len = %d, want 2 — the order on the arm must survive", q.Len())
	}
	// The order waiting behind it is still fair game.
	if _, err := q.Remove(b.ID); err != nil {
		t.Errorf("Remove(waiting) error = %v, want nil", err)
	}
}

// Once the current order retires into recent it is no longer in flight, so a
// cancel for it reports "already made" rather than pointing at `cancel`.
func TestQueue_Remove_AfterCompleteReportsCompleted(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	q.Enqueue(a)
	q.Start()
	q.Complete()

	if _, err := q.Remove(a.ID); !errors.Is(err, errOrderCompleted) {
		t.Errorf("Remove(completed) error = %v, want errOrderCompleted", err)
	}
}

func TestQueue_Remove_UnknownAndEmptyID(t *testing.T) {
	q := NewOrderQueue()
	q.Enqueue(NewOrder("espresso", "Alice", "", ""))

	if _, err := q.Remove("not-a-real-id"); !errors.Is(err, errOrderNotQueued) {
		t.Errorf("Remove(unknown) error = %v, want errOrderNotQueued", err)
	}
	// Every order carries a UUID, so an empty ID is a miss like any other
	// rather than something that matches an empty slot.
	if _, err := q.Remove(""); !errors.Is(err, errOrderNotQueued) {
		t.Errorf(`Remove("") error = %v, want errOrderNotQueued`, err)
	}
	if q.Len() != 1 {
		t.Errorf("Len = %d, want 1", q.Len())
	}
}

func TestCancelOrder_DropsQueuedOrderAndAnnounces(t *testing.T) {
	c, speech := newTestCoffee(t, &Config{CanServeDecaf: true, Conversational: true})
	alice := NewOrder("espresso", "Alice", "", "")
	bob := NewOrder("lungo", "Bob", "", "")
	c.queue.Enqueue(alice)
	c.queue.Enqueue(bob)

	resp, err := c.cancelOrder(context.Background(), bob.ID)
	if err != nil {
		t.Fatalf("cancelOrder error: %v", err)
	}
	if resp["status"] != "cancelled" || resp["order_id"] != bob.ID {
		t.Errorf("resp = %v, want cancelled with Bob's order_id", resp)
	}
	if resp["customer_name"] != "Bob" || resp["drink"] != "lungo" {
		t.Errorf("resp = %v, want the cancelled order's customer and drink", resp)
	}
	if got, _ := resp["remaining"].(float64); got != 1 {
		t.Errorf("remaining = %v, want 1", resp["remaining"])
	}
	if list := c.queue.List(); len(list) != 1 || list[0].ID != alice.ID {
		t.Errorf("queue = %+v, want Alice's order only", list)
	}
	said := speech.calls()
	if len(said) != 1 || !strings.Contains(said[0], "Bob") {
		t.Errorf("speech = %v, want one line naming Bob", said)
	}
	// Cancelling a queued order is not an operator stop: nothing pauses.
	if c.paused.Load() {
		t.Error("cancel_order must not pause the queue")
	}
}

// Spoken text uses the name the customer was shown, like every other line
// the machine says about an order.
func TestCancelOrder_SpeaksDisplayName(t *testing.T) {
	c, speech := newTestCoffee(t, &Config{CanServeDecaf: true, Conversational: true})
	o := NewOrder("lungo", "Realname", "", "")
	o.ModifiedCustomerName = "Shownname"
	c.queue.Enqueue(o)

	if _, err := c.cancelOrder(context.Background(), o.ID); err != nil {
		t.Fatalf("cancelOrder error: %v", err)
	}
	said := speech.calls()
	if len(said) != 1 || !strings.Contains(said[0], "Shownname") || strings.Contains(said[0], "Realname") {
		t.Errorf("speech = %v, want one line naming Shownname and not Realname", said)
	}
}

func TestCancelOrder_RefusesInFlightOrder(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	alice := NewOrder("espresso", "Alice", "", "")
	c.queue.Enqueue(alice)
	c.queue.Start()

	_, err := c.cancelOrder(context.Background(), alice.ID)
	if !errors.Is(err, errOrderInFlight) {
		t.Fatalf("error = %v, want errOrderInFlight", err)
	}
	if c.queue.CurrentID() != alice.ID {
		t.Error("the order on the arm must still be current after a refused cancel")
	}
}

func TestCancelOrder_RejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"non-string", true},
		{"empty", ""},
		{"blank", "   "},
		{"unknown id", "not-a-real-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoffee(t, nil)
			c.queue.Enqueue(NewOrder("espresso", "Alice", "", ""))
			if _, err := c.cancelOrder(context.Background(), tc.v); err == nil {
				t.Fatalf("expected an error for %v, got nil", tc.v)
			}
			if c.queue.Len() != 1 {
				t.Errorf("Len = %d, want the queue untouched", c.queue.Len())
			}
		})
	}
}

// Status marks exactly the orders a client may cancel: everything still
// waiting in the backlog, but not the one on the arm and not the completed
// ones the recent buffer is still showing.
func TestStatus_CancellableFlag(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	done := NewOrder("espresso", "Zoe", "", "")
	alice := NewOrder("espresso", "Alice", "", "")
	bob := NewOrder("lungo", "Bob", "", "")
	c.queue.Enqueue(done)
	c.queue.Start()
	c.queue.Complete()
	c.queue.Enqueue(alice)
	c.queue.Enqueue(bob)
	c.queue.Start() // Alice is now the order on the arm

	resp, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	want := map[string]bool{done.ID: false, alice.ID: false, bob.ID: true}
	orders, _ := resp["orders"].([]any)
	if len(orders) != 3 {
		t.Fatalf("orders length = %d, want 3", len(orders))
	}
	for _, o := range orders {
		m, _ := o.(map[string]any)
		id, _ := m["id"].(string)
		if got, _ := m["cancellable"].(bool); got != want[id] {
			t.Errorf("order %s cancellable = %v, want %v", m["customer_name"], got, want[id])
		}
	}
}
