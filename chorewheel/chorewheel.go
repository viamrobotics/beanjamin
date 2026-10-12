// Package chorewheel registers the viam:beanjamin:chore-wheel model, a generic
// service that posts the weekly maintenance rota to Slack: the upkeep around
// the machine that isn't the arm's job, turned one notch every Monday.
//
// The schedule lives in the machine config under "jobs", not here. See README.
package chorewheel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"

	"beanjamin/coffee/report"
)

// Model is the full model triplet for this service.
var Model = resource.NewModel("viam", "beanjamin", "chore-wheel")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newChoreWheel,
		},
	)
}

// Config is the attribute set of the viam:beanjamin:chore-wheel service.
type Config struct {
	// People is the order of the wheel, so keep it stable and append newcomers
	// at the end.
	People []string `json:"people"`
	Chores []string `json:"chores"`
	// SlackNotifierName is the viam:notifications:slack service the wheel is
	// posted through.
	SlackNotifierName string `json:"slack_notifier_name"`
}

// Validate rejects lists the rotation cannot use. One person has nothing to
// rotate, and a repeated name would give someone two slots and double their
// share. More chores than people is fine; the list just wraps around.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.SlackNotifierName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "slack_notifier_name")
	}
	if len(cfg.People) < 2 {
		return nil, nil, fmt.Errorf("%s: people needs at least 2 names", path)
	}
	if len(cfg.Chores) == 0 {
		return nil, nil, fmt.Errorf("%s: chores needs at least 1 chore", path)
	}
	for field, list := range map[string][]string{"people": cfg.People, "chores": cfg.Chores} {
		seen := map[string]bool{}
		for _, v := range list {
			v = strings.TrimSpace(v)
			if v == "" {
				return nil, nil, fmt.Errorf("%s: %s contains an empty entry", path, field)
			}
			if seen[v] {
				return nil, nil, fmt.Errorf("%s: %s lists %q twice", path, field, v)
			}
			seen[v] = true
		}
	}
	return []string{generic.Named(cfg.SlackNotifierName).String()}, nil, nil
}

// sendTimeout caps the Slack send. Only one cron job runs at a time, so a stuck
// post would block every later one.
const sendTimeout = 15 * time.Second

type choreWheel struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger
	people []string
	chores []string
	slack  *report.Notifier
}

func newChoreWheel(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}
	svc, err := generic.FromProvider(deps, conf.SlackNotifierName)
	if err != nil {
		return nil, fmt.Errorf("slack_notifier_name %q: %w", conf.SlackNotifierName, err)
	}
	return &choreWheel{
		name:   rawConf.ResourceName(),
		logger: logger,
		people: conf.People,
		chores: conf.Chores,
		slack:  report.NewNotifier(svc),
	}, nil
}

func (c *choreWheel) Name() resource.Name {
	return c.name
}

// Status reports this week's assignment without posting it.
func (c *choreWheel) Status(context.Context) (map[string]any, error) {
	week := weekNumber(time.Now())
	out := map[string]any{"week": float64(week)}
	for _, a := range assignChores(c.people, c.chores, week) {
		for _, chore := range a.Chores {
			out[chore] = a.Person
		}
	}
	return out, nil
}

func (c *choreWheel) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	v, ok := cmd["send_weekly_chores"]
	if !ok {
		return nil, fmt.Errorf("unknown command, supported: send_weekly_chores")
	}
	res, err := c.sendWeeklyChores(ctx, v)
	if err != nil {
		c.logger.Warnw("DoCommand", "error", err)
	}
	return res, err
}

// sendWeeklyChores handles send_weekly_chores. A cron job normally runs it on
// Mondays, but you can also call it by hand: true posts the current week, and
// {"date": "2026-10-05"} posts the week containing that date.
func (c *choreWheel) sendWeeklyChores(ctx context.Context, v any) (map[string]any, error) {
	now, err := parseTime(v, time.Now())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	c.logger.Infof("posting week %d assignments", weekNumber(now))
	return post(ctx, c.slack, c.people, c.chores, now)
}
