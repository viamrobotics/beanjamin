// Package crm registers the viam:beanjamin:crm model, a generic service holding
// customer profiles keyed by email. Today a profile is a loyalty account: the
// coffee service credits a point per completed drink, and customers pool their
// points toward rewards, improvements to the barista setup, configured here.
package crm

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"

	"beanjamin/coffee/report"
)

// Model is the full model triplet for this service.
var Model = resource.NewModel("viam", "beanjamin", "crm")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newCRM,
		},
	)
}

// Config is the attribute set of the viam:beanjamin:crm service.
type Config struct {
	// DataDir holds the ledger file. It must survive module upgrades, since it
	// is the only record of everyone's points.
	DataDir string `json:"data_dir"`
	// Rewards customers can fund. A reward's id keys its contributions, so keep
	// it stable; name, description and points_required can change freely.
	Rewards []Reward `json:"rewards,omitempty"`
	// SlackNotifierName is an optional viam:notifications:slack service that
	// announces each reward as it becomes fully funded.
	SlackNotifierName string `json:"slack_notifier_name,omitempty"`
}

// Validate checks data_dir and the rewards, and returns the Slack notifier as
// an optional dependency.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.DataDir == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "data_dir")
	}
	if err := validateRewards(path, cfg.Rewards); err != nil {
		return nil, nil, err
	}
	var optDeps []string
	if cfg.SlackNotifierName != "" {
		optDeps = append(optDeps, generic.Named(cfg.SlackNotifierName).String())
	}
	return nil, optDeps, nil
}

// notifySlackTimeout caps one Slack send, which runs detached from the command.
const notifySlackTimeout = 10 * time.Second

type crm struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger
	store  *Store
	slack  *report.Notifier // nil when slack_notifier_name is unset
}

func newCRM(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(conf.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("data_dir %q: %w", conf.DataDir, err)
	}
	path := filepath.Join(conf.DataDir, ledgerFileName)
	store, err := Open(path, conf.Rewards)
	if err != nil {
		return nil, err
	}

	// A missing notifier costs only the announcement, so it must not take the
	// ledger down with it. It is an optional dependency, so the service is
	// rebuilt with it once it comes up.
	var slack *report.Notifier
	if conf.SlackNotifierName != "" {
		if svc, err := generic.FromProvider(deps, conf.SlackNotifierName); err != nil {
			logger.Warnf("slack_notifier_name %q not available, funded rewards won't be announced: %v", conf.SlackNotifierName, err)
		} else {
			slack = report.NewNotifier(svc)
		}
	}

	logger.Infof("ledger at %s, %d reward(s) configured", path, len(conf.Rewards))
	return &crm{name: rawConf.ResourceName(), logger: logger, store: store, slack: slack}, nil
}

func (c *crm) Name() resource.Name {
	return c.name
}

// Status reports every reward's progress.
func (c *crm) Status(context.Context) (map[string]any, error) {
	return map[string]any{"rewards": rewardMaps(c.store.Rewards())}, nil
}

func (c *crm) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	_, span := trace.StartSpan(ctx, "crm::DoCommand")
	defer span.End()

	var (
		res map[string]any
		err error
	)
	switch {
	case cmd["get_rewards"] != nil:
		res, err = c.getRewards(cmd["get_rewards"])
	case cmd["contribute_points"] != nil:
		res, err = c.contributePoints(cmd["contribute_points"])
	case cmd["credit_points"] != nil:
		res, err = c.creditPoints(cmd["credit_points"])
	default:
		err = fmt.Errorf("unknown command, supported: get_rewards, contribute_points, credit_points")
	}
	if err != nil {
		c.logger.Warnw("DoCommand", "error", err)
	}
	return res, err
}

