package coffee

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

// OrderSensor queues one reading per order when processing finishes.
// When the queue is empty, Readings returns data.ErrNoCaptureToStore (for Data Management capture filtering).
var OrderSensor = resource.NewModel("viam", "beanjamin", "order-sensor")

func init() {
	resource.RegisterComponent(sensor.API, OrderSensor,
		resource.Registration[sensor.Sensor, *OrderSensorConfig]{
			Constructor: newOrderSensor,
		})
}

// OrderSensorConfig has no attributes; name the component in the coffee service config instead.
type OrderSensorConfig struct{}

func (cfg *OrderSensorConfig) Validate(string) ([]string, []string, error) {
	return nil, nil, nil
}

// Implemented by orderSensor; coffee calls this after each order attempt.
type orderSensorSink interface {
	pushOrderReading(r order.Reading)
}

type orderSensor struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger

	mu      sync.Mutex
	pending []map[string]any
}

func newOrderSensor(_ context.Context, _ resource.Dependencies, rawConf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	_, err := resource.NativeConfig[*OrderSensorConfig](rawConf)
	if err != nil {
		return nil, err
	}
	return &orderSensor{
		name:   rawConf.ResourceName(),
		logger: logger,
	}, nil
}

func (s *orderSensor) Name() resource.Name {
	return s.name
}

func (s *orderSensor) Status(context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}

func (s *orderSensor) Readings(ctx context.Context, _ map[string]any) (map[string]any, error) {
	_, span := trace.StartSpan(ctx, "order-sensor::Readings")
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

func (*orderSensor) DoCommand(ctx context.Context, _ map[string]any) (map[string]any, error) {
	_, span := trace.StartSpan(ctx, "order-sensor::DoCommand")
	defer span.End()
	return nil, nil
}

func (*orderSensor) Close(context.Context) error {
	return nil
}

func (s *orderSensor) pushOrderReading(r order.Reading) {
	ok := r.ExecErr == nil
	errMsg := ""
	if r.ExecErr != nil {
		errMsg = r.ExecErr.Error()
	}
	// failedStep is the step label the order errored at; empty on success.
	failedStep := r.FailedStep
	if ok {
		failedStep = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, map[string]any{
		"order_id":      r.Order.ID,
		"drink":         r.Order.Drink,
		"customer_name": r.Order.CustomerName,
		// What the customer was actually shown.
		"modified_customer_name": r.Order.ModifiedCustomerName,
		"order_ok":               ok,
		"operator_cancelled":     r.OperatorCancelled,
		"error_message":          errMsg,
		"failed_step":            failedStep,
		"trace_id":               r.TraceID,
		"decaf":                  r.Decaf,
		"start_time":             r.StartedAt.UTC().Format(time.RFC3339Nano),
		"end_time":               r.EndedAt.UTC().Format(time.RFC3339Nano),
		"duration_ms":            float64(r.EndedAt.Sub(r.StartedAt).Milliseconds()),
	})
}
