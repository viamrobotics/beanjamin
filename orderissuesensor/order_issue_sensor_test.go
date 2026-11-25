package orderissuesensor

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"beanjamin/coffee/order"
)

func newTestSensor(t *testing.T) *orderIssueSensor {
	t.Helper()
	return &orderIssueSensor{
		name:   resource.NewName(sensor.API, "issues-test"),
		logger: logging.NewTestLogger(t),
	}
}

func TestReadings_ErrNoCaptureWhenEmpty(t *testing.T) {
	_, err := newTestSensor(t).Readings(context.Background(), nil)
	if !errors.Is(err, data.ErrNoCaptureToStore) {
		t.Fatalf("expected ErrNoCaptureToStore, got %v", err)
	}
}

func TestPushIssueReport(t *testing.T) {
	s := newTestSensor(t)
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	s.PushIssueReport(order.IssueReport{
		IssueRequest: order.IssueRequest{
			OrderID: "o1", Issue: "other", Description: "foam everywhere", Severity: "medium",
		},
		ReportedAt: at,
	})
	s.PushIssueReport(order.IssueReport{
		IssueRequest: order.IssueRequest{OrderID: "o2", Issue: "small_spill", Severity: "minor"},
		ReportedAt:   at,
	})

	r, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"order_id": "o1", "issue": "other", "description": "foam everywhere",
		"severity": "medium", "reported_at": at.Format(time.RFC3339Nano),
	}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("%s: want %#v got %#v", k, v, r[k])
		}
	}

	r, err = s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r["order_id"] != "o2" {
		t.Fatalf("second reading: want o2, got %#v", r["order_id"])
	}
	if _, err := s.Readings(context.Background(), nil); !errors.Is(err, data.ErrNoCaptureToStore) {
		t.Fatalf("drained: expected ErrNoCaptureToStore, got %v", err)
	}
}
