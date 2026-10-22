package chorewheel

// The rotation and the Slack message it posts. Who is on duty is worked out
// from the date, so nothing is stored. Restarting the module or running the
// command twice gives the same answer.

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

// pickCrew returns the perWeek people on duty in the given week.
//
// The crew is the next perWeek people in roster order, wrapping around the end
// of the list: week w takes positions w*perWeek, w*perWeek+1, ... mod n. Each
// week starts where the last one stopped, so the crews tile the roster lap
// after lap and everyone is picked equally often. Nobody is picked two weeks
// running unless perWeek is more than half the roster.
//
// The order of people is the order of the wheel, so keep it stable in config.
func pickCrew(people []string, perWeek, week int) []string {
	n := len(people)
	// Go's % can return a negative number, so add n to bring it back in range.
	start := ((week*perWeek)%n + n) % n
	crew := make([]string, perWeek)
	for i := range crew {
		crew[i] = people[(start+i)%n]
	}
	return crew
}

// messageText is the plain-text version Slack shows in notifications. It
// lists everything, so you can read your duty off a lock screen. Slack
// resolves <@U…> mentions in this field too, so the crew is pinged by it.
func messageText(crew, chores []string, weekOf time.Time) string {
	return fmt.Sprintf("Chore wheel, week of %s — %s: %s",
		weekOf.Format("Jan 2"), strings.Join(crew, ", "), strings.Join(chores, "; "))
}

// messageBlocks builds the Slack message: a heading, the crew, then one line
// per chore. The crew stays out of the header because header blocks are
// plain_text and would print a mention in its raw <@U…> form; the mrkdwn
// section renders it as a ping.
func messageBlocks(crew, chores []string, weekOf time.Time) []any {
	lines := []string{fmt.Sprintf("*On duty:* %s", strings.Join(crew, ", ")), ""}
	for _, c := range chores {
		lines = append(lines, fmt.Sprintf("• %s", c))
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

// assignment is the result shape shared by Status and send_weekly_chores. The
// lists are []any because the structpb-backed DoCommand wire format rejects
// []string.
func assignment(week int, crew, chores []string) map[string]any {
	people := make([]any, len(crew))
	for i, p := range crew {
		people[i] = p
	}
	todo := make([]any, len(chores))
	for i, c := range chores {
		todo[i] = c
	}
	return map[string]any{"week": float64(week), "people": people, "chores": todo}
}

// post works out the crew for the week containing now and sends it to Slack.
// The message is dated by that week's Monday, so running it on a Tuesday still
// shows the same date.
func post(ctx context.Context, n *report.Notifier, people, chores []string, perWeek int, now time.Time) (map[string]any, error) {
	week := weekNumber(now)
	crew := pickCrew(people, perWeek, week)

	// Step back to Monday. Weekday() counts Sunday as 0, so Sunday goes back 6.
	back := (int(now.Weekday()) + 6) % 7
	weekOf := now.AddDate(0, 0, -back)

	if _, err := n.Send(ctx, messageText(crew, chores, weekOf), messageBlocks(crew, chores, weekOf)); err != nil {
		return nil, fmt.Errorf("sending chore wheel to slack: %w", err)
	}

	result := assignment(week, crew, chores)
	result["sent"] = true
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
