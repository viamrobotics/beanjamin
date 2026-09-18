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

func TestQueue_SetCurrentStep_UpdatesCurrentOrder(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "hi", "bye")
	q.Enqueue(o)
	q.Start()

	q.SetCurrentStep("Grinding")
	q.SetCurrentStep("Brewing")

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

// TestQueue_SetCurrentStep_IdleQueueIsNoOp pins that a step published while
// nothing is on the arm — a keep-alive purge, say — attaches to no order, and
// in particular does not leak onto the next order waiting in the backlog.
func TestQueue_SetCurrentStep_IdleQueueIsNoOp(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)

	q.SetCurrentStep("Grinding")

	got := q.List()
	if got[0].RawStep != "" {
		t.Errorf("expected RawStep unchanged, got %q", got[0].RawStep)
	}
	if len(got[0].StepHistory) != 0 {
		t.Errorf("expected empty StepHistory, got %d entries", len(got[0].StepHistory))
	}
}

func TestQueue_SetCurrentStep_OnlyAffectsCurrentOrder(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("lungo", "Bob", "", "")
	q.Enqueue(a)
	q.Enqueue(b)
	q.Start() // a goes on the arm, b stays in the backlog

	q.SetCurrentStep("Brewing")

	got := q.List()
	// current renders ahead of the backlog.
	if got[0].ID != a.ID || got[0].RawStep != "Brewing" {
		t.Errorf("expected order A RawStep=Brewing, got %+v", got[0])
	}
	if got[1].ID != b.ID || got[1].RawStep != "" {
		t.Errorf("expected backlog order B untouched, got %+v", got[1])
	}
}

func TestQueue_List_DeepCopiesStepHistory(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)
	q.Start()

	q.SetCurrentStep("Grinding")
	snapshot := q.List()

	// Mutate after taking the snapshot — the snapshot must not see it.
	q.SetCurrentStep("Brewing")

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

func TestQueue_SetCurrentStep_ConcurrentSafety(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)
	q.Start()

	const writers = 8
	const writes = 200
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(label string) {
			defer wg.Done()
			for j := 0; j < writes; j++ {
				q.SetCurrentStep(label)
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

func TestQueue_Complete_MovesCurrentToRecentWithTimestamp(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)
	q.Start()
	q.SetCurrentStep("Brewing")

	q.Complete()

	if got := q.Len(); got != 0 {
		t.Errorf("Len after Complete = %d, want 0 (nothing left to make)", got)
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

// TestQueue_Complete_IdleQueueIsNoOp covers Complete arriving with nothing on
// the arm: the backlog must not be touched, least of all silently retired.
func TestQueue_Complete_IdleQueueIsNoOp(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)

	q.Complete()

	if got := q.Len(); got != 1 {
		t.Errorf("Len = %d, want 1 (Complete on an idle queue is a no-op)", got)
	}
	if got := q.List(); len(got) != 1 || got[0].ID != o.ID {
		t.Errorf("List should still contain the original backlog order")
	}
}

func TestQueue_List_RendersRecentThenCurrentThenBacklog(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("espresso", "Bob", "", "")
	c := NewOrder("espresso", "Carol", "", "")
	d := NewOrder("espresso", "Dave", "", "")
	q.Enqueue(a)
	q.Enqueue(b)
	q.Enqueue(c)
	q.Enqueue(d)

	// Run a then b through the arm. b is most recent → must appear first.
	q.Start()
	q.Complete()
	time.Sleep(2 * time.Millisecond) // ensure distinct CompletedAt
	q.Start()
	q.Complete()
	q.Start() // c is now on the arm, d still in the backlog

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
	q.Start()
	q.Complete()

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

	q.Start()
	q.Complete()

	if got := q.Len(); got != 1 {
		t.Errorf("Len = %d, want 1 (recent excluded)", got)
	}

	// The order on the arm still counts — depth is "drinks still to make".
	q.Start()
	if got := q.Len(); got != 1 {
		t.Errorf("Len with an order on the arm = %d, want 1", got)
	}
}

func TestQueue_Clear_ClearsEveryStage(t *testing.T) {
	q := NewOrderQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("espresso", "Bob", "", "")
	c := NewOrder("espresso", "Carol", "", "")
	q.Enqueue(a)
	q.Enqueue(b)
	q.Enqueue(c)
	q.Start()    // a on the arm
	q.Complete() // a → recent
	q.Start()    // b on the arm, c still in the backlog

	n := q.Clear()
	if n != 3 {
		t.Errorf("Clear returned %d, want 3 (recent + current + backlog)", n)
	}
	if got := q.List(); len(got) != 0 {
		t.Errorf("List after Clear length = %d, want 0", len(got))
	}
	if got := q.Len(); got != 0 {
		t.Errorf("Len after Clear = %d, want 0", got)
	}
	if got := q.CurrentID(); got != "" {
		t.Errorf("CurrentID after Clear = %q, want empty", got)
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
				// Required for the delivery case; harmless for the others.
				"customer_email": "alice@example.com",
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

func TestEnqueueOrder_DeliveryRequiresEmail(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	_, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":         "espresso",
		"customer_name": "Alice",
		"fulfillment":   "delivery",
	})
	if err == nil {
		t.Fatal("expected error for delivery order without customer_email, got nil")
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
	}

	resp, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":          "espresso",
		"customer_name":  "Alice",
		"customer_email": "alice@example.com",
		"fulfillment":    "delivery",
	})
	if err != nil {
		t.Fatalf("unexpected error with email present: %v", err)
	}
	if resp["status"] != "queued" {
		t.Errorf("status = %v, want queued", resp["status"])
	}
	o, ok := c.queue.Peek()
	if !ok || o.Fulfillment != FulfillmentDelivery || o.CustomerEmail != "alice@example.com" {
		t.Errorf("queued order = %+v, want delivery with email", o)
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

func TestEnqueueOrder_IcedLatteGatedByCanServeIcedLatte(t *testing.T) {
	// Rejected on a machine that serves iced coffee but has no milk configured —
	// can_serve_iced alone doesn't buy the fridge trip.
	c, _ := newTestCoffee(t, &Config{CanServeIced: true})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "iced_latte",
	}); err == nil {
		t.Fatal("expected rejection for iced_latte when can_serve_iced_latte=false")
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
	}

	c2, _ := newTestCoffee(t, &Config{CanServeIced: true, CanServeIcedLatte: true})
	if _, err := c2.enqueueOrder(context.Background(), map[string]any{
		"drink": "iced_latte",
	}); err != nil {
		t.Fatalf("unexpected error enqueuing iced_latte with can_serve_iced_latte=true: %v", err)
	}
	if c2.queue.Len() != 1 {
		t.Errorf("queue length = %d, want 1", c2.queue.Len())
	}
}

