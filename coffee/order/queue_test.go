package order

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestQueue_SetCurrentStep_UpdatesCurrentOrder(t *testing.T) {
	q := NewQueue()
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
	q := NewQueue()
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
	q := NewQueue()
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
	q := NewQueue()
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
	q := NewQueue()
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
	q := NewQueue()
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
	q := NewQueue()
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
	q := NewQueue()
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
	q := NewQueue()
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
	q := NewQueue()
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
	q := NewQueue()
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

// TestQueue_SetCurrentStep_AfterCompleteIsDropped pins the narrowing that came
// with the current slot. A step published once the order has been retired
// attaches to nothing: there is no order on the arm to attribute it to, and
// reaching back into recent would let a straggling goroutine rewrite the step
// label on a drink already sitting on the shelf.
func TestQueue_SetCurrentStep_AfterCompleteIsDropped(t *testing.T) {
	q := NewQueue()
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
	q := NewQueue()

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
	q := NewQueue()
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
	if got := q.CurrentID(); got != current.ID {
		t.Errorf("CurrentID after ClearPending = %q, want %q (the only order left is on the arm)", got, current.ID)
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
	q.SetCurrentStep("Brewing")
	q.Complete()
	if got := q.Len(); got != 0 {
		t.Errorf("Len after completing the spared order = %d, want 0", got)
	}
	for _, o := range q.List() {
		if o.ID != current.ID {
			continue
		}
		if o.RawStep != "Brewing" {
			t.Errorf("spared order raw_step = %q, want %q", o.RawStep, "Brewing")
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
	q := NewQueue()
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

func TestQueue_Remove_DropsWaitingOrderAndKeepsFIFO(t *testing.T) {
	q := NewQueue()
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
	q := NewQueue()
	a := NewOrder("espresso", "Alice", "", "")
	b := NewOrder("lungo", "Bob", "", "")
	q.Enqueue(a)
	q.Enqueue(b)

	started, ok := q.Start()
	if !ok || started.ID != a.ID {
		t.Fatalf("Start = %+v, %v; want Alice's order", started, ok)
	}

	if _, err := q.Remove(a.ID); !errors.Is(err, ErrInFlight) {
		t.Errorf("Remove(current) error = %v, want ErrInFlight", err)
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
	q := NewQueue()
	a := NewOrder("espresso", "Alice", "", "")
	q.Enqueue(a)
	q.Start()
	q.Complete()

	if _, err := q.Remove(a.ID); !errors.Is(err, ErrCompleted) {
		t.Errorf("Remove(completed) error = %v, want ErrCompleted", err)
	}
}

func TestQueue_Remove_UnknownAndEmptyID(t *testing.T) {
	q := NewQueue()
	q.Enqueue(NewOrder("espresso", "Alice", "", ""))

	if _, err := q.Remove("not-a-real-id"); !errors.Is(err, ErrNotQueued) {
		t.Errorf("Remove(unknown) error = %v, want ErrNotQueued", err)
	}
	// Every order carries a UUID, so an empty ID is a miss like any other
	// rather than something that matches an empty slot.
	if _, err := q.Remove(""); !errors.Is(err, ErrNotQueued) {
		t.Errorf(`Remove("") error = %v, want ErrNotQueued`, err)
	}
	if q.Len() != 1 {
		t.Errorf("Len = %d, want 1", q.Len())
	}
}
