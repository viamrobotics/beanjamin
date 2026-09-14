package coffee

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
)

func TestSummarizeOrders(t *testing.T) {
	rows := []orderRow{
		{Drink: "espresso", OrderOK: true, DurationMs: 120000},
		{Drink: "espresso", OrderOK: true, Decaf: true, DurationMs: 140000},
		{Drink: "iced_latte", OrderOK: true, DurationMs: 220000},
		// A genuine fault, and an operator cancel that must not be counted as one.
		{Drink: "espresso", FailedStep: stepLockingPortafilter, DurationMs: 30000},
		{Drink: "lungo", OperatorCancelled: true, FailedStep: stepGrinding, DurationMs: 5000},
	}

	sum := summarizeOrders(rows)

	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"attempted", sum.attempted, 5},
		{"succeeded", sum.succeeded, 3},
		{"faulted", sum.faulted, 1},
		{"cancelled", sum.cancelled, 1},
		{"decaf", sum.decaf, 1},
		{"drinks[espresso]", sum.drinks["espresso"], 3},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	// An operator cancel contributes a drink but no failed step, even though the
	// reading carries the step it was stopped at.
	if len(sum.failedSteps) != 1 || sum.failedSteps[stepLockingPortafilter] != 1 {
		t.Errorf("failedSteps = %v, want one %q entry", sum.failedSteps, stepLockingPortafilter)
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

// A reading whose fields are all absent must not be silently counted as a
// success, and must still be attributable in the breakdowns.
func TestSummarizeOrdersZeroRow(t *testing.T) {
	sum := summarizeOrders([]orderRow{{}})
	if sum.attempted != 1 || sum.succeeded != 0 || sum.faulted != 1 {
		t.Errorf("attempted/succeeded/faulted = %d/%d/%d, want 1/0/1",
			sum.attempted, sum.succeeded, sum.faulted)
	}
	if sum.drinks["unknown"] != 1 {
		t.Errorf("drinks = %v, want one \"unknown\" entry", sum.drinks)
	}
	if sum.failedSteps["an unknown step"] != 1 {
		t.Errorf("failedSteps = %v, want one \"an unknown step\" entry", sum.failedSteps)
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

// The projection must name exactly the fields orderRow decodes; a drift here is
// the bug that would show up as a day of silently-zero counters.
func TestDailySummaryStagesMatchOrderRow(t *testing.T) {
	stages := dailySummaryStages()
	if len(stages) != 1 {
		t.Fatalf("got %d stages, want 1", len(stages))
	}
	project, ok := stages[0]["$project"].(map[string]any)
	if !ok {
		t.Fatalf("stage is not a $project: %#v", stages[0])
	}
	for _, field := range []string{"drink", "order_ok", "operator_cancelled", "failed_step", "decaf", "duration_ms"} {
		want := "$data.readings." + field
		if got := project[field]; got != want {
			t.Errorf("$project[%q] = %v, want %q", field, got, want)
		}
	}
	// _id plus the six projected fields; a different count means a tag drifted.
	if len(project) != 7 {
		t.Errorf("$project has %d keys, want 7: %#v", len(project), project)
	}
}

func TestDecodeOrderRows(t *testing.T) {
	logger := logging.NewTestLogger(t)
	raw := []map[string]any{
		{"drink": "espresso", "order_ok": true, "duration_ms": float64(60000)},
		// duration_ms arriving as an integer from BSON must still decode.
		{"drink": "lungo", "order_ok": true, "duration_ms": int64(30000)},
		// A structurally wrong reading is dropped rather than failing the digest.
		{"drink": "espresso", "order_ok": "yes please"},
	}
	rows := decodeOrderRows(raw, logger)
	if len(rows) != 2 {
		t.Fatalf("decoded %d rows, want 2: %#v", len(rows), rows)
	}
	if rows[0].Drink != "espresso" || rows[0].DurationMs != 60000 {
		t.Errorf("rows[0] = %#v", rows[0])
	}
	if rows[1].DurationMs != 30000 {
		t.Errorf("rows[1].DurationMs = %v, want 30000", rows[1].DurationMs)
	}
}

func TestConsecutiveSuccesses(t *testing.T) {
	ctx := context.Background()

	// No usage sensor configured: the digest omits the field rather than
	// claiming a broken streak.
	s, _ := newTestCoffee(t, nil)
	if streak, ok := s.consecutiveSuccesses(ctx); ok {
		t.Errorf("consecutiveSuccesses() = (%d, true) with no usage sensor, want ok=false", streak)
	}

	s.usageSensor = newFakeUsageSensor(map[string]any{
		"successful_consecutive_orders": float64(12),
		"regular_grinds":                float64(40),
	})
	streak, ok := s.consecutiveSuccesses(ctx)
	if !ok || streak != 12 {
		t.Errorf("consecutiveSuccesses() = (%d, %v), want (12, true)", streak, ok)
	}

	// A sensor that answers but has never recorded a streak reads as 0, which is
	// a real value — the machine's last order failed.
	s.usageSensor = newFakeUsageSensor(map[string]any{"regular_grinds": float64(40)})
	if streak, ok := s.consecutiveSuccesses(ctx); !ok || streak != 0 {
		t.Errorf("consecutiveSuccesses() = (%d, %v) for an absent key, want (0, true)", streak, ok)
	}

	// A non-numeric value is unreadable, not zero.
	s.usageSensor = newFakeUsageSensor(map[string]any{"successful_consecutive_orders": "lots"})
	if streak, ok := s.consecutiveSuccesses(ctx); ok {
		t.Errorf("consecutiveSuccesses() = (%d, true) for a non-numeric value, want ok=false", streak)
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
	now := time.Date(2026, 9, 14, 17, 30, 0, 0, loc) // a Monday
	windowStart := now.Add(-dailySummaryWindow)

	sum := summarizeOrders([]orderRow{
		{Drink: "espresso", OrderOK: true, Decaf: true, DurationMs: 120000},
		{Drink: "lungo", FailedStep: stepGrinding},
	})
	blocks := dailySummaryBlocks(sum, 12, true, windowStart, now)

	// header, stats grid, drinks, faults, footer
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

	// The stats grid must be `fields`, not `text` — that is what makes Slack lay
	// it out in two columns.
	stats := blocks[1].(map[string]any)
	fields, ok := stats["fields"].([]any)
	if !ok {
		t.Fatalf("stats block has no fields array: %#v", stats)
	}
	// 4 counters + avg/total brew + decaf + streak.
	if len(fields) != 8 {
		t.Errorf("stats grid has %d fields, want 8: %#v", len(fields), fields)
	}

	footer := blocks[4].(map[string]any)
	text := footer["elements"].([]any)[0].(map[string]any)["text"].(string)
	// Both ends carry their weekday, so a reader can see which days the window
	// spans rather than guessing from two bare clock times.
	if want := "Sun 5:30 PM – Mon 5:30 PM EDT"; text != want {
		t.Errorf("footer = %q, want %q", text, want)
	}
}

// Consecutive digests must tile: the window a digest covers has to start
// exactly where the previous run's ended, or orders are dropped or counted
// twice.
func TestDailySummaryWindowTiles(t *testing.T) {
	run := time.Date(2026, 9, 14, 17, 30, 0, 0, time.UTC)
	previous := run.Add(-24 * time.Hour)
	if got := run.Add(-dailySummaryWindow); !got.Equal(previous) {
		t.Errorf("window starts at %v, want the previous daily run at %v", got, previous)
	}
}

// A day with no faults must not render an empty "Faults by step" section, and
// the decaf and streak fields must be absent rather than zeroed — a "0 in a row"
// would read as a streak that had just broken.
func TestDailySummaryBlocksOmitsEmptySections(t *testing.T) {
	now := time.Now()
	sum := summarizeOrders([]orderRow{{Drink: "espresso", OrderOK: true, DurationMs: 60000}})
	blocks := dailySummaryBlocks(sum, 0, false, now, now)
	// header, stats, drinks, footer — no faults section.
	if len(blocks) != 4 {
		t.Errorf("got %d blocks, want 4 (no faults section): %#v", len(blocks), blocks)
	}
	fields := blocks[1].(map[string]any)["fields"].([]any)
	// 4 counters + avg/total brew; no decaf and no streak.
	if len(fields) != 6 {
		t.Errorf("stats grid has %d fields, want 6 (no decaf, no streak): %#v", len(fields), fields)
	}
	for _, f := range fields {
		if text := f.(map[string]any)["text"].(string); strings.Contains(text, "streak") {
			t.Errorf("rendered a streak field with no usage sensor: %q", text)
		}
	}
}

func TestDailySummaryBlocksNoOrders(t *testing.T) {
	now := time.Now()
	blocks := dailySummaryBlocks(summarizeOrders(nil), 7, true, now, now)
	// header, "No orders today.", footer — no empty grid or breakdowns.
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %#v", len(blocks), blocks)
	}
	if !strings.Contains(dailySummaryText(summarizeOrders(nil)), "No orders") {
		t.Error("fallback text does not say there were no orders")
	}
}
