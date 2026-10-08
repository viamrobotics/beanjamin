package coffee

// The pull side of the delivery channel: a ledger of delivery orders whose
// drink is on the shelf and not yet collected, which a rover polls with
// get_pending_deliveries and clears with delivery_collected. The push in
// delivery_messaging.go is still sent; this exists so a rover that was busy or
// offline when it went out can still find the drink.

import (
	"fmt"
	"time"

	"beanjamin/coffee/order"
)

// recordPendingDelivery puts a served delivery order on the ledger, not yet
// acknowledged. o.PickupPosition must already hold the slot it was served into.
func (s *beanjaminCoffee) recordPendingDelivery(o order.Order) {
	s.pendingDeliveries.Record(order.PendingDelivery{
		OrderID:        o.ID,
		Drink:          o.Drink,
		CupType:        deliveryCupType(o.Drink),
		CustomerName:   o.CustomerName,
		CustomerEmail:  o.CustomerEmail,
		PickupPosition: o.PickupPosition,
		OrderTimestamp: o.EnqueuedAt,
	})
}

// releaseServingSlot drops any pending delivery recorded at slot, now that
// something else has been served into it. Slot selection assumes the earlier
// cup was taken by the time its slot comes round again (serving_slots.go), so
// that drink is treated as gone. Warns, because a rover still on its way would
// otherwise be sent for a cup that is no longer there.
func (s *beanjaminCoffee) releaseServingSlot(slot int) {
	for _, p := range s.pendingDeliveries.ClearSlot(slot) {
		s.activeOrderLogger().Warnf("serving slot %d reused — dropping uncollected delivery for order %s (served %s ago)",
			slot, p.OrderID, time.Since(p.ServedAt).Round(time.Second))
	}
}

// getPendingDeliveries reports the uncollected delivery orders, oldest first.
// Numbers are float64 to match what gRPC callers see, as in Status.
func (s *beanjaminCoffee) getPendingDeliveries() (map[string]any, error) {
	pending := s.pendingDeliveries.List()
	// structpb only accepts []any for list values.
	deliveries := make([]any, len(pending))
	for i, p := range pending {
		deliveries[i] = map[string]any{
			"order_id":        p.OrderID,
			"drink":           p.Drink,
			"cup_type":        p.CupType,
			"customer_name":   p.CustomerName,
			"customer_email":  p.CustomerEmail,
			"pickup_position": float64(p.PickupPosition),
			"order_timestamp": p.OrderTimestamp.UTC().Format(time.RFC3339),
			"served_at":       p.ServedAt.UTC().Format(time.RFC3339),
			"acknowledged":    p.Acknowledged,
		}
	}
	return map[string]any{
		"deliveries": deliveries,
		"count":      float64(len(deliveries)),
	}, nil
}

// deliveryCollected takes the order off the ledger once the rover has the
// drink. An order that is not pending — already collected, expired, or never
// served — reports collected: false rather than an error, so a poller that
// retries after a lost response doesn't see a failure.
func (s *beanjaminCoffee) deliveryCollected(v any) (map[string]any, error) {
	id, ok := v.(string)
	if !ok || id == "" {
		return nil, fmt.Errorf("delivery_collected must be a non-empty order ID string, got %T %v", v, v)
	}
	collected := s.pendingDeliveries.Collect(id)
	if collected {
		s.logger.Infof("delivery collected for order %s", id)
	} else {
		s.logger.Debugf("delivery_collected: order %s is not pending", id)
	}
	return map[string]any{"collected": collected, "order_id": id}, nil
}
