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
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"text/template"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"
)

// dailySummaryTimeout caps the cloud query plus the Slack send. Cron jobs run
// in singleton mode, so a wedged digest would suppress every later firing until
// it returns.
const dailySummaryTimeout = 60 * time.Second

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

// sendDailySummary posts a Slack digest of every order recorded so far today,
// where "today" starts at midnight in the timezone named in the command
// ({"timezone": "America/New_York"}) and defaults to the host's timezone.
//
// ponytail: a failure here is only a returned error — the job manager logs it
// and records it in the job's history, but nothing reaches Slack. Silence in
// the channel therefore means "no digest ran" as well as "no orders". The
// always-posted no-orders line is what keeps that distinguishable by eye; if it
// stops being enough, post the error to Slack too.
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

	raw, err := s.QueryTabularDataForResource(ctx, s.cfg.OrderSensorName,
		&module.QueryTabularDataOptions{
			TimeBack:         now.Sub(dayStart),
			AdditionalStages: dailySummaryStages(),
		})
	if err != nil {
		return nil, fmt.Errorf("querying today's orders from %q: %w", s.cfg.OrderSensorName, err)
	}

	sum := summarizeOrders(decodeOrderRows(raw, s.logger))
	body, err := renderDailySummary(sum)
	if err != nil {
		return nil, err
	}

	s.logger.Infof("daily summary: %d orders between %s and %s (%s)",
		sum.Attempted, dayStart.Format(time.Kitchen), now.Format(time.Kitchen), loc)

	if _, err := s.slackNotifier.DoCommand(ctx, map[string]any{
		"command": "send",
		"text":    dailySummaryText(sum, now),
		"blocks":  dailySummaryBlocks(body, dayStart, now),
	}); err != nil {
		return nil, fmt.Errorf("sending daily summary to slack: %w", err)
	}
	return map[string]any{"orders": float64(sum.Attempted), "sent": true}, nil
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

// nameCount is one entry of a ranked breakdown, ordered for display.
type nameCount struct {
	Name  string
	Count int
}

// daySummary is one day's orders reduced to what the digest prints. Its fields
// are exported because the message template reads them directly.
type daySummary struct {
	Attempted   int
	Succeeded   int
	Faulted     int
	Cancelled   int
	Decaf       int
	Drinks      []nameCount
	FailedSteps []nameCount

	// brewTotal sums successful orders only: a fault that died at "Grinding"
	// would otherwise drag the average toward zero and read as a speed-up.
	brewTotal time.Duration
}

func (d daySummary) SuccessRate() float64 {
	if d.Attempted == 0 {
		return 0
	}
	return float64(d.Succeeded) / float64(d.Attempted) * 100
}

func (d daySummary) AvgBrew() time.Duration {
	if d.Succeeded == 0 {
		return 0
	}
	return (d.brewTotal / time.Duration(d.Succeeded)).Round(time.Second)
}

func (d daySummary) TotalBrew() time.Duration {
	return d.brewTotal.Round(time.Second)
}

// summarizeOrders reduces the day's readings. Kept a pure function over typed
// rows so the whole aggregation is testable without a cloud connection.
func summarizeOrders(rows []orderRow) daySummary {
	sum := daySummary{}
	drinks := map[string]int{}
	failedSteps := map[string]int{}

	for _, row := range rows {
		sum.Attempted++
		drink := row.Drink
		if drink == "" {
			drink = "unknown"
		}
		drinks[drink]++
		if row.Decaf {
			sum.Decaf++
		}

		switch {
		case row.OrderOK:
			sum.Succeeded++
			sum.brewTotal += time.Duration(row.DurationMs) * time.Millisecond
		// An operator stopping a run is not a fault; counting the two together
		// would make a busy day of manual cancels look like failing hardware.
		case row.OperatorCancelled:
			sum.Cancelled++
		default:
			sum.Faulted++
			step := row.FailedStep
			if step == "" {
				step = "an unknown step"
			}
			failedSteps[step]++
		}
	}

	sum.Drinks = ranked(drinks)
	sum.FailedSteps = ranked(failedSteps)
	return sum
}

// ranked orders a count map most-frequent-first, breaking ties by name so the
// same day always renders identically.
func ranked(counts map[string]int) []nameCount {
	out := make([]nameCount, 0, len(counts))
	for name, count := range counts {
		out = append(out, nameCount{Name: name, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// dailySummaryTmpl renders the digest body as Slack mrkdwn. The whole message
// layout lives here as text rather than as nested Block Kit maps, so the copy
// can be read and edited as the document it is.
var dailySummaryTmpl = template.Must(template.New("daily-summary").Parse(
	`{{- if eq .Attempted 0 -}}
No orders today.
{{- else -}}
*{{ .Attempted }} orders* · {{ .Succeeded }} succeeded ({{ printf "%.0f" .SuccessRate }}%) · {{ .Faulted }} faulted · {{ .Cancelled }} cancelled by an operator
{{ if gt .Succeeded 0 }}Average brew {{ .AvgBrew }}, {{ .TotalBrew }} of brewing in total.
{{ end }}
*Drinks*{{ if gt .Decaf 0 }} _({{ .Decaf }} decaf)_{{ end }}
{{ range .Drinks }}• {{ .Name }} — {{ .Count }}
{{ end }}
{{- if .FailedSteps }}
*Faults by step*
{{ range .FailedSteps }}• {{ .Name }} — {{ .Count }}
{{ end }}
{{- end -}}
{{- end -}}`))

// renderDailySummary renders the template into the message body. An error here
// means the template itself is broken, which is a bug rather than a bad day of
// data, so it fails the whole digest rather than posting something half-formed.
func renderDailySummary(sum daySummary) (string, error) {
	var b strings.Builder
	if err := dailySummaryTmpl.Execute(&b, sum); err != nil {
		return "", fmt.Errorf("rendering the daily summary: %w", err)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// dailySummaryText is the short line Slack shows in notifications and in the
// channel list. It stays a Sprintf rather than a second template: a push
// notification has room for one sentence, and the layout that earned a template
// is the body, not this.
func dailySummaryText(sum daySummary, day time.Time) string {
	if sum.Attempted == 0 {
		return fmt.Sprintf(":coffee: No orders on %s.", day.Format("Mon, Jan 2"))
	}
	return fmt.Sprintf(":coffee: %s: %d orders, %d succeeded, %d faulted, %d cancelled (%.0f%% success).",
		day.Format("Mon, Jan 2"), sum.Attempted, sum.Succeeded, sum.Faulted, sum.Cancelled, sum.SuccessRate())
}

// dailySummaryBlocks wraps the rendered body in Block Kit: a header, the body,
// and a footer naming the window. Returned as []any of map[string]any so it
// serializes through the structpb-backed DoCommand wire format, which rejects
// []map[string]any as a list value.
func dailySummaryBlocks(body string, dayStart, now time.Time) []any {
	// The window is the visible check on the timezone: if the job's CRON_TZ and
	// the command's timezone drift apart, the wrong hours show up here rather
	// than going unnoticed.
	window := fmt.Sprintf("%s – %s", dayStart.Format("3:04 PM"), now.Format("3:04 PM MST"))
	return []any{
		map[string]any{
			"type": "header",
			"text": map[string]any{
				"type":  "plain_text",
				"text":  ":coffee: Orders for " + now.Format("Monday, January 2"),
				"emoji": true,
			},
		},
		map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": body},
		},
		map[string]any{
			"type":     "context",
			"elements": []any{map[string]any{"type": "mrkdwn", "text": window}},
		},
	}
}
