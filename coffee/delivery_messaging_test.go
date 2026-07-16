package coffee

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// fakePeer records DoCommand calls so tests can assert what was sent to the
// peer machine, and replies with a canned response.
type fakePeer struct {
	resource.AlwaysRebuild
	mu   sync.Mutex
	got  []map[string]any
	resp map[string]any
	err  error
}

func (f *fakePeer) Name() resource.Name { return resource.Name{} }
func (f *fakePeer) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	f.mu.Lock()
	f.got = append(f.got, cmd)
	f.mu.Unlock()
	return f.resp, f.err
}
func (f *fakePeer) Close(_ context.Context) error { return nil }
func (f *fakePeer) Status(_ context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestSendDeliveryMessage(t *testing.T) {
	peer := &fakePeer{resp: map[string]any{"received": true}}
	c := &beanjaminCoffee{
		logger:          logging.NewTestLogger(t),
		deliveryHandler: peer,
	}

	payload := map[string]any{"text": "hello from the coffee machine"}
	resp, err := c.sendDeliveryMessage(context.Background(), payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sent, _ := resp["sent"].(bool); !sent {
		t.Errorf("response sent = %v, want true", resp["sent"])
	}
	if len(peer.got) != 1 {
		t.Fatalf("peer received %d commands, want 1", len(peer.got))
	}
	// The payload must arrive under the receive_message key — the command the
	// peer's own dispatch table answers.
	forwarded, ok := peer.got[0]["receive_message"].(map[string]any)
	if !ok {
		t.Fatalf("peer command missing receive_message key: %v", peer.got[0])
	}
	if forwarded["text"] != "hello from the coffee machine" {
		t.Errorf("forwarded payload = %v, want original text", forwarded)
	}
}

func TestSendDeliveryMessage_NotConfigured(t *testing.T) {
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t)}
	if _, err := c.sendDeliveryMessage(context.Background(), "hi"); err == nil {
		t.Fatal("expected error when no delivery handler is configured, got nil")
	}
}

func TestSendDeliveryMessage_PeerError(t *testing.T) {
	peer := &fakePeer{err: errors.New("peer offline")}
	c := &beanjaminCoffee{
		logger:          logging.NewTestLogger(t),
		deliveryHandler: peer,
	}
	if _, err := c.sendDeliveryMessage(context.Background(), "hi"); err == nil {
		t.Fatal("expected error when the peer DoCommand fails, got nil")
	}
}
