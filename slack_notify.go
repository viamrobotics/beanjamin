package beanjamin

import (
	"context"
	"fmt"
	"time"
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
func (s *beanjaminCoffee) notifyOrderFailureSlack(r orderReading) {
	if s.slackNotifier == nil || r.execErr == nil {
		return
	}
	text := slackFailureText(r)
	// Only link to a clip when one was actually requested (cam storage
	// configured) and we know the location to filter within.
	clipURL := ""
	if s.camStorage != nil {
		clipURL = buildClipDataURL(s.dataLocationID, r.order.ID)
	}
	blocks := slackFailureBlocks(r, s.machineLogsURL, clipURL)
	// Tag with the order ID so the send logs join the rest of the order's
	// trail. The send is queued and runs detached, possibly after the order
	// has left the queue, so we build the tagged logger from the reading in
	// hand rather than activeOrderLogger().
	logger := s.logger.WithFields("order_id", r.order.ID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifySlackTimeout)
		defer cancel()
		// The viam:notifications:slack service defaults command to "send". blocks
		// renders the rich layout; text is the notification/accessibility
		// fallback Slack shows when blocks can't render. channel_id/webhook are
		// configured on the service.
		resp, err := s.slackNotifier.DoCommand(ctx, map[string]interface{}{
			"command": "send",
			"text":    text,
			"blocks":  blocks,
		})
		if err != nil {
			logger.Warnf("slack notifier: failed to send: %v", err)
			return
		}
		logger.Infof("slack notifier: sent failure notification (response: %+v)", resp)
	}()
}

// slackFailureText builds the plain-text fallback Slack shows in notifications
// and when Block Kit can't render. Operator cancels and genuine faults get
// distinct wording so readers can tell a deliberate stop from a real fault at a
// glance.
func slackFailureText(r orderReading) string {
	customer := slackCustomer(r)
	step := slackStep(r)
	if r.operatorCancelled {
		return fmt.Sprintf(":warning: Order %s (%s) for %s was cancelled by an operator at %q.",
			r.order.ID, r.order.Drink, customer, step)
	}
	return fmt.Sprintf(":x: Order %s (%s) for %s failed at %q: %s",
		r.order.ID, r.order.Drink, customer, step, slackErrMsg(r))
}

// slackFailureBlocks builds a Slack Block Kit layout for a failed or cancelled
// order. It mirrors the per-attempt fields the order sensor records (drink,
// customer, failed step, decaf,
// duration, start time, and trace ID) so the message is a self-contained
// record without a round-trip to the order-events sensor: a header that
// distinguishes a fault from an operator cancel, a fields section with the
// order details at a glance, the error in a code block (faults only), and a
// context footer with the order ID, trace ID, and start time. Returned as
// []interface{} of map[string]interface{} so it serializes cleanly through the
// structpb-backed DoCommand wire format (which rejects []map[string]interface{}
// as a list value). machineLogsURL and clipDataURL, when non-empty, add
// clickable app.viam.com deep-links (machine logs, and the order's video clip
// filtered by tag) to the footer.
func slackFailureBlocks(r orderReading, machineLogsURL, clipDataURL string) []interface{} {
	header := ":x: Order failed"
	stepLabel := "*Failed at:*"
	if r.operatorCancelled {
		header = ":warning: Order cancelled by operator"
		stepLabel = "*Cancelled at:*"
	}

	duration := r.endedAt.Sub(r.startedAt).Round(time.Second)
	blocks := []interface{}{
		map[string]interface{}{
			"type": "header",
			"text": map[string]interface{}{"type": "plain_text", "text": header, "emoji": true},
		},
		map[string]interface{}{
			"type": "section",
			"fields": []interface{}{
				slackField("*Drink:*", r.order.Drink),
				slackField("*Customer:*", slackCustomer(r)),
				slackField(stepLabel, slackStep(r)),
				slackField("*Duration:*", duration.String()),
				slackField("*Decaf:*", slackBool(r.decaf)),
			},
		},
	}

	// Show the error in a code block for faults; an operator cancel has no
	// meaningful error to surface.
	if !r.operatorCancelled {
		blocks = append(blocks, map[string]interface{}{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": fmt.Sprintf("*Error:*\n```%s```", slackErrMsg(r)),
			},
		})
	}

	footer := fmt.Sprintf("Order `%s`", r.order.ID)
	if !r.startedAt.IsZero() {
		// Slack renders <!date> in the reader's timezone; the trailing pipe value
		// is the fallback shown when it can't.
		footer += fmt.Sprintf(" · started <!date^%d^{date_short_pretty} {time}|%s>",
			r.startedAt.Unix(), r.startedAt.UTC().Format(time.RFC3339))
	}
	if r.traceID != "" {
		footer += fmt.Sprintf(" · trace `%s`", r.traceID)
	}
	if machineLogsURL != "" {
		footer += fmt.Sprintf(" · <%s|machine logs>", machineLogsURL)
	}
	if clipDataURL != "" {
		footer += fmt.Sprintf(" · <%s|video clip> _(may take ~a minute to appear)_", clipDataURL)
	}
	blocks = append(blocks, map[string]interface{}{
		"type":     "context",
		"elements": []interface{}{map[string]interface{}{"type": "mrkdwn", "text": footer}},
	})

	return blocks
}

// buildMachineLogsURL constructs an app.viam.com deep-link to this machine's
// logs from the VIAM_MACHINE_ID and VIAM_PRIMARY_ORG_ID env vars Viam injects
// into cloud-connected modules. Returns "" when either is unset (e.g. a local
// or test machine not connected to the cloud), so callers can omit the link.
func buildMachineLogsURL(machineID, orgID string) string {
	if machineID == "" || orgID == "" {
		return ""
	}
	return fmt.Sprintf("https://app.viam.com/machine/%s/logs?org=%s", machineID, orgID)
}

// buildClipDataURL constructs an app.viam.com data-page deep-link filtered to
// the order's video clip. The clip is tagged with the order ID (a UUID, so the
// tag filter alone uniquely identifies it); locationID — from VIAM_LOCATION_ID
// — scopes the view. robotName is intentionally omitted: there is no
// robot-name env var, and the UUID tag makes it redundant. Returns "" when
// locationID is empty (e.g. a local/test machine), so callers can omit the
// link. Note: the clip is uploaded asynchronously after the notification is
// sent, so the link may show no results for the first ~15-60s.
func buildClipDataURL(locationID, orderID string) string {
	if locationID == "" {
		return ""
	}
	return fmt.Sprintf("https://app.viam.com/data/all?locationId=%s&tags=%s&view=media", locationID, orderID)
}

// slackField builds a single mrkdwn field ("*Label:*\nvalue") for a Block Kit
// section fields array.
func slackField(label, value string) map[string]interface{} {
	return map[string]interface{}{"type": "mrkdwn", "text": fmt.Sprintf("%s\n%s", label, value)}
}

func slackBool(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func slackCustomer(r orderReading) string {
	if r.order.CustomerName == "" {
		return "an unnamed customer"
	}
	return r.order.CustomerName
}

func slackStep(r orderReading) string {
	if r.failedStep == "" {
		return "an unknown step"
	}
	return r.failedStep
}

func slackErrMsg(r orderReading) string {
	if r.execErr != nil {
		return r.execErr.Error()
	}
	return "unknown error"
}
