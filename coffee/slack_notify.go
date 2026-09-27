package coffee

import (
	"context"
	"time"

	"beanjamin/coffee/order"
	"beanjamin/coffee/report"
)

// notifySlackTimeout caps how long a single Slack DoCommand may take. The
// notifier runs off the queue goroutine, so a wedged Slack call must not stall
// the next order indefinitely.
const notifySlackTimeout = 10 * time.Second

// notifyOrderFailureSlack sends a best-effort Slack message for a
// non-successful order attempt when a slack_notifier_name is configured. It is
// a no-op for successful orders and when no notifier is configured. The send
// runs in its own goroutine so it never blocks queue processing, and every
// failure is logged rather than propagated.
func (s *beanjaminCoffee) notifyOrderFailureSlack(r order.Reading) {
	if s.slackNotifier == nil || r.ExecErr == nil {
		return
	}
	text := report.FailureText(r)
	// Only link to a clip when one was actually requested (cam storage
	// configured) and we know the location to filter within.
	clipURL := ""
	if s.clips.Configured() {
		clipURL = report.ClipDataURL(s.dataLocationID, s.primaryOrgID, r.Order.ID)
	}
	planRequestURL := report.PlanRequestDataURL(s.dataLocationID, s.primaryOrgID, r.Order.ID)
	blocks := report.FailureBlocks(r, s.machineLogsURL, clipURL, planRequestURL)
	// Tag with the order ID so the send logs join the rest of the order's
	// trail. The send is queued and runs detached, possibly after the order
	// has left the queue, so we build the tagged logger from the reading in
	// hand rather than activeOrderLogger().
	logger := s.logger.WithFields("order_id", r.Order.ID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifySlackTimeout)
		defer cancel()
		resp, err := s.slackNotifier.Send(ctx, text, blocks)
		if err != nil {
			logger.Warnf("slack notifier: failed to send: %v", err)
			return
		}
		logger.Infof("slack notifier: sent failure notification (response: %+v)", resp)
	}()
}
