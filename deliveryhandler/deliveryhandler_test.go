package deliveryhandler

import (
	"context"
	"testing"

	"go.viam.com/rdk/logging"
)

func TestReceiveMessage(t *testing.T) {
	h := &deliveryHandler{logger: logging.NewTestLogger(t)}
	resp, err := h.DoCommand(context.Background(), map[string]any{
		"receive_message": map[string]any{"text": "drink ready"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received, _ := resp["received"].(bool); !received {
		t.Errorf("received = %v, want true", resp["received"])
	}
	msg, _ := resp["message"].(map[string]any)
	if msg["text"] != "drink ready" {
		t.Errorf("echoed message = %v, want original payload", resp["message"])
	}
}

func TestUnknownCommand(t *testing.T) {
	h := &deliveryHandler{logger: logging.NewTestLogger(t)}
	if _, err := h.DoCommand(context.Background(), map[string]any{"bogus": true}); err == nil {
		t.Fatal("expected error for unknown command, got nil")
	}
}
