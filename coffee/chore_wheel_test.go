package coffee

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	testPeople = []string{"Vijay", "Nicolas P", "Julie", "Daniel", "Cheuk", "Ale"}
	testChores = []string{"Cleaning the ice maker", "Cleaning the table", "Tightening the claws"}
)

// Over one full cycle every person draws every chore exactly once and takes
// the same number of free weeks. This is the property the whole design rests
// on, so it is checked exhaustively rather than by example.
func TestAssignChoresIsFairOverACycle(t *testing.T) {
	n := len(testPeople)
	seen := map[string]map[string]int{}
	for _, p := range testPeople {
		seen[p] = map[string]int{}
	}

	for week := 0; week < n; week++ {
		for _, a := range assignChores(testPeople, testChores, week) {
			seen[a.Person][a.Chore]++
		}
	}

	freeWeeks := n - len(testChores)
	for _, p := range testPeople {
		for _, c := range testChores {
			if seen[p][c] != 1 {
				t.Errorf("%s did %q %d times in a cycle, want exactly 1", p, c, seen[p][c])
			}
		}
		if seen[p][""] != freeWeeks {
			t.Errorf("%s had %d free weeks, want %d", p, seen[p][""], freeWeeks)
		}
	}
}

// Each week every chore is assigned to exactly one person and nobody holds
// two — the message would be nonsense otherwise.
func TestAssignChoresCoversEveryChoreOnce(t *testing.T) {
	for week := -3; week < 20; week++ {
		holders := map[string]string{}
		for _, a := range assignChores(testPeople, testChores, week) {
			if a.Chore == "" {
				continue
			}
			if prev, dup := holders[a.Chore]; dup {
				t.Errorf("week %d: %q assigned to both %s and %s", week, a.Chore, prev, a.Person)
			}
			holders[a.Chore] = a.Person
		}
		if len(holders) != len(testChores) {
			t.Errorf("week %d: %d chores assigned, want %d", week, len(holders), len(testChores))
		}
	}
}

// The wheel is a pure function of the week, so the same week always reads the
// same, and consecutive weeks differ — a stuck wheel is the failure to catch.
func TestAssignChoresIsDeterministicAndAdvances(t *testing.T) {
	a := assignChores(testPeople, testChores, 7)
	b := assignChores(testPeople, testChores, 7)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("week 7 read twice disagrees: %v vs %v", a[i], b[i])
		}
	}
	c := assignChores(testPeople, testChores, 8)
	same := true
	for i := range a {
		if a[i] != c[i] {
			same = false
		}
	}
	if same {
		t.Error("week 7 and week 8 produced the same assignment; the wheel did not turn")
	}
}

// Negative weeks (a manual run dated before the epoch) must wrap rather than
// index out of range — Go's % keeps the dividend's sign.
func TestAssignChoresHandlesNegativeWeek(t *testing.T) {
	got := assignChores(testPeople, testChores, -1)
	if len(got) != len(testPeople) {
		t.Fatalf("got %d assignments, want %d", len(got), len(testPeople))
	}
	// -1 mod 6 is 5, so person 0 gets slot 1, person 1 slot 2, etc.
	if got[0].Chore != testChores[1] {
		t.Errorf("week -1: %s got %q, want %q", got[0].Person, got[0].Chore, testChores[1])
	}
}

// The week number is taken from the local calendar date, so 9am on a Monday
// anywhere in the world is the same week as 9am UTC that Monday, and Sunday
// evening in New York does not tip over into UTC's Monday.
func TestChoreWheelWeekUsesLocalDate(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	mondayNY := time.Date(2026, time.September, 21, 9, 0, 0, 0, ny)
	mondayUTC := time.Date(2026, time.September, 21, 9, 0, 0, 0, time.UTC)
	if choreWheelWeek(mondayNY) != choreWheelWeek(mondayUTC) {
		t.Errorf("same Monday in NY and UTC gave weeks %d and %d",
			choreWheelWeek(mondayNY), choreWheelWeek(mondayUTC))
	}

	// 11pm Sunday in New York is 03:00 Monday UTC. It belongs to the week
	// that is ending, not the one starting.
	sundayNightNY := time.Date(2026, time.September, 20, 23, 0, 0, 0, ny)
	if choreWheelWeek(sundayNightNY) != choreWheelWeek(mondayNY)-1 {
		t.Errorf("Sunday 11pm NY is week %d, Monday 9am NY is week %d; want consecutive",
			choreWheelWeek(sundayNightNY), choreWheelWeek(mondayNY))
	}

	// Consecutive Mondays are consecutive weeks.
	if choreWheelWeek(mondayNY.AddDate(0, 0, 7)) != choreWheelWeek(mondayNY)+1 {
		t.Error("a Monday and the next Monday are not consecutive weeks")
	}
}

type fakeSlack struct {
	sent []map[string]any
	err  error
}

