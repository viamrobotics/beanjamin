package coffee

// Daily order digest: a Slack summary of one business day's orders, read back
// out of the cloud tabular store that the order sensor syncs into.
//
// Scheduling lives in the machine config's "jobs" block, not here — see README.
// A schedule string without a CRON_TZ= prefix fires in the host's timezone, not
// the one this command is passed.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"
)

// dailySummaryTimeout caps the cloud query plus the Slack send. Cron jobs run
// in singleton mode, so a wedged digest would suppress every later firing until
// it returns.
const dailySummaryTimeout = 60 * time.Second

// dailySummaryWindow is how far back each digest reaches. A rolling window
// matched to the firing interval is what makes consecutive digests tile: run
// daily and every order is reported exactly once, with no gap for evening
// orders and no order counted twice. A calendar-day window would instead end at
// the moment the digest runs and silently drop anything brewed after it.
const dailySummaryWindow = 24 * time.Hour

// orderRow is one order-sensor reading, projected flat out of the tabular
// document. The json tags are the single source of truth for the field names:
// dailySummaryStages builds its $project from them by reflection, so a renamed
// field can't leave the query and the decode disagreeing — which would show up
// as a day of zeroed-out counters rather than as an error.
type orderRow struct {
	Drink             string  `json:"drink"`
	OrderOK           bool    `json:"order_ok"`
	OperatorCancelled bool    `json:"operator_cancelled"`
	FailedStep        string  `json:"failed_step"`
	Decaf             bool    `json:"decaf"`
	DurationMs        float64 `json:"duration_ms"`
}

// sendDailySummary posts a Slack digest of every order from the last 24 hours.
// Timestamps render in the host's timezone; the window itself is timezone-free,
// so the digest takes no arguments.
//
// ponytail: a failure here is only a returned error — the job manager logs it
// and records it in the job's history, but nothing reaches Slack. Silence in
// the channel therefore means "no digest ran" as well as "no orders". The
// always-posted no-orders line is what keeps that distinguishable by eye; if it
// stops being enough, post the error to Slack too.
func (s *beanjaminCoffee) sendDailySummary(ctx context.Context) (map[string]any, error) {
	if s.slackNotifier == nil {
		return nil, fmt.Errorf("send_daily_summary requires slack_notifier_name to be configured")
	}
	if s.cfg.OrderSensorName == "" {
		return nil, fmt.Errorf("send_daily_summary requires order_sensor_name to be configured (it names the component the digest queries)")
	}

	ctx, cancel := context.WithTimeout(ctx, dailySummaryTimeout)
	defer cancel()

	now := time.Now()
	windowStart := now.Add(-dailySummaryWindow)

	raw, err := s.QueryTabularDataForResource(ctx, s.cfg.OrderSensorName,
		&module.QueryTabularDataOptions{
			TimeBack:         dailySummaryWindow,
			AdditionalStages: dailySummaryStages(),
		})
	if err != nil {
		return nil, fmt.Errorf("querying today's orders from %q: %w", s.cfg.OrderSensorName, err)
	}

	sum := summarizeOrders(decodeOrderRows(raw, s.logger))
	streak, hasStreak := s.consecutiveSuccesses(ctx)

	s.logger.Infof("daily summary: %d orders between %s and %s",
		sum.attempted, windowStart.Format(time.RFC3339), now.Format(time.RFC3339))

	if _, err := s.slackNotifier.DoCommand(ctx, map[string]any{
		"command": "send",
		"text":    dailySummaryText(sum),
		"blocks":  dailySummaryBlocks(sum, streak, hasStreak, windowStart, now),
	}); err != nil {
		return nil, fmt.Errorf("sending daily summary to slack: %w", err)
	}
	return map[string]any{"orders": float64(sum.attempted), "sent": true}, nil
}

// consecutiveSuccesses reads the machine's successful-order streak off the usage
// sensor. This is a live counter rather than a property of the day — it is reset
// by any fault or operator cancel whenever it happens — so the digest labels it
// as the current streak. ok is false when no usage sensor is configured or the
// read fails, in which case the field is omitted rather than printed as 0, which
// would read as "the streak just broke".
func (s *beanjaminCoffee) consecutiveSuccesses(ctx context.Context) (int, bool) {
	if s.usageSensor == nil {
		return 0, false
	}
	readings, ok := s.readSensorFields(ctx, s.usageSensor, "daily summary")
	if !ok {
		return 0, false
	}
	streak, ok := numericReading(readings["successful_consecutive_orders"])
	if !ok {
		return 0, false
	}
	return int(streak), true
}

// dailySummaryStages projects the orderRow fields flat, one $project entry per
// json tag. The raw tabular document nests the reading under data.readings,
// which is also why this has to be MQL — SQL can't resolve that path.
func dailySummaryStages() []map[string]any {
	project := map[string]any{"_id": 0}
	for field := range reflect.TypeFor[orderRow]().Fields() {
		tag := field.Tag.Get("json")
		project[tag] = "$data.readings." + tag
	}
	return []map[string]any{{"$project": project}}
}

