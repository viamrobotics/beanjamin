package coffee

// Peer-machine messaging over the delivery_handler_name generic service —
// the coffee → delivery-machine notification channel. The channel is one-way
// by design: this machine announces a finished delivery order (via the
// delivery_request command the delivery bot's own service understands) and
// doesn't need progress reports back — slot availability is observed by this
// machine's own camera. send_delivery_message additionally forwards an
// arbitrary command verbatim, as a manual test hook for the channel. The
// pull side, which a rover that missed the push polls and reports collections
// to, is pending_deliveries.go.

import (
	"context"
	"fmt"
	"time"

	"beanjamin/coffee/order"
	"beanjamin/coffee/speech"
)

// deliveryMessageTimeout caps how long a single peer DoCommand may take, so a
// wedged peer connection fails the send instead of hanging the caller.
const deliveryMessageTimeout = 10 * time.Second

// buildDeliveryRequest assembles the delivery_request DoCommand the delivery
// bot's service expects for a finished delivery order. pickup_position is the
// 0-based serving-area slot the drink was placed in (Order.PickupPosition).
// customer_email is always non-empty here: enqueueOrder rejects delivery
// orders without one.
func buildDeliveryRequest(o order.Order) map[string]any {
	return map[string]any{
		"delivery_request": map[string]any{
			"order_id":        o.ID,
			"order_timestamp": o.EnqueuedAt.UTC().Format(time.RFC3339),
			"cup_type":        deliveryCupType(o.Drink),
			"customer_email":  o.CustomerEmail,
			"pickup_position": o.PickupPosition,
		},
	}
}

// deliveryCupType is the container a drink is served in, as the delivery bot
// knows it: iced drinks go in the tall glass, everything else in the standard
// espresso cup. Same container labels as the pickup pipeline.
func deliveryCupType(drink string) string {
	if order.IsIced(drink) {
		return pickupLabelGlass
	}
	return pickupLabelCup
}

// notifyDeliveryRequest sends the delivery_request for a finished delivery
// order to the peer service and waits (up to deliveryMessageTimeout) for its
// {"received": bool} acknowledgment. Deliberately synchronous: the order
// isn't treated as handed off until the bot has confirmed it took the
// request, so a failed or unacknowledged send is known — and loggable —
// before this machine moves on. Best-effort beyond that: a no-op when no
// delivery_handler_name is configured, and failures are logged rather than
// failing the order (the drink is already sitting in the serving area).
// Reports whether the bot acknowledged the request.
func (s *beanjaminCoffee) notifyDeliveryRequest(ctx context.Context, order order.Order) bool {
	if s.deliveryHandler == nil {
		s.logger.Warnf("no delivery_handler_name configured — skipping delivery request for order %s", order.ID)
		return false
	}
	logger := s.logger.WithFields("order_id", order.ID)
	ctx, cancel := context.WithTimeout(ctx, deliveryMessageTimeout)
	defer cancel()
	resp, err := s.deliveryHandler.DoCommand(ctx, buildDeliveryRequest(order))
	if err != nil {
		logger.Warnf("failed to send delivery request: %v", err)
		return false
	}
	if received, _ := resp["received"].(bool); !received {
		logger.Warnf("delivery request not acknowledged by the delivery machine (response: %v)", resp)
		return false
	}
	logger.Infof("delivery request acknowledged, pickup position %d", order.PickupPosition)
	return true
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

// readyForDelivery handles the cup-handoff moment for delivery-fulfillment
// orders, replacing the pickup drink-ready announcement: it sends the
// delivery_request to the delivery machine and waits for its acknowledgment
// (bounded by deliveryMessageTimeout) before speaking, so the order isn't
// announced as handed off on the strength of a request nobody confirmed.
// The caller sets order.PickupPosition from the serving step.
//
// The order goes on the pending-deliveries ledger before the push, so a rover
// that collects the drink the moment it is told about it can't race the record,
// and whether the push landed or not, get_pending_deliveries can still find it.
func (s *beanjaminCoffee) readyForDelivery(ctx context.Context, order order.Order) error {
	s.recordPendingDelivery(order)
	if s.notifyDeliveryRequest(ctx, order) {
		s.pendingDeliveries.Acknowledge(order.ID)
	}
	drink := speech.SpeakableDrink(order.Drink)
	text := fmt.Sprintf("%s ready for delivery!", drink)
	if name := order.DisplayName(); name != "" {
		text = fmt.Sprintf("%s for %s, ready for delivery!", drink, name)
	}
	return s.speaker.SayAlways(ctx, text)
}
