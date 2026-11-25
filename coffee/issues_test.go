package coffee

import (
	"errors"
	"testing"

	"go.viam.com/rdk/logging"

	"beanjamin/coffee/order"
)

type recordingIssueSink struct{ reports []order.IssueReport }

func (r *recordingIssueSink) PushIssueReport(rep order.IssueReport) {
	r.reports = append(r.reports, rep)
}

func TestReportIssue(t *testing.T) {
	sink := &recordingIssueSink{}
	s := &beanjaminCoffee{queue: order.NewQueue(), issueSink: sink, logger: logging.NewTestLogger(t)}

	if _, err := s.reportIssue(map[string]any{"order": "previous", "issue": "small_spill"}); err == nil {
		t.Fatal("want an error with no previous order")
	}

	s.queue.Enqueue(order.Order{ID: "done", Drink: "latte", CustomerName: "Ada"})
	s.queue.Start()
	s.queue.Complete()
	s.queue.Enqueue(order.Order{ID: "brewing", Drink: "espresso"})
	s.queue.Start()

	resp, err := s.reportIssue(map[string]any{"order": "previous", "issue": "dropped_cup"})
	if err != nil {
		t.Fatal(err)
	}
	if resp["order_id"] != "done" || resp["severity"] != order.SeverityCritical {
		t.Fatalf("unexpected response %#v", resp)
	}
	if _, err := s.reportIssue(map[string]any{"order": "current", "issue": "other", "description": "it squeaked", "severity": "minor"}); err != nil {
		t.Fatal(err)
	}

	if len(sink.reports) != 2 {
		t.Fatalf("want 2 reports, got %d", len(sink.reports))
	}
	if got := sink.reports[0]; got.Order.ID != "done" || got.Issue != "dropped_cup" {
		t.Fatalf("first report: %#v", got)
	}
	if got := sink.reports[1]; got.Order.ID != "brewing" || got.Description != "it squeaked" {
		t.Fatalf("second report: %#v", got)
	}

	status := s.reportableOrders()
	if status["current"].(map[string]any)["id"] != "brewing" || status["previous"].(map[string]any)["id"] != "done" {
		t.Fatalf("reportable orders: %#v", status)
	}
}

func TestReportIssue_NoSensor(t *testing.T) {
	s := &beanjaminCoffee{queue: order.NewQueue()}
	if _, err := s.reportIssue(map[string]any{"order": "current", "issue": "small_spill"}); !errors.Is(err, errNoIssueSensor) {
		t.Fatalf("want errNoIssueSensor, got %v", err)
	}
}
