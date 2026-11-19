package coffee

// Expected wait: how long the drinks already queued will keep the arm busy,
// estimated from how long recent successful drinks of each kind took.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.viam.com/rdk/logging"

	"beanjamin/coffee/order"
)

const (
	drinkDurationsFile = "drink-durations.json"
	// drinkDurationWindow is how many of each drink's latest successful orders
	// its estimate averages over. Kept per drink so a newly added or rarely
	// ordered drink still has its own history instead of being crowded out by
	// the popular ones; small enough to follow a change in the routine quickly.
	drinkDurationWindow = 10
)

// drinkDuration is how long one successful order held the arm, from the queue
// handing it over to the drink being served and the routine cleaned up.
type drinkDuration struct {
	Drink   string  `json:"drink"`
	Seconds float64 `json:"seconds"`
}

// drinkDurationStore keeps the durations of the last drinkDurationWindow
// successful orders of each drink, oldest first. Mirrored to VIAM_MODULE_DATA because the service is
// AlwaysRebuild: without persisting, every reconfigure would forget the
// estimates and the kiosk would show no wait time until new drinks were made.
type drinkDurationStore struct {
	mu      sync.Mutex
	entries []drinkDuration
	// path is "" when VIAM_MODULE_DATA is unset (tests, a local run): in-memory only.
	path string
}

// newDrinkDurationStore loads any persisted durations. A read failure starts
// from an empty window — the wait time is a courtesy, not worth failing over.
func newDrinkDurationStore(logger logging.Logger) *drinkDurationStore {
	d := &drinkDurationStore{}
	dir := os.Getenv("VIAM_MODULE_DATA")
	if dir == "" {
		logger.Warnf("drink durations: VIAM_MODULE_DATA is unset — wait-time estimates will " +
			"be kept in memory only and reset on every reconfigure")
		return d
	}
	d.path = filepath.Join(dir, drinkDurationsFile)

	raw, err := os.ReadFile(d.path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("drink durations: reading %s: %v — starting without wait-time history", d.path, err)
		}
		return d
	}
	var entries []drinkDuration
	if err := json.Unmarshal(raw, &entries); err != nil {
		logger.Warnf("drink durations: %s is not valid JSON: %v — starting without wait-time history", d.path, err)
		return d
	}
	d.entries = trimToWindow(entries)
	return d
}

// trimToWindow drops every entry older than its drink's latest
// drinkDurationWindow, keeping the rest in order.
func trimToWindow(entries []drinkDuration) []drinkDuration {
	seen := map[string]int{}
	keep := make([]bool, len(entries))
	n := 0
	for i := len(entries) - 1; i >= 0; i-- {
		seen[entries[i].Drink]++
		if seen[entries[i].Drink] <= drinkDurationWindow {
			keep[i] = true
			n++
		}
	}
	out := make([]drinkDuration, 0, n)
	for i, e := range entries {
		if keep[i] {
			out = append(out, e)
		}
	}
	return out
}

// record adds one successful drink, dropping that drink's oldest past the window, and
// persists best-effort: a write failure leaves the in-memory window correct.
func (d *drinkDurationStore) record(logger logging.Logger, drink string, took time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.entries = trimToWindow(append(d.entries, drinkDuration{Drink: drink, Seconds: took.Seconds()}))
	path := d.path
	var data []byte
	var err error
	if path != "" {
		data, err = json.Marshal(d.entries)
	}
	d.mu.Unlock()

	if path == "" {
		return
	}
	if err == nil {
		err = os.WriteFile(path, data, 0o644)
	}
	if err != nil {
		logger.Warnf("drink durations: persisting to %s: %v", path, err)
	}
}

// estimates returns the average duration of each drink over its window, plus
// the average over every stored entry for a drink that has never been made. ok
// is false when nothing has been made yet and nothing can be estimated.
func (d *drinkDurationStore) estimates() (perDrink map[string]time.Duration, overall time.Duration, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.entries) == 0 {
		return nil, 0, false
	}
	sums := map[string]float64{}
	counts := map[string]int{}
	var total float64
	for _, e := range d.entries {
		sums[e.Drink] += e.Seconds
		counts[e.Drink]++
		total += e.Seconds
	}
	perDrink = make(map[string]time.Duration, len(sums))
	for drink, sum := range sums {
		perDrink[drink] = secondsToDuration(sum / float64(counts[drink]))
	}
	return perDrink, secondsToDuration(total / float64(len(d.entries))), true
}

// expectedWait estimates how long a new order would wait before the arm starts
// on it: the remaining time of the order being made plus the full time of each
// order still pending ahead of it. Completed orders in the list are skipped. ok
// is false when there is no history to estimate from.
//
// The order on the arm counts as done once it overruns its estimate, so a
// slow drink never makes the wait negative.
func (d *drinkDurationStore) expectedWait(orders []order.Order, currentID string, now time.Time) (time.Duration, bool) {
	if d == nil {
		return 0, false
	}
	perDrink, overall, ok := d.estimates()
	if !ok {
		return 0, false
	}
	var wait time.Duration
	for _, o := range orders {
		if !o.CompletedAt.IsZero() {
			continue
		}
		est, known := perDrink[o.Drink]
		if !known {
			est = overall
		}
		if o.ID == currentID && !o.StartedAt.IsZero() {
			est = max(est-now.Sub(o.StartedAt), 0)
		}
		wait += est
	}
	return wait, true
}
