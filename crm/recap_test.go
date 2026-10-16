package crm

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecapRewardsRanksOpenRewardsByShare(t *testing.T) {
	status := func(id string, contributed, required int) RewardStatus {
		return RewardStatus{
			Reward:      Reward{ID: id, Name: id, PointsRequired: required},
			Contributed: contributed,
			Funded:      contributed >= required,
		}
	}
	rs := []RewardStatus{
		status("funded", 10, 10),
		status("quarter", 1, 4),
		status("half-small", 1, 2),
		status("half-big", 50, 100),
		status("third", 1, 3),
		status("tenth", 1, 10),
		status("empty", 0, 5),
	}

	var got []string
	for _, r := range recapRewards(rs) {
		got = append(got, r.ID)
	}
	want := []string{"half-big", "half-small", "third", "quarter", "tenth"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("recapRewards = %v, want %v", got, want)
	}

	if got := recapRewards([]RewardStatus{status("funded", 3, 3)}); len(got) != 0 {
		t.Errorf("recapRewards with only funded rewards = %v, want none", got)
	}
}

func TestRewardsRecapHeaderDefaults(t *testing.T) {
	for in, want := range map[string]string{
		"":                           defaultRewardsRecapHeader,
		"   ":                        defaultRewardsRecapHeader,
		"Rewards status: Cappuccina": "Rewards status: Cappuccina",
	} {
		if got := (&Config{RewardsRecapHeader: in}).rewardsRecapHeader(); got != want {
			t.Errorf("rewardsRecapHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecapRoundsDown(t *testing.T) {
	r := RewardStatus{Reward: Reward{PointsRequired: 200}, Contributed: 199}
	if got := percentFunded(r); got != 99 {
		t.Errorf("percentFunded(199/200) = %d, want 99", got)
	}
	if got := progressBar(r); strings.Count(got, "█") != recapBarWidth-1 || strings.Count(got, "░") != 1 {
		t.Errorf("progressBar(199/200) = %q, want one empty cell", got)
	}
}

func TestSendRewardsRecapPostsTopRewardsWithLink(t *testing.T) {
	sent := make(chan map[string]any, 1)
	c := newTestCRM(t, sent)
	rewards := make([]Reward, 7)
	for i := range rewards {
		rewards[i] = Reward{ID: fmt.Sprintf("r%d", i), Name: fmt.Sprintf("Reward %d", i), PointsRequired: 10}
	}
	store, err := Open(filepath.Join(t.TempDir(), ledgerFileName), rewards)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c.store = store
	c.rewardsURL = "https://example.com/?view=rewards&partId=p1"
	c.recapHeader = "Rewards status: Cappuccina"
	mustCredit(t, store, "ada@example.com", 100)
	// r0 gets 1 point, r1 2, ... r5 6; r6 is funded and must not appear.
	for i := range 6 {
		if _, err := store.Contribute("ada@example.com", rewards[i].ID, i+1); err != nil {
			t.Fatalf("Contribute: %v", err)
		}
	}
	if _, err := store.Contribute("ada@example.com", "r6", 10); err != nil {
		t.Fatalf("Contribute: %v", err)
	}

	res := doCmd(t, c, map[string]any{"send_rewards_recap": true})
	listed := res["rewards"].([]any)
	if len(listed) != recapTopN || listed[0].(map[string]any)["id"] != "r5" || listed[4].(map[string]any)["id"] != "r1" {
		t.Errorf("recap rewards = %v, want r5 down to r1", listed)
	}

	var msg map[string]any
	select {
	case msg = <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("no Slack message for the recap")
	}
	var body strings.Builder
	for _, b := range msg["blocks"].([]any) {
		fmt.Fprintf(&body, "%v\n", b)
	}
	for _, want := range []string{"☕ Rewards status: Cappuccina", "*Reward 5*", "*60%* · 6 / 10 points", "*20%* · 2 / 10 points", c.rewardsURL} {
		if !strings.Contains(body.String(), want) {
			t.Errorf("recap blocks lack %q:\n%s", want, body.String())
		}
	}
	for _, absent := range []string{"Reward 6", "Reward 0"} {
		if strings.Contains(body.String(), absent) {
			t.Errorf("recap blocks list %q, which is funded or outside the top %d", absent, recapTopN)
		}
	}
	if text, _ := msg["text"].(string); !strings.HasPrefix(text, "Rewards status: Cappuccina · Reward 5 60%") {
		t.Errorf("recap text = %q", text)
	}
}

func TestSendRewardsRecapNeedsSlack(t *testing.T) {
	c := newTestCRM(t, nil)
	if _, err := c.DoCommand(context.Background(), map[string]any{"send_rewards_recap": true}); err == nil {
		t.Error("send_rewards_recap without Slack succeeded, want an error")
	}
	sent := make(chan map[string]any, 1)
	c = newTestCRM(t, sent)
	if _, err := c.DoCommand(context.Background(), map[string]any{"send_rewards_recap": false}); err == nil {
		t.Error("send_rewards_recap: false succeeded, want an error")
	}
}
