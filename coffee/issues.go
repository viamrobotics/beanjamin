package coffee

// Customer issue reports: report_issue records a problem a customer saw with
// the order being made or the one just finished, even when that order
// technically succeeded, on the sensor named by issue_sensor_name.

import (
	"errors"
	"fmt"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"beanjamin/coffee/order"
)

// issueSink is what the coffee service needs from the sensor that captures
// issue reports: the viam:beanjamin:order-sensor model implements it.
type issueSink interface {
	PushIssueReport(r order.IssueReport)
}

// issueSinkFromDeps resolves issue_sensor_name; nil when it is unset.
func issueSinkFromDeps(deps resource.Dependencies, name string, logger logging.Logger) (issueSink, error) {
	if name == "" {
		return nil, nil
	}
	sen, err := sensor.FromProvider(deps, name)
	if err != nil {
		return nil, fmt.Errorf("issue sensor %q: %w", name, err)
	}
	sink, ok := sen.(issueSink)
	if !ok {
		return nil, fmt.Errorf("resource %q must be model viam:beanjamin:order-sensor", name)
	}
	logger.Infof("issue sensor %q connected", name)
	return sink, nil
}

var errNoIssueSensor = errors.New("report_issue needs issue_sensor_name to be configured")

// reportIssue records one customer report against the current or the previous
// order.
func (s *beanjaminCoffee) reportIssue(raw any) (map[string]any, error) {
	if s.issueSink == nil {
		return nil, errNoIssueSensor
	}
	req, err := order.ParseIssueRequest(raw)
	if err != nil {
		return nil, err
	}
	current, previous := s.queue.Reportable()
	o := current
	if req.Which == order.ReportPreviousOrder {
		o = previous
	}
	if o == nil {
		return nil, fmt.Errorf("report_issue: there is no %s order to report", req.Which)
	}
	s.issueSink.PushIssueReport(order.IssueReport{
		IssueRequest: req,
		Order:        *o,
		ReportedAt:   time.Now(),
	})
	s.logger.Warnw("customer reported an issue",
		"order_id", o.ID, "which", req.Which, "issue", req.Issue,
		"severity", req.Severity, "description", req.Description)
	return map[string]any{"reported": true, "order_id": o.ID, "severity": req.Severity}, nil
}

// reportableOrders describes the orders report_issue can name, keyed by
// "current" and "previous", for the kiosk to show which drink each one is.
func (s *beanjaminCoffee) reportableOrders() map[string]any {
	current, previous := s.queue.Reportable()
	out := map[string]any{}
	if current != nil {
		out[order.ReportCurrentOrder] = reportableOrder(*current)
	}
	if previous != nil {
		out[order.ReportPreviousOrder] = reportableOrder(*previous)
	}
	return out
}

func reportableOrder(o order.Order) map[string]any {
	m := map[string]any{
		"id":            o.ID,
		"drink":         o.WireDrink(),
		"beans":         o.Beans,
		"customer_name": o.CustomerName,
		"started_at":    o.StartedAt.Format(time.RFC3339),
	}
	if !o.CompletedAt.IsZero() {
		m["completed_at"] = o.CompletedAt.Format(time.RFC3339)
	}
	return m
}
