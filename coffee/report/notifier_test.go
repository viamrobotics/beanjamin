package report

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

type fakeSlack struct {
	sent []map[string]any
	resp map[string]any
	err  error
}

func (f *fakeSlack) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	f.sent = append(f.sent, cmd)
	return f.resp, f.err
}

func TestNewNotifierNilIsUnconfigured(t *testing.T) {
	if n := NewNotifier(nil); n != nil {
		t.Errorf("NewNotifier(nil) = %v, want nil", n)
	}
}

// The payload is what the Slack service receives, so it must hold exactly the
// command, the text, and the blocks when there are any — nothing else.
func TestNotifierSendPayload(t *testing.T) {
	blocks := []any{mrkdwnSection("hello")}
	for _, tc := range []struct {
		name   string
		blocks []any
		want   map[string]any
	}{
		{"with blocks", blocks, map[string]any{"command": "send", "text": "fallback", "blocks": blocks}},
		{"text only", nil, map[string]any{"command": "send", "text": "fallback"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slack := &fakeSlack{resp: map[string]any{"ok": true}}
			resp, err := NewNotifier(slack).Send(context.Background(), "fallback", tc.blocks)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resp, slack.resp) {
				t.Errorf("resp = %v, want the service's response %v", resp, slack.resp)
			}
			if len(slack.sent) != 1 {
				t.Fatalf("sent %d commands, want 1", len(slack.sent))
			}
			if !reflect.DeepEqual(slack.sent[0], tc.want) {
				t.Errorf("payload = %#v, want %#v", slack.sent[0], tc.want)
			}
			if _, err := structpb.NewStruct(slack.sent[0]); err != nil {
				t.Errorf("payload does not convert to structpb: %v", err)
			}
		})
	}
}

func TestNotifierSendReturnsServiceError(t *testing.T) {
	want := errors.New("channel_not_found")
	if _, err := NewNotifier(&fakeSlack{err: want}).Send(context.Background(), "x", nil); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}
