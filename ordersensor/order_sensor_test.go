package ordersensor

import (
	"context"
	"errors"
	"testing"
	"time"

	"beanjamin/coffee/order"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

func newTestOrderSensor(t *testing.T) *orderSensor {
	t.Helper()
	return &orderSensor{
		name:   resource.NewName(sensor.API, "order-test"),
		logger: logging.NewTestLogger(t),
	}
}

func TestOrderSensor_Readings_ErrNoCaptureWhenEmpty(t *testing.T) {
	s := newTestOrderSensor(t)
	_, err := s.Readings(context.Background(), nil)
	if !errors.Is(err, data.ErrNoCaptureToStore) {
		t.Fatalf("expected ErrNoCaptureToStore, got %v", err)
	}
}

func TestOrderSensor_Readings_Success(t *testing.T) {
	s := newTestOrderSensor(t)
	start := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	end := start.Add(1234 * time.Millisecond)
	o := order.Order{
		ID: "o1", Drink: "latte", CustomerName: "Ada",
	}
	s.PushOrderReading(order.Reading{Order: o, StartedAt: start, EndedAt: end})

	r, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r["order_id"] != "o1" || r["drink"] != "latte" || r["customer_name"] != "Ada" {
		t.Fatalf("unexpected order fields: %#v", r)
	}
	if ok, _ := r["order_ok"].(bool); !ok {
		t.Fatalf("expected order_ok true, got %#v", r["order_ok"])
	}
	if r["error_message"] != "" {
		t.Fatalf("expected empty error_message, got %q", r["error_message"])
	}
	if r["failed_step"] != "" {
		t.Fatalf("expected empty failed_step on success, got %q", r["failed_step"])
	}
	wantStart := start.UTC().Format(time.RFC3339Nano)
	wantEnd := end.UTC().Format(time.RFC3339Nano)
	if r["start_time"] != wantStart {
		t.Fatalf("start_time: want %q got %v", wantStart, r["start_time"])
	}
	if r["end_time"] != wantEnd {
		t.Fatalf("end_time: want %q got %v", wantEnd, r["end_time"])
	}
	if dur, ok := r["duration_ms"].(float64); !ok || dur != 1234 {
		t.Fatalf("duration_ms: want 1234, got %#v", r["duration_ms"])
	}

	_, err = s.Readings(context.Background(), nil)
	if !errors.Is(err, data.ErrNoCaptureToStore) {
		t.Fatalf("second read: expected ErrNoCaptureToStore, got %v", err)
	}
}

func TestOrderSensor_Readings_Failure(t *testing.T) {
	s := newTestOrderSensor(t)
	start := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Millisecond)
	o := order.Order{ID: "o2", Drink: "decaf", CustomerName: "Bob"}
	s.PushOrderReading(order.Reading{
		Order:      o,
		ExecErr:    errors.New("grinder jam"),
		FailedStep: "Grinding",
		TraceID:    "abc123",
		Decaf:      true,
		StartedAt:  start,
		EndedAt:    end,
	})

	r, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := r["order_ok"].(bool); ok {
		t.Fatal("expected order_ok false")
	}
	if cancelled, _ := r["operator_cancelled"].(bool); cancelled {
		t.Fatal("expected operator_cancelled false for a genuine fault")
	}
	if r["error_message"] != "grinder jam" {
		t.Fatalf("error_message: %q", r["error_message"])
	}
	if r["trace_id"] != "abc123" {
		t.Fatalf("trace_id: want %q got %q", "abc123", r["trace_id"])
	}
	if d, _ := r["decaf"].(bool); !d {
		t.Fatalf("decaf: want true got %#v", r["decaf"])
	}
	if r["failed_step"] != "Grinding" {
		t.Fatalf("failed_step: want %q got %q", "Grinding", r["failed_step"])
	}
}

func TestOrderSensor_Readings_OperatorCancelled(t *testing.T) {
	s := newTestOrderSensor(t)
	t0 := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	s.PushOrderReading(order.Reading{
		Order:             order.Order{ID: "o3", Drink: "latte"},
		ExecErr:           context.Canceled,
		FailedStep:        "Brewing",
		OperatorCancelled: true,
		StartedAt:         t0,
		EndedAt:           t0,
	})

	r, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := r["order_ok"].(bool); ok {
		t.Fatal("expected order_ok false")
	}
	if cancelled, _ := r["operator_cancelled"].(bool); !cancelled {
		t.Fatal("expected operator_cancelled true")
	}
	// failed_step is still recorded so we know where the cancel interrupted.
	if r["failed_step"] != "Brewing" {
		t.Fatalf("failed_step: want %q got %q", "Brewing", r["failed_step"])
	}
}

func TestOrderSensor_Readings_FIFO(t *testing.T) {
	s := newTestOrderSensor(t)
	t0 := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	s.PushOrderReading(order.Reading{Order: order.Order{ID: "first"}, StartedAt: t0, EndedAt: t0})
	s.PushOrderReading(order.Reading{Order: order.Order{ID: "second"}, StartedAt: t0, EndedAt: t0})

	r1, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1["order_id"] != "first" {
		t.Fatalf("first reading: %#v", r1)
	}
	r2, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2["order_id"] != "second" {
		t.Fatalf("second reading: %#v", r2)
	}
}

func TestOrderSensor_Readings_CustomerNames(t *testing.T) {
	tests := []struct {
		name         string
		order        order.Order
		wantName     string
		wantModified string
	}{
		{
			name:         "kiosk order carries both the real name and the misspelling",
			order:        order.Order{CustomerName: "Vijay", ModifiedCustomerName: "Vijoy"},
			wantName:     "Vijay",
			wantModified: "Vijoy",
		},
		{
			name:         "caller that does not misspell reports no modified name",
			order:        order.Order{CustomerName: "Ada"},
			wantName:     "Ada",
			wantModified: "",
		},
		{
			name:         "anonymous order stays anonymous",
			order:        order.Order{},
			wantName:     "",
			wantModified: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestOrderSensor(t)
			s.PushOrderReading(order.Reading{Order: tc.order})

			r, err := s.Readings(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if r["customer_name"] != tc.wantName {
				t.Errorf("customer_name: want %q got %#v", tc.wantName, r["customer_name"])
			}
			if r["modified_customer_name"] != tc.wantModified {
				t.Errorf("modified_customer_name: want %q got %#v", tc.wantModified, r["modified_customer_name"])
			}
		})
	}
}
