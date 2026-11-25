package coffee

// Customer issue reports: report_issue records a problem a customer saw with
// an order, even one that technically succeeded, on the
// viam:beanjamin:order-issue-sensor named by issue_sensor_name.

import (
	"errors"
	"fmt"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"beanjamin/coffee/order"
)

// issueSink is what the coffee service needs from the
// viam:beanjamin:order-issue-sensor component.
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
		return nil, fmt.Errorf("resource %q must be model viam:beanjamin:order-issue-sensor", name)
	}
	logger.Infof("issue sensor %q connected", name)
	return sink, nil
}

var errNoIssueSensor = errors.New("report_issue needs issue_sensor_name to be configured")

// reportIssue records one customer report against the order it names. The
// order is not looked up: finished orders live in tabular data, which the
// service cannot read, and the dashboard joins reports to order-events by
// order_id.
func (s *beanjaminCoffee) reportIssue(raw any) (map[string]any, error) {
	if s.issueSink == nil {
		return nil, errNoIssueSensor
	}
	req, err := order.ParseIssueRequest(raw)
	if err != nil {
		return nil, err
	}
	s.issueSink.PushIssueReport(order.IssueReport{IssueRequest: req, ReportedAt: time.Now()})
	s.logger.Warnw("customer reported an issue",
		"order_id", req.OrderID, "issue", req.Issue,
		"severity", req.Severity, "description", req.Description)
	return map[string]any{"reported": true, "order_id": req.OrderID, "severity": req.Severity}, nil
}
