package coffee

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.viam.com/rdk/testutils/inject"

	"beanjamin/coffee/order"
)

func TestCreditLoyaltyPoint(t *testing.T) {
	var sent []map[string]any
	crm := inject.NewGenericService("crm")
	crm.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
		sent = append(sent, cmd)
		return nil, errors.New("ledger unavailable")
	}
	s := newStatusService(t, nil)
	s.crm = crm

	// A CRM error is logged, never surfaced into the brew.
	s.creditLoyaltyPoint(context.Background(), order.Order{ID: "o1", CustomerEmail: "ada@example.com"})
	s.creditLoyaltyPoint(context.Background(), order.Order{ID: "o2"})

	want := []map[string]any{{"credit_points": map[string]any{
		"email": "ada@example.com", "points": 1.0, "reason": "order", "order_id": "o1",
	}}}
	if !reflect.DeepEqual(sent, want) {
		t.Errorf("CRM commands = %v, want only the credit for the order with an email: %v", sent, want)
	}

	s.crm = nil
	s.creditLoyaltyPoint(context.Background(), order.Order{ID: "o3", CustomerEmail: "ada@example.com"})
}
