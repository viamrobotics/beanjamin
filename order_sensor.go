package beanjamin

import (
	"context"
	"sync"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
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

// orderSensorSink is implemented only by orderSensor; the coffee service pushes when an order completes.
type orderSensorSink interface {
	pushOrderReading(order Order, execErr error, startedAt, endedAt time.Time)
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

func (s *orderSensor) Readings(context.Context, map[string]interface{}) (map[string]interface{}, error) {
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

func (*orderSensor) DoCommand(context.Context, map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}

func (*orderSensor) Close(context.Context) error {
	return nil
}

func (s *orderSensor) pushOrderReading(order Order, execErr error, startedAt, endedAt time.Time) {
	ok := execErr == nil
	errMsg := ""
	if execErr != nil {
		errMsg = execErr.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, map[string]interface{}{
		"order_id":      order.ID,
		"drink":         order.Drink,
		"customer_name": order.CustomerName,
		"order_ok":      ok,
		"error_message": errMsg,
		"start_time":    startedAt.UTC().Format(time.RFC3339Nano),
		"end_time":      endedAt.UTC().Format(time.RFC3339Nano),
		"duration_ms":   float64(endedAt.Sub(startedAt).Milliseconds()),
	})
}
