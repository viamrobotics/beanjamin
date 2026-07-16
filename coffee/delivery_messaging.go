package coffee

// Peer-machine messaging over the delivery_handler_name generic service —
// the coffee → delivery-machine notification channel. The channel is one-way
// by design: this machine announces a finished delivery order (via the
// delivery_request command the delivery bot's own service understands) and
// doesn't need progress reports back — slot availability is observed by this
// machine's own camera. send_delivery_message additionally forwards an
// arbitrary command verbatim, as a manual test hook for the channel.

import (
	"context"
	"fmt"
	"time"
)

// deliveryMessageTimeout caps how long a single peer DoCommand may take, so a
// wedged peer connection fails the send instead of hanging the caller.
const deliveryMessageTimeout = 10 * time.Second

// buildDeliveryRequest assembles the delivery_request DoCommand the delivery
// bot's service expects for a finished delivery order. pickupPosition is the
// 1-based serving-area slot the drink was placed in. customer_email is always
// non-empty here: enqueueOrder rejects delivery orders without one.
func buildDeliveryRequest(order Order, pickupPosition int) map[string]any {
	// Iced drinks are served in the tall glass; everything else in the
	// standard espresso cup. Same container labels as the pickup pipeline.
	container := pickupLabelCup
	if isIcedDrink(order.Drink) {
		container = pickupLabelGlass
	}
	return map[string]any{
		"delivery_request": map[string]any{
			"order_id":        order.ID,
			"order_timestamp": order.EnqueuedAt.UTC().Format(time.RFC3339),
			"cup_type":        container,
			"customer_email":  order.CustomerEmail,
			"pickup_position": pickupPosition,
		},
	}
}

// notifyDeliveryRequest sends the delivery_request for a finished delivery
// order to the peer service and waits (up to deliveryMessageTimeout) for its
// {"received": bool} acknowledgment. Deliberately synchronous: the order
// isn't treated as handed off until the bot has confirmed it took the
// request, so a failed or unacknowledged send is known — and loggable —
// before this machine moves on. Best-effort beyond that: a no-op when no
// delivery_handler_name is configured, and failures are logged rather than
// failing the order (the drink is already sitting in the serving area).
func (s *beanjaminCoffee) notifyDeliveryRequest(ctx context.Context, order Order, pickupPosition int) {
	if s.deliveryHandler == nil {
		s.logger.Debugf("no delivery_handler_name configured — skipping delivery request for order %s", order.ID)
		return
	}
	logger := s.logger.WithFields("order_id", order.ID)
	ctx, cancel := context.WithTimeout(ctx, deliveryMessageTimeout)
	defer cancel()
	resp, err := s.deliveryHandler.DoCommand(ctx, buildDeliveryRequest(order, pickupPosition))
	if err != nil {
		logger.Warnf("failed to send delivery request: %v", err)
		return
	}
	if received, _ := resp["received"].(bool); !received {
		logger.Warnf("delivery request not acknowledged by the delivery machine (response: %v)", resp)
		return
	}
	logger.Infof("delivery request acknowledged, pickup position %d", pickupPosition)
}

// sendDeliveryMessage runs command verbatim as a DoCommand on the configured
// peer service and returns the peer's response — a manual test hook for the
// channel. Sending is synchronous: the caller wants the round-trip
// confirmation.
func (s *beanjaminCoffee) sendDeliveryMessage(ctx context.Context, command any) (map[string]any, error) {
	if s.deliveryHandler == nil {
		return nil, fmt.Errorf("no delivery_handler_name configured")
	}
	cmd, ok := command.(map[string]any)
	if !ok || len(cmd) == 0 {
		return nil, fmt.Errorf("send_delivery_message value must be a non-empty object: the DoCommand to run on the peer service, got %T", command)
	}
	ctx, cancel := context.WithTimeout(ctx, deliveryMessageTimeout)
	defer cancel()
	resp, err := s.deliveryHandler.DoCommand(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to send command to delivery handler: %w", err)
	}
	s.logger.Infof("sent command to delivery handler: %v (response: %v)", cmd, resp)
	return map[string]any{
		"sent":          true,
		"peer_response": resp,
	}, nil
}
