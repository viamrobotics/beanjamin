package beanjamin

import (
	"context"
	"fmt"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

var MaintenanceSensor = resource.NewModel("viam", "beanjamin", "maintenance-sensor")

func init() {
	resource.RegisterComponent(sensor.API, MaintenanceSensor,
		resource.Registration[sensor.Sensor, *MaintenanceSensorConfig]{
			Constructor: newMaintenanceSensor,
		},
	)
}

type MaintenanceSensorConfig struct {
	CoffeeServiceName string `json:"coffee_service_name"`
	ArmName           string `json:"arm_name"`
}

func (cfg *MaintenanceSensorConfig) Validate(path string) ([]string, []string, error) {
	if cfg.CoffeeServiceName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "coffee_service_name")
	}
	if cfg.ArmName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm_name")
	}
	return []string{
		resource.NewName(generic.API, cfg.CoffeeServiceName).String(),
		arm.Named(cfg.ArmName).String(),
	}, nil, nil
}

type maintenanceSensor struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger
	coffee *beanjaminCoffee
	arm    arm.Arm
}

func newMaintenanceSensor(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	conf, err := resource.NativeConfig[*MaintenanceSensorConfig](rawConf)
	if err != nil {
		return nil, err
	}

	coffeeRes, ok := deps[resource.NewName(generic.API, conf.CoffeeServiceName)]
	if !ok {
		return nil, fmt.Errorf("coffee service %q not found in dependencies", conf.CoffeeServiceName)
	}
	coffee, ok := coffeeRes.(*beanjaminCoffee)
	if !ok {
		return nil, fmt.Errorf("resource %q is not a beanjamin coffee service", conf.CoffeeServiceName)
	}

	armComp, err := arm.FromProvider(deps, conf.ArmName)
	if err != nil {
		return nil, fmt.Errorf("arm %q not found in dependencies: %w", conf.ArmName, err)
	}

	return &maintenanceSensor{
		name:   rawConf.ResourceName(),
		logger: logger,
		coffee: coffee,
		arm:    armComp,
	}, nil
}

func (m *maintenanceSensor) Name() resource.Name {
	return m.name
}

func (m *maintenanceSensor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	// Check if the arm is physically moving.
	armMoving, err := m.arm.IsMoving(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check arm movement: %w", err)
	}

	// Check if an espresso sequence is in progress.
	sequenceRunning := m.coffee.running.Load()

	// Check if there are orders waiting in the queue.
	hasOrders := m.coffee.queue.Len() > 0

	return map[string]interface{}{
		"is_safe": !armMoving && !sequenceRunning && !hasOrders,
	}, nil
}

func (m *maintenanceSensor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}

func (m *maintenanceSensor) Close(context.Context) error {
	return nil
}
