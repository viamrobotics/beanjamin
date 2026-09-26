package coffee

import (
	"context"
	"fmt"
	"strings"
	"time"

	"beanjamin/coffee/order"
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
	text := slackFailureText(r)
	// Only link to a clip when one was actually requested (cam storage
	// configured) and we know the location to filter within.
	clipURL := ""
	if s.camStorage != nil {
		clipURL = buildClipDataURL(s.dataLocationID, s.primaryOrgID, r.Order.ID)
	}
	planRequestURL := buildPlanRequestDataURL(s.dataLocationID, s.primaryOrgID, r.Order.ID)
	blocks := slackFailureBlocks(r, s.machineLogsURL, clipURL, planRequestURL)
	// Tag with the order ID so the send logs join the rest of the order's
	// trail. The send is queued and runs detached, possibly after the order
	// has left the queue, so we build the tagged logger from the reading in
	// hand rather than activeOrderLogger().
	logger := s.logger.WithFields("order_id", r.Order.ID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifySlackTimeout)
		defer cancel()
		// The viam:notifications:slack service defaults command to "send". blocks
		// renders the rich layout; text is the notification/accessibility
		// fallback Slack shows when blocks can't render. channel_id/webhook are
		// configured on the service.
		resp, err := s.slackNotifier.DoCommand(ctx, map[string]any{
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
func slackFailureText(r order.Reading) string {
	drink := escapeSlackMrkdwn(r.Order.Drink)
	customer := slackCustomer(r)
	step := slackStep(r)
	if r.OperatorCancelled {
		return fmt.Sprintf(":warning: Order %s (%s) for %s was cancelled by an operator at %q.",
			r.Order.ID, drink, customer, step)
	}
	return fmt.Sprintf(":x: Order %s (%s) for %s failed at %q: %s",
		r.Order.ID, drink, customer, step, slackErrMsg(r))
}

// slackFailureBlocks builds a Slack Block Kit layout for a failed or cancelled
// order. It mirrors the per-attempt fields the order sensor records (drink,
// customer, failed step, decaf,
// duration, start time, and trace ID) so the message is a self-contained
// record without a round-trip to the order-events sensor: a header that
// distinguishes a fault from an operator cancel, a fields section with the
// order details at a glance, the error in a code block (faults only), and a
// context footer with the order ID, trace ID, and start time. Returned as
// []any of map[string]any so it serializes cleanly through the
// structpb-backed DoCommand wire format (which rejects []map[string]any
// as a list value). machineLogsURL, clipDataURL, and planRequestURL, when
// non-empty, add clickable app.viam.com deep-links (machine logs, the order's
// video clip filtered by tag, and the order's plan-request files) to the
// footer.
func slackFailureBlocks(r order.Reading, machineLogsURL, clipDataURL, planRequestURL string) []any {
	header := ":x: Order failed"
	stepLabel := "*Failed at:*"
	if r.OperatorCancelled {
		header = ":warning: Order cancelled by operator"
		stepLabel = "*Cancelled at:*"
	}

	duration := r.EndedAt.Sub(r.StartedAt).Round(time.Second)
	blocks := []any{
		map[string]any{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": header, "emoji": true},
		},
		map[string]any{
			"type": "section",
			"fields": []any{
				slackField("*Drink:*", escapeSlackMrkdwn(r.Order.Drink)),
				slackField("*Customer:*", slackCustomer(r)),
				slackField(stepLabel, slackStep(r)),
				slackField("*Duration:*", duration.String()),
				slackField("*Decaf:*", slackBool(r.Decaf)),
			},
		},
	}

	// Show the error in a code block for faults; an operator cancel has no
	// meaningful error to surface.
	if !r.OperatorCancelled {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": fmt.Sprintf("*Error:*\n```%s```", slackErrMsg(r)),
			},
		})
	}

	footer := fmt.Sprintf("Order `%s`", r.Order.ID)
	if !r.StartedAt.IsZero() {
		// Slack renders <!date> in the reader's timezone; the trailing pipe value
		// is the fallback shown when it can't.
		footer += fmt.Sprintf(" · started <!date^%d^{date_short_pretty} {time}|%s>",
			r.StartedAt.Unix(), r.StartedAt.UTC().Format(time.RFC3339))
	}
	if r.TraceID != "" {
		footer += fmt.Sprintf(" · trace `%s`", r.TraceID)
	}
	if machineLogsURL != "" {
		footer += fmt.Sprintf(" · <%s|machine logs>", machineLogsURL)
	}
	if clipDataURL != "" {
		footer += fmt.Sprintf(" · <%s|video clip> _(may take ~a minute to appear)_", clipDataURL)
	}
	if planRequestURL != "" {
		footer += fmt.Sprintf(" · <%s|plan requests> _(may take ~a minute to appear)_", planRequestURL)
	}
	blocks = append(blocks, map[string]any{
		"type":     "context",
		"elements": []any{map[string]any{"type": "mrkdwn", "text": footer}},
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
// — scopes the view and orgID — from VIAM_PRIMARY_ORG_ID — scopes the org.
// robotName is intentionally omitted: there is no robot-name env var, and the
// UUID tag makes it redundant. Returns "" when locationID is empty (e.g. a
// local/test machine), so callers can omit the link. Note: the clip is uploaded
// asynchronously after the notification is sent, so the link may show no
// results for the first ~15-60s.
func buildClipDataURL(locationID, orgID, orderID string) string {
	if locationID == "" {
		return ""
	}
	return fmt.Sprintf("https://app.viam.com/data/all?locationId=%s&tags=%s&view=media%s",
		locationID, orderID, orgQueryParam(orgID))
}

// buildPlanRequestDataURL constructs an app.viam.com data-page deep-link to the
// order's plan-request files, filtered by the order-ID tag (view=files keeps it
// distinct from the order's video clip; the reader can narrow further by the
// planning_failure or step tags). Returns "" when locationID is empty (e.g. a
// local/test machine). Like the clip link, the files sync asynchronously, so the
// page may be empty for the first sync interval.
func buildPlanRequestDataURL(locationID, orgID, orderID string) string {
	if locationID == "" {
		return ""
	}
	return fmt.Sprintf("https://app.viam.com/data/all?locationId=%s&tags=%s&view=files%s",
		locationID, orderID, orgQueryParam(orgID))
}

// orgQueryParam renders the "&org=..." suffix that pins an app.viam.com link to
// the owning org, so a reader who belongs to several orgs lands in the right one
// instead of whichever org their session last used. Returns "" when the org is
// unknown, leaving a link that still resolves in the reader's current org.
func orgQueryParam(orgID string) string {
	if orgID == "" {
		return ""
	}
	return "&org=" + orgID
}

// slackField builds a single mrkdwn field ("*Label:*\nvalue") for a Block Kit
// section fields array.
func slackField(label, value string) map[string]any {
	return map[string]any{"type": "mrkdwn", "text": fmt.Sprintf("%s\n%s", label, value)}
}

// slackMrkdwnEscaper applies Slack's three control-character escapes. Slack
// parses <...> in mrkdwn and in a message's top-level text as a mention or a
// link, so an unescaped "<!channel>" pings the channel and "<https://x|Click
// here>" renders a disguised link.
var slackMrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// escapeSlackMrkdwn makes a value safe to embed in Slack mrkdwn or fallback
// text. Apply it to every value a customer or peer can influence; markup this
// package builds itself (links, dates) must stay raw.
func escapeSlackMrkdwn(s string) string {
	return slackMrkdwnEscaper.Replace(s)
}

func slackBool(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// slackCustomer is the name typed at the kiosk, escaped for mrkdwn.
func slackCustomer(r order.Reading) string {
	if r.Order.CustomerName == "" {
		return "an unnamed customer"
	}
	return escapeSlackMrkdwn(r.Order.CustomerName)
}

func slackStep(r order.Reading) string {
	if r.FailedStep == "" {
		return "an unknown step"
	}
	return r.FailedStep
}

// slackErrMsg is escaped because an error can wrap arbitrary strings, such as
// a peer's response or a request value.
func slackErrMsg(r order.Reading) string {
	if r.ExecErr != nil {
		return escapeSlackMrkdwn(r.ExecErr.Error())
	}
	return "unknown error"
}
