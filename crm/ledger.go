package crm

// The loyalty ledger: points earned per customer email, and the points
// customers pool toward shared rewards.
//
// Balances are never stored. Every credit and contribution is recorded, and a
// balance is what an email has been credited minus what it has contributed, so
// the numbers cannot drift from the history that explains them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ledgerFileName is the ledger's file under data_dir.
const ledgerFileName = "loyalty.json"

// Reward is one improvement customers can fund, as configured on the service.
type Reward struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	PointsRequired int    `json:"points_required"`
}

// validateRewards rejects rewards that could not be funded or told apart.
// Reward IDs key the recorded contributions, so renaming one orphans what was
// put in.
func validateRewards(path string, rewards []Reward) error {
	seen := map[string]bool{}
	for i, r := range rewards {
		field := fmt.Sprintf("%s: rewards[%d]", path, i)
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("%s.id is required", field)
		}
		if seen[r.ID] {
			return fmt.Errorf("%s.id %q is used twice", field, r.ID)
		}
		seen[r.ID] = true
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("%s.name is required", field)
		}
		if r.PointsRequired <= 0 {
			return fmt.Errorf("%s.points_required must be > 0", field)
		}
	}
	return nil
}

// Credit is points given to one email, for a completed order or by hand.
type Credit struct {
	Points int    `json:"points"`
	Reason string `json:"reason,omitempty"`
	// OrderID is set for credits tied to an order, and makes crediting that
	// order again a no-op.
	OrderID string    `json:"order_id,omitempty"`
	At      time.Time `json:"at"`
}

// Contribution is points one email put toward one reward. Contributions are
// final: nothing takes them back.
type Contribution struct {
	Email    string    `json:"email"`
	RewardID string    `json:"reward_id"`
	Points   int       `json:"points"`
	At       time.Time `json:"at"`
}

// ledger is the persisted file.
type ledger struct {
	Credits       map[string][]Credit `json:"credits"`
	Contributions []Contribution      `json:"contributions"`
	// FundedAt records when each reward first reached its target.
	FundedAt map[string]time.Time `json:"funded_at,omitempty"`
}

// Account is one email's standing.
type Account struct {
	Email       string
	Points      int // available to contribute
	Earned      int
	Contributed int
}

// Contributor is one email's total toward a reward.
type Contributor struct {
	Email  string
	Points int
}

// RewardStatus is a reward and how far along its funding is.
type RewardStatus struct {
	Reward
	Contributed int
	Funded      bool
	// FundedAt is zero unless Funded.
	FundedAt time.Time
	// Contributors is sorted by points, largest first.
	Contributors []Contributor
}

// Remaining is how many more points the reward needs.
func (r RewardStatus) Remaining() int {
	return max(r.PointsRequired-r.Contributed, 0)
}

// ContributeResult reports what a contribution did.
type ContributeResult struct {
	// Applied is what was taken from the balance. It is less than asked when
	// the reward needed fewer points than were offered.
	Applied int
	Account Account
	Reward  RewardStatus
	// JustFunded is true when this contribution reached the reward's target.
	JustFunded bool
}

// Errors callers may want to tell apart.
var (
	ErrUnknownReward     = errors.New("unknown reward")
	ErrRewardFunded      = errors.New("reward is already fully funded")
	ErrInsufficientFunds = errors.New("not enough points")
)

// Store is the loyalty ledger, persisted to one JSON file.
//
// Every write rewrites the whole file, which is fine at an office's scale of a
// few thousand credits a year.
type Store struct {
	path    string
	rewards []Reward
	now     func() time.Time

	mu     sync.Mutex
	ledger ledger
}

// Open loads the ledger at path, or starts an empty one when the file does not
// exist yet. A file that exists but cannot be read is an error rather than an
// empty ledger, since the first write would then erase everyone's points.
func Open(path string, rewards []Reward) (*Store, error) {
	s := &Store{path: path, rewards: rewards, now: time.Now}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("reading loyalty ledger: %w", err)
	default:
		if err := json.Unmarshal(data, &s.ledger); err != nil {
			return nil, fmt.Errorf("parsing loyalty ledger %s: %w", path, err)
		}
	}
	if s.ledger.Credits == nil {
		s.ledger.Credits = map[string][]Credit{}
	}
	if s.ledger.FundedAt == nil {
		s.ledger.FundedAt = map[string]time.Time{}
	}
	return s, nil
}

// NormalizeEmail returns email trimmed and lowercased, so the address typed at
// the kiosk and the one on a Viam account land on the same account.
func NormalizeEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return "", fmt.Errorf("email is required")
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e {
		return "", fmt.Errorf("email %q is not a valid address", email)
	}
	return e, nil
}

