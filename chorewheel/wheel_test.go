package chorewheel

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"beanjamin/coffee/report"
)

type fakeSlack struct {
	sent []map[string]any
	err  error
}

func (f *fakeSlack) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	f.sent = append(f.sent, cmd)
	return nil, f.err
}

var (
	testPeople = []string{"Vijay", "<@U01NICOLAS>", "Julie", "Daniel", "Cheuk", "Ale"}
	testChores = []string{"Cleaning the ice maker", "Cleaning the table", "Tightening the claws"}
)

// Over a full cycle everyone should be on duty the same number of times. This
// is the main thing the design promises, so check it for every crew size,
// including ones that don't divide the roster.
func TestPickCrewIsFairOverACycle(t *testing.T) {
	n := len(testPeople)
	for perWeek := 1; perWeek < n; perWeek++ {
		count := map[string]int{}
		// n weeks of perWeek seats is exactly perWeek laps of the roster.
		for week := 0; week < n; week++ {
			for _, p := range pickCrew(testPeople, perWeek, week) {
				count[p]++
			}
		}
		for _, p := range testPeople {
			if count[p] != perWeek {
				t.Errorf("perWeek=%d: %s was on duty %d times in %d weeks, want %d",
					perWeek, p, count[p], n, perWeek)
			}
		}
	}
}

// A crew never names the same person twice.
func TestPickCrewHasDistinctPeople(t *testing.T) {
	for perWeek := 1; perWeek < len(testPeople); perWeek++ {
		for week := -10; week < 20; week++ {
			crew := pickCrew(testPeople, perWeek, week)
			if len(crew) != perWeek {
				t.Fatalf("perWeek=%d week %d: crew of %d", perWeek, week, len(crew))
			}
			seen := map[string]bool{}
			for _, p := range crew {
				if seen[p] {
					t.Errorf("perWeek=%d week %d: %s twice in %v", perWeek, week, p, crew)
				}
				seen[p] = true
			}
		}
	}
}

// Each week picks up where the last stopped, so with a crew of at most half the
// roster nobody serves two weeks running.
func TestPickCrewNoBackToBack(t *testing.T) {
	for perWeek := 1; perWeek <= len(testPeople)/2; perWeek++ {
		for week := -5; week < 20; week++ {
			this := pickCrew(testPeople, perWeek, week)
			for _, p := range pickCrew(testPeople, perWeek, week+1) {
				if slices.Contains(this, p) {
					t.Errorf("perWeek=%d: %s on duty in weeks %d and %d", perWeek, p, week, week+1)
				}
			}
		}
	}
}

// The same week always gives the same answer, and the next week gives a
// different one. A wheel that never turns is the bug to catch.
func TestPickCrewIsDeterministicAndAdvances(t *testing.T) {
	a := pickCrew(testPeople, 2, 7)
	if b := pickCrew(testPeople, 2, 7); !slices.Equal(a, b) {
		t.Fatalf("week 7 read twice disagrees: %v vs %v", a, b)
	}
	if c := pickCrew(testPeople, 2, 8); slices.Equal(a, c) {
		t.Error("week 7 and week 8 produced the same crew; the wheel did not turn")
	}
}

// A date before the epoch gives a negative week. It should wrap around instead
// of going out of range.
func TestPickCrewHandlesNegativeWeek(t *testing.T) {
	// -1*2 mod 6 is 4, so the crew is people 4 and 5.
	got := pickCrew(testPeople, 2, -1)
	want := []string{"Cheuk", "Ale"}
	if !slices.Equal(got, want) {
		t.Errorf("week -1: crew %v, want %v", got, want)
	}
}

// The week comes from the local date, so 9am Monday is the same week anywhere,
// and Sunday night in New York does not count as the next week.
func TestWeekNumberUsesLocalDate(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	mondayNY := time.Date(2026, time.September, 21, 9, 0, 0, 0, ny)
	mondayUTC := time.Date(2026, time.September, 21, 9, 0, 0, 0, time.UTC)
	if weekNumber(mondayNY) != weekNumber(mondayUTC) {
		t.Errorf("same Monday in NY and UTC gave weeks %d and %d",
			weekNumber(mondayNY), weekNumber(mondayUTC))
	}

	// 11pm Sunday in New York is 3am Monday UTC, but it is still last week.
	sundayNightNY := time.Date(2026, time.September, 20, 23, 0, 0, 0, ny)
	if weekNumber(sundayNightNY) != weekNumber(mondayNY)-1 {
		t.Errorf("Sunday 11pm NY is week %d, Monday 9am NY is week %d; want consecutive",
			weekNumber(sundayNightNY), weekNumber(mondayNY))
	}

	// Consecutive Mondays are consecutive weeks.
	if weekNumber(mondayNY.AddDate(0, 0, 7)) != weekNumber(mondayNY)+1 {
		t.Error("a Monday and the next Monday are not consecutive weeks")
	}
}

