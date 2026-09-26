package coffee

// Daily order digest: a Slack summary of one business day's orders, read back
// out of the cloud tabular store that the order sensor syncs into.
//
// Scheduling lives in the machine config's "jobs" block, not here — see README.
// A schedule string without a CRON_TZ= prefix fires in the host's timezone, not
// the one this command is passed.

import (
	"context"
	"fmt"
	"time"

	"go.viam.com/rdk/module"

	"beanjamin/coffee/report"
)

// dailySummaryTimeout caps the cloud query plus the Slack send. Cron jobs run
// in singleton mode, so a wedged digest would suppress every later firing until
// it returns.
const dailySummaryTimeout = 60 * time.Second

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
	windowStart := now.Add(-report.DailySummaryWindow)

	raw, err := s.QueryTabularDataForResource(ctx, s.cfg.OrderSensorName,
		&module.QueryTabularDataOptions{
			TimeBack:         report.DailySummaryWindow,
			AdditionalStages: report.DailySummaryStages(),
		})
	if err != nil {
		return nil, fmt.Errorf("querying today's orders from %q: %w", s.cfg.OrderSensorName, err)
	}

	sum := report.SummarizeOrders(report.DecodeOrderRows(raw, s.logger))
	streak, hasStreak := s.consecutiveSuccesses(ctx)

	s.logger.Infof("daily summary: %d orders between %s and %s",
		sum.Attempted(), windowStart.Format(time.RFC3339), now.Format(time.RFC3339))

	if err := report.PostDailySummary(ctx, s.slackNotifier, sum, streak, hasStreak, windowStart, now); err != nil {
		return nil, err
	}
	return map[string]any{"orders": float64(sum.Attempted()), "sent": true}, nil
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
