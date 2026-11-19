package coffee

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.viam.com/rdk/logging"

	"beanjamin/coffee/order"
)

func storeWith(t *testing.T, entries ...drinkDuration) *drinkDurationStore {
	t.Helper()
	t.Setenv("VIAM_MODULE_DATA", "")
	d := newDrinkDurationStore(logging.NewTestLogger(t))
	for _, e := range entries {
		d.record(logging.NewTestLogger(t), e.Drink, secondsToDuration(e.Seconds))
	}
	return d
}

func TestExpectedWaitWithoutHistory(t *testing.T) {
	d := storeWith(t)
	if _, ok := d.expectedWait([]order.Order{{ID: "a", Drink: "espresso"}}, "", time.Now()); ok {
		t.Fatal("expected no estimate from an empty window")
	}
	var nilStore *drinkDurationStore
	if _, ok := nilStore.expectedWait(nil, "", time.Now()); ok {
		t.Fatal("expected no estimate from a nil store")
	}
}

func TestExpectedWaitSumsOrdersAhead(t *testing.T) {
	d := storeWith(t,
		drinkDuration{Drink: "espresso", Seconds: 100},
		drinkDuration{Drink: "espresso", Seconds: 140},
		drinkDuration{Drink: "iced_latte", Seconds: 300},
	)
	now := time.Now()
	orders := []order.Order{
		{ID: "done", Drink: "iced_latte", CompletedAt: now.Add(-time.Second)},
		{ID: "cur", Drink: "espresso", StartedAt: now.Add(-20 * time.Second)},
		{ID: "p1", Drink: "iced_latte"},
		// No lungo made yet: falls back to the overall average, 180s.
		{ID: "p2", Drink: "lungo"},
	}
	wait, ok := d.expectedWait(orders, "cur", now)
	if !ok {
		t.Fatal("expected an estimate")
	}
	// espresso avg 120 - 20 elapsed + 300 + 180.
	if want := 580 * time.Second; wait != want {
		t.Errorf("wait = %v, want %v", wait, want)
	}
}

func TestExpectedWaitOverrunningCurrentCountsAsDone(t *testing.T) {
	d := storeWith(t, drinkDuration{Drink: "espresso", Seconds: 60})
	now := time.Now()
	orders := []order.Order{{ID: "cur", Drink: "espresso", StartedAt: now.Add(-5 * time.Minute)}}
	wait, ok := d.expectedWait(orders, "cur", now)
	if !ok || wait != 0 {
		t.Errorf("wait = %v, ok = %v; want 0, true", wait, ok)
	}
}

func TestDrinkDurationWindowKeepsLatest(t *testing.T) {
	d := storeWith(t)
	logger := logging.NewTestLogger(t)
	for range drinkDurationWindow {
		d.record(logger, "espresso", 10*time.Second)
	}
	for range drinkDurationWindow {
		d.record(logger, "espresso", 30*time.Second)
	}
	perDrink, _, _ := d.estimates()
	if got := perDrink["espresso"]; got != 30*time.Second {
		t.Errorf("espresso estimate = %v, want 30s (only the latest %d espressos)", got, drinkDurationWindow)
	}
}

func TestDrinkDurationWindowIsPerDrink(t *testing.T) {
	d := storeWith(t)
	logger := logging.NewTestLogger(t)
	d.record(logger, "iced_latte", 300*time.Second)
	// Many espressos after it must not push the one iced latte out.
	for range 5 * drinkDurationWindow {
		d.record(logger, "espresso", 60*time.Second)
	}
	perDrink, _, _ := d.estimates()
	if got := perDrink["iced_latte"]; got != 300*time.Second {
		t.Errorf("iced_latte estimate = %v, want 300s", got)
	}
	if got := len(d.entries); got != drinkDurationWindow+1 {
		t.Errorf("stored %d entries, want %d", got, drinkDurationWindow+1)
	}
}

func TestDrinkDurationsPersistAcrossRebuilds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)
	logger := logging.NewTestLogger(t)
	newDrinkDurationStore(logger).record(logger, "lungo", 90*time.Second)

	if _, err := os.Stat(filepath.Join(dir, drinkDurationsFile)); err != nil {
		t.Fatalf("durations not persisted: %v", err)
	}
	perDrink, _, ok := newDrinkDurationStore(logger).estimates()
	if !ok || perDrink["lungo"] != 90*time.Second {
		t.Errorf("reloaded lungo estimate = %v (ok=%v), want 90s", perDrink["lungo"], ok)
	}
}

func TestStatusReportsExpectedWait(t *testing.T) {
	s := newStatusService(t, &Config{})
	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, present := st["expected_wait_sec"]; present {
		t.Error("expected_wait_sec should be absent without history")
	}

	s.drinkDurations = storeWith(t, drinkDuration{Drink: "espresso", Seconds: 75})
	s.queue.Enqueue(order.NewOrder("espresso", "Ada", "", ""))
	s.queue.Enqueue(order.NewOrder("espresso", "Bob", "", ""))
	st, err = s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := st["expected_wait_sec"]; got != 150.0 {
		t.Errorf("expected_wait_sec = %v, want 150", got)
	}
}
