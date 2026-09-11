package coffee

// Daily order digest: a Slack summary of one business day's orders, read back
// out of the cloud tabular store that the order sensor syncs into.
//
// Scheduling deliberately does not live here. viam-server's job manager
// (robot/jobmanager) calls DoCommand({"send_daily_summary": {...}}) on a cron
// set in the machine config, so changing the hour is a config edit rather than
// a module redeploy. See README for the "jobs" entry — note in particular that
// a schedule string without a CRON_TZ= prefix fires in the *host's* timezone,
// not the one named here.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.viam.com/rdk/module"
)

// dailySummaryTimeout caps the cloud query plus the Slack send. Cron jobs run
// in singleton mode, so a wedged digest would suppress every later firing until
// it returns.
const dailySummaryTimeout = 60 * time.Second

// sendDailySummary posts a Slack digest of every order recorded so far today,
// where "today" starts at midnight in the timezone named in the command
// ({"timezone": "America/New_York"}) and defaults to the host's timezone.
//
// ponytail: a failure here is only a returned error — the job manager logs it
// and records it in the job's history, but nothing reaches Slack. Silence in
// the channel therefore means "no digest ran" as well as "no orders". The
// always-posted no-orders line below is what keeps that distinguishable by eye;
// if it stops being enough, post the error to Slack too.
func (s *beanjaminCoffee) sendDailySummary(ctx context.Context, arg any) (map[string]any, error) {
	if s.slackNotifier == nil {
		return nil, fmt.Errorf("send_daily_summary requires slack_notifier_name to be configured")
	}
	if s.cfg.OrderSensorName == "" {
		return nil, fmt.Errorf("send_daily_summary requires order_sensor_name to be configured (it names the component the digest queries)")
	}

	loc, err := summaryLocation(arg)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, dailySummaryTimeout)
	defer cancel()

	now := time.Now().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	rows, err := s.QueryTabularDataForResource(ctx, s.cfg.OrderSensorName,
		&module.QueryTabularDataOptions{
			TimeBack:         now.Sub(dayStart),
			AdditionalStages: dailySummaryStages(),
		})
	if err != nil {
		return nil, fmt.Errorf("querying today's orders from %q: %w", s.cfg.OrderSensorName, err)
	}

	sum := summarizeOrders(rows)
	blocks := dailySummaryBlocks(sum, dayStart, now, s.machineLogsURL)

	s.logger.Infof("daily summary: %d orders between %s and %s (%s)",
		sum.attempted, dayStart.Format(time.Kitchen), now.Format(time.Kitchen), loc)

	if _, err := s.slackNotifier.DoCommand(ctx, map[string]any{
		"command": "send",
		"text":    dailySummaryText(sum, now),
		"blocks":  blocks,
	}); err != nil {
		return nil, fmt.Errorf("sending daily summary to slack: %w", err)
	}
	return map[string]any{"orders": float64(sum.attempted), "sent": true}, nil
}

// summaryLocation resolves the timezone the business day is measured in. The
// host's zone is the fallback because a machine sitting in the office is
// normally set to the office's zone — but the digest always prints the window it
// used, so a mismatch with the job's CRON_TZ is visible in the message itself
// rather than silently slicing the day at the wrong hour.
func summaryLocation(arg any) (*time.Location, error) {
	m, ok := arg.(map[string]any)
	if !ok {
		return time.Local, nil
	}
	name, _ := m["timezone"].(string)
	if strings.TrimSpace(name) == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", name, err)
	}
	return loc, nil
}

// dailySummaryStages projects the order-sensor fields flat. The raw tabular
// document nests the reading under data.readings (which is why this has to be
// MQL — SQL can't resolve that path), and flattening it here keeps the
// aggregation below working on a plain map.
func dailySummaryStages() []map[string]any {
	return []map[string]any{
		{"$project": map[string]any{
			"_id":                0,
			"drink":              "$data.readings.drink",
			"order_ok":           "$data.readings.order_ok",
			"operator_cancelled": "$data.readings.operator_cancelled",
			"failed_step":        "$data.readings.failed_step",
			"decaf":              "$data.readings.decaf",
			"duration_ms":        "$data.readings.duration_ms",
		}},
	}
}

// daySummary is one day's orders reduced to what the digest prints.
type daySummary struct {
	attempted   int
	succeeded   int
	faulted     int
	cancelled   int
	decaf       int
	drinks      map[string]int
	failedSteps map[string]int
	// brewTotal sums successful orders only: a fault that died at "Grinding"
	// would otherwise drag the average toward zero and read as a speed-up.
	brewTotal time.Duration
}

func (d daySummary) successRate() float64 {
	if d.attempted == 0 {
		return 0
	}
	return float64(d.succeeded) / float64(d.attempted) * 100
}

