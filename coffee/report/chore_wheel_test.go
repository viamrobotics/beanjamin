package report

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var (
	testPeople = []string{"Vijay", "Nicolas P", "Julie", "Daniel", "Cheuk", "Ale"}
	testChores = []string{"Cleaning the ice maker", "Cleaning the table", "Tightening the claws"}
)

// Over a full cycle everyone should do every chore exactly once. This is the
// main thing the design promises, so check it for a few different list sizes.
func TestAssignChoresIsFairOverACycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chores []string
	}{
		{"fewer chores than people", testChores},
		{"one chore per person", testPeople},
		{"more chores than people", append(append([]string{}, testChores...),
			"Descaling", "Restocking cups", "Emptying the grounds", "Wiping the wand")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := len(testPeople)
			seen := map[string]map[string]int{}
			free := map[string]int{}
			for _, p := range testPeople {
				seen[p] = map[string]int{}
			}

			for week := 0; week < n; week++ {
				for _, a := range assignChores(testPeople, tc.chores, week) {
					if len(a.Chores) == 0 {
						free[a.Person]++
					}
					for _, c := range a.Chores {
						seen[a.Person][c]++
					}
				}
			}

			for _, p := range testPeople {
				for _, c := range tc.chores {
					if seen[p][c] != 1 {
						t.Errorf("%s did %q %d times in a cycle, want exactly 1", p, c, seen[p][c])
					}
				}
			}
			// Free weeks should be shared out evenly too.
			want := free[testPeople[0]]
			for _, p := range testPeople {
				if free[p] != want {
					t.Errorf("%s had %d free weeks, %s had %d — should be equal",
						p, free[p], testPeople[0], want)
				}
			}
		})
	}
}

// Every chore should go to exactly one person each week.
func TestAssignChoresCoversEveryChoreOnce(t *testing.T) {
	for week := -3; week < 20; week++ {
		holders := map[string]string{}
		for _, a := range assignChores(testPeople, testChores, week) {
			for _, c := range a.Chores {
				if prev, dup := holders[c]; dup {
					t.Errorf("week %d: %q assigned to both %s and %s", week, c, prev, a.Person)
				}
				holders[c] = a.Person
			}
		}
		if len(holders) != len(testChores) {
			t.Errorf("week %d: %d chores assigned, want %d", week, len(holders), len(testChores))
		}
	}
}

// With more chores than people, someone does two that week instead of a chore
// being skipped.
func TestAssignChoresMoreChoresThanPeople(t *testing.T) {
	people := []string{"A", "B", "C"}
	chores := []string{"v", "w", "x", "y", "z"}

	got := assignChores(people, chores, 0)
	holders := map[string]string{}
	doubled := 0
	for _, a := range got {
		if len(a.Chores) > 1 {
			doubled++
		}
		for _, c := range a.Chores {
			holders[c] = a.Person
		}
	}
	if len(holders) != len(chores) {
		t.Errorf("assigned %d of %d chores: %v", len(holders), len(chores), got)
	}
	if doubled != 2 {
		t.Errorf("%d people drew two chores, want 2 (5 chores, 3 people)", doubled)
	}
}

// The same week always gives the same answer, and the next week gives a
// different one. A wheel that never turns is the bug to catch.
func TestAssignChoresIsDeterministicAndAdvances(t *testing.T) {
	flat := func(as []choreAssignment) string {
		var b strings.Builder
		for _, a := range as {
			fmt.Fprintf(&b, "%s=%s;", a.Person, strings.Join(a.Chores, ","))
		}
		return b.String()
	}
	if a, b := flat(assignChores(testPeople, testChores, 7)), flat(assignChores(testPeople, testChores, 7)); a != b {
		t.Fatalf("week 7 read twice disagrees:\n%s\n%s", a, b)
	}
	if a, c := flat(assignChores(testPeople, testChores, 7)), flat(assignChores(testPeople, testChores, 8)); a == c {
		t.Error("week 7 and week 8 produced the same assignment; the wheel did not turn")
	}
}

