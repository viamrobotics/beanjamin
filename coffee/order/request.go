package order

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// Request is the decoded prepare_order payload. The json tags are the wire
// keys; every field is optional on the wire and its zero value is the default.
// Count is a pointer so an explicit 0 can be told apart from absent, and a
// float64 because that is how structpb carries every number.
type Request struct {
	Drink                string   `json:"drink"`
	CustomerName         string   `json:"customer_name"`
	ModifiedCustomerName string   `json:"modified_customer_name"`
	CustomerEmail        string   `json:"customer_email"`
	InitialGreeting      string   `json:"initial_greeting"`
	CompletionStatement  string   `json:"completion_statement"`
	Count                *float64 `json:"count"`
	Fulfillment          string   `json:"fulfillment"`
}

// requestKeys lists the wire keys of Request in declaration order, for error
// messages.
var requestKeys = func() []string {
	t := reflect.TypeOf(Request{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		keys = append(keys, t.Field(i).Tag.Get("json"))
	}
	return keys
}()

// DecodeRequest decodes a prepare_order payload into a Request. Absent and nil
// values leave the field at its zero value; a value of the wrong type is an
// error naming the key, so a mis-typed field is rejected rather than silently
// replaced by its default. Keys that match no field are ignored.
func DecodeRequest(raw any) (Request, error) {
	var req Request
	m, ok := raw.(map[string]any)
	if !ok {
		return req, fmt.Errorf("prepare_order value must be an object with keys: %s", strings.Join(requestKeys, ", "))
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

// ParseFulfillment validates the optional "fulfillment" field on a
// prepare_order payload. Empty → pickup. Anything other than "pickup" or
// "delivery" → error. Caller should reject before any enqueue.
func ParseFulfillment(f string) (string, error) {
	switch f {
	case "":
		return FulfillmentPickup, nil
	case FulfillmentPickup, FulfillmentDelivery:
		return f, nil
	default:
		return "", fmt.Errorf("fulfillment must be %q or %q, got %q", FulfillmentPickup, FulfillmentDelivery, f)
	}
}

// ParseCount validates the optional "count" field on a prepare_order payload
// against limit, the largest batch the machine accepts. Absent → 1. Fractional
// or out-of-range values → error. Caller should reject before any enqueue.
func ParseCount(v *float64, limit int) (int, error) {
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
	if f > float64(limit) {
		return 0, fmt.Errorf("count must be <= %d, got %v", limit, f)
	}
	return int(f), nil
}
