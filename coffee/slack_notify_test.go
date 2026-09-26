package coffee

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"

	"beanjamin/coffee/order"
	"beanjamin/coffee/report"
)

// newSlackTestCoffee wires a coffee service to a fake Slack service that hands
// every DoCommand payload to the returned channel.
func newSlackTestCoffee(t *testing.T) (*beanjaminCoffee, <-chan map[string]any) {
	t.Helper()
	sent := make(chan map[string]any, 1)
	slack := inject.NewGenericService("slack")
	slack.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
		sent <- cmd
		return map[string]any{}, nil
	}
	return &beanjaminCoffee{
		logger:         logging.NewTestLogger(t),
		cfg:            &Config{},
		slackNotifier:  report.NewNotifier(slack),
		machineLogsURL: "https://app.viam.com/machine/m1/logs?org=o1",
		dataLocationID: "loc1",
		primaryOrgID:   "o1",
	}, sent
}

func awaitSlack(t *testing.T, sent <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case cmd := <-sent:
		return cmd
	case <-time.After(5 * time.Second):
		t.Fatal("no Slack DoCommand was sent")
		return nil
	}
}

// The failure alert's DoCommand payload is exactly command, text, and blocks.
func TestNotifyOrderFailureSlackPayload(t *testing.T) {
	s, sent := newSlackTestCoffee(t)
	started := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	r := order.Reading{
		Order:      order.Order{ID: "order-1", Drink: "espresso", CustomerName: "Jane"},
		ExecErr:    errors.New("grinder jammed"),
		FailedStep: stepGrinding,
		StartedAt:  started,
		EndedAt:    started.Add(time.Minute),
	}

	s.notifyOrderFailureSlack(r)

	planURL := report.PlanRequestDataURL("loc1", "o1", "order-1")
	want := map[string]any{
		"command": "send",
		"text":    report.FailureText(r),
		"blocks":  report.FailureBlocks(r, s.machineLogsURL, "", planURL),
	}
	if got := awaitSlack(t, sent); !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %#v, want %#v", got, want)
	}
}

// The keep-alive notice is plain text: the payload carries no blocks key.
func TestNotifyKeepAliveFailureSlackPayload(t *testing.T) {
	s, sent := newSlackTestCoffee(t)

	s.notifyKeepAliveFailureSlack(errors.New("arm busy"))

	got := awaitSlack(t, sent)
	if len(got) != 2 || got["command"] != "send" {
		t.Fatalf("payload = %#v, want only command and text", got)
	}
	text, _ := got["text"].(string)
	if !strings.HasPrefix(text, ":warning: Keep-alive purge failed") || !strings.HasSuffix(text, "Error: arm busy") {
		t.Errorf("text = %q", text)
	}
}