func (f *fakeSlack) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	f.sent = append(f.sent, cmd)
	return nil, f.err
}

func TestPostChoreWheelRendersAndReports(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	cfg := &ChoreWheelConfig{People: testPeople, Chores: testChores}
	slack := &fakeSlack{}

	// A Tuesday: the message must still be dated by its Monday.
	tuesday := time.Date(2026, time.September, 22, 14, 0, 0, 0, ny)
	res, err := postChoreWheel(context.Background(), slack, cfg, tuesday)
	if err != nil {
		t.Fatal(err)
	}
	if len(slack.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(slack.sent))
	}
	msg := slack.sent[0]
	if msg["command"] != "send" {
		t.Errorf("command = %v, want send", msg["command"])
	}

	text, _ := msg["text"].(string)
	if !strings.Contains(text, "week of Sep 21") {
		t.Errorf("fallback text %q should be dated by Monday Sep 21", text)
	}
	for _, c := range testChores {
		if !strings.Contains(text, c) {
			t.Errorf("fallback text missing chore %q: %q", c, text)
		}
	}

	blocks, _ := msg["blocks"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want header and section", len(blocks))
	}
	section := blocks[1].(map[string]any)["text"].(map[string]any)["text"].(string)
	if !strings.Contains(section, "_free week_") {
		t.Errorf("section should group the three free weeks: %q", section)
	}
	if got := strings.Count(section, "•"); got != len(testChores)+1 {
		t.Errorf("section has %d lines, want %d chores + 1 free line", got, len(testChores)+1)
	}

	// The result echoes the assignment so a caller (or the job history) can
	// see what was posted without reading Slack.
	if res["sent"] != true {
		t.Errorf("result.sent = %v, want true", res["sent"])
	}
	for _, c := range testChores {
		if _, ok := res[c].(string); !ok {
			t.Errorf("result missing assignment for %q: %v", c, res)
		}
	}
}

// With as many chores as people, nobody is free and the free line is omitted
// rather than printed empty.
func TestPostChoreWheelNoFreeWeeks(t *testing.T) {
	people := []string{"A", "B", "C"}
	chores := []string{"x", "y", "z"}
	slack := &fakeSlack{}
	if _, err := postChoreWheel(context.Background(), slack,
		&ChoreWheelConfig{People: people, Chores: chores}, time.Now()); err != nil {
		t.Fatal(err)
	}
	section := slack.sent[0]["blocks"].([]any)[1].(map[string]any)["text"].(map[string]any)["text"].(string)
	if strings.Contains(section, "free week") {
		t.Errorf("no free weeks expected, got %q", section)
	}
}

func TestPostChoreWheelPropagatesSendError(t *testing.T) {
	slack := &fakeSlack{err: errors.New("channel_not_found")}
	_, err := postChoreWheel(context.Background(), slack,
		&ChoreWheelConfig{People: testPeople, Chores: testChores}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("err = %v, want the slack error wrapped", err)
	}
}

func TestParseChoreWheelTime(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)

	if got, err := parseChoreWheelTime(true, now); err != nil || !got.Equal(now) {
		t.Errorf("true -> (%v, %v), want now", got, err)
	}
	if got, err := parseChoreWheelTime(map[string]any{}, now); err != nil || !got.Equal(now) {
		t.Errorf("{} -> (%v, %v), want now", got, err)
	}
	got, err := parseChoreWheelTime(map[string]any{"date": "2026-10-05"}, now)
	if err != nil || got.Format("2006-01-02") != "2026-10-05" {
		t.Errorf("date override -> (%v, %v), want 2026-10-05", got, err)
	}
	if _, err := parseChoreWheelTime(map[string]any{"date": "next monday"}, now); err == nil {
		t.Error("unparseable date should error")
	}
	if _, err := parseChoreWheelTime("yes", now); err == nil {
		t.Error("a string value should error")
	}
}

func TestChoreWheelConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ChoreWheelConfig
		bad  bool
	}{
		{"ok with free weeks", ChoreWheelConfig{People: testPeople, Chores: testChores}, false},
		{"ok fully assigned", ChoreWheelConfig{People: []string{"A", "B"}, Chores: []string{"x", "y"}}, false},
		{"one person", ChoreWheelConfig{People: []string{"A"}, Chores: []string{"x"}}, true},
		{"no chores", ChoreWheelConfig{People: []string{"A", "B"}}, true},
		{"more chores than people", ChoreWheelConfig{People: []string{"A", "B"}, Chores: []string{"x", "y", "z"}}, true},
		{"duplicate person", ChoreWheelConfig{People: []string{"A", "A"}, Chores: []string{"x"}}, true},
		{"blank chore", ChoreWheelConfig{People: []string{"A", "B"}, Chores: []string{" "}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate("test")
			if (err != nil) != tc.bad {
				t.Errorf("validate() = %v, want error=%v", err, tc.bad)
			}
		})
	}
}
