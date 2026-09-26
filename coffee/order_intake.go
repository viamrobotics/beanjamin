package coffee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// prepareOrderRequest is the decoded prepare_order payload. The json tags are
// the wire keys; every field is optional on the wire and its zero value is the
// default. Count is a pointer so an explicit 0 can be told apart from absent,
// and a float64 because that is how structpb carries every number.
type prepareOrderRequest struct {
	Drink                string   `json:"drink"`
	CustomerName         string   `json:"customer_name"`
	ModifiedCustomerName string   `json:"modified_customer_name"`
	CustomerEmail        string   `json:"customer_email"`
	InitialGreeting      string   `json:"initial_greeting"`
	CompletionStatement  string   `json:"completion_statement"`
	Count                *float64 `json:"count"`
	Fulfillment          string   `json:"fulfillment"`
}

// prepareOrderKeys lists the wire keys of prepareOrderRequest in declaration
// order, for error messages.
var prepareOrderKeys = func() []string {
	t := reflect.TypeOf(prepareOrderRequest{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		keys = append(keys, t.Field(i).Tag.Get("json"))
	}
	return keys
}()

// decodePrepareOrder decodes a prepare_order payload into a
// prepareOrderRequest. Absent and nil values leave the field at its zero
// value; a value of the wrong type is an error naming the key, so a
// mis-typed field is rejected rather than silently replaced by its default.
// Keys that match no field are ignored.
func decodePrepareOrder(raw any) (prepareOrderRequest, error) {
	var req prepareOrderRequest
	m, ok := raw.(map[string]any)
	if !ok {
		return req, fmt.Errorf("prepare_order value must be an object with keys: %s", strings.Join(prepareOrderKeys, ", "))
	}
	// A JSON round trip gives encoding/json's field matching and type checks;
	// the payload is a handful of keys, so the copy costs nothing.
	b, err := json.Marshal(m)
	if err != nil {
		return req, fmt.Errorf("prepare_order: %w", err)
	}
	if err := json.Unmarshal(b, &req); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return req, fmt.Errorf("prepare_order field %q must be a %s, got %s", typeErr.Field, typeErr.Type, typeErr.Value)
		}
		return req, fmt.Errorf("prepare_order: %w", err)
	}
	return req, nil
}

// drinkSupport reports whether this machine can make drink and, when it
// can't, why: the config flag that disables it, or that the drink is unknown.
func (s *beanjaminCoffee) drinkSupport(drink string) (supported bool, reason string) {
	switch drink {
	case "espresso", "lungo":
		return true, ""
	case "decaf", "decaf_lungo":
		return s.cfg.CanServeDecaf, "can_serve_decaf=false"
	case "iced_coffee":
		return s.cfg.CanServeIced, "can_serve_iced=false"
	case "iced_latte":
		// can_serve_iced_latte implies can_serve_iced (Validate rejects it
		// otherwise), so the one flag is the whole gate.
		return s.cfg.CanServeIcedLatte, "can_serve_iced_latte=false"
	default:
		return false, "unsupported drink"
	}
}

// enqueueOrder validates the order and adds it to the queue.
// It returns immediately with the queue position. When the optional
// "count" field is > 1, N identical orders are enqueued back-to-back
// (each with its own UUID) and the per-order "Order received" line is
// replaced with a single consolidated batch announcement.
func (s *beanjaminCoffee) enqueueOrder(ctx context.Context, orderRaw any) (map[string]any, error) {
	s.logger.Infof("received order request")

	req, err := decodePrepareOrder(orderRaw)
	if err != nil {
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}
	drink := req.Drink
	customerName := strings.TrimSpace(req.CustomerName)
	s.logger.Infof("order request: drink=%q customer=%q", drink, customerName)

	if ok, reason := s.drinkSupport(drink); !ok {
		s.logger.Infof("rejected order for drink %q from %s (%s)", drink, customerName, reason)
		msg := pickUnsupportedDrink(drink)
		if err := s.say(ctx, msg); err != nil {
			s.logger.Warnf("failed to say rejection: %v", err)
		}
		return nil, fmt.Errorf("unsupported drink %q: %s", drink, msg)
	}

	fulfillment, err := parseFulfillment(req.Fulfillment)
	if err != nil {
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}
	// Delivery orders must be attributable: the delivery bot identifies the
	// recipient by email, so an anonymous delivery has nowhere to go. Pickup
	// (the default) stays open to anonymous walk-ups.
	if fulfillment == FulfillmentDelivery && req.CustomerEmail == "" {
		err := fmt.Errorf("delivery orders require a customer_email")
		s.logger.Warnf("rejected order: %v", err)
		return nil, err
	}

	displayName := req.ModifiedCustomerName
	if displayName == "" {
		displayName = customerName
	}

	count, err := s.parseOrderCount(req.Count)
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
		o := NewOrder(drink, customerName, greeting, req.CompletionStatement)
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

// parseFulfillment validates the optional "fulfillment" field on a
// prepare_order payload. Empty → pickup. Anything other than "pickup" or
// "delivery" → error. Caller should reject before any enqueue.
func parseFulfillment(f string) (string, error) {
	switch f {
	case "":
		return FulfillmentPickup, nil
	case FulfillmentPickup, FulfillmentDelivery:
		return f, nil
	default:
		return "", fmt.Errorf("fulfillment must be %q or %q, got %q", FulfillmentPickup, FulfillmentDelivery, f)
	}
}

// parseOrderCount validates the optional "count" field on a prepare_order
// payload. Absent → 1. Fractional or out-of-range values → error. Caller
// should reject before any enqueue.
func (s *beanjaminCoffee) parseOrderCount(v *float64) (int, error) {
	if v == nil {
		return 1, nil
	}
	f := *v
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, fmt.Errorf("count must be a whole number, got %v", f)
	}
	if f < 1 {
		return 0, fmt.Errorf("count must be >= 1, got %v", f)
	}
	limit := s.maxBatchSize()
	if f > float64(limit) {
		return 0, fmt.Errorf("count must be <= %d, got %v", limit, f)
	}
	return int(f), nil
}
