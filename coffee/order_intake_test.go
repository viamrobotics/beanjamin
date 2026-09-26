package coffee

import (
	"context"
	"strings"
	"testing"
)

func TestDecodePrepareOrder_RejectsWrongTypedField(t *testing.T) {
	cases := []struct {
		key string
		v   any
	}{
		{"drink", float64(1)},
		{"customer_name", true},
		{"modified_customer_name", float64(2)},
		{"customer_email", []any{"a@example.com"}},
		{"initial_greeting", map[string]any{}},
		{"completion_statement", float64(3)},
		{"count", "2"},
		{"fulfillment", float64(1)},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			_, err := decodePrepareOrder(map[string]any{"drink": "espresso", tc.key: tc.v})
			if err == nil {
				t.Fatalf("expected error for %s=%#v, got nil", tc.key, tc.v)
			}
			if !strings.Contains(err.Error(), `"`+tc.key+`"`) {
				t.Errorf("error %q should name the field %q", err, tc.key)
			}
		})
	}
}

func TestDecodePrepareOrder_AbsentAndNilFieldsDefault(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"absent": {},
		"nil": {
			"drink": nil, "customer_name": nil, "modified_customer_name": nil,
			"customer_email": nil, "initial_greeting": nil, "completion_statement": nil,
			"count": nil, "fulfillment": nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := decodePrepareOrder(payload)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req != (prepareOrderRequest{}) {
				t.Errorf("decoded %+v, want the zero request", req)
			}
		})
	}
}

func TestDecodePrepareOrder_FullPayload(t *testing.T) {
	req, err := decodePrepareOrder(map[string]any{
		"drink":                  "lungo",
		"customer_name":          "Vijay",
		"modified_customer_name": "Vijoy",
		"customer_email":         "vijay@example.com",
		"initial_greeting":       "hi",
		"completion_statement":   "bye",
		"count":                  float64(2),
		"fulfillment":            "delivery",
		"unrecognized":           "ignored",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Count == nil || *req.Count != 2 {
		t.Fatalf("Count = %v, want 2", req.Count)
	}
	req.Count = nil
	want := prepareOrderRequest{
		Drink:                "lungo",
		CustomerName:         "Vijay",
		ModifiedCustomerName: "Vijoy",
		CustomerEmail:        "vijay@example.com",
		InitialGreeting:      "hi",
		CompletionStatement:  "bye",
		Fulfillment:          "delivery",
	}
	if req != want {
		t.Errorf("decoded %+v, want %+v", req, want)
	}
}

func TestDecodePrepareOrder_NonObjectListsEveryKey(t *testing.T) {
	_, err := decodePrepareOrder("espresso")
	if err == nil {
		t.Fatal("expected error for a non-object payload, got nil")
	}
	for _, key := range []string{
		"drink", "customer_name", "modified_customer_name", "customer_email",
		"initial_greeting", "completion_statement", "count", "fulfillment",
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q should list key %q", err, key)
		}
	}
}

func TestEnqueueOrder_RejectsWrongTypedFieldBeforeQueueing(t *testing.T) {
	c, sp := newTestCoffee(t, nil)
	_, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":          "espresso",
		"customer_name":  "Alice",
		"customer_email": float64(42),
	})
	if err == nil || !strings.Contains(err.Error(), "customer_email") {
		t.Fatalf("error = %v, want one naming customer_email", err)
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
	}
	if calls := sp.calls(); len(calls) != 0 {
		t.Errorf("a malformed payload should not be spoken to, got %v", calls)
	}
}
