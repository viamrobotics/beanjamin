package coffee

import (
	"context"
	"testing"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"beanjamin/ordersensor"
)

// newBeanjaminCoffee only accepts an order_sensor_name dependency that
// satisfies orderSensorSink, and that check happens at runtime, so pin it here.
func TestOrderSensorModelSatisfiesSink(t *testing.T) {
	reg, ok := resource.LookupRegistration(sensor.API, ordersensor.Model)
	if !ok {
		t.Fatalf("%s is not registered", ordersensor.Model)
	}
	res, err := reg.Constructor(context.Background(), nil, resource.Config{
		Name:                "order-events",
		API:                 sensor.API,
		Model:               ordersensor.Model,
		ConvertedAttributes: &ordersensor.OrderSensorConfig{},
	}, logging.NewTestLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.(orderSensorSink); !ok {
		t.Fatalf("%T does not implement orderSensorSink", res)
	}
}
