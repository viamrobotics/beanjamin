package coffee

import (
	"strings"
	"testing"
	"time"
)

// row builds one projected query row the way dailySummaryStages shapes it.
func row(drink string, ok, cancelled, decaf bool, failedStep string, durationMs float64) map[string]any {
	return map[string]any{
		"drink":              drink,
		"order_ok":           ok,
		"operator_cancelled": cancelled,
		"decaf":              decaf,
		"failed_step":        failedStep,
		"duration_ms":        durationMs,
	}
}

func TestSummarizeOrders(t *testing.T) {
	rows := []map[string]any{
		row("espresso", true, false, false, "", 120000),
		row("espresso", true, false, true, "", 140000),
		row("iced_latte", true, false, false, "", 220000),
		// A genuine fault, and an operator cancel that must not be counted as one.
		row("espresso", false, false, false, stepLockingPortafilter, 30000),
		row("lungo", false, true, false, stepGrinding, 5000),
	}

	sum := summarizeOrders(rows)

	if sum.attempted != 5 {
		t.Errorf("attempted = %d, want 5", sum.attempted)
	}
	if sum.succeeded != 3 {
		t.Errorf("succeeded = %d, want 3", sum.succeeded)
	}
	if sum.faulted != 1 {
		t.Errorf("faulted = %d, want 1", sum.faulted)
	}
	if sum.cancelled != 1 {
		t.Errorf("cancelled = %d, want 1", sum.cancelled)
	}
	if sum.decaf != 1 {
		t.Errorf("decaf = %d, want 1", sum.decaf)
	}
	if got, want := sum.drinks["espresso"], 3; got != want {
		t.Errorf("drinks[espresso] = %d, want %d", got, want)
	}
	// An operator cancel contributes a drink but no failed step.
	if got, want := len(sum.failedSteps), 1; got != want {
		t.Errorf("failedSteps has %d entries, want %d: %v", got, want, sum.failedSteps)
	}
	if got := sum.failedSteps[stepLockingPortafilter]; got != 1 {
		t.Errorf("failedSteps[%q] = %d, want 1", stepLockingPortafilter, got)
	}

	// Only successful orders contribute to brew time: 120+140+220 = 480s over 3.
	if want := 480 * time.Second; sum.brewTotal != want {
		t.Errorf("brewTotal = %v, want %v", sum.brewTotal, want)
	}
	if want := 160 * time.Second; sum.avgBrew() != want {
		t.Errorf("avgBrew() = %v, want %v", sum.avgBrew(), want)
	}
	if got, want := sum.successRate(), 60.0; got != want {
		t.Errorf("successRate() = %v, want %v", got, want)
	}
}

func TestSummarizeOrdersEmpty(t *testing.T) {
	sum := summarizeOrders(nil)
	if sum.attempted != 0 {
		t.Errorf("attempted = %d, want 0", sum.attempted)
	}
	// Both must be division-safe on a day with no orders.
	if got := sum.successRate(); got != 0 {
		t.Errorf("successRate() = %v, want 0", got)
	}
	if got := sum.avgBrew(); got != 0 {
		t.Errorf("avgBrew() = %v, want 0", got)
	}
}

// A reading that arrives without the fields we expect must not panic or be
// silently miscounted: an absent order_ok is not a success.
func TestSummarizeOrdersMissingFields(t *testing.T) {
	sum := summarizeOrders([]map[string]any{{}})
	if sum.attempted != 1 {
		t.Errorf("attempted = %d, want 1", sum.attempted)
	}
	if sum.succeeded != 0 {
		t.Errorf("succeeded = %d, want 0", sum.succeeded)
	}
	if sum.faulted != 1 {
		t.Errorf("faulted = %d, want 1", sum.faulted)
	}
	if got := sum.drinks["unknown"]; got != 1 {
		t.Errorf("drinks[unknown] = %d, want 1", got)
	}
	if got := sum.failedSteps["an unknown step"]; got != 1 {
		t.Errorf("failedSteps[an unknown step] = %d, want 1", got)
	}
}

