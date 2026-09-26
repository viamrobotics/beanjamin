package order

import (
	"strings"
	"testing"
)

func TestDecodeRequest_RejectsWrongTypedField(t *testing.T) {
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
			_, err := DecodeRequest(map[string]any{"drink": "espresso", tc.key: tc.v})
			if err == nil {
				t.Fatalf("expected error for %s=%#v, got nil", tc.key, tc.v)
			}
			if !strings.Contains(err.Error(), `"`+tc.key+`"`) {
				t.Errorf("error %q should name the field %q", err, tc.key)
			}
		})
	}
}

func TestDecodeRequest_AbsentAndNilFieldsDefault(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"absent": {},
		"nil": {
			"drink": nil, "customer_name": nil, "modified_customer_name": nil,
			"customer_email": nil, "initial_greeting": nil, "completion_statement": nil,
			"count": nil, "fulfillment": nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := DecodeRequest(payload)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req != (Request{}) {
				t.Errorf("decoded %+v, want the zero request", req)
			}
		})
	}
}

func TestDecodeRequest_FullPayload(t *testing.T) {
	req, err := DecodeRequest(map[string]any{
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
	want := Request{
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

func TestDecodeRequest_NonObjectListsEveryKey(t *testing.T) {
	_, err := DecodeRequest("espresso")
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
