package coffee

// Weekly chore wheel: one Slack message every Monday naming who has which
// maintenance chore this week.
//
// It is the paper chore wheel — names on an inner disc, chores on an outer ring
// — turned one notch a week. The rotation is a pure function of the calendar, so
// there is nothing to persist: restarts, redeploys and repeat runs all say the
// same thing.
//
// Scheduling lives in the machine config's "jobs" block, not here. See README.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// choreWheelTimeout caps the Slack send. Cron jobs are singletons, so a wedged
// post would suppress every later firing.
const choreWheelTimeout = 15 * time.Second

// choreWheelEpoch anchors the week counter. Any fixed Monday works; a Monday
// keeps week boundaries where the job fires rather than mid-week.
var choreWheelEpoch = time.Date(2026, time.January, 5, 0, 0, 0, 0, time.UTC)

// choreWheelWeek is the number of whole weeks since the epoch, counted from the
// local calendar date so 9am Monday is the same week in any timezone.
func choreWheelWeek(now time.Time) int {
	y, m, d := now.Date()
	local := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return int(local.Sub(choreWheelEpoch).Hours() / (24 * 7))
}

// choreAssignment is one person and the chores they drew. Chores is empty for a
// free week.
type choreAssignment struct {
	Person string
	Chores []string
}

// assignChores turns the wheel `week` notches and reads it off. Person i draws
// slot (i - week) mod n and every n-th slot after that, where the slots are the
// chores padded with free weeks to a whole number of turns around the wheel.
//
// Fewer chores than people means one turn: one slot each, and the people past
// the last chore get a free week. More chores than people means the wheel goes
// round again, so some people draw two. Either way each person sees every slot
// exactly once over n weeks, so everyone does every chore once per cycle.
//
// The order of people is the order on the wheel and in the message, so keep it
// stable in config.
func assignChores(people, chores []string, week int) []choreAssignment {
	n := len(people)
	turns := (len(chores) + n - 1) / n
	if turns < 1 {
		turns = 1
	}
	slots := make([]string, turns*n)
	copy(slots, chores)

	out := make([]choreAssignment, n)
	for i, who := range people {
		// Go's % keeps the dividend's sign, so normalise for negative weeks.
		start := ((i-week)%n + n) % n
		var drawn []string
		for t := 0; t < turns; t++ {
			if c := slots[start+t*n]; c != "" {
				drawn = append(drawn, c)
			}
		}
		out[i] = choreAssignment{Person: who, Chores: drawn}
	}
	return out
}

// choreWheelText is the flat fallback Slack shows in notification previews. It
// carries the whole assignment so a lock screen is enough to know your chore.
func choreWheelText(assignments []choreAssignment, weekOf time.Time) string {
	var parts []string
	for _, a := range assignments {
		for _, c := range a.Chores {
			parts = append(parts, fmt.Sprintf("%s: %s", c, a.Person))
		}
	}
	return fmt.Sprintf("Chore wheel, week of %s — %s",
		weekOf.Format("Jan 2"), strings.Join(parts, "; "))
}

// choreWheelBlocks renders the assignment as Block Kit: a header, then one line
// per chore with free weeks grouped on a final line.
func choreWheelBlocks(assignments []choreAssignment, weekOf time.Time) []any {
	var lines, free []string
	for _, a := range assignments {
		if len(a.Chores) == 0 {
			free = append(free, a.Person)
			continue
		}
		for _, c := range a.Chores {
			lines = append(lines, fmt.Sprintf("• *%s* — %s", c, a.Person))
		}
	}
	if len(free) > 0 {
		lines = append(lines, fmt.Sprintf("• _free week_ — %s", strings.Join(free, ", ")))
	}

	return []any{
		map[string]any{
			"type": "header",
			"text": map[string]any{
				"type":  "plain_text",
				"text":  fmt.Sprintf("🎡 Chore wheel — week of %s", weekOf.Format("Jan 2")),
				"emoji": true,
			},
		},
		mrkdwnSection(strings.Join(lines, "\n")),
	}
}

// slackSender is the one notifier method the wheel needs, narrowed so the send
// path can be tested without a real Slack service.
type slackSender interface {
	DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error)
}

// postChoreWheel computes the assignment for the week containing `now` and sends
// it. The message is dated by that week's Monday, so a Tuesday re-run agrees.
func postChoreWheel(ctx context.Context, sender slackSender, cfg *ChoreWheelConfig, now time.Time) (map[string]any, error) {
	week := choreWheelWeek(now)
	assignments := assignChores(cfg.People, cfg.Chores, week)

	// Back up to Monday. Weekday() is Sunday=0, so Sunday backs up 6 days.
	back := (int(now.Weekday()) + 6) % 7
	weekOf := now.AddDate(0, 0, -back)

	if _, err := sender.DoCommand(ctx, map[string]any{
		"command": "send",
		"text":    choreWheelText(assignments, weekOf),
		"blocks":  choreWheelBlocks(assignments, weekOf),
	}); err != nil {
		return nil, fmt.Errorf("sending chore wheel to slack: %w", err)
	}

	result := map[string]any{"sent": true, "week": float64(week)}
	for _, a := range assignments {
		for _, c := range a.Chores {
			result[c] = a.Person
		}
	}
	return result, nil
}

// parseChoreWheelTime reads the DoCommand value: `true` means now, an object
// with "date": "2026-09-21" reads the wheel for that day instead.
func parseChoreWheelTime(v any, now time.Time) (time.Time, error) {
	switch x := v.(type) {
	case bool:
		return now, nil
	case map[string]any:
		raw, ok := x["date"]
		if !ok {
			return now, nil
		}
		s, ok := raw.(string)
		if !ok {
			return time.Time{}, fmt.Errorf("send_weekly_chores.date must be a YYYY-MM-DD string")
		}
		t, err := time.ParseInLocation("2006-01-02", s, now.Location())
		if err != nil {
			return time.Time{}, fmt.Errorf("send_weekly_chores.date: %w", err)
		}
		return t, nil
	default:
		return time.Time{}, fmt.Errorf("send_weekly_chores must be true or an object")
	}
}

// sendWeeklyChores is the DoCommand entry point. Normally cron-fired from the
// machine config's "jobs" block; safe to call by hand to post off-schedule.
func (s *beanjaminCoffee) sendWeeklyChores(ctx context.Context, v any) (map[string]any, error) {
	if s.cfg.ChoreWheel == nil {
		return nil, fmt.Errorf("send_weekly_chores requires chore_wheel to be configured")
	}
	if s.slackNotifier == nil {
		return nil, fmt.Errorf("send_weekly_chores requires slack_notifier_name to be configured")
	}

	now, err := parseChoreWheelTime(v, time.Now())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, choreWheelTimeout)
	defer cancel()

	s.logger.Infof("chore wheel: posting week %d assignments", choreWheelWeek(now))
	return postChoreWheel(ctx, s.slackNotifier, s.cfg.ChoreWheel, now)
}
