package crm

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// recapTopN is how many rewards the Slack recap lists.
const recapTopN = 5

// recapBarWidth is the number of cells in a recap progress bar.
const recapBarWidth = 20

// sendRewardsRecap answers send_rewards_recap: true. It posts the open rewards
// closest to their target to Slack, and waits for the send so the caller
// learns whether it went out.
func (c *crm) sendRewardsRecap(ctx context.Context, raw any) (map[string]any, error) {
	if v, ok := raw.(bool); !ok || !v {
		return nil, fmt.Errorf("send_rewards_recap must be true, got %v", raw)
	}
	if c.slack == nil {
		return nil, fmt.Errorf("send_rewards_recap needs slack_notifier_name to be configured and available")
	}
	top := recapRewards(c.store.Rewards())
	title := recapTitle(c.machineName(ctx))
	ctx, cancel := context.WithTimeout(ctx, notifySlackTimeout)
	defer cancel()
	if _, err := c.slack.Send(ctx, rewardsRecapText(title, top), rewardsRecapBlocks(title, top, c.rewardsURL)); err != nil {
		return nil, fmt.Errorf("sending rewards recap to Slack: %w", err)
	}
	c.logger.Infof("sent the rewards recap to Slack with %d reward(s)", len(top))
	return map[string]any{"sent": true, "rewards": rewardMaps(top)}, nil
}

// recapRewards picks the open rewards furthest along, by share of their target.
// Funded rewards are left out: they were announced when they unlocked, and at
// 100% they would hold the top places for good. Equal shares go to the reward
// holding more points, then to config order.
func recapRewards(rs []RewardStatus) []RewardStatus {
	open := make([]RewardStatus, 0, len(rs))
	for _, r := range rs {
		if !r.Funded {
			open = append(open, r)
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		a, b := open[i], open[j]
		// Cross-multiplied so the comparison is exact.
		ai, bj := a.Contributed*b.PointsRequired, b.Contributed*a.PointsRequired
		if ai != bj {
			return ai > bj
		}
		return a.Contributed > b.Contributed
	})
	return open[:min(len(open), recapTopN)]
}

// percentFunded rounds down, so an open reward never reads as 100%.
func percentFunded(r RewardStatus) int {
	return min(r.Contributed*100/r.PointsRequired, 100)
}

// progressBar draws the share funded as filled and empty cells, rounding down
// like percentFunded.
func progressBar(r RewardStatus) string {
	filled := min(r.Contributed*recapBarWidth/r.PointsRequired, recapBarWidth)
	return strings.Repeat("█", filled) + strings.Repeat("░", recapBarWidth-filled)
}

// recapTitle names the machine the rewards belong to, since several machines
// can post to one channel. It drops the name when the lookup failed.
func recapTitle(machineName string) string {
	if machineName == "" {
		return "Rewards status"
	}
	return "Rewards status: " + machineName
}

// rewardsRecapText is the plain-text fallback Slack shows in notifications.
func rewardsRecapText(title string, rs []RewardStatus) string {
	if len(rs) == 0 {
		return title + " · no rewards are open right now"
	}
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = fmt.Sprintf("%s %d%%", r.Name, percentFunded(r))
	}
	return title + " · " + strings.Join(parts, ", ")
}

// rewardsRecapBlocks is the Slack layout: a heading, one section per reward
// with its bar, share and points, and a footer linking the rewards page.
func rewardsRecapBlocks(title string, rs []RewardStatus, rewardsURL string) []any {
	section := func(text string) map[string]any {
		return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
	}
	blocks := []any{
		map[string]any{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": "☕ " + title, "emoji": true},
		},
	}
	if len(rs) == 0 {
		blocks = append(blocks, section("No rewards are open right now."))
	}
	for _, r := range rs {
		blocks = append(blocks, section(fmt.Sprintf("*%s*\n`%s` *%d%%* · %d / %d points",
			r.Name, progressBar(r), percentFunded(r), r.Contributed, r.PointsRequired)))
	}
	return append(blocks, map[string]any{
		"type": "context",
		"elements": []any{map[string]any{
			"type": "mrkdwn",
			"text": fmt.Sprintf("Every drink earns a point. <%s|Put yours toward a reward>", rewardsURL),
		}},
	})
}