// Credit gives points to email. With an orderID that was already credited it
// changes nothing and reports false, so re-running a backfill is safe.
func (s *Store) Credit(email string, points int, reason, orderID string) (bool, Account, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return false, Account{}, err
	}
	if points <= 0 {
		return false, Account{}, fmt.Errorf("points must be > 0, got %d", points)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if orderID != "" && s.orderCredited(orderID) {
		return false, s.account(email), nil
	}
	prev := s.ledger.Credits[email]
	s.ledger.Credits[email] = append(prev, Credit{Points: points, Reason: reason, OrderID: orderID, At: s.now()})
	if err := s.saveInLock(); err != nil {
		s.ledger.Credits[email] = prev
		return false, Account{}, err
	}
	return true, s.account(email), nil
}

// Contribute moves up to points from email's balance to the reward. It takes
// only what the reward still needs, so nobody overpays a nearly funded reward.
func (s *Store) Contribute(email, rewardID string, points int) (ContributeResult, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return ContributeResult{}, err
	}
	if points <= 0 {
		return ContributeResult{}, fmt.Errorf("points must be > 0, got %d", points)
	}
	reward, ok := s.reward(rewardID)
	if !ok {
		return ContributeResult{}, fmt.Errorf("%w %q", ErrUnknownReward, rewardID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status(reward)
	if status.Funded {
		return ContributeResult{}, fmt.Errorf("%q: %w", reward.Name, ErrRewardFunded)
	}
	acct := s.account(email)
	if points > acct.Points {
		return ContributeResult{}, fmt.Errorf("%w: %s has %d, tried to contribute %d", ErrInsufficientFunds, email, acct.Points, points)
	}

	applied := min(points, status.Remaining())
	now := s.now()
	prevLen := len(s.ledger.Contributions)
	s.ledger.Contributions = append(s.ledger.Contributions, Contribution{
		Email: email, RewardID: reward.ID, Points: applied, At: now,
	})
	justFunded := status.Contributed+applied >= reward.PointsRequired
	prevFundedAt, hadFundedAt := s.ledger.FundedAt[reward.ID]
	if justFunded {
		s.ledger.FundedAt[reward.ID] = now
	}
	if err := s.saveInLock(); err != nil {
		s.ledger.Contributions = s.ledger.Contributions[:prevLen]
		if hadFundedAt {
			s.ledger.FundedAt[reward.ID] = prevFundedAt
		} else {
			delete(s.ledger.FundedAt, reward.ID)
		}
		return ContributeResult{}, err
	}
	return ContributeResult{
		Applied:    applied,
		Account:    s.account(email),
		Reward:     s.status(reward),
		JustFunded: justFunded,
	}, nil
}

// Account returns email's standing; an email never seen has zero points.
func (s *Store) Account(email string) (Account, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Account{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.account(email), nil
}

// Rewards returns every configured reward's status, in config order.
func (s *Store) Rewards() []RewardStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RewardStatus, len(s.rewards))
	for i, r := range s.rewards {
		out[i] = s.status(r)
	}
	return out
}

func (s *Store) reward(id string) (Reward, bool) {
	for _, r := range s.rewards {
		if r.ID == id {
			return r, true
		}
	}
	return Reward{}, false
}

// orderCredited reports whether any email was credited for orderID. Callers
// hold mu.
func (s *Store) orderCredited(orderID string) bool {
	for _, credits := range s.ledger.Credits {
		for _, c := range credits {
			if c.OrderID == orderID {
				return true
			}
		}
	}
	return false
}

// account computes email's standing. Callers hold mu.
func (s *Store) account(email string) Account {
	a := Account{Email: email}
	for _, c := range s.ledger.Credits[email] {
		a.Earned += c.Points
	}
	for _, c := range s.ledger.Contributions {
		if c.Email == email {
			a.Contributed += c.Points
		}
	}
	a.Points = a.Earned - a.Contributed
	return a
}

// status computes a reward's funding. A reward whose points_required is later
// raised above what it holds reopens; FundedAt is only reported while funded.
// Callers hold mu.
func (s *Store) status(r Reward) RewardStatus {
	st := RewardStatus{Reward: r}
	byEmail := map[string]int{}
	for _, c := range s.ledger.Contributions {
		if c.RewardID == r.ID {
			st.Contributed += c.Points
			byEmail[c.Email] += c.Points
		}
	}
	st.Funded = st.Contributed >= r.PointsRequired
	if st.Funded {
		st.FundedAt = s.ledger.FundedAt[r.ID]
	}
	for email, pts := range byEmail {
		st.Contributors = append(st.Contributors, Contributor{Email: email, Points: pts})
	}
	sort.Slice(st.Contributors, func(i, j int) bool {
		a, b := st.Contributors[i], st.Contributors[j]
		if a.Points != b.Points {
			return a.Points > b.Points
		}
		return a.Email < b.Email
	})
	return st
}

// saveInLock writes the ledger through a temp file and a rename, so a crash mid-write
// leaves the previous ledger intact rather than a truncated one. Callers hold
// mu.
func (s *Store) saveInLock() error {
	data, err := json.MarshalIndent(s.ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding loyalty ledger: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing loyalty ledger: %w", err)
	}
	// A no-op once the rename below has moved the file into place.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing loyalty ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing loyalty ledger: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("writing loyalty ledger: %w", err)
	}
	return nil
}