// TestQueue_SetCurrentStep_AfterCompleteIsDropped pins the narrowing that came
// with the current slot. A step published once the order has been retired
// attaches to nothing: there is no order on the arm to attribute it to, and
// reaching back into recent would let a straggling goroutine rewrite the step
// label on a drink already sitting on the shelf.
func TestQueue_SetCurrentStep_AfterCompleteIsDropped(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "", "")
	q.Enqueue(o)
	q.Start()
	q.SetCurrentStep("Brewing")
	q.Complete()

	q.SetCurrentStep("AfterComplete")

	got := q.List()
	if got[0].RawStep != "Brewing" {
		t.Errorf("RawStep on the retired order = %q, want Brewing (the step it finished on)", got[0].RawStep)
	}
}

// TestQueue_Enqueue_PositionCountsTheOrderOnTheArm guards the spoken
// acknowledgement: enqueueOrder says "Order received" only above position 1, so
// an order placed while a drink is being made has to come back as 2. Counting
// the backlog alone would return 1 and leave that customer unacknowledged.
func TestQueue_Enqueue_PositionCountsTheOrderOnTheArm(t *testing.T) {
	q := NewOrderQueue()

	if pos := q.Enqueue(NewOrder("espresso", "Ada", "", "")); pos != 1 {
		t.Errorf("first order into an idle queue = position %d, want 1", pos)
	}
	q.Start() // Ada's drink goes on the arm, backlog is empty again

	if pos := q.Enqueue(NewOrder("espresso", "Bob", "", "")); pos != 2 {
		t.Errorf("order placed mid-brew = position %d, want 2", pos)
	}
	if pos := q.Enqueue(NewOrder("espresso", "Cleo", "", "")); pos != 3 {
		t.Errorf("order placed behind one already waiting = position %d, want 3", pos)
	}
}