// duration_ms crosses the wire as a float64 but may arrive as another numeric
// type; numericReading absorbs that, and a non-numeric value must be skipped
// rather than counted as zero-length brew.
func TestSummarizeOrdersDurationTypes(t *testing.T) {
	rows := []map[string]any{
		{"drink": "espresso", "order_ok": true, "duration_ms": float64(60000)},
		{"drink": "espresso", "order_ok": true, "duration_ms": int64(30000)},
		{"drink": "espresso", "order_ok": true, "duration_ms": "not a number"},
	}
	sum := summarizeOrders(rows)
	if sum.succeeded != 3 {
		t.Errorf("succeeded = %d, want 3", sum.succeeded)
	}
	if want := 90 * time.Second; sum.brewTotal != want {
		t.Errorf("brewTotal = %v, want %v", sum.brewTotal, want)
	}
}

func TestRankedCounts(t *testing.T) {
	// Ties break by name so the same day always renders identically.
	got := rankedCounts(map[string]int{"lungo": 2, "espresso": 5, "americano": 2})
	want := "• espresso — 5\n• americano — 2\n• lungo — 2"
	if got != want {
		t.Errorf("rankedCounts() =\n%s\nwant\n%s", got, want)
	}
	if got := rankedCounts(map[string]int{}); got != "" {
		t.Errorf("rankedCounts(empty) = %q, want empty", got)
	}
}

func TestSummaryLocation(t *testing.T) {
	loc, err := summaryLocation(map[string]any{"timezone": "America/New_York"})
	if err != nil {
		t.Fatalf("summaryLocation returned error: %v", err)
	}
	if loc.String() != "America/New_York" {
		t.Errorf("loc = %q, want America/New_York", loc)
	}

	// A bare `true` (a hand-fired command) and an omitted timezone both fall
	// back to the host zone rather than erroring.
	for _, arg := range []any{true, map[string]any{}, map[string]any{"timezone": "  "}} {
		loc, err := summaryLocation(arg)
		if err != nil {
			t.Errorf("summaryLocation(%v) returned error: %v", arg, err)
		}
		if loc != time.Local {
			t.Errorf("summaryLocation(%v) = %v, want host local", arg, loc)
		}
	}

	if _, err := summaryLocation(map[string]any{"timezone": "Mars/Olympus_Mons"}); err == nil {
		t.Error("summaryLocation accepted an invalid timezone, want error")
	}
}

func TestDailySummaryBlocks(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	dayStart := time.Date(2026, 9, 11, 0, 0, 0, 0, loc)
	now := time.Date(2026, 9, 11, 17, 30, 0, 0, loc)

	sum := summarizeOrders([]map[string]any{
		row("espresso", true, false, false, "", 120000),
		row("lungo", false, false, false, stepGrinding, 4000),
	})
	blocks := dailySummaryBlocks(sum, dayStart, now, "https://app.viam.com/machine/abc/logs")

	// header, stats, drinks, faults, footer
	if len(blocks) != 5 {
		t.Fatalf("got %d blocks, want 5: %#v", len(blocks), blocks)
	}
	// structpb rejects []map[string]any as a list value, so every block must be
	// a map[string]any inside an []any.
	for i, b := range blocks {
		if _, ok := b.(map[string]any); !ok {
			t.Errorf("block %d is %T, want map[string]any", i, b)
		}
	}

	footer := blocks[len(blocks)-1].(map[string]any)
	elems := footer["elements"].([]any)
	text := elems[0].(map[string]any)["text"].(string)
	// The window is the visible check on a CRON_TZ/timezone mismatch.
	if !strings.Contains(text, "12:00 AM") || !strings.Contains(text, "5:30 PM") {
		t.Errorf("footer %q does not show the 12:00 AM – 5:30 PM window", text)
	}
	if !strings.Contains(text, "machine logs") {
		t.Errorf("footer %q is missing the machine logs link", text)
	}
}

func TestDailySummaryBlocksNoOrders(t *testing.T) {
	now := time.Now()
	blocks := dailySummaryBlocks(summarizeOrders(nil), now, now, "")
	// header, "No orders today.", footer — no empty drinks or faults sections.
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %#v", len(blocks), blocks)
	}
	if !strings.Contains(dailySummaryText(summarizeOrders(nil), now), "No orders") {
		t.Error("fallback text does not say there were no orders")
	}
}