// A date before the epoch gives a negative week. It should wrap around instead
// of going out of range.
func TestAssignChoresHandlesNegativeWeek(t *testing.T) {
	got := assignChores(testPeople, testChores, -1)
	if len(got) != len(testPeople) {
		t.Fatalf("got %d assignments, want %d", len(got), len(testPeople))
	}
	// -1 mod 6 is 5, so person 0 gets slot 1, person 1 slot 2, etc.
	if len(got[0].Chores) != 1 || got[0].Chores[0] != testChores[1] {
		t.Errorf("week -1: %s got %v, want %q", got[0].Person, got[0].Chores, testChores[1])
	}
}

// The week comes from the local date, so 9am Monday is the same week anywhere,
// and Sunday night in New York does not count as the next week.
func TestChoreWheelWeekUsesLocalDate(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	mondayNY := time.Date(2026, time.September, 21, 9, 0, 0, 0, ny)
	mondayUTC := time.Date(2026, time.September, 21, 9, 0, 0, 0, time.UTC)
	if ChoreWheelWeek(mondayNY) != ChoreWheelWeek(mondayUTC) {
		t.Errorf("same Monday in NY and UTC gave weeks %d and %d",
			ChoreWheelWeek(mondayNY), ChoreWheelWeek(mondayUTC))
	}

	// 11pm Sunday in New York is 3am Monday UTC, but it is still last week.
	sundayNightNY := time.Date(2026, time.September, 20, 23, 0, 0, 0, ny)
	if ChoreWheelWeek(sundayNightNY) != ChoreWheelWeek(mondayNY)-1 {
		t.Errorf("Sunday 11pm NY is week %d, Monday 9am NY is week %d; want consecutive",
			ChoreWheelWeek(sundayNightNY), ChoreWheelWeek(mondayNY))
	}

	// Consecutive Mondays are consecutive weeks.
	if ChoreWheelWeek(mondayNY.AddDate(0, 0, 7)) != ChoreWheelWeek(mondayNY)+1 {
		t.Error("a Monday and the next Monday are not consecutive weeks")
	}
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
	res, err := PostChoreWheel(context.Background(), NewNotifier(slack), cfg, tuesday)
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

	// The result repeats the assignment, so you can see what was posted without
	// opening Slack.
	if res["sent"] != true {
		t.Errorf("result.sent = %v, want true", res["sent"])
	}
	for _, c := range testChores {
		if _, ok := res[c].(string); !ok {
			t.Errorf("result missing assignment for %q: %v", c, res)
		}
	}
}

// With one chore each, nobody is free, so the free-week line should not appear.
func TestPostChoreWheelNoFreeWeeks(t *testing.T) {
	people := []string{"A", "B", "C"}
	chores := []string{"x", "y", "z"}
	slack := &fakeSlack{}
	if _, err := PostChoreWheel(context.Background(), NewNotifier(slack),
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
	_, err := PostChoreWheel(context.Background(), NewNotifier(slack),
		&ChoreWheelConfig{People: testPeople, Chores: testChores}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("err = %v, want the slack error wrapped", err)
	}
}

func TestParseChoreWheelTime(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)

	if got, err := ParseChoreWheelTime(true, now); err != nil || !got.Equal(now) {
		t.Errorf("true -> (%v, %v), want now", got, err)
	}
	if got, err := ParseChoreWheelTime(map[string]any{}, now); err != nil || !got.Equal(now) {
		t.Errorf("{} -> (%v, %v), want now", got, err)
	}
	got, err := ParseChoreWheelTime(map[string]any{"date": "2026-10-05"}, now)
	if err != nil || got.Format("2006-01-02") != "2026-10-05" {
		t.Errorf("date override -> (%v, %v), want 2026-10-05", got, err)
	}
	if _, err := ParseChoreWheelTime(map[string]any{"date": "next monday"}, now); err == nil {
		t.Error("unparseable date should error")
	}
	if _, err := ParseChoreWheelTime("yes", now); err == nil {
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
		{"more chores than people", ChoreWheelConfig{People: []string{"A", "B"}, Chores: []string{"x", "y", "z"}}, false},
		{"duplicate person", ChoreWheelConfig{People: []string{"A", "A"}, Chores: []string{"x"}}, true},
		{"blank chore", ChoreWheelConfig{People: []string{"A", "B"}, Chores: []string{" "}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate("test")
			if (err != nil) != tc.bad {
				t.Errorf("Validate() = %v, want error=%v", err, tc.bad)
			}
		})
	}
}
