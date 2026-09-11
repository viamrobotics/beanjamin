package coffee

import (
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
		{"attempted", sum.Attempted, 5},
		{"succeeded", sum.Succeeded, 3},
		{"faulted", sum.Faulted, 1},
		{"cancelled", sum.Cancelled, 1},
		{"decaf", sum.Decaf, 1},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	// An operator cancel contributes a drink but no failed step, even though the
	// reading carries the step it was stopped at.
	if len(sum.FailedSteps) != 1 || sum.FailedSteps[0] != (nameCount{stepLockingPortafilter, 1}) {
		t.Errorf("FailedSteps = %v, want one %q entry", sum.FailedSteps, stepLockingPortafilter)
	}

	// Only successful orders contribute to brew time: 120+140+220 = 480s over 3.
	if want := 480 * time.Second; sum.TotalBrew() != want {
		t.Errorf("TotalBrew() = %v, want %v", sum.TotalBrew(), want)
	}
	if want := 160 * time.Second; sum.AvgBrew() != want {
		t.Errorf("AvgBrew() = %v, want %v", sum.AvgBrew(), want)
	}
	if got, want := sum.SuccessRate(), 60.0; got != want {
		t.Errorf("SuccessRate() = %v, want %v", got, want)
	}
}

func TestSummarizeOrdersEmpty(t *testing.T) {
	sum := summarizeOrders(nil)
	if sum.Attempted != 0 {
		t.Errorf("Attempted = %d, want 0", sum.Attempted)
	}
	// Both must be division-safe on a day with no orders.
	if got := sum.SuccessRate(); got != 0 {
		t.Errorf("SuccessRate() = %v, want 0", got)
	}
	if got := sum.AvgBrew(); got != 0 {
		t.Errorf("AvgBrew() = %v, want 0", got)
	}
}

// A reading whose fields are all absent must not be silently counted as a
// success, and must still be attributable in the breakdowns.
func TestSummarizeOrdersZeroRow(t *testing.T) {
	sum := summarizeOrders([]orderRow{{}})
	if sum.Attempted != 1 || sum.Succeeded != 0 || sum.Faulted != 1 {
		t.Errorf("attempted/succeeded/faulted = %d/%d/%d, want 1/0/1",
			sum.Attempted, sum.Succeeded, sum.Faulted)
	}
	if len(sum.Drinks) != 1 || sum.Drinks[0].Name != "unknown" {
		t.Errorf("Drinks = %v, want one \"unknown\" entry", sum.Drinks)
	}
	if len(sum.FailedSteps) != 1 || sum.FailedSteps[0].Name != "an unknown step" {
		t.Errorf("FailedSteps = %v, want one \"an unknown step\" entry", sum.FailedSteps)
	}
}

func TestRanked(t *testing.T) {
	// Ties break by name so the same day always renders identically.
	got := ranked(map[string]int{"lungo": 2, "espresso": 5, "americano": 2})
	want := []nameCount{{"espresso", 5}, {"americano", 2}, {"lungo", 2}}
	if len(got) != len(want) {
		t.Fatalf("ranked() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ranked()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if got := ranked(map[string]int{}); len(got) != 0 {
		t.Errorf("ranked(empty) = %v, want empty", got)
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
	// _id plus the six projected fields; an extra key means a tag went missing.
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

// Pins the rendered body exactly. The template's whitespace control is the
// fiddly part, and nothing else would catch a stray blank line.
func TestRenderDailySummary(t *testing.T) {
	sum := summarizeOrders([]orderRow{
		{Drink: "espresso", OrderOK: true, DurationMs: 120000},
		{Drink: "espresso", OrderOK: true, Decaf: true, DurationMs: 140000},
		{Drink: "iced_latte", OrderOK: true, DurationMs: 220000},
		{Drink: "espresso", FailedStep: stepLockingPortafilter},
		{Drink: "lungo", OperatorCancelled: true},
	})
	got, err := renderDailySummary(sum)
	if err != nil {
		t.Fatalf("renderDailySummary: %v", err)
	}
	want := strings.Join([]string{
		"*5 orders* · 3 succeeded (60%) · 1 faulted · 1 cancelled by an operator",
		"Average brew 2m40s, 8m0s of brewing in total.",
		"",
		"*Drinks* _(1 decaf)_",
		"• espresso — 3",
		"• iced_latte — 1",
		"• lungo — 1",
		"",
		"*Faults by step*",
		"• " + stepLockingPortafilter + " — 1",
	}, "\n")
	if got != want {
		t.Errorf("renderDailySummary() =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderDailySummaryNoOrders(t *testing.T) {
	got, err := renderDailySummary(summarizeOrders(nil))
	if err != nil {
		t.Fatalf("renderDailySummary: %v", err)
	}
	if got != "No orders today." {
		t.Errorf("renderDailySummary() = %q, want %q", got, "No orders today.")
	}
	if !strings.Contains(dailySummaryText(summarizeOrders(nil), time.Now()), "No orders") {
		t.Error("fallback text does not say there were no orders")
	}
}

// A day with no faults must not render an empty "Faults by step" heading.
func TestRenderDailySummaryNoFaults(t *testing.T) {
	sum := summarizeOrders([]orderRow{{Drink: "espresso", OrderOK: true, DurationMs: 60000}})
	got, err := renderDailySummary(sum)
	if err != nil {
		t.Fatalf("renderDailySummary: %v", err)
	}
	if strings.Contains(got, "Faults by step") {
		t.Errorf("rendered a faults section with no faults:\n%s", got)
	}
	if strings.Contains(got, "decaf") {
		t.Errorf("rendered a decaf note with no decaf orders:\n%s", got)
	}
}

func TestDailySummaryBlocks(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	dayStart := time.Date(2026, 9, 11, 0, 0, 0, 0, loc)
	now := time.Date(2026, 9, 11, 17, 30, 0, 0, loc)

	blocks := dailySummaryBlocks("body text", dayStart, now)
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %#v", len(blocks), blocks)
	}
	// structpb rejects []map[string]any as a list value, so every block must be
	// a map[string]any inside an []any.
	for i, b := range blocks {
		if _, ok := b.(map[string]any); !ok {
			t.Errorf("block %d is %T, want map[string]any", i, b)
		}
	}

	footer := blocks[2].(map[string]any)
	text := footer["elements"].([]any)[0].(map[string]any)["text"].(string)
	// The window is the visible check on a CRON_TZ/timezone mismatch.
	if !strings.Contains(text, "12:00 AM") || !strings.Contains(text, "5:30 PM") {
		t.Errorf("footer %q does not show the 12:00 AM – 5:30 PM window", text)
	}
}
