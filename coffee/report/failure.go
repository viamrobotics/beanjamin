package report

import (
	"fmt"
	"strings"
	"time"

	"beanjamin/coffee/order"
)

// FailureText builds the plain-text fallback Slack shows in notifications
// and when Block Kit can't render. Operator cancels and genuine faults get
// distinct wording so readers can tell a deliberate stop from a real fault at a
// glance.
func FailureText(r order.Reading) string {
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

// FailureBlocks builds a Slack Block Kit layout for a failed or cancelled
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
func FailureBlocks(r order.Reading, machineLogsURL, clipDataURL, planRequestURL string) []any {
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
