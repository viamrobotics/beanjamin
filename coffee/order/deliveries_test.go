package order

import (
	"testing"
	"time"
)

// testClock is a hand-advanced clock for the ledger's TTL.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newTestDeliveries() (*Deliveries, *testClock) {
	c := &testClock{t: time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)}
	return &Deliveries{now: c.now}, c
}

func ids(ps []PendingDelivery) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.OrderID
	}
	return out
}

func TestDeliveries_RecordAndListOldestFirst(t *testing.T) {
	d, clock := newTestDeliveries()
	d.Record(PendingDelivery{OrderID: "a", Drink: "espresso", PickupPosition: 0})
	clock.advance(time.Minute)
	d.Record(PendingDelivery{OrderID: "b", Drink: "iced_coffee", PickupPosition: 1})

	got := d.List()
	if len(got) != 2 || got[0].OrderID != "a" || got[1].OrderID != "b" {
		t.Fatalf("List = %v, want [a b]", ids(got))
	}
	// Record stamps ServedAt from the ledger's clock.
	if !got[1].ServedAt.Equal(clock.t) {
		t.Errorf("ServedAt = %v, want %v", got[1].ServedAt, clock.t)
	}
	if got[0].Drink != "espresso" || got[1].PickupPosition != 1 {
		t.Errorf("entries lost their fields: %+v", got)
	}
}

func TestDeliveries_RecordSameOrderReplaces(t *testing.T) {
	d, _ := newTestDeliveries()
	d.Record(PendingDelivery{OrderID: "a", PickupPosition: 0})
	d.Record(PendingDelivery{OrderID: "a", PickupPosition: 2})
	got := d.List()
	if len(got) != 1 || got[0].PickupPosition != 2 {
		t.Fatalf("List = %+v, want one entry at slot 2", got)
	}
}

func TestDeliveries_ListIsACopy(t *testing.T) {
	d, _ := newTestDeliveries()
	d.Record(PendingDelivery{OrderID: "a"})
	d.List()[0].OrderID = "mutated"
	if got := d.List(); got[0].OrderID != "a" {
		t.Errorf("List exposed the ledger's own slice: %v", ids(got))
	}
}

func TestDeliveries_Acknowledge(t *testing.T) {
	d, _ := newTestDeliveries()
	d.Record(PendingDelivery{OrderID: "a"})
	if got := d.List(); got[0].Acknowledged {
		t.Fatal("a fresh entry should not be acknowledged")
	}
	if !d.Acknowledge("a") {
		t.Fatal("Acknowledge(a) = false, want true")
	}
	if got := d.List(); !got[0].Acknowledged {
		t.Error("entry should be acknowledged after Acknowledge")
	}
	if d.Acknowledge("missing") {
		t.Error("Acknowledge of an unknown order should report false")
	}
}

func TestDeliveries_ClearSlotEvictsOnlyThatSlot(t *testing.T) {
	d, _ := newTestDeliveries()
	d.Record(PendingDelivery{OrderID: "a", PickupPosition: 0})
	d.Record(PendingDelivery{OrderID: "b", PickupPosition: 1})
	d.Record(PendingDelivery{OrderID: "c", PickupPosition: 2})

	dropped := d.ClearSlot(1)
	if len(dropped) != 1 || dropped[0].OrderID != "b" {
		t.Fatalf("ClearSlot(1) dropped %v, want [b]", ids(dropped))
	}
	if got := ids(d.List()); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("List after ClearSlot(1) = %v, want [a c]", got)
	}
	if dropped := d.ClearSlot(5); len(dropped) != 0 {
		t.Errorf("ClearSlot on an empty slot dropped %v, want nothing", ids(dropped))
	}
}

func TestDeliveries_TTLPrune(t *testing.T) {
	d, clock := newTestDeliveries()
	d.Record(PendingDelivery{OrderID: "old"})
	clock.advance(PendingDeliveryTTL - time.Minute)
	d.Record(PendingDelivery{OrderID: "new"})

	if got := ids(d.List()); len(got) != 2 {
		t.Fatalf("List before expiry = %v, want both", got)
	}
	clock.advance(time.Minute)
	if got := ids(d.List()); len(got) != 1 || got[0] != "new" {
		t.Errorf("List at the TTL = %v, want only [new]", got)
	}
	// Expired entries are gone for writes too, not just hidden from List.
	clock.advance(PendingDeliveryTTL)
	if d.Collect("new") {
		t.Error("Collect of an expired entry should report false")
	}
}

func TestDeliveries_CollectIsIdempotent(t *testing.T) {
	d, _ := newTestDeliveries()
	d.Record(PendingDelivery{OrderID: "a"})
	d.Record(PendingDelivery{OrderID: "b"})

	if !d.Collect("a") {
		t.Fatal("Collect(a) = false, want true")
	}
	if d.Collect("a") {
		t.Error("second Collect(a) = true, want false")
	}
	if d.Collect("never-served") {
		t.Error("Collect of an unknown order = true, want false")
	}
	if got := ids(d.List()); len(got) != 1 || got[0] != "b" {
		t.Errorf("List after collecting a = %v, want [b]", got)
	}
}

func TestDeliveries_Clear(t *testing.T) {
	d, _ := newTestDeliveries()
	d.Record(PendingDelivery{OrderID: "a"})
	d.Record(PendingDelivery{OrderID: "b"})
	if n := d.Clear(); n != 2 {
		t.Errorf("Clear = %d, want 2", n)
	}
	if got := d.List(); len(got) != 0 {
		t.Errorf("List after Clear = %v, want empty", ids(got))
	}
}

func TestDeliveries_ZeroValueUsable(t *testing.T) {
	var d Deliveries
	d.Record(PendingDelivery{OrderID: "a"})
	got := d.List()
	if len(got) != 1 || got[0].ServedAt.IsZero() {
		t.Fatalf("zero-value ledger List = %+v, want one stamped entry", got)
	}
}
