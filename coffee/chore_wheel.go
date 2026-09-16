package coffee

// Weekly chore wheel: one Slack message every Monday naming who has which
// maintenance chore this week.
//
// It is the paper chore wheel — two discs, names on the inner one and chores on
// the outer one — turned one notch a week. The rotation is a pure function of
// the calendar, so there is nothing to persist and nothing to get out of sync:
// restart the module, redeploy it, run the command twice in one morning, and it
// says the same thing. Over a full cycle (one week per person) everyone does
// every chore exactly once and takes the same number of free weeks.
//
// Scheduling lives in the machine config's "jobs" block, not here — see README.
// A schedule string without a CRON_TZ= prefix fires in the host's timezone.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// choreWheelTimeout caps the Slack send. Cron jobs run in singleton mode, so a
// wedged post would suppress every later firing until it returned.
const choreWheelTimeout = 15 * time.Second

// choreWheelEpoch anchors the week counter. Any fixed Monday works; this one is
// chosen so the counter is small and positive for the life of the machine. It
// is a Monday so that "week N" boundaries fall where the job fires rather than
// mid-week, which would let a Sunday-night manual run disagree with Monday's.
var choreWheelEpoch = time.Date(2026, time.January, 5, 0, 0, 0, 0, time.UTC)

// choreWheelWeek is the number of whole weeks since the epoch in the given
// location. Taking the calendar date in the machine's own zone (rather than
// UTC) is what keeps a 9am New York firing and a 9am London firing on the same
// Monday in the same week; a naive Unix-seconds division would put an early
// Sunday-evening run in New York into UTC's Monday.
func choreWheelWeek(now time.Time) int {
	y, m, d := now.Date()
	local := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return int(local.Sub(choreWheelEpoch).Hours() / (24 * 7))
}

// choreAssignment is one person and the chore they drew. Chore is "" for a
// free week, which the renderer groups onto one line rather than repeating.
type choreAssignment struct {
	Person string
	Chore  string
}

// assignChores turns the wheel `week` notches and reads it off. Person i gets
// slot (i - week) mod n, where the slots are the chores in order followed by
// enough free weeks to make one slot per person. Because every person sees
// every slot exactly once as week runs 0..n-1, the rotation is fair by
// construction — no history, no bookkeeping.
//
// The order of people is the order on the wheel and the order in the message,
// so it should be stable in config; reordering the roster reshuffles who has
// what this week.
func assignChores(people, chores []string, week int) []choreAssignment {
	n := len(people)
	slots := make([]string, n)
	copy(slots, chores)

	out := make([]choreAssignment, n)
	for i, who := range people {
		// Go's % keeps the sign of the dividend, so normalise for negative weeks
		// (a manual run dated before the epoch) rather than index out of range.
		k := ((i-week)%n + n) % n
		out[i] = choreAssignment{Person: who, Chore: slots[k]}
	}
	return out
}

// choreWheelText is the flat fallback Slack shows in notification previews and
// wherever blocks can't render. It carries the whole assignment so a phone
// lock-screen is enough to know your chore.
func choreWheelText(assignments []choreAssignment, weekOf time.Time) string {
	parts := make([]string, 0, len(assignments))
	for _, a := range assignments {
		if a.Chore == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", a.Chore, a.Person))
	}
	return fmt.Sprintf("Chore wheel, week of %s — %s",
		weekOf.Format("Jan 2"), strings.Join(parts, "; "))
}

// choreWheelBlocks lays the assignment out as Block Kit: a header with the
// week, then one line per chore with the free weeks grouped on a final line.
// Just the assignment — how the rotation works is documented in the README, not
// repeated in every week's message.
func choreWheelBlocks(assignments []choreAssignment, weekOf time.Time) []any {
	var lines, free []string
	for _, a := range assignments {
		if a.Chore == "" {
			free = append(free, a.Person)
			continue
		}
		lines = append(lines, fmt.Sprintf("• *%s* — %s", a.Chore, a.Person))
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

// slackSender is the one method of the notifier the wheel needs. Narrowing the
// dependency to an interface is what lets the send path be tested without a
// viam:notifications:slack service in the loop.
type slackSender interface {
	DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error)
}

// postChoreWheel computes this week's assignment and sends it. `now` is the
// moment the wheel is read; the message is dated by the Monday of that week so
// a Tuesday re-run still says "week of" the same date.
func postChoreWheel(ctx context.Context, sender slackSender, cfg *ChoreWheelConfig, now time.Time) (map[string]any, error) {
	week := choreWheelWeek(now)
	assignments := assignChores(cfg.People, cfg.Chores, week)

	// Back up to Monday. Weekday() is Sunday=0, so Monday's offset is 0 and
	// Sunday's is 6 — a Sunday run is the tail of the week that started six
	// days earlier, not the head of the next one.
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
		if a.Chore != "" {
			result[a.Chore] = a.Person
		}
	}
	return result, nil
}

// parseChoreWheelTime accepts the DoCommand value: `true` reads the wheel now;
// an object with `"date": "2026-09-21"` reads it for that day instead, which is
// how you preview a future week or confirm what last Monday should have said.
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

// --- repo-bound below this line -------------------------------------------

// sendWeeklyChores is the DoCommand entry point. Normally fired by a cron entry
// in the machine config's "jobs" block; safe to call by hand to post off-schedule.
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