// TestQueue_ClearPending_SparesCurrentAndRecent pins the clear_queue contract:
// it drops the backlog only, leaving the order on the arm and the
// recently-completed buffer untouched.
func TestQueue_ClearPending_SparesCurrentAndRecent(t *testing.T) {
	q := NewOrderQueue()
	done := NewOrder("espresso", "Ada", "", "")
	current := NewOrder("espresso", "Bob", "", "")
	waiting := NewOrder("lungo", "Cleo", "", "")
	extra := NewOrder("espresso", "Dev", "", "")
	for _, o := range []Order{done, current, waiting, extra} {
		q.Enqueue(o)
	}
	q.Start()    // done on the arm
	q.Complete() // done → recent
	q.Start()    // current on the arm

	removed, currentID := q.ClearPending()
	if removed != 2 || currentID != current.ID {
		t.Errorf("ClearPending = (%d, %q), want (2, %q)", removed, currentID, current.ID)
	}
	if got := q.Len(); got != 1 {
		t.Fatalf("Len after ClearPending = %d, want 1 (the order on the arm)", got)
	}
	if _, ok := q.Peek(); ok {
		t.Error("backlog should be empty after ClearPending")
	}

	// Both the order on the arm and the completed one must still be visible to
	// the webapp, which polls List() through get_queue.
	list := q.List()
	if len(list) != 2 {
		t.Fatalf("List after ClearPending = %d orders, want 2", len(list))
	}
	if list[0].ID != done.ID || list[1].ID != current.ID {
		t.Errorf("List = [%s %s], want [%s %s] (recent first, then the arm)",
			list[0].ID, list[1].ID, done.ID, current.ID)
	}

	// The spared order must still take step updates and still be completable.
	q.SetCurrentStep(stepBrewing)
	q.Complete()
	if got := q.Len(); got != 0 {
		t.Errorf("Len after completing the spared order = %d, want 0", got)
	}
	for _, o := range q.List() {
		if o.ID != current.ID {
			continue
		}
		if o.RawStep != stepBrewing {
			t.Errorf("spared order raw_step = %q, want %q", o.RawStep, stepBrewing)
		}
		if o.CompletedAt.IsZero() {
			t.Error("spared order should have completed_at set, so the UI renders its Ready! card")
		}
		return
	}
	t.Errorf("spared order %s missing from List after Complete", current.ID)
}

// TestQueue_ClearPending_IdleQueue covers the no-order-running case, where
// there is nothing to spare and the whole backlog goes.
func TestQueue_ClearPending_IdleQueue(t *testing.T) {
	q := NewOrderQueue()
	q.Enqueue(NewOrder("espresso", "Ada", "", ""))
	q.Enqueue(NewOrder("espresso", "Bob", "", ""))

	removed, currentID := q.ClearPending()
	if removed != 2 || currentID != "" {
		t.Errorf("ClearPending on an idle queue = (%d, %q), want (2, \"\")", removed, currentID)
	}
	if got := q.Len(); got != 0 {
		t.Errorf("Len after ClearPending = %d, want 0", got)
	}
}

func TestOrderDisplayName(t *testing.T) {
	tests := []struct {
		name  string
		order Order
		want  string
	}{
		{
			name:  "the misspelling is what the customer sees and hears",
			order: Order{CustomerName: "Vijay", ModifiedCustomerName: "Vijoy"},
			want:  "Vijoy",
		},
		{
			// Voice and operator orders never misspell, so there is nothing to
			// show but the name they gave.
			name:  "falls back to the real name when nothing misspelled it",
			order: Order{CustomerName: "Ada"},
			want:  "Ada",
		},
		{
			name:  "anonymous order has nothing to show",
			order: Order{},
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.order.DisplayName(); got != tc.want {
				t.Errorf("DisplayName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnqueueOrder_CarriesBothNames(t *testing.T) {
	tests := []struct {
		name         string
		payload      map[string]any
		wantName     string
		wantModified string
	}{
		{
			name: "kiosk sends the real name and the misspelling it displayed",
			payload: map[string]any{
				"drink":                  "espresso",
				"customer_name":          "Vijay",
				"modified_customer_name": "Vijoy",
			},
			wantName:     "Vijay",
			wantModified: "Vijoy",
		},
		{
			// customer_name is the aggregation key now, so stray whitespace must
			// not hand one customer a second identity.
			name: "surrounding whitespace is trimmed off the tracking name",
			payload: map[string]any{
				"drink":                  "espresso",
				"customer_name":          "  Vijay  ",
				"modified_customer_name": "Vijoy",
			},
			wantName:     "Vijay",
			wantModified: "Vijoy",
		},
		{
			name: "caller that never misspells sends only the real name",
			payload: map[string]any{
				"drink":         "espresso",
				"customer_name": "Ada",
			},
			wantName:     "Ada",
			wantModified: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoffee(t, nil)
			if _, err := c.enqueueOrder(context.Background(), tc.payload); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			orders := c.queue.List()
			if len(orders) != 1 {
				t.Fatalf("queue length = %d, want 1", len(orders))
			}
			if got := orders[0].CustomerName; got != tc.wantName {
				t.Errorf("CustomerName = %q, want %q", got, tc.wantName)
			}
			if got := orders[0].ModifiedCustomerName; got != tc.wantModified {
				t.Errorf("ModifiedCustomerName = %q, want %q", got, tc.wantModified)
			}
		})
	}
}