func (d daySummary) avgBrew() time.Duration {
	if d.succeeded == 0 {
		return 0
	}
	return (d.brewTotal / time.Duration(d.succeeded)).Round(time.Second)
}

// summarizeOrders reduces the projected query rows. Kept a pure function over
// plain maps so the whole aggregation is testable without a cloud connection.
func summarizeOrders(rows []map[string]any) daySummary {
	sum := daySummary{
		drinks:      map[string]int{},
		failedSteps: map[string]int{},
	}
	for _, row := range rows {
		sum.attempted++
		drink, _ := row["drink"].(string)
		if drink == "" {
			drink = "unknown"
		}
		sum.drinks[drink]++
		if decaf, _ := row["decaf"].(bool); decaf {
			sum.decaf++
		}

		if ok, _ := row["order_ok"].(bool); ok {
			sum.succeeded++
			if ms, valid := numericReading(row["duration_ms"]); valid {
				sum.brewTotal += time.Duration(ms) * time.Millisecond
			}
			continue
		}
		// An operator stopping a run is not a fault; counting the two together
		// would make a busy day of manual cancels look like failing hardware.
		if cancelled, _ := row["operator_cancelled"].(bool); cancelled {
			sum.cancelled++
			continue
		}
		sum.faulted++
		step, _ := row["failed_step"].(string)
		if step == "" {
			step = "an unknown step"
		}
		sum.failedSteps[step]++
	}
	return sum
}

// dailySummaryText is the plain-text fallback Slack shows in notifications and
// when Block Kit can't render.
func dailySummaryText(sum daySummary, day time.Time) string {
	if sum.attempted == 0 {
		return fmt.Sprintf(":coffee: No orders on %s.", day.Format("Mon, Jan 2"))
	}
	return fmt.Sprintf(":coffee: %s: %d orders, %d succeeded, %d faulted, %d cancelled (%.0f%% success).",
		day.Format("Mon, Jan 2"), sum.attempted, sum.succeeded, sum.faulted, sum.cancelled, sum.successRate())
}

// dailySummaryBlocks renders the digest as Block Kit. Returned as []any of
// map[string]any so it serializes through the structpb-backed DoCommand wire
// format, which rejects []map[string]any as a list value.
func dailySummaryBlocks(sum daySummary, dayStart, now time.Time, machineLogsURL string) []any {
	blocks := []any{
		map[string]any{
			"type": "header",
			"text": map[string]any{
				"type":  "plain_text",
				"text":  ":coffee: Orders for " + now.Format("Monday, January 2"),
				"emoji": true,
			},
		},
	}

	if sum.attempted == 0 {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": "No orders today."},
		})
		return append(blocks, dailySummaryFooter(dayStart, now, machineLogsURL))
	}

	fields := []any{
		slackField("*Orders:*", fmt.Sprintf("%d", sum.attempted)),
		slackField("*Succeeded:*", fmt.Sprintf("%d (%.0f%%)", sum.succeeded, sum.successRate())),
		slackField("*Faulted:*", fmt.Sprintf("%d", sum.faulted)),
		slackField("*Cancelled by operator:*", fmt.Sprintf("%d", sum.cancelled)),
	}
	if sum.succeeded > 0 {
		fields = append(fields,
			slackField("*Avg brew:*", sum.avgBrew().String()),
			slackField("*Total brewing:*", sum.brewTotal.Round(time.Second).String()))
	}
	if sum.decaf > 0 {
		fields = append(fields, slackField("*Decaf:*", fmt.Sprintf("%d", sum.decaf)))
	}
	blocks = append(blocks, map[string]any{"type": "section", "fields": fields})

	blocks = append(blocks, map[string]any{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": "*Drinks*\n" + rankedCounts(sum.drinks)},
	})

	if len(sum.failedSteps) > 0 {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": "*Faults by step*\n" + rankedCounts(sum.failedSteps)},
		})
	}

	return append(blocks, dailySummaryFooter(dayStart, now, machineLogsURL))
}

// dailySummaryFooter prints the window the numbers actually cover. It is the
// check on the timezone: if the job's CRON_TZ and the command's timezone drift
// apart, the wrong hours show up here rather than going unnoticed.
func dailySummaryFooter(dayStart, now time.Time, machineLogsURL string) map[string]any {
	footer := fmt.Sprintf("%s – %s", dayStart.Format("3:04 PM"), now.Format("3:04 PM MST"))
	if machineLogsURL != "" {
		footer += fmt.Sprintf(" · <%s|machine logs>", machineLogsURL)
	}
	return map[string]any{
		"type":     "context",
		"elements": []any{map[string]any{"type": "mrkdwn", "text": footer}},
	}
}

// rankedCounts renders a count map as Slack bullets, most frequent first with
// ties broken by name so the same day always renders identically.
func rankedCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = fmt.Sprintf("• %s — %d", k, counts[k])
	}
	return strings.Join(lines, "\n")
}
