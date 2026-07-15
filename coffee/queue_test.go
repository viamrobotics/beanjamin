package coffee

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

func TestQueue_SetStep_UpdatesOrder(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "hi", "bye")
	q.Enqueue(o)

	q.SetStep(o.ID, "Grinding")
	q.SetStep(o.ID, "Brewing")

	got := q.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 order in list, got %d", len(got))
	}
	if got[0].RawStep != "Brewing" {
		t.Errorf("RawStep = %q, want %q", got[0].RawStep, "Brewing")
	}
	if len(got[0].StepHistory) != 2 {
		t.Fatalf("StepHistory length = %d, want 2", len(got[0].StepHistory))
	}
	if got[0].StepHistory[0].Step != "Grinding" {
		t.Errorf("history[0] = %q, want Grinding", got[0].StepHistory[0].Step)
	}
	if got[0].StepHistory[1].Step != "Brewing" {
		t.Errorf("history[1] = %q, want Brewing", got[0].StepHistory[1].Step)
	}
	if got[0].StepHistory[0].StartedAt.IsZero() {
		t.Error("StartedAt should not be zero")
	}
}

func TestQueue_SetStep_UnknownIDIsNoOp(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)

	q.SetStep("not-a-real-id", "Grinding")

	got := q.List()
	if got[0].RawStep != "" {
		t.Errorf("expected RawStep unchanged, got %q", got[0].RawStep)
	}
	if len(got[0].StepHistory) != 0 {
		t.Errorf("expected empty StepHistory, got %d entries", len(got[0].StepHistory))
	}
}

func TestQueue_SetStep_OnlyAffectsMatchingOrder(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("lungo", "Bob", "", "")
	q.Enqueue(a)
	q.Enqueue(b)

	q.SetStep(b.ID, "Brewing")

	got := q.List()
	// Both pending; FIFO order
	if got[0].ID != a.ID || got[0].RawStep != "" {
		t.Errorf("expected order A untouched, got %+v", got[0])
	}
	if got[1].ID != b.ID || got[1].RawStep != "Brewing" {
		t.Errorf("expected order B RawStep=Brewing, got %+v", got[1])
	}
}

func TestQueue_List_DeepCopiesStepHistory(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)

	q.SetStep(o.ID, "Grinding")
	snapshot := q.List()

	// Mutate after taking the snapshot — the snapshot must not see it.
	q.SetStep(o.ID, "Brewing")

	if len(snapshot[0].StepHistory) != 1 {
		t.Errorf("snapshot StepHistory length = %d, want 1 (snapshot must be deep-copied)",
			len(snapshot[0].StepHistory))
	}
	if snapshot[0].StepHistory[0].Step != "Grinding" {
		t.Errorf("snapshot history[0] = %q, want Grinding",
			snapshot[0].StepHistory[0].Step)
	}

	current := q.List()
	if len(current[0].StepHistory) != 2 {
		t.Errorf("current StepHistory length = %d, want 2", len(current[0].StepHistory))
	}
}

func TestQueue_SetStep_ConcurrentSafety(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)

	const writers = 8
	const writes = 200
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(label string) {
			defer wg.Done()
			for j := 0; j < writes; j++ {
				q.SetStep(o.ID, label)
				_ = q.List()
			}
		}([]string{"Grinding", "Tamping", "Brewing", "Serving"}[i%4])
	}
	wg.Wait()

	got := q.List()
	if total := len(got[0].StepHistory); total != writers*writes {
		t.Errorf("expected %d history entries, got %d", writers*writes, total)
	}
}

func TestQueue_Complete_MovesPendingToRecentWithTimestamp(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)
	q.SetStep(o.ID, "Brewing")

	q.Complete(o.ID)

	if got := q.Len(); got != 0 {
		t.Errorf("Len after Complete = %d, want 0 (pending only)", got)
	}
	got := q.List()
	if len(got) != 1 {
		t.Fatalf("List after Complete length = %d, want 1", len(got))
	}
	if got[0].ID != o.ID {
		t.Errorf("ID = %q, want %q", got[0].ID, o.ID)
	}
	if got[0].CompletedAt.IsZero() {
		t.Error("CompletedAt should be set after Complete")
	}
	if got[0].RawStep != "Brewing" {
		t.Errorf("RawStep should be preserved through Complete, got %q", got[0].RawStep)
	}
}

func TestQueue_Complete_UnknownIDIsNoOp(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)

	q.Complete("does-not-exist")

	if got := q.Len(); got != 1 {
		t.Errorf("Len = %d, want 1 (Complete with unknown id is no-op)", got)
	}
	if got := q.List(); len(got) != 1 || got[0].ID != o.ID {
		t.Errorf("List should still contain the original pending order")
	}
}

