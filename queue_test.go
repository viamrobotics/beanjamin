package beanjamin

import (
	"sync"
	"testing"
	"time"
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
