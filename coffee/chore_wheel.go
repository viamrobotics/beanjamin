package coffee

import (
	"context"
	"fmt"
	"time"

	"beanjamin/coffee/report"
)

// choreWheelTimeout caps the Slack send. Only one cron job runs at a time, so a
// stuck post would block every later one.
const choreWheelTimeout = 15 * time.Second

// sendWeeklyChores handles the send_weekly_chores command. A cron job normally
// runs it, but you can also call it by hand.
func (s *beanjaminCoffee) sendWeeklyChores(ctx context.Context, v any) (map[string]any, error) {
	if s.cfg.ChoreWheel == nil {
		return nil, fmt.Errorf("send_weekly_chores requires chore_wheel to be configured")
	}
	if s.slackNotifier == nil {
		return nil, fmt.Errorf("send_weekly_chores requires slack_notifier_name to be configured")
	}

	now, err := report.ParseChoreWheelTime(v, time.Now())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, choreWheelTimeout)
	defer cancel()

	s.logger.Infof("chore wheel: posting week %d assignments", report.ChoreWheelWeek(now))
	return report.PostChoreWheel(ctx, s.slackNotifier, s.cfg.ChoreWheel, now)
}