func TestQueue_List_OrdersRecentFirstThenPending(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("espresso", "Bob", "", "")
	c := NewOrder("espresso", "Carol", "", "")
	d := NewOrder("espresso", "Dave", "", "")
	q.Enqueue(a)
	q.Enqueue(b)
	q.Enqueue(c)
	q.Enqueue(d)

	// Complete a then b. b is most recent → must appear first in List.
	q.Complete(a.ID)
	time.Sleep(2 * time.Millisecond) // ensure distinct CompletedAt
	q.Complete(b.ID)

	got := q.List()
	if len(got) != 4 {
		t.Fatalf("List length = %d, want 4", len(got))
	}
	want := []string{b.ID, a.ID, c.ID, d.ID}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("List[%d] = %s, want %s", i, got[i].ID, id)
		}
	}
}

func TestQueue_List_PrunesExpiredRecent(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)
	q.Complete(o.ID)

	// Force the recent entry to look expired by stomping CompletedAt directly.
	q.mu.Lock()
	q.recent[0].CompletedAt = time.Now().Add(-2 * RecentDisplayDuration)
	q.mu.Unlock()

	got := q.List()
	if len(got) != 0 {
		t.Errorf("expected expired recent to be pruned, got %d entries", len(got))
	}
}

func TestQueue_Len_ExcludesRecent(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("espresso", "Bob", "", "")
	q.Enqueue(a)
	q.Enqueue(b)

	q.Complete(a.ID)

	if got := q.Len(); got != 1 {
		t.Errorf("Len = %d, want 1 (recent excluded)", got)
	}
}

func TestQueue_Clear_ClearsBothPendingAndRecent(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("espresso", "Bob", "", "")
	q.Enqueue(a)
	q.Enqueue(b)
	q.Complete(a.ID)

	n := q.Clear()
	if n != 2 {
		t.Errorf("Clear returned %d, want 2", n)
	}
	if got := q.List(); len(got) != 0 {
		t.Errorf("List after Clear length = %d, want 0", len(got))
	}
	if got := q.Len(); got != 0 {
		t.Errorf("Len after Clear = %d, want 0", got)
	}
}

// fakeSpeech records say_async calls dispatched through s.say so tests can
// assert how many announcements enqueueOrder produced.
type fakeSpeech struct {
	resource.AlwaysRebuild
	mu   sync.Mutex
	said []string
}

func (f *fakeSpeech) Name() resource.Name { return resource.Name{} }
func (f *fakeSpeech) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	if t, ok := cmd["say_async"].(string); ok {
		f.mu.Lock()
		f.said = append(f.said, t)
		f.mu.Unlock()
	}
	return map[string]any{}, nil
}
func (f *fakeSpeech) Close(_ context.Context) error { return nil }
func (f *fakeSpeech) Status(_ context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}
func (f *fakeSpeech) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.said))
	copy(out, f.said)
	return out
}

// newTestCoffee builds a minimal *beanjaminCoffee suitable for exercising
// enqueueOrder. Real motion/arm wiring is intentionally absent — only the
// fields enqueueOrder touches are populated.
func newTestCoffee(t *testing.T, cfg *Config) (*beanjaminCoffee, *fakeSpeech) {
	t.Helper()
	if cfg == nil {
		cfg = &Config{CanServeDecaf: true}
	}
	speech := &fakeSpeech{}
	return &beanjaminCoffee{
		logger: logging.NewTestLogger(t),
		cfg:    cfg,
		queue:  NewOrderQueue(),
		speech: speech,
	}, speech
}

func TestEnqueueOrder_DefaultsCountToOne(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	resp, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":         "espresso",
		"customer_name": "Alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := resp["count"].(int); got != 1 {
		t.Errorf("count = %v, want 1", resp["count"])
	}
	ids, _ := resp["order_ids"].([]string)
	if len(ids) != 1 {
		t.Fatalf("order_ids length = %d, want 1", len(ids))
	}
	if id, _ := resp["order_id"].(string); id != ids[0] {
		t.Errorf("order_id %q must match order_ids[0] %q", id, ids[0])
	}
	if c.queue.Len() != 1 {
		t.Errorf("queue length = %d, want 1", c.queue.Len())
	}
}

func TestEnqueueOrder_BatchEnqueuesN(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	resp, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":         "espresso",
		"customer_name": "Esha",
		"count":         float64(3),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := resp["count"].(int); got != 3 {
		t.Errorf("count = %v, want 3", resp["count"])
	}
	ids, _ := resp["order_ids"].([]string)
	if len(ids) != 3 {
		t.Fatalf("order_ids length = %d, want 3", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			t.Errorf("empty UUID in order_ids")
		}
		if seen[id] {
			t.Errorf("duplicate UUID %q in order_ids", id)
		}
		seen[id] = true
	}
	if c.queue.Len() != 3 {
		t.Errorf("queue length = %d, want 3", c.queue.Len())
	}
	if pos, _ := resp["queue_position"].(int); pos != 1 {
		t.Errorf("queue_position = %v, want 1 (first order)", resp["queue_position"])
	}
}

