package coffee

import (
	"context"
	"fmt"
	"strings"

	"beanjamin/coffee/order"
	"beanjamin/coffee/speech"
)

// menu is the set of optional drinks this machine's config lets it serve.
func (s *beanjaminCoffee) menu() order.Menu {
	return order.Menu{
		Beans:     s.cfg.beanNames(),
		Iced:      s.cfg.CanServeIced,
		IcedLatte: s.cfg.CanServeIcedLatte,
		Milks:     s.cfg.milkNames(),
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
	// Legacy decaf ids name the beans too; they may not contradict an
	// explicit beans.
	if base, beans, ok := order.ParseLegacyDrink(drink); ok {
		if req.Beans != "" && req.Beans != beans {
			err := fmt.Errorf("drink %q is %s beans, but the order asks for beans %q", drink, beans, req.Beans)
			s.logger.Warnf("rejected order: %v", err)
			return nil, err
		}
		drink, req.Beans = base, beans
	}
	customerName := strings.TrimSpace(req.CustomerName)
	s.logger.Infof("order request: drink=%q beans=%q milk=%q customer=%q", drink, req.Beans, req.Milk, customerName)

	if ok, reason := s.menu().Supports(drink); !ok {
		s.logger.Infof("rejected order for drink %q from %s (%s)", drink, customerName, reason)
		msg := speech.UnsupportedDrink(drink)
		if err := s.speaker.Say(ctx, msg); err != nil {
			s.logger.Warnf("failed to say rejection: %v", err)
		}
		return nil, fmt.Errorf("unsupported drink %q: %s", drink, msg)
	}

	beans, err := s.menu().ResolveBeans(req.Beans)
	if err != nil {
		s.logger.Infof("rejected order for %q beans from %s", req.Beans, customerName)
		if sayErr := s.speaker.Say(ctx, speech.UnsupportedBeans(req.Beans)); sayErr != nil {
			s.logger.Warnf("failed to say rejection: %v", sayErr)
		}
		return nil, err
	}
	label := order.DrinkLabel(drink, beans)

	milk, err := s.menu().ResolveMilk(drink, req.Milk)
	if err != nil {
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
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
	if fulfillment == order.FulfillmentDelivery {
		if ok, reason := s.deliveryAvailability(); !ok {
			err := fmt.Errorf("delivery orders are not being accepted: %s", reason)
			s.logger.Warnf("rejected order: %v", err)
			return nil, err
		}
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
			greeting = speech.Greeting(label, displayName)
		}
		o := order.NewOrder(drink, customerName, greeting, req.CompletionStatement)
		o.Beans = beans
		o.ModifiedCustomerName = req.ModifiedCustomerName
		o.CustomerEmail = req.CustomerEmail
		o.Fulfillment = fulfillment
		o.Milk = milk
		s.delivery.track(o)
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
		if err := s.speaker.Say(ctx, speech.OrderReceived(label, displayName)); err != nil {
			s.logger.Warnf("failed to announce order %s: %v", ids[0], err)
		}
	case count > 1:
		if err := s.speaker.Say(ctx, speech.OrderReceivedBatch(label, displayName, count)); err != nil {
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

// defaultMaxBatchSize is used when Config.MaxBatchSize is unset or zero.
const defaultMaxBatchSize = 10

// maxBatchSize returns the configured cap on prepare_order count, falling
// back to defaultMaxBatchSize.
func (s *beanjaminCoffee) maxBatchSize() int {
	return orDefault(s.cfg.MaxBatchSize, defaultMaxBatchSize)
}