func TestPostRendersAndReports(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	slack := &fakeSlack{}

	// A Tuesday: the message must still be dated by its Monday.
	tuesday := time.Date(2026, time.September, 22, 14, 0, 0, 0, ny)
	res, err := post(context.Background(), report.NewNotifier(slack), testPeople, testChores, 2, tuesday)
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
	crew := pickCrew(testPeople, 2, weekNumber(tuesday))

	text, _ := msg["text"].(string)
	if !strings.Contains(text, "week of Sep 21") {
		t.Errorf("fallback text %q should be dated by Monday Sep 21", text)
	}
	for _, s := range append(append([]string{}, testChores...), crew...) {
		if !strings.Contains(text, s) {
			t.Errorf("fallback text missing %q: %q", s, text)
		}
	}

	blocks, _ := msg["blocks"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want header and section", len(blocks))
	}
	section := blocks[1].(map[string]any)["text"].(map[string]any)["text"].(string)
	if want := "*On duty:* " + strings.Join(crew, ", "); !strings.Contains(section, want) {
		t.Errorf("section should name the crew as %q: %q", want, section)
	}
	if got := strings.Count(section, "•"); got != len(testChores) {
		t.Errorf("section has %d chore lines, want %d", got, len(testChores))
	}

	// The result repeats the assignment, so you can see what was posted without
	// opening Slack.
	if res["sent"] != true {
		t.Errorf("result.sent = %v, want true", res["sent"])
	}
	if got, _ := res["people"].([]any); len(got) != 2 || got[0] != crew[0] || got[1] != crew[1] {
		t.Errorf("result.people = %v, want %v", res["people"], crew)
	}
	if got, _ := res["chores"].([]any); len(got) != len(testChores) {
		t.Errorf("result.chores = %v, want %v", res["chores"], testChores)
	}
}

// A Slack mention must reach the message untouched: only the mrkdwn section
// and the fallback text render <@U…> as a ping, so it must appear there
// verbatim and never in the plain_text header.
func TestPostKeepsSlackMentionsVerbatim(t *testing.T) {
	mention := "<@U01NICOLAS>"
	// Week 1 with one person a week puts people[1], the mention, on duty.
	monday := epoch.AddDate(0, 0, 7)
	slack := &fakeSlack{}
	if _, err := post(context.Background(), report.NewNotifier(slack), testPeople, testChores, 1, monday); err != nil {
		t.Fatal(err)
	}
	msg := slack.sent[0]
	blocks := msg["blocks"].([]any)
	header := blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	section := blocks[1].(map[string]any)["text"].(map[string]any)["text"].(string)

	if !strings.Contains(section, "*On duty:* "+mention+"\n") {
		t.Errorf("section should carry the raw mention outside any formatting: %q", section)
	}
	if !strings.Contains(msg["text"].(string), mention) {
		t.Errorf("fallback text should carry the raw mention: %q", msg["text"])
	}
	if strings.Contains(header, "<@") {
		t.Errorf("plain_text header can't render mentions, got %q", header)
	}
}

func TestPostPropagatesSendError(t *testing.T) {
	slack := &fakeSlack{err: errors.New("channel_not_found")}
	_, err := post(context.Background(), report.NewNotifier(slack),
		testPeople, testChores, 1, time.Now())
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("err = %v, want the slack error wrapped", err)
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)

	if got, err := parseTime(true, now); err != nil || !got.Equal(now) {
		t.Errorf("true -> (%v, %v), want now", got, err)
	}
	if got, err := parseTime(map[string]any{}, now); err != nil || !got.Equal(now) {
		t.Errorf("{} -> (%v, %v), want now", got, err)
	}
	got, err := parseTime(map[string]any{"date": "2026-10-05"}, now)
	if err != nil || got.Format("2006-01-02") != "2026-10-05" {
		t.Errorf("date override -> (%v, %v), want 2026-10-05", got, err)
	}
	if _, err := parseTime(map[string]any{"date": "next monday"}, now); err == nil {
		t.Error("unparseable date should error")
	}
	if _, err := parseTime("yes", now); err == nil {
		t.Error("a string value should error")
	}
}

func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		bad  bool
	}{
		{"ok default crew", Config{SlackNotifierName: "slack", People: testPeople, Chores: testChores}, false},
		{"ok crew of 3", Config{SlackNotifierName: "slack", People: testPeople, Chores: testChores, PeoplePerWeek: 3}, false},
		{"ok two people", Config{SlackNotifierName: "slack", People: []string{"A", "B"}, Chores: []string{"x"}}, false},
		{"crew as big as roster", Config{SlackNotifierName: "slack", People: []string{"A", "B"}, Chores: []string{"x"}, PeoplePerWeek: 2}, true},
		{"negative crew", Config{SlackNotifierName: "slack", People: []string{"A", "B"}, Chores: []string{"x"}, PeoplePerWeek: -1}, true},
		{"one person", Config{SlackNotifierName: "slack", People: []string{"A"}, Chores: []string{"x"}}, true},
		{"no chores", Config{SlackNotifierName: "slack", People: []string{"A", "B"}}, true},
		{"duplicate person", Config{SlackNotifierName: "slack", People: []string{"A", "A"}, Chores: []string{"x"}}, true},
		{"duplicate mention", Config{SlackNotifierName: "slack", People: []string{"<@U1>", "<@U1> "}, Chores: []string{"x"}}, true},
		{"blank chore", Config{SlackNotifierName: "slack", People: []string{"A", "B"}, Chores: []string{" "}}, true},
		{"no slack notifier", Config{People: []string{"A", "B"}, Chores: []string{"x"}}, true},
		{"workspace mention", Config{SlackNotifierName: "slack", People: []string{"<@U01ABC>", "<@W02XYZ>"}, Chores: []string{"x"}}, false},
		{"mention by display name", Config{SlackNotifierName: "slack", People: []string{"<@nicolas>", "B"}, Chores: []string{"x"}}, true},
		{"mention missing @", Config{SlackNotifierName: "slack", People: []string{"<U01ABC>", "B"}, Chores: []string{"x"}}, true},
		{"unclosed mention", Config{SlackNotifierName: "slack", People: []string{"<@U01ABC", "B"}, Chores: []string{"x"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.cfg.Validate("test")
			if (err != nil) != tc.bad {
				t.Errorf("Validate() = %v, want error=%v", err, tc.bad)
			}
		})
	}
}
