package coffee

import (
	"context"
	"errors"
	"testing"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"beanjamin/coffee/order"
	"beanjamin/orderissuesensor"
)

type recordingIssueSink struct{ reports []order.IssueReport }

func (r *recordingIssueSink) PushIssueReport(rep order.IssueReport) {
	r.reports = append(r.reports, rep)
}

func TestReportIssue(t *testing.T) {
	sink := &recordingIssueSink{}
	s := &beanjaminCoffee{issueSink: sink, logger: logging.NewTestLogger(t)}

	resp, err := s.reportIssue(map[string]any{"order_id": "o1", "issue": "dropped_cup"})
	if err != nil {
		t.Fatal(err)
	}
	if resp["order_id"] != "o1" || resp["severity"] != order.SeverityCritical {
		t.Fatalf("unexpected response %#v", resp)
	}
	if _, err := s.reportIssue(map[string]any{"order_id": "o2", "issue": "other", "description": "it squeaked", "severity": "minor"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reportIssue(map[string]any{"issue": "small_spill"}); err == nil {
		t.Fatal("want an error without an order_id")
	}

	if len(sink.reports) != 2 {
		t.Fatalf("want 2 reports, got %d", len(sink.reports))
	}
	if got := sink.reports[0]; got.OrderID != "o1" || got.Issue != "dropped_cup" || got.ReportedAt.IsZero() {
		t.Fatalf("first report: %#v", got)
	}
	if got := sink.reports[1]; got.OrderID != "o2" || got.Description != "it squeaked" {
		t.Fatalf("second report: %#v", got)
	}
}

func TestReportIssue_NoSensor(t *testing.T) {
	s := &beanjaminCoffee{}
	if _, err := s.reportIssue(map[string]any{"order_id": "o1", "issue": "small_spill"}); !errors.Is(err, errNoIssueSensor) {
		t.Fatalf("want errNoIssueSensor, got %v", err)
	}
}

// issueSinkFromDeps only accepts a dependency that satisfies issueSink, and
// that check happens at runtime, so pin it here.
func TestOrderIssueSensorModelSatisfiesSink(t *testing.T) {
	reg, ok := resource.LookupRegistration(sensor.API, orderissuesensor.Model)
	if !ok {
		t.Fatalf("%s is not registered", orderissuesensor.Model)
	}
	res, err := reg.Constructor(context.Background(), nil, resource.Config{
		Name:                "order-issues",
		API:                 sensor.API,
		Model:               orderissuesensor.Model,
		ConvertedAttributes: &orderissuesensor.Config{},
	}, logging.NewTestLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.(issueSink); !ok {
		t.Fatalf("%T does not implement issueSink", res)
	}
}
