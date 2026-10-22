// Package chorewheel registers the viam:beanjamin:chore-wheel model, a generic
// service that posts the weekly maintenance rota to Slack: the upkeep around
// the machine that isn't the arm's job, turned one notch every Monday.
//
// The schedule lives in the machine config under "jobs", not here. See README.
package chorewheel

import (
	"context"
	"fmt"
	"regexp"
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
	// at the end. An entry is a display name or a Slack mention, <@U…>.
	People []string `json:"people"`
	Chores []string `json:"chores"`
	// PeoplePerWeek is how many people share all the chores each week.
	PeoplePerWeek int `json:"people_per_week,omitempty"`
	// SlackNotifierName is the viam:notifications:slack service the wheel is
	// posted through.
	SlackNotifierName string `json:"slack_notifier_name"`
}

const defaultPeoplePerWeek = 1

func (cfg *Config) peoplePerWeek() int {
	if cfg.PeoplePerWeek > 0 {
		return cfg.PeoplePerWeek
	}
	return defaultPeoplePerWeek
}

// slackMention is the only angle-bracket form allowed in people. Slack renders
// <@U…> as a ping; anything else in brackets (a display name, a lowercase ID)
// would post as literal text and ping nobody, so it is caught here instead.
var slackMention = regexp.MustCompile(`^<@[UW][A-Z0-9]+>$`)

// Validate rejects lists the rotation cannot use. One person has nothing to
// rotate, a crew as big as the roster never rotates, and a repeated name would
// give someone two seats and double their share.
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
	if cfg.PeoplePerWeek < 0 {
		return nil, nil, fmt.Errorf("%s: people_per_week must be positive", path)
	}
	if cfg.peoplePerWeek() >= len(cfg.People) {
		return nil, nil, fmt.Errorf("%s: people_per_week (%d) must be less than the %d people, or the wheel never turns",
			path, cfg.peoplePerWeek(), len(cfg.People))
	}
	for _, p := range cfg.People {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "<") && !slackMention.MatchString(p) {
			return nil, nil, fmt.Errorf("%s: people entry %q is not a Slack mention; use <@USERID>, e.g. <@U01ABCDEF>", path, p)
		}
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

	name    resource.Name
	logger  logging.Logger
	people  []string
	chores  []string
	perWeek int
	slack   *report.Notifier
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
		name:    rawConf.ResourceName(),
		logger:  logger,
		people:  trimAll(conf.People),
		chores:  conf.Chores,
		perWeek: conf.peoplePerWeek(),
		slack:   report.NewNotifier(svc),
	}, nil
}

func (c *choreWheel) Name() resource.Name {
	return c.name
}

// Status reports this week's assignment without posting it.
func (c *choreWheel) Status(context.Context) (map[string]any, error) {
	week := weekNumber(time.Now())
	return assignment(week, pickCrew(c.people, c.perWeek, week), c.chores), nil
}

// trimAll strips surrounding whitespace, which Validate already ignores, so a
// stray space can't turn "<@U01ABCDEF> " into text Slack won't resolve.
func trimAll(list []string) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = strings.TrimSpace(v)
	}
	return out
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
	return post(ctx, c.slack, c.people, c.chores, c.perWeek, now)
}
