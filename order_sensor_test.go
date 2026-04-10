package beanjamin

import (
	"context"
	"errors"
	"testing"
	"time"

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
	order := Order{
		ID: "o1", Drink: "latte", CustomerName: "Ada",
	}
	s.pushOrderReading(order, nil, start, end)

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
	order := Order{ID: "o2", Drink: "espresso", CustomerName: "Bob"}
	s.pushOrderReading(order, errors.New("grinder jam"), start, end)

	r, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := r["order_ok"].(bool); ok {
		t.Fatal("expected order_ok false")
	}
	if r["error_message"] != "grinder jam" {
		t.Fatalf("error_message: %q", r["error_message"])
	}
}

func TestOrderSensor_Readings_FIFO(t *testing.T) {
	s := newTestOrderSensor(t)
	t0 := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	s.pushOrderReading(Order{ID: "first"}, nil, t0, t0)
	s.pushOrderReading(Order{ID: "second"}, nil, t0, t0)

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
