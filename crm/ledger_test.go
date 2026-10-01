package crm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var testRewards = []Reward{
	{ID: "grinder", Name: "Better grinder", PointsRequired: 5},
	{ID: "milk", Name: "Oat milk", PointsRequired: 3},
}

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ledgerFileName)
	s, err := Open(path, testRewards)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

func mustCredit(t *testing.T, s *Store, email string, points int) {
	t.Helper()
	if _, _, err := s.Credit(email, points, "test", ""); err != nil {
		t.Fatalf("Credit(%q, %d): %v", email, points, err)
	}
}

func TestCreditIsCaseInsensitiveAndDedupesOrders(t *testing.T) {
	s, _ := openTest(t)

	ok, acct, err := s.Credit("Ada@Example.com ", 1, "order", "order-1")
	if err != nil || !ok {
		t.Fatalf("first credit: ok=%v err=%v", ok, err)
	}
	if acct.Email != "ada@example.com" || acct.Points != 1 {
		t.Fatalf("account = %+v, want ada@example.com with 1 point", acct)
	}

	ok, acct, err = s.Credit("ada@example.com", 1, "order", "order-1")
	if err != nil {
		t.Fatalf("repeat credit: %v", err)
	}
	if ok || acct.Points != 1 {
		t.Fatalf("repeat credit of the same order: ok=%v points=%d, want false and 1", ok, acct.Points)
	}
}

func TestCreditRejectsBadInput(t *testing.T) {
	s, _ := openTest(t)
	for _, tc := range []struct {
		email  string
		points int
	}{
		{"", 1},
		{"not-an-email", 1},
		{"Ada <ada@example.com>", 1},
		{"ada@example.com", 0},
		{"ada@example.com", -3},
	} {
		if _, _, err := s.Credit(tc.email, tc.points, "", ""); err == nil {
			t.Errorf("Credit(%q, %d) succeeded, want an error", tc.email, tc.points)
		}
	}
}

func TestContributeCapsAtRemainingAndLocksFundedReward(t *testing.T) {
	s, _ := openTest(t)
	mustCredit(t, s, "ada@example.com", 4)
	mustCredit(t, s, "bob@example.com", 10)

	res, err := s.Contribute("ada@example.com", "grinder", 4)
	if err != nil {
		t.Fatalf("ada contribute: %v", err)
	}
	if res.Applied != 4 || res.JustFunded || res.Account.Points != 0 {
		t.Fatalf("ada result = %+v, want 4 applied, not funded, 0 left", res)
	}

	res, err = s.Contribute("bob@example.com", "grinder", 7)
	if err != nil {
		t.Fatalf("bob contribute: %v", err)
	}
	if res.Applied != 1 {
		t.Errorf("bob applied %d, want 1 (only what the reward still needed)", res.Applied)
	}
	if !res.JustFunded || !res.Reward.Funded || res.Reward.FundedAt.IsZero() {
		t.Errorf("reward after bob = %+v, want funded with a FundedAt", res.Reward)
	}
	if res.Account.Points != 9 {
		t.Errorf("bob has %d points, want 9", res.Account.Points)
	}
	want := []Contributor{{"ada@example.com", 4}, {"bob@example.com", 1}}
	if len(res.Reward.Contributors) != 2 || res.Reward.Contributors[0] != want[0] || res.Reward.Contributors[1] != want[1] {
		t.Errorf("contributors = %+v, want %+v", res.Reward.Contributors, want)
	}

	if _, err := s.Contribute("bob@example.com", "grinder", 1); !errors.Is(err, ErrRewardFunded) {
		t.Errorf("contribute to funded reward: err = %v, want ErrRewardFunded", err)
	}
}

func TestContributeRejectsOverdraftAndUnknownReward(t *testing.T) {
	s, _ := openTest(t)
	mustCredit(t, s, "ada@example.com", 2)

	if _, err := s.Contribute("ada@example.com", "milk", 3); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("overdraft: err = %v, want ErrInsufficientFunds", err)
	}
	if _, err := s.Contribute("ada@example.com", "espresso-machine", 1); !errors.Is(err, ErrUnknownReward) {
		t.Errorf("unknown reward: err = %v, want ErrUnknownReward", err)
	}
	if _, err := s.Contribute("nobody@example.com", "milk", 1); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("never-credited email: err = %v, want ErrInsufficientFunds", err)
	}
}

func TestLedgerSurvivesReopen(t *testing.T) {
	s, path := openTest(t)
	fixed := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	mustCredit(t, s, "ada@example.com", 5)
	if _, err := s.Contribute("ada@example.com", "milk", 3); err != nil {
		t.Fatalf("contribute: %v", err)
	}

	reopened, err := Open(path, testRewards)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	acct, err := reopened.Account("ada@example.com")
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if acct != (Account{Email: "ada@example.com", Points: 2, Earned: 5, Contributed: 3}) {
		t.Errorf("account after reopen = %+v", acct)
	}
	rewards := reopened.Rewards()
	if len(rewards) != 2 || rewards[1].ID != "milk" || !rewards[1].Funded || !rewards[1].FundedAt.Equal(fixed) {
		t.Errorf("rewards after reopen = %+v, want milk funded at %v", rewards, fixed)
	}
}

func TestRaisedTargetReopensReward(t *testing.T) {
	s, path := openTest(t)
	mustCredit(t, s, "ada@example.com", 3)
	if _, err := s.Contribute("ada@example.com", "milk", 3); err != nil {
		t.Fatalf("contribute: %v", err)
	}

	raised := []Reward{{ID: "milk", Name: "Oat milk", PointsRequired: 6}}
	reopened, err := Open(path, raised)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	st := reopened.Rewards()[0]
	if st.Funded || !st.FundedAt.IsZero() || st.Remaining() != 3 {
		t.Errorf("reward after raising its target = %+v, want open with 3 remaining", st)
	}
}

func TestOpenRefusesCorruptLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), ledgerFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, testRewards); err == nil {
		t.Fatal("Open succeeded on a corrupt ledger; it must refuse rather than start empty and overwrite it")
	}
}

func TestValidateRewards(t *testing.T) {
	for name, tc := range map[string]struct {
		rewards []Reward
		wantErr bool
	}{
		"valid":       {testRewards, false},
		"empty list":  {nil, false},
		"missing id":  {[]Reward{{Name: "x", PointsRequired: 1}}, true},
		"duplicate":   {[]Reward{{ID: "a", Name: "x", PointsRequired: 1}, {ID: "a", Name: "y", PointsRequired: 1}}, true},
		"no name":     {[]Reward{{ID: "a", PointsRequired: 1}}, true},
		"zero points": {[]Reward{{ID: "a", Name: "x"}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateRewards("services.crm", tc.rewards)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateRewards() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