// decodeOrderRows converts the query's untyped documents into orderRows through
// a JSON round-trip, so the struct tags do the field mapping. A row that won't
// decode is dropped with a warning rather than failing the digest: one odd
// reading shouldn't cost the whole day's numbers.
func decodeOrderRows(raw []map[string]any, logger logging.Logger) []orderRow {
	rows := make([]orderRow, 0, len(raw))
	for _, doc := range raw {
		encoded, err := json.Marshal(doc)
		if err != nil {
			logger.Warnf("daily summary: skipping an unencodable order reading: %v", err)
			continue
		}
		var row orderRow
		if err := json.Unmarshal(encoded, &row); err != nil {
			logger.Warnf("daily summary: skipping an unreadable order reading: %v", err)
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// drinkStats is one drink's share of the window. Timings are kept per drink
// because the drinks are not comparable: an iced latte's fridge trip runs
// minutes longer than an espresso, so one average across all of them tracks the
// day's drink mix rather than the machine.
type drinkStats struct {
	ordered   int // every attempt, including faults and cancels
	succeeded int
	// brewTotal sums successful orders only: a fault that died at "Grinding"
	// would otherwise drag the average toward zero and read as a speed-up.
	brewTotal time.Duration
}

// avgBrew is the mean time for this drink. ok is false when nothing succeeded,
// so the caller prints no timing rather than a zero.
func (d drinkStats) avgBrew() (time.Duration, bool) {
	if d.succeeded == 0 {
		return 0, false
	}
	return (d.brewTotal / time.Duration(d.succeeded)).Round(time.Second), true
}

// daySummary is one window's orders reduced to what the digest prints.
type daySummary struct {
	attempted   int
	succeeded   int
	faulted     int
	cancelled   int
	decaf       int
	drinks      map[string]drinkStats
	failedSteps map[string]int
	// brewTotal is the machine's total time brewing, summed across drinks. A
	// sum stays meaningful across a mixed set in a way a mean does not — it is
	// utilization, not a per-drink expectation.
	brewTotal time.Duration
}

func (d daySummary) successRate() float64 {
	if d.attempted == 0 {
		return 0
	}
	return float64(d.succeeded) / float64(d.attempted) * 100
}

// summarizeOrders reduces the day's readings. Kept a pure function over typed
// rows so the whole aggregation is testable without a cloud connection.
func summarizeOrders(rows []orderRow) daySummary {
	sum := daySummary{
		drinks:      map[string]drinkStats{},
		failedSteps: map[string]int{},
	}
	for _, row := range rows {
		sum.attempted++
		drink := row.Drink
		if drink == "" {
			drink = "unknown"
		}
		stats := sum.drinks[drink]
		stats.ordered++
		if row.Decaf {
			sum.decaf++
		}

		switch {
		case row.OrderOK:
			sum.succeeded++
			brewed := time.Duration(row.DurationMs) * time.Millisecond
			sum.brewTotal += brewed
			stats.succeeded++
			stats.brewTotal += brewed
		// An operator stopping a run is not a fault; counting the two together
		// would make a busy day of manual cancels look like failing hardware.
		case row.OperatorCancelled:
			sum.cancelled++
		default:
			sum.faulted++
			step := row.FailedStep
			if step == "" {
				step = "an unknown step"
			}
			sum.failedSteps[step]++
		}
		// drinkStats is a value in the map, so the accumulated copy has to be
		// written back — including on the fault and cancel paths, which still
		// bumped `ordered`.
		sum.drinks[drink] = stats
	}
	return sum
}

// dailySummaryText is the short line Slack shows in notifications and in the
// channel list, and the fallback when Block Kit can't render.
func dailySummaryText(sum daySummary) string {
	if sum.attempted == 0 {
		return ":coffee: No orders in the last 24 hours."
	}
	return fmt.Sprintf(":coffee: Last 24 hours: %d orders, %d succeeded, %d faulted, %d cancelled (%.0f%% success).",
		sum.attempted, sum.succeeded, sum.faulted, sum.cancelled, sum.successRate())
}

// dailySummaryBlocks renders the digest as Block Kit: a header, a headline
// stat line, the outcome table, the breakdowns, and a footer naming the window.
// Returned as []any of map[string]any so it serializes through the
// structpb-backed DoCommand wire format, which rejects []map[string]any as a
// list value. streak is the machine's current run of successful orders, shown
// only when hasStreak (see consecutiveSuccesses).
func dailySummaryBlocks(sum daySummary, streak int, hasStreak bool, windowStart, now time.Time) []any {
	blocks := []any{
		map[string]any{
			"type": "header",
			"text": map[string]any{
				"type":  "plain_text",
				"text":  ":coffee: Orders in the last 24 hours",
				"emoji": true,
			},
		},
	}

	if sum.attempted == 0 {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": "No orders in the last 24 hours."},
		})
		return append(blocks, dailySummaryFooter(windowStart, now))
	}

	blocks = append(blocks, mrkdwnSection(headlineStats(sum)))
	blocks = append(blocks, mrkdwnSection(outcomeTable(sum)))

	if aside := asideStats(sum, streak, hasStreak); aside != "" {
		blocks = append(blocks, mrkdwnSection(aside))
	}

	blocks = append(blocks, mrkdwnSection(":coffee: *Drinks*\n"+rankedDrinks(sum.drinks)))

	if len(sum.failedSteps) > 0 {
		blocks = append(blocks, mrkdwnSection(":x: *Faults by step*\n"+rankedCounts(sum.failedSteps)))
	}

	return append(blocks, dailySummaryFooter(windowStart, now))
}

// mrkdwnSection wraps mrkdwn text in a Block Kit section block.
func mrkdwnSection(text string) map[string]any {
	return map[string]any{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": text},
	}
}

// headlineStats is the one line that has to land at a glance in a notification
// preview: how long the machine worked, how much of that landed, and how many
// drinks came out. The emoji carry the meaning so the line survives being
// skimmed, and the sections below repeat the same emoji for the same ideas.
func headlineStats(sum daySummary) string {
	orders := "orders"
	if sum.attempted == 1 {
		orders = "order"
	}
	return fmt.Sprintf(":clock3: %s brewing  ·  :white_check_mark: %d/%d succeeded  ·  :coffee: %d %s",
		sum.brewTotal.Round(time.Second), sum.succeeded, sum.attempted, sum.attempted, orders)
}

// asideStats is the decaf count and the current streak, on one line when either
// applies. Both are omitted rather than zeroed — a "0 in a row" reads as a
// streak that just broke, and "0 decaf" is noise on a day nobody ordered one.
func asideStats(sum daySummary, streak int, hasStreak bool) string {
	var parts []string
	if sum.decaf > 0 {
		parts = append(parts, fmt.Sprintf(":sleeping: %d decaf", sum.decaf))
	}
	if hasStreak {
		parts = append(parts, fmt.Sprintf(":fire: %d in a row", streak))
	}
	return strings.Join(parts, "  ·  ")
}

// outcomeTable renders the outcome counts as a fixed-width table inside a code
// fence. Slack has no table block that renders reliably across clients, and a
// code fence is the only place it honours column alignment. The cost is that
// emoji do not render inside one, which is why they live on the line above.
func outcomeTable(sum daySummary) string {
	rows := []struct {
		label string
		count int
	}{
		{"Succeeded", sum.succeeded},
		{"Faulted", sum.faulted},
		{"Cancelled by operator", sum.cancelled},
	}

	width := 0
	for _, r := range rows {
		width = max(width, len(r.label))
	}

	var b strings.Builder
	b.WriteString("```\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s  %4d\n", width, r.label, r.count)
	}
	fmt.Fprintf(&b, "%s\n", strings.Repeat("─", width+6))
	fmt.Fprintf(&b, "%-*s  %4d\n", width, "Total", sum.attempted)
	b.WriteString("```")
	return b.String()
}

