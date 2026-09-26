// Package report builds what the coffee service posts to Slack — the per-order
// failure alert, the daily order digest, and the weekly chore wheel — along
// with the app.viam.com deep-links those messages carry, and sends them through
// a Notifier wrapping the viam:notifications:slack service.
package report

import "context"

// DoCommander is the one method Notifier needs from the Slack service. Keeping
// it this small lets tests pass in a fake.
type DoCommander interface {
	DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error)
}

// Notifier sends messages through a viam:notifications:slack generic service.
// A nil *Notifier means no slack_notifier_name is configured; callers check for
// nil before sending.
type Notifier struct {
	svc DoCommander
}

// NewNotifier wraps the Slack service, returning nil when svc is nil so the
// unconfigured case stays a plain nil check.
func NewNotifier(svc DoCommander) *Notifier {
	if svc == nil {
		return nil
	}
	return &Notifier{svc: svc}
}

// Send posts one message. The viam:notifications:slack service defaults command
// to "send". blocks renders the rich layout and is left out of the payload when
// nil; text is the notification/accessibility fallback Slack shows when blocks
// can't render. channel_id/webhook are configured on the service. blocks must be
// []any of map[string]any, since the structpb-backed DoCommand wire format
// rejects []map[string]any as a list value.
func (n *Notifier) Send(ctx context.Context, text string, blocks []any) (map[string]any, error) {
	cmd := map[string]any{
		"command": "send",
		"text":    text,
	}
	if blocks != nil {
		cmd["blocks"] = blocks
	}
	return n.svc.DoCommand(ctx, cmd)
}
