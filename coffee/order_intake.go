package coffee

import (
	"context"
	"fmt"
	"strings"

	"beanjamin/coffee/order"
)

// menu is the set of optional drinks this machine's config lets it serve.
func (s *beanjaminCoffee) menu() order.Menu {
	return order.Menu{
		Decaf:     s.cfg.CanServeDecaf,
		Iced:      s.cfg.CanServeIced,
		IcedLatte: s.cfg.CanServeIcedLatte,
	}
}

// enqueueOrder validates the order and adds it to the queue.
// It returns immediately with the queue position. When the optional
// "count" field is > 1, N identical orders are enqueued back-to-back
// (each with its own UUID) and the per-order "Order received" line is
// replaced with a single consolidated batch announcement.
func (s *beanjaminCoffee) enqueueOrder(ctx context.Context, orderRaw any) (map[string]any, error) {
	s.logger.Infof("received order request")

	req, err := order.DecodeRequest(orderRaw)
	if err != nil {
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}
	drink := req.Drink
	customerName := strings.TrimSpace(req.CustomerName)
	s.logger.Infof("order request: drink=%q customer=%q", drink, customerName)

	if ok, reason := s.menu().Supports(drink); !ok {
		s.logger.Infof("rejected order for drink %q from %s (%s)", drink, customerName, reason)
		msg := pickUnsupportedDrink(drink)
		if err := s.say(ctx, msg); err != nil {
			s.logger.Warnf("failed to say rejection: %v", err)
		}
		return nil, fmt.Errorf("unsupported drink %q: %s", drink, msg)
	}

	fulfillment, err := order.ParseFulfillment(req.Fulfillment)
	if err != nil {
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}
	// Delivery orders must be attributable: the delivery bot identifies the
	// recipient by email, so an anonymous delivery has nowhere to go. Pickup
	// (the default) stays open to anonymous walk-ups.
	if fulfillment == order.FulfillmentDelivery && req.CustomerEmail == "" {
		err := fmt.Errorf("delivery orders require a customer_email")
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}

	displayName := req.ModifiedCustomerName
	if displayName == "" {
		displayName = customerName
	}

	count, err := order.ParseCount(req.Count, s.maxBatchSize())
	if err != nil {
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}

	ids := make([]string, 0, count)
	var firstPos int
	for i := 0; i < count; i++ {
		// For single orders, auto-pick a brew-start greeting if one wasn't
		// supplied. For batches we deliberately leave Greeting empty: the
		// consolidated batch announcement at submission already covered
		// "we got your order", so executeQueuedOrder speaking another
		// "let me whip up your espresso!" before each of N cups is just
		// noise. (executeQueuedOrder skips the speech when Greeting is "".)
		greeting := req.InitialGreeting
		if greeting == "" && count == 1 {
			greeting = pickGreeting(drink, displayName)
		}
		o := order.NewOrder(drink, customerName, greeting, req.CompletionStatement)
		o.ModifiedCustomerName = req.ModifiedCustomerName
		o.CustomerEmail = req.CustomerEmail
		o.Fulfillment = fulfillment
		if count > 1 {
			o.BatchIndex = i + 1
			o.BatchSize = count
		}
		pos := s.queue.Enqueue(o)
		if i == 0 {
			firstPos = pos
		}
		ids = append(ids, o.ID)
		s.logger.Infof("order %s queued at position %d for %s (batch %d/%d)",
			o.ID, pos, customerName, i+1, count)
	}

	// A batch gets one consolidated announcement rather than N blocking TTS
	// calls back-to-back. A single order is acknowledged only when it isn't
	// first in line; at position 1 the brew-start greeting follows right away
	// and covers it.
	switch {
	case count == 1 && firstPos > 1:
		if err := s.say(ctx, pickOrderReceived(drink, displayName)); err != nil {
			s.logger.Warnf("failed to announce order %s: %v", ids[0], err)
		}
	case count > 1:
		if err := s.say(ctx, pickOrderReceivedBatch(drink, displayName, count)); err != nil {
			s.logger.Warnf("failed to announce batch: %v", err)
		}
	}

	return map[string]any{
		"status":         "queued",
		"order_id":       ids[0],
		"queue_position": firstPos,
		"customer_name":  customerName,
		"order_ids":      ids,
		"count":          count,
	}, nil
}
