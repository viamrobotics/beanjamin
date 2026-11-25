// Package orderissuesensor registers a viam:beanjamin:order-issue-sensor model
// that implements the rdk:component:sensor API. The coffee service hands it
// one order.IssueReport per report_issue, and each is returned once from
// Readings, so Data Management captures customer issue reports as their own
// tabular data, linked to order-events by order_id.
package orderissuesensor

import (
	"context"
	"sync"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/resource"

	"beanjamin/coffee/order"
)

// Model queues one reading per customer issue report. When the queue is
// empty, Readings returns data.ErrNoCaptureToStore so Data Management stores
// nothing.
var Model = resource.NewModel("viam", "beanjamin", "order-issue-sensor")

func init() {
	resource.RegisterComponent(sensor.API, Model,
		resource.Registration[sensor.Sensor, *Config]{
			Constructor: newOrderIssueSensor,
		})
}

// Config has no attributes; name the component in the coffee service's
// issue_sensor_name instead.
type Config struct{}

// Validate accepts any config; the model has no attributes.
func (cfg *Config) Validate(string) ([]string, []string, error) {
	return nil, nil, nil
}

type orderIssueSensor struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger

	mu      sync.Mutex
	pending []map[string]any
}

func newOrderIssueSensor(_ context.Context, _ resource.Dependencies, rawConf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	if _, err := resource.NativeConfig[*Config](rawConf); err != nil {
		return nil, err
	}
	return &orderIssueSensor{name: rawConf.ResourceName(), logger: logger}, nil
}

func (s *orderIssueSensor) Name() resource.Name {
	return s.name
}

func (s *orderIssueSensor) Status(context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}

func (s *orderIssueSensor) Readings(ctx context.Context, _ map[string]any) (map[string]any, error) {
	_, span := trace.StartSpan(ctx, "order-issue-sensor::Readings")
	defer span.End()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil, data.ErrNoCaptureToStore
	}
	payload := s.pending[0]
	s.pending[0] = nil
	s.pending = s.pending[1:]
	return payload, nil
}

func (*orderIssueSensor) DoCommand(context.Context, map[string]any) (map[string]any, error) {
	return nil, nil
}

func (*orderIssueSensor) Close(context.Context) error {
	return nil
}

// PushIssueReport queues r as the payload of a future Readings call. The
// coffee service calls it on each report_issue.
func (s *orderIssueSensor) PushIssueReport(r order.IssueReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, map[string]any{
		// The order-events reading with the same order_id describes the order.
		"order_id": r.OrderID,
		// A catalog key, or "other" for a freeform report.
		"issue":       r.Issue,
		"description": r.Description,
		"severity":    r.Severity,
		"reported_at": r.ReportedAt.UTC().Format(time.RFC3339Nano),
	})
}
