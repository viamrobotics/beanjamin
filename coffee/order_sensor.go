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

// orderReading is the per-attempt observability record coffee hands to the
// order sensor after each order finishes or fails. Grouped into a struct
// rather than positional args so new fields don't churn every call site.
type orderReading struct {
	order      Order
	execErr    error  // nil on success
	failedStep string // step the order errored at; "" on success
	// operatorCancelled is true when the failure was an operator cancel
	// (context.Canceled propagated from cancelCtx), not a genuine fault.
	// Filter these out of step error-rate metrics.
	operatorCancelled bool
	traceID           string // OTel trace ID; links the reading to the order's full trace
	// decaf records whether the order took the decaf grinder branch, to explain
	// why a given step ran (or didn't) without cross-referencing config.
	decaf     bool
	startedAt time.Time
	endedAt   time.Time
}

// Implemented by orderSensor; coffee calls this after each order attempt.
type orderSensorSink interface {
	pushOrderReading(r orderReading)
}

type orderSensor struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger

	mu      sync.Mutex
	pending []map[string]interface{}
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

func (s *orderSensor) Status(context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (s *orderSensor) Readings(ctx context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
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

func (*orderSensor) DoCommand(ctx context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	_, span := trace.StartSpan(ctx, "order-sensor::DoCommand")
	defer span.End()
	return nil, nil
}

func (*orderSensor) Close(context.Context) error {
	return nil
}

func (s *orderSensor) pushOrderReading(r orderReading) {
	ok := r.execErr == nil
	errMsg := ""
	if r.execErr != nil {
		errMsg = r.execErr.Error()
	}
	// failedStep is the step label the order errored at; empty on success.
	failedStep := r.failedStep
	if ok {
		failedStep = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, map[string]interface{}{
		"order_id":           r.order.ID,
		"drink":              r.order.Drink,
		"customer_name":      r.order.CustomerName,
		"order_ok":           ok,
		"operator_cancelled": r.operatorCancelled,
		"error_message":      errMsg,
		"failed_step":        failedStep,
		"trace_id":           r.traceID,
		"decaf":              r.decaf,
		"start_time":         r.startedAt.UTC().Format(time.RFC3339Nano),
		"end_time":           r.endedAt.UTC().Format(time.RFC3339Nano),
		"duration_ms":        float64(r.endedAt.Sub(r.startedAt).Milliseconds()),
	})
}