// getRewards answers get_rewards: every reward's progress, plus the account of
// the email passed as {"email": ...}. true alone returns just the rewards.
func (c *crm) getRewards(raw any) (map[string]any, error) {
	out := map[string]any{"rewards": rewardMaps(c.store.Rewards())}
	switch v := raw.(type) {
	case bool:
	case map[string]any:
		email, err := stringArg(v, "get_rewards", "email", true)
		if err != nil {
			return nil, err
		}
		acct, err := c.store.Account(email)
		if err != nil {
			return nil, err
		}
		out["account"] = accountMap(acct)
	default:
		return nil, fmt.Errorf(`get_rewards must be true or {"email": "..."}, got %T`, raw)
	}
	return out, nil
}

// contributePoints answers contribute_points: {"email", "reward_id", "points"}.
// The email is trusted as sent: modules aren't told who the caller is.
func (c *crm) contributePoints(raw any) (map[string]any, error) {
	args, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(`contribute_points must be {"email", "reward_id", "points"}, got %T`, raw)
	}
	email, err := stringArg(args, "contribute_points", "email", true)
	if err != nil {
		return nil, err
	}
	rewardID, err := stringArg(args, "contribute_points", "reward_id", true)
	if err != nil {
		return nil, err
	}
	points, err := pointsArg(args, "contribute_points")
	if err != nil {
		return nil, err
	}

	res, err := c.store.Contribute(email, rewardID, points)
	if err != nil {
		return nil, err
	}
	c.logger.Infof("%s put %d point(s) toward %q (%d/%d)",
		res.Account.Email, res.Applied, res.Reward.ID, res.Reward.Contributed, res.Reward.PointsRequired)
	if res.JustFunded {
		c.notifyRewardFunded(res.Reward)
	}
	return map[string]any{
		"contributed": float64(res.Applied),
		"account":     accountMap(res.Account),
		"reward":      rewardMap(res.Reward),
		"funded":      res.JustFunded,
	}, nil
}

// creditRequest is one decoded credit_points entry.
type creditRequest struct {
	email, reason, orderID string
	points                 int
}

// creditPoints answers credit_points. It takes {"email", "points", "reason"?,
// "order_id"?} or a list of them: the coffee service sends one per completed
// drink, and operators send lists to backfill past orders. Every entry is
// checked before any is applied, so a typo in a backfill doesn't leave it half
// done; entries naming an order that was already credited are skipped.
func (c *crm) creditPoints(raw any) (map[string]any, error) {
	var entries []any
	single := false
	switch v := raw.(type) {
	case map[string]any:
		entries, single = []any{v}, true
	case []any:
		entries = v
	default:
		return nil, fmt.Errorf(`credit_points must be {"email", "points"} or a list of them, got %T`, raw)
	}

	reqs := make([]creditRequest, len(entries))
	for i, e := range entries {
		field := fmt.Sprintf("credit_points[%d]", i)
		if single {
			field = "credit_points"
		}
		args, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object, got %T", field, e)
		}
		var r creditRequest
		var err error
		if r.email, err = stringArg(args, field, "email", true); err != nil {
			return nil, err
		}
		if r.email, err = NormalizeEmail(r.email); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		if r.points, err = pointsArg(args, field); err != nil {
			return nil, err
		}
		if r.reason, err = stringArg(args, field, "reason", false); err != nil {
			return nil, err
		}
		if r.orderID, err = stringArg(args, field, "order_id", false); err != nil {
			return nil, err
		}
		if r.reason == "" {
			r.reason = "manual"
		}
		reqs[i] = r
	}

	results := make([]any, len(reqs))
	creditedCount := 0
	for i, r := range reqs {
		credited, acct, err := c.store.Credit(r.email, r.points, r.reason, r.orderID)
		if err != nil {
			return nil, fmt.Errorf("credit_points: stopped at entry %d of %d (%d applied): %w", i+1, len(reqs), creditedCount, err)
		}
		if credited {
			creditedCount++
			c.logger.Infof("credited %d point(s) to %s (%s), now at %d", r.points, acct.Email, r.reason, acct.Points)
		}
		results[i] = map[string]any{"credited": credited, "account": accountMap(acct)}
	}
	if single {
		return results[0].(map[string]any), nil
	}
	return map[string]any{
		"credited": float64(creditedCount),
		"skipped":  float64(len(reqs) - creditedCount),
		"results":  results,
	}, nil
}

