package crm

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"

	"beanjamin/coffee/report"
)

var serviceRewards = []Reward{
	{ID: "grinder", Name: "Better grinder", Description: "Burrs that don't squeak.", PointsRequired: 3},
}

// newTestCRM builds the service over a fresh ledger. With a non-nil channel it
// also wires a fake Slack service that hands every payload to it.
func newTestCRM(t *testing.T, sent chan map[string]any) *crm {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), ledgerFileName), serviceRewards)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := &crm{logger: logging.NewTestLogger(t), store: store}
	if sent != nil {
		slack := inject.NewGenericService("slack")
		slack.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
			sent <- cmd
			return map[string]any{}, nil
		}
		c.slack = report.NewNotifier(slack)
	}
	return c
}

func doCmd(t *testing.T, c *crm, cmd map[string]any) map[string]any {
	t.Helper()
	res, err := c.DoCommand(context.Background(), cmd)
	if err != nil {
		t.Fatalf("DoCommand(%v): %v", cmd, err)
	}
	return res
}

func accountPoints(t *testing.T, c *crm, email string) float64 {
	t.Helper()
	res := doCmd(t, c, map[string]any{"get_rewards": map[string]any{"email": email}})
	return res["account"].(map[string]any)["points"].(float64)
}

func TestContributeFundsRewardAndNotifiesSlack(t *testing.T) {
	sent := make(chan map[string]any, 1)
	c := newTestCRM(t, sent)
	doCmd(t, c, map[string]any{"credit_points": []any{
		map[string]any{"email": "ada@example.com", "points": 2.0, "order_id": "old-1"},
		map[string]any{"email": "bob@example.com", "points": 5.0, "reason": "backfill"},
	}})

	doCmd(t, c, map[string]any{"contribute_points": map[string]any{
		"email": "ada@example.com", "reward_id": "grinder", "points": 2.0,
	}})
	res := doCmd(t, c, map[string]any{"contribute_points": map[string]any{
		"email": "bob@example.com", "reward_id": "grinder", "points": 4.0,
	}})

	if res["contributed"] != 1.0 || res["funded"] != true {
		t.Errorf("bob's contribution = %v, want 1 applied and funded", res)
	}
	if got := res["account"].(map[string]any)["points"]; got != 4.0 {
		t.Errorf("bob's balance = %v, want 4", got)
	}
	reward := res["reward"].(map[string]any)
	if reward["funded_at"] == "" || len(reward["contributors"].([]any)) != 2 {
		t.Errorf("reward = %v, want funded_at set and 2 contributors", reward)
	}

	select {
	case msg := <-sent:
		if text, _ := msg["text"].(string); !strings.Contains(text, "Better grinder") {
			t.Errorf("slack text = %q, want it to name the reward", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Slack message for the funded reward")
	}

	if _, err := c.DoCommand(context.Background(), map[string]any{"contribute_points": map[string]any{
		"email": "bob@example.com", "reward_id": "grinder", "points": 1.0,
	}}); !errors.Is(err, ErrRewardFunded) {
		t.Errorf("contributing to a funded reward: err = %v, want ErrRewardFunded", err)
	}
}

func TestCreditPointsSkipsCreditedOrdersAndValidatesFirst(t *testing.T) {
	c := newTestCRM(t, nil)
	res := doCmd(t, c, map[string]any{"credit_points": map[string]any{"email": "Ada@Example.com", "points": 1.0, "order_id": "o1"}})
	if res["credited"] != true {
		t.Fatalf("single credit = %v, want credited", res)
	}

	res = doCmd(t, c, map[string]any{"credit_points": []any{
		map[string]any{"email": "ada@example.com", "points": 1.0, "order_id": "o1"},
		map[string]any{"email": "ada@example.com", "points": 1.0, "order_id": "o2"},
	}})
	if res["credited"] != 1.0 || res["skipped"] != 1.0 {
		t.Errorf("backfill result = %v, want 1 credited and 1 skipped", res)
	}

	// The bad second entry must stop the first from being applied too.
	if _, err := c.DoCommand(context.Background(), map[string]any{"credit_points": []any{
		map[string]any{"email": "ada@example.com", "points": 1.0},
		map[string]any{"email": "not-an-email", "points": 1.0},
	}}); err == nil {
		t.Fatal("credit_points with an invalid entry succeeded")
	}
	if got := accountPoints(t, c, "ada@example.com"); got != 2 {
		t.Errorf("points = %v, want 2 (nothing from the rejected batch)", got)
	}
}

func TestArgumentValidation(t *testing.T) {
	c := newTestCRM(t, nil)
	for name, cmd := range map[string]map[string]any{
		"fractional points": {"credit_points": map[string]any{"email": "a@b.co", "points": 1.5}},
		"string points":     {"credit_points": map[string]any{"email": "a@b.co", "points": "3"}},
		"missing reward":    {"contribute_points": map[string]any{"email": "ada@example.com", "points": 1.0}},
		"bad get_rewards":   {"get_rewards": "a@b.co"},
		"unknown command":   {"refund": true},
	} {
		if _, err := c.DoCommand(context.Background(), cmd); err == nil {
			t.Errorf("%s: DoCommand(%v) succeeded, want an error", name, cmd)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	if _, _, err := (&Config{}).Validate("services.crm"); err == nil {
		t.Error("Validate without data_dir succeeded")
	}
	_, opt, err := (&Config{DataDir: "/tmp/crm", Rewards: serviceRewards, SlackNotifierName: "slack"}).Validate("services.crm")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(opt) != 1 || !strings.HasSuffix(opt[0], "/slack") {
		t.Errorf("optional deps = %v, want the slack service", opt)
	}
}
