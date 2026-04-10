package beanjamin

import (
	"sync"
	"testing"
)

func TestQueue_SetStep_UpdatesOrder(t *testing.T) {
	q := NewOrderQueue()
	o := NewOrder("espresso", "Ale", "hi", "bye")
	q.Enqueue(o)

	q.SetStep(o.ID, "Grinding")
	q.SetStep(o.ID, "Brewing")

	got := q.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 order in queue, got %d", len(got))
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
	if got[0].RawStep != "" {
		t.Errorf("expected order A untouched, got RawStep=%q", got[0].RawStep)
	}
	if got[1].RawStep != "Brewing" {
		t.Errorf("expected order B RawStep=%q, got %q", "Brewing", got[1].RawStep)
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

	// And the queue itself should reflect the latest mutation.
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
