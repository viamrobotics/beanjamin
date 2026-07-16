package coffee

// Peer-machine messaging over the delivery_handler_name generic service —
// the transport groundwork for coffee → delivery-machine notifications. The
// channel is one-way by design: this machine announces (e.g. drink ready and
// where it was placed) and doesn't need to hear back — slot availability is
// observed by this machine's own camera. Only the channel exists so far:
// send_delivery_message forwards an arbitrary payload to the peer. The real
// delivery vocabulary (drink_ready, …) will build on this.

import (
	"context"
	"fmt"
	"time"
)

// deliveryMessageTimeout caps how long a single peer DoCommand may take, so a
// wedged peer connection fails the send instead of hanging the caller.
const deliveryMessageTimeout = 10 * time.Second

// sendDeliveryMessage forwards payload to the configured peer service as a
// receive_message DoCommand and returns the peer's response. Sending is
// synchronous — the caller (a test DoCommand for now) wants the round-trip
// confirmation.
//
// TODO(delivery): when this gets called from readyForDelivery mid-brew, a
// wedged peer would stall the brew loop for the full timeout — switch that
// call site to a detached goroutine like notifyOrderFailureSlack.
func (s *beanjaminCoffee) sendDeliveryMessage(ctx context.Context, payload any) (map[string]any, error) {
	if s.deliveryHandler == nil {
		return nil, fmt.Errorf("no delivery_handler_name configured")
	}
	ctx, cancel := context.WithTimeout(ctx, deliveryMessageTimeout)
	defer cancel()
	resp, err := s.deliveryHandler.DoCommand(ctx, map[string]any{
		"receive_message": payload,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to send message to delivery handler: %w", err)
	}
	s.logger.Infof("sent message to delivery handler: %v (response: %v)", payload, resp)
	return map[string]any{
		"sent":          true,
		"peer_response": resp,
	}, nil
}