// notifyRewardFunded posts the unlocked reward to Slack, detached from the
// command so a slow Slack call can't hold up the contributor's page.
func (c *crm) notifyRewardFunded(r RewardStatus) {
	c.logger.Infof("reward %q is fully funded", r.ID)
	if c.slack == nil {
		return
	}
	text, blocks := rewardFundedText(r), rewardFundedBlocks(r)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifySlackTimeout)
		defer cancel()
		if _, err := c.slack.Send(ctx, text, blocks); err != nil {
			c.logger.Warnf("slack notifier: failed to send reward %q unlock: %v", r.ID, err)
		}
	}()
}

// rewardFundedText is the plain-text fallback Slack shows in notifications.
func rewardFundedText(r RewardStatus) string {
	return fmt.Sprintf("Reward unlocked: %s (%d points from %d contributor%s)",
		r.Name, r.Contributed, len(r.Contributors), plural(len(r.Contributors)))
}

// rewardFundedBlocks is the Slack layout: a heading, the reward's description
// when it has one, and every contributor with their points.
func rewardFundedBlocks(r RewardStatus) []any {
	section := func(text string) map[string]any {
		return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
	}
	blocks := []any{
		map[string]any{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": "🎉 Reward unlocked: " + r.Name, "emoji": true},
		},
	}
	if r.Description != "" {
		blocks = append(blocks, section(r.Description))
	}
	lines := make([]string, len(r.Contributors))
	for i, ct := range r.Contributors {
		lines[i] = fmt.Sprintf("• %s — %d point%s", ct.Email, ct.Points, plural(ct.Points))
	}
	blocks = append(blocks, section(fmt.Sprintf("*%d points* pooled by:\n%s", r.Contributed, strings.Join(lines, "\n"))))
	return blocks
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Wire shapes. Numbers go out as float64 so in-process callers see what gRPC
// callers do, and lists as []any, which structpb requires.

func accountMap(a Account) map[string]any {
	return map[string]any{
		"email":       a.Email,
		"points":      float64(a.Points),
		"earned":      float64(a.Earned),
		"contributed": float64(a.Contributed),
	}
}

func rewardMap(r RewardStatus) map[string]any {
	contributors := make([]any, len(r.Contributors))
	for i, ct := range r.Contributors {
		contributors[i] = map[string]any{"email": ct.Email, "points": float64(ct.Points)}
	}
	fundedAt := ""
	if !r.FundedAt.IsZero() {
		fundedAt = r.FundedAt.Format(time.RFC3339)
	}
	return map[string]any{
		"id":              r.ID,
		"name":            r.Name,
		"description":     r.Description,
		"points_required": float64(r.PointsRequired),
		"contributed":     float64(r.Contributed),
		"remaining":       float64(r.Remaining()),
		"funded":          r.Funded,
		"funded_at":       fundedAt,
		"contributors":    contributors,
	}
}

func rewardMaps(rs []RewardStatus) []any {
	out := make([]any, len(rs))
	for i, r := range rs {
		out[i] = rewardMap(r)
	}
	return out
}

// stringArg reads a string field of a command argument.
func stringArg(args map[string]any, cmd, key string, required bool) (string, error) {
	v, ok := args[key]
	if !ok {
		if required {
			return "", fmt.Errorf("%s.%s is required", cmd, key)
		}
		return "", nil
	}
	str, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s.%s must be a string, got %T", cmd, key, v)
	}
	if required && str == "" {
		return "", fmt.Errorf("%s.%s is required", cmd, key)
	}
	return str, nil
}

// pointsArg reads the required "points" field: a positive whole number, which
// arrives as float64 over the wire.
func pointsArg(args map[string]any, cmd string) (int, error) {
	v, ok := args["points"]
	if !ok {
		return 0, fmt.Errorf("%s.points is required", cmd)
	}
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f <= 0 || f > math.MaxInt32 {
		return 0, fmt.Errorf("%s.points must be a positive whole number, got %v", cmd, v)
	}
	return int(f), nil
}
