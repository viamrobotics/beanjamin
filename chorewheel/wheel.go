package chorewheel

// The rotation and the Slack message it posts. Who gets what is worked out from
// the date, so nothing is stored. Restarting the module or running the command
// twice gives the same answer.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"beanjamin/coffee/report"
)

// epoch is the Monday the week count starts from. Any fixed Monday
// works, but it has to be a Monday so weeks start when the job runs.
var epoch = time.Date(2026, time.January, 5, 0, 0, 0, 0, time.UTC)

// weekNumber returns how many weeks have passed since the epoch. It uses the
// local calendar date, so 9am Monday is the same week in every timezone.
func weekNumber(now time.Time) int {
	y, m, d := now.Date()
	local := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return int(local.Sub(epoch).Hours() / (24 * 7))
}

// choreAssignment is one person and the chores they got. Chores is empty if they
// have a free week.
type choreAssignment struct {
	Person string
	Chores []string
}

// assignChores works out who does what in the given week.
//
// The chores go into a list of slots. If there are fewer chores than people, the
// leftover slots are free weeks. If there are more, the list wraps around again
// so some people get two.
//
// Person i takes slot (i - week) mod n, and every n-th slot after that. The
// whole list shifts by one each week, so over n weeks everyone does every chore
// once.
//
// The order of people is the order of the wheel, so keep it stable in config.
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
		// Go's % can return a negative number, so add n to bring it back in range.
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

// messageText is the plain-text version Slack shows in notifications. It
// lists everything, so you can read your chore off a lock screen.
func messageText(assignments []choreAssignment, weekOf time.Time) string {
	var parts []string
	for _, a := range assignments {
		for _, c := range a.Chores {
			parts = append(parts, fmt.Sprintf("%s: %s", c, a.Person))
		}
	}
	return fmt.Sprintf("Chore wheel, week of %s — %s",
		weekOf.Format("Jan 2"), strings.Join(parts, "; "))
}

// messageBlocks builds the Slack message: a heading, then one line per chore,
// with anyone on a free week grouped on the last line.
func messageBlocks(assignments []choreAssignment, weekOf time.Time) []any {
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
		map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": strings.Join(lines, "\n")},
		},
	}
}

// post works out the assignment for the week containing now and sends
// it to Slack. The message is dated by that week's Monday, so running it on a
// Tuesday still shows the same date.
func post(ctx context.Context, n *report.Notifier, people, chores []string, now time.Time) (map[string]any, error) {
	week := weekNumber(now)
	assignments := assignChores(people, chores, week)

	// Step back to Monday. Weekday() counts Sunday as 0, so Sunday goes back 6.
	back := (int(now.Weekday()) + 6) % 7
	weekOf := now.AddDate(0, 0, -back)

	if _, err := n.Send(ctx, messageText(assignments, weekOf), messageBlocks(assignments, weekOf)); err != nil {
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

// parseTime reads the command argument. true means now. An object like
// {"date": "2026-09-21"} reads the wheel for that day instead.
func parseTime(v any, now time.Time) (time.Time, error) {
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