func TestEnqueueOrder_Fulfillment(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"absent defaults to pickup", nil, FulfillmentPickup},
		{"empty defaults to pickup", "", FulfillmentPickup},
		{"pickup", "pickup", FulfillmentPickup},
		{"delivery", "delivery", FulfillmentDelivery},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoffee(t, nil)
			payload := map[string]any{
				"drink":         "espresso",
				"customer_name": "Alice",
			}
			if tc.v != nil {
				payload["fulfillment"] = tc.v
			}
			if _, err := c.enqueueOrder(context.Background(), payload); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			o, ok := c.queue.Peek()
			if !ok {
				t.Fatal("queue is empty")
			}
			if o.Fulfillment != tc.want {
				t.Errorf("Fulfillment = %q, want %q", o.Fulfillment, tc.want)
			}
		})
	}
}

func TestEnqueueOrder_RejectsBadFulfillment(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"unknown value", "drone"},
		{"non-string", float64(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoffee(t, nil)
			_, err := c.enqueueOrder(context.Background(), map[string]any{
				"drink":       "espresso",
				"fulfillment": tc.v,
			})
			if err == nil {
				t.Fatalf("expected error for fulfillment=%v, got nil", tc.v)
			}
			if c.queue.Len() != 0 {
				t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
			}
		})
	}
}

func TestEnqueueOrder_RejectsBadCount(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"zero", float64(0)},
		{"negative", float64(-1)},
		{"fractional", 1.5},
		{"string", "5"},
		{"nan", math.NaN()},
		{"inf", math.Inf(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoffee(t, nil)
			_, err := c.enqueueOrder(context.Background(), map[string]any{
				"drink": "espresso",
				"count": tc.v,
			})
			if err == nil {
				t.Fatalf("expected error for count=%v, got nil", tc.v)
			}
			if c.queue.Len() != 0 {
				t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
			}
		})
	}
}

func TestEnqueueOrder_RejectsAboveCap(t *testing.T) {
	c, _ := newTestCoffee(t, &Config{CanServeDecaf: true, MaxBatchSize: 5})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "espresso",
		"count": float64(5),
	}); err != nil {
		t.Fatalf("count=cap should be allowed; got error: %v", err)
	}
	if c.queue.Len() != 5 {
		t.Errorf("queue length after at-cap batch = %d, want 5", c.queue.Len())
	}

	c2, _ := newTestCoffee(t, &Config{CanServeDecaf: true, MaxBatchSize: 5})
	if _, err := c2.enqueueOrder(context.Background(), map[string]any{
		"drink": "espresso",
		"count": float64(6),
	}); err == nil {
		t.Fatal("count=cap+1 should be rejected; got nil error")
	}
	if c2.queue.Len() != 0 {
		t.Errorf("queue should stay empty after over-cap rejection, got len=%d", c2.queue.Len())
	}
}

func TestEnqueueOrder_BatchSuppressesPerOrderAnnouncement(t *testing.T) {
	c, sp := newTestCoffee(t, &Config{CanServeDecaf: true, Conversational: true})
	// Pre-populate the queue so the single-order path would normally fire
	// pickOrderReceived (pos > 1). The batch path should fire exactly one
	// pickOrderReceivedBatch line instead.
	c.queue.Enqueue(NewOrder("lungo", "Bob", "", ""))

	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":         "espresso",
		"customer_name": "Esha",
		"count":         float64(5),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := sp.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 speech call for batch, got %d: %v", len(calls), calls)
	}
	// The consolidated batch line mentions the count and the (plural) drink.
	if !strings.Contains(calls[0], "5") || !strings.Contains(calls[0], "espressos") {
		t.Errorf("batch line %q should reference count=5 and 'espressos'", calls[0])
	}
}

func TestEnqueueOrder_RejectsBatchOfUnsupportedDrink(t *testing.T) {
	c, _ := newTestCoffee(t, &Config{CanServeDecaf: false})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "decaf",
		"count": float64(3),
	}); err == nil {
		t.Fatal("expected rejection for decaf when can_serve_decaf=false")
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after pre-loop rejection, got len=%d", c.queue.Len())
	}
}

func TestEnqueueOrder_IcedGatedByCanServeIced(t *testing.T) {
	// Rejected when can_serve_iced is off.
	c, _ := newTestCoffee(t, &Config{})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "iced_coffee",
	}); err == nil {
		t.Fatal("expected rejection for iced_coffee when can_serve_iced=false")
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
	}

	// Accepted when can_serve_iced is on.
	c2, _ := newTestCoffee(t, &Config{CanServeIced: true})
	if _, err := c2.enqueueOrder(context.Background(), map[string]any{
		"drink": "iced_coffee",
	}); err != nil {
		t.Fatalf("unexpected error enqueuing iced_coffee with can_serve_iced=true: %v", err)
	}
	if c2.queue.Len() != 1 {
		t.Errorf("queue length = %d, want 1", c2.queue.Len())
	}
}

func TestQueue_SetStep_FindsRecentOrder(t *testing.T) {
	// SetStep called after Complete (e.g. straggling step from a goroutine)
	// should still be attributed to the order if it's still in recent.
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)
	q.Complete(o.ID)

	q.SetStep(o.ID, "AfterComplete")

	got := q.List()
	if got[0].RawStep != "AfterComplete" {
		t.Errorf("RawStep on recent order = %q, want AfterComplete", got[0].RawStep)
	}
}
