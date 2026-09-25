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
		// espresso was ordered 3 times but only 2 of them succeeded.
		{"drinks[espresso].ordered", sum.drinks["espresso"].ordered, 3},
		{"drinks[espresso].succeeded", sum.drinks["espresso"].succeeded, 2},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	// Per-drink timings cover that drink's successes only. espresso: 120+140
	// over 2; the failed espresso's 30s is excluded.
	if avg, ok := sum.drinks["espresso"].avgBrew(); !ok || avg != 130*time.Second {
		t.Errorf("espresso avgBrew() = (%v, %v), want (2m10s, true)", avg, ok)
	}
	if avg, ok := sum.drinks["iced_latte"].avgBrew(); !ok || avg != 220*time.Second {
		t.Errorf("iced_latte avgBrew() = (%v, %v), want (3m40s, true)", avg, ok)
	}
	// lungo was only ever cancelled, so it has no timing at all rather than 0s.
	if avg, ok := sum.drinks["lungo"].avgBrew(); ok {
		t.Errorf("lungo avgBrew() = (%v, true), want ok=false", avg)
	}

	// An operator cancel contributes a drink but no failed step, even though the
	// reading carries the step it was stopped at.
	if len(sum.failedSteps) != 1 || sum.failedSteps[stepLockingPortafilter] != 1 {
		t.Errorf("failedSteps = %v, want one %q entry", sum.failedSteps, stepLockingPortafilter)
	}

	// Only successful orders contribute to brew time: 120+140+220 = 480s.
	if want := 480 * time.Second; sum.brewTotal != want {
		t.Errorf("brewTotal = %v, want %v", sum.brewTotal, want)
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
	// Must be division-safe on a window with no orders.
	if got := sum.successRate(); got != 0 {
		t.Errorf("successRate() = %v, want 0", got)
	}
	// And so must a drink nothing was ever ordered of.
	if avg, ok := sum.drinks["espresso"].avgBrew(); ok {
		t.Errorf("avgBrew() on an absent drink = (%v, true), want ok=false", avg)
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
	if sum.drinks["unknown"].ordered != 1 {
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

// Keys come back out of cloud data, so they are escaped like any other value
// that reaches a mrkdwn section.
func TestRankedLists_EscapeKeys(t *testing.T) {
	if got, want := rankedCounts(map[string]int{"<!channel>": 1}), "• &lt;!channel&gt; — 1"; got != want {
		t.Errorf("rankedCounts() = %q, want %q", got, want)
	}
	if got, want := rankedDrinks(map[string]drinkStats{"<!here>": {ordered: 1}}), "• &lt;!here&gt; — 1"; got != want {
		t.Errorf("rankedDrinks() = %q, want %q", got, want)
	}
}

func TestRankedDrinks(t *testing.T) {
	got := rankedDrinks(map[string]drinkStats{
		// Ordered most, so it ranks first despite being the quickest.
		"espresso": {ordered: 5, succeeded: 4, brewTotal: 8 * time.Minute},
		// Ties with americano on count, so the name breaks it.
		"lungo": {ordered: 2, succeeded: 2, brewTotal: 5 * time.Minute},
		// Every attempt failed: a count, but no invented timing.
		"americano": {ordered: 2},
	})
	want := strings.Join([]string{
		"• espresso — 5 _(avg 2m0s)_",
		"• americano — 2",
		"• lungo — 2 _(avg 2m30s)_",
	}, "\n")
	if got != want {
		t.Errorf("rankedDrinks() =\n%s\nwant\n%s", got, want)
	}
	if got := rankedDrinks(map[string]drinkStats{}); got != "" {
		t.Errorf("rankedDrinks(empty) = %q, want empty", got)
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
	fields := []string{"drink", "customer_name", "order_ok", "operator_cancelled", "failed_step", "decaf", "duration_ms"}
	for _, field := range fields {
		want := "$data.readings." + field
		if got := project[field]; got != want {
			t.Errorf("$project[%q] = %v, want %q", field, got, want)
		}
	}
	// _id plus the projected fields; a different count means a tag drifted.
	if len(project) != len(fields)+1 {
		t.Errorf("$project has %d keys, want %d: %#v", len(project), len(fields)+1, project)
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

	// header, headline, table, aside, drinks, faults, footer
	if len(blocks) != 7 {
		t.Fatalf("got %d blocks, want 7: %#v", len(blocks), blocks)
	}
	// structpb rejects []map[string]any as a list value, so every block must be
	// a map[string]any inside an []any.
	for i, b := range blocks {
		if _, ok := b.(map[string]any); !ok {
			t.Errorf("block %d is %T, want map[string]any", i, b)
		}
	}

	text := func(i int) string {
		return blocks[i].(map[string]any)["text"].(map[string]any)["text"].(string)
	}

	// The drinks section carries the per-drink timing.
	drinks := text(4)
	if !strings.Contains(drinks, "espresso — 1 _(avg 2m0s)_") {
		t.Errorf("drinks section missing the per-drink average:\n%s", drinks)
	}
	// The lungo attempt faulted, so it gets a count and no timing.
	if !strings.Contains(drinks, "lungo — 1\n") && !strings.HasSuffix(drinks, "lungo — 1") {
		t.Errorf("drinks section should show lungo with no timing:\n%s", drinks)
	}

	footer := blocks[6].(map[string]any)["elements"].([]any)[0].(map[string]any)["text"].(string)
	// Both ends carry their weekday, so a reader can see which days the window
	// spans rather than guessing from two bare clock times.
	want := ":clock3: Sun 5:30 PM – Mon 5:30 PM EDT  ·  <" + beanjaminAppURL + "|order a coffee>"
	if footer != want {
		t.Errorf("footer = %q, want %q", footer, want)
	}
}

// Pins the headline line, the table and the aside exactly. These are the parts
// a reader takes at a glance, and column alignment in particular breaks
// silently — it still renders, just crookedly.
func TestDigestRendering(t *testing.T) {
	sum := summarizeOrders([]orderRow{
		{Drink: "espresso", OrderOK: true, DurationMs: 120000},
		{Drink: "espresso", OrderOK: true, Decaf: true, DurationMs: 140000},
		{Drink: "iced_latte", OrderOK: true, DurationMs: 220000},
		{Drink: "espresso", FailedStep: stepLockingPortafilter},
		{Drink: "lungo", OperatorCancelled: true},
	})

	if got, want := headlineStats(sum), ":clock3: 8m0s brewing  ·  :white_check_mark: 3/5 succeeded  ·  :coffee: 5 orders"; got != want {
		t.Errorf("headlineStats() =\n%s\nwant\n%s", got, want)
	}

	wantTable := strings.Join([]string{
		"```",
		"Succeeded                 3",
		"Faulted                   1",
		"Cancelled by operator     1",
		"───────────────────────────",
		"Total                     5",
		"```",
	}, "\n")
	if got := outcomeTable(sum); got != wantTable {
		t.Errorf("outcomeTable() =\n%s\nwant\n%s", got, wantTable)
	}

	if got, want := asideStats(sum, 12, true), ":sleeping: 1 decaf  ·  :fire: 12 in a row"; got != want {
		t.Errorf("asideStats() = %q, want %q", got, want)
	}
}

func TestTopCustomers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		customers map[string]int
		want      string
	}{
		{"nobody named", map[string]int{}, ""},
		{"a clear winner", map[string]int{"Alice": 5, "Bob": 2}, "Alice — 5"},
		{"one customer", map[string]int{"Alice": 1}, "Alice — 1"},
		// A tie is the normal case on a quiet day, so everyone at the top is
		// named rather than one of them picked arbitrarily.
		{"a tie", map[string]int{"Bob": 3, "Alice": 3, "Carol": 1}, "Alice, Bob — 3 each"},
		{"everyone tied", map[string]int{"Bob": 1, "Alice": 1}, "Alice, Bob — 1 each"},
		// Names are typed at the kiosk; unescaped, this one would ping the
		// whole channel from the digest.
		{"mention escaped", map[string]int{"<!channel>": 2}, "&lt;!channel&gt; — 2"},
		{"tied mentions escaped", map[string]int{"<!here>": 1, "A & B": 1}, "&lt;!here&gt;, A &amp; B — 1 each"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := topCustomers(tc.customers); got != tc.want {
				t.Errorf("topCustomers() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Walk-ups who skip the name screen are left out of the leaderboard entirely.
// The deliberate consequence is that the customer counts do not sum to the
// order count.
func TestSummarizeOrdersSkipsAnonymous(t *testing.T) {
	sum := summarizeOrders([]orderRow{
		{Drink: "espresso", CustomerName: "Alice", OrderOK: true, DurationMs: 60000},
		{Drink: "espresso", CustomerName: "", OrderOK: true, DurationMs: 60000},
		{Drink: "espresso", CustomerName: "   ", OrderOK: true, DurationMs: 60000},
		// A faulted order still counts toward its customer's tally: they asked
		// for a drink, and the machine failing is the machine's record.
		{Drink: "lungo", CustomerName: "Alice", FailedStep: stepGrinding},
	})

	if sum.attempted != 4 {
		t.Errorf("attempted = %d, want 4", sum.attempted)
	}
	if len(sum.customers) != 1 || sum.customers["Alice"] != 2 {
		t.Errorf("customers = %v, want only Alice with 2", sum.customers)
	}
	if got, want := topCustomers(sum.customers), "Alice — 2"; got != want {
		t.Errorf("topCustomers() = %q, want %q", got, want)
	}
}

// A window of entirely anonymous orders shows no leaderboard at all rather than
// an empty trophy.
func TestAsideStatsAllAnonymous(t *testing.T) {
	sum := summarizeOrders([]orderRow{{Drink: "espresso", OrderOK: true, DurationMs: 60000}})
	if got := asideStats(sum, 0, false); got != "" {
		t.Errorf("asideStats() = %q, want empty", got)
	}
}

// The aside disappears entirely when neither half applies, rather than leaving
// an empty section block in the message.
func TestAsideStatsOmitted(t *testing.T) {
	sum := summarizeOrders([]orderRow{{Drink: "espresso", OrderOK: true, DurationMs: 60000}})
	if got := asideStats(sum, 0, false); got != "" {
		t.Errorf("asideStats() = %q, want empty", got)
	}
	if got, want := asideStats(sum, 4, true), ":fire: 4 in a row"; got != want {
		t.Errorf("asideStats() = %q, want %q", got, want)
	}

	blocks := dailySummaryBlocks(sum, 0, false, time.Now(), time.Now())
	// header, headline, table, drinks, footer — no aside, no faults.
	if len(blocks) != 5 {
		t.Errorf("got %d blocks, want 5 (no aside, no faults): %#v", len(blocks), blocks)
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
