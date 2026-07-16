package coffee

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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

// commands returns a copy of the DoCommands received so far.
func (f *fakePeer) commands() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.got))
	copy(out, f.got)
	return out
}

func TestSendDeliveryMessage(t *testing.T) {
	peer := &fakePeer{resp: map[string]any{"mission": "started"}}
	c := &beanjaminCoffee{
		logger:          logging.NewTestLogger(t),
		deliveryHandler: peer,
	}

	command := map[string]any{"start_mission": map[string]any{"waypoint": "coffee-counter"}}
	resp, err := c.sendDeliveryMessage(context.Background(), command)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sent, _ := resp["sent"].(bool); !sent {
		t.Errorf("response sent = %v, want true", resp["sent"])
	}
	peerResp, _ := resp["peer_response"].(map[string]any)
	if peerResp["mission"] != "started" {
		t.Errorf("peer_response = %v, want the peer's reply", resp["peer_response"])
	}
	if len(peer.got) != 1 {
		t.Fatalf("peer received %d commands, want 1", len(peer.got))
	}
	// The command must arrive verbatim — no wrapping, no extra keys — since
	// it has to match whatever vocabulary the peer's own service speaks.
	mission, ok := peer.got[0]["start_mission"].(map[string]any)
	if !ok || len(peer.got[0]) != 1 {
		t.Fatalf("peer command not forwarded verbatim: %v", peer.got[0])
	}
	if mission["waypoint"] != "coffee-counter" {
		t.Errorf("forwarded command = %v, want original", peer.got[0])
	}
}

func TestSendDeliveryMessage_NotConfigured(t *testing.T) {
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t)}
	if _, err := c.sendDeliveryMessage(context.Background(), map[string]any{"x": 1}); err == nil {
		t.Fatal("expected error when no delivery handler is configured, got nil")
	}
}

func TestSendDeliveryMessage_RejectsNonObject(t *testing.T) {
	peer := &fakePeer{}
	c := &beanjaminCoffee{
		logger:          logging.NewTestLogger(t),
		deliveryHandler: peer,
	}
	for _, bad := range []any{"just a string", true, nil, map[string]any{}} {
		if _, err := c.sendDeliveryMessage(context.Background(), bad); err == nil {
			t.Errorf("expected error for payload %v, got nil", bad)
		}
	}
	if len(peer.got) != 0 {
		t.Errorf("peer should receive nothing for rejected payloads, got %d", len(peer.got))
	}
}

func TestNotifyDeliveryRequest(t *testing.T) {
	peer := &fakePeer{resp: map[string]any{"received": true}}
	c := &beanjaminCoffee{
		logger:          logging.NewTestLogger(t),
		deliveryHandler: peer,
	}
	enqueued := time.Date(2026, 7, 16, 15, 4, 5, 0, time.UTC)
	order := Order{
		ID:            "order-123",
		Drink:         "iced_coffee",
		CustomerName:  "Alice",
		CustomerEmail: "alice@example.com",
		EnqueuedAt:    enqueued,
	}

	// Synchronous: the request has been sent (and acknowledged) by return.
	c.notifyDeliveryRequest(context.Background(), order, 3)

	got := peer.commands()
	if len(got) != 1 {
		t.Fatalf("peer received %d commands, want 1", len(got))
	}
	req, ok := got[0]["delivery_request"].(map[string]any)
	if !ok {
		t.Fatalf("peer command missing delivery_request key: %v", got[0])
	}
	if req["order_id"] != "order-123" {
		t.Errorf("order_id = %v, want order-123", req["order_id"])
	}
	if req["order_timestamp"] != "2026-07-16T15:04:05Z" {
		t.Errorf("order_timestamp = %v, want RFC3339 of EnqueuedAt", req["order_timestamp"])
	}
	if req["cup_type"] != "glass" {
		t.Errorf("cup_type = %v, want %q for iced_coffee", req["cup_type"], "glass")
	}
	if req["customer_email"] != "alice@example.com" {
		t.Errorf("customer_email = %v, want alice@example.com", req["customer_email"])
	}
	if req["pickup_position"] != 3 {
		t.Errorf("pickup_position = %v, want 3", req["pickup_position"])
	}
}

func TestBuildDeliveryRequest_HotDrinkUsesCup(t *testing.T) {
	req := buildDeliveryRequest(Order{ID: "x", Drink: "espresso"}, 1)["delivery_request"].(map[string]any)
	if req["cup_type"] != "cup" {
		t.Errorf("cup_type = %v, want %q for espresso", req["cup_type"], "cup")
	}
}

func TestNotifyDeliveryRequest_NotConfigured(t *testing.T) {
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t)}
	// Must be a silent no-op, not a panic — delivery orders still brew fine
	// on machines without a delivery bot.
	c.notifyDeliveryRequest(context.Background(), Order{ID: "x"}, 1)
}

func TestNotifyDeliveryRequest_PeerErrorDoesNotPanic(t *testing.T) {
	peer := &fakePeer{err: errors.New("peer offline")}
	c := &beanjaminCoffee{
		logger:          logging.NewTestLogger(t),
		deliveryHandler: peer,
	}
	// A failed send is logged, never fatal — the drink is already served.
	c.notifyDeliveryRequest(context.Background(), Order{ID: "x"}, 1)
}

func TestSendDeliveryMessage_PeerError(t *testing.T) {
	peer := &fakePeer{err: errors.New("peer offline")}
	c := &beanjaminCoffee{
		logger:          logging.NewTestLogger(t),
		deliveryHandler: peer,
	}
	if _, err := c.sendDeliveryMessage(context.Background(), map[string]any{"x": 1}); err == nil {
		t.Fatal("expected error when the peer DoCommand fails, got nil")
	}
}