// beanjaminAppURL is the customer-facing ordering app, linked from every digest
// so the channel has a way to act on it rather than only read it. One URL for
// the whole fleet, so it is a constant rather than a config field; if a second
// deployment ever needs a different one, that is when it earns a knob.
const beanjaminAppURL = "https://beanjamin_viam.viamapplications.com/"

// dailySummaryFooter prints the window the numbers actually cover, and links the
// ordering app. The window spans two dates, so both ends carry their weekday — a
// reader checking whether Saturday's orders were reported needs to see which
// days are in it.
func dailySummaryFooter(windowStart, now time.Time) map[string]any {
	return map[string]any{
		"type": "context",
		"elements": []any{map[string]any{
			"type": "mrkdwn",
			"text": fmt.Sprintf(":clock3: %s – %s  ·  <%s|order a coffee>",
				windowStart.Format("Mon 3:04 PM"), now.Format("Mon 3:04 PM MST"), beanjaminAppURL),
		}},
	}
}

// rankedKeys orders a map's keys by count descending, breaking ties by name so
// the same data always renders identically.
func rankedKeys[V any](m map[string]V, count func(V) int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ci, cj := count(m[keys[i]]), count(m[keys[j]])
		if ci != cj {
			return ci > cj
		}
		return keys[i] < keys[j]
	})
	return keys
}

// rankedCounts renders a count map as Slack bullets, most frequent first.
func rankedCounts(counts map[string]int) string {
	keys := rankedKeys(counts, func(n int) int { return n })
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = fmt.Sprintf("• %s — %d", k, counts[k])
	}
	return strings.Join(lines, "\n")
}

// rankedDrinks renders the per-drink breakdown, most ordered first, with each
// drink's own mean brew time. A drink whose every attempt failed shows its count
// without a timing rather than an invented zero.
func rankedDrinks(drinks map[string]drinkStats) string {
	keys := rankedKeys(drinks, func(d drinkStats) int { return d.ordered })
	lines := make([]string, len(keys))
	for i, k := range keys {
		stats := drinks[k]
		lines[i] = fmt.Sprintf("• %s — %d", k, stats.ordered)
		if avg, ok := stats.avgBrew(); ok {
			lines[i] += fmt.Sprintf(" _(avg %s)_", avg)
		}
	}
	return strings.Join(lines, "\n")
}
