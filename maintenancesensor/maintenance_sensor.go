// Package maintenancesensor registers a viam:beanjamin:maintenance-sensor model
// that implements the rdk:component:sensor API. It reports is_safe=false while
// the arm is moving or the coffee service has orders queued or in progress.
// A dependency that cannot be queried counts as idle, so a failing arm or
// coffee service never blocks the reconfiguration that would repair it.
package maintenancesensor

import (
	"context"
	"fmt"
	"time"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

var Model = resource.NewModel("viam", "beanjamin", "maintenance-sensor")

// dependencyCheckTimeout bounds each dependency call so both checks finish
// inside viam-server's 5s maintenance-sensor deadline; a hung dependency would
// otherwise time out the whole reading, which also blocks reconfiguration.
const dependencyCheckTimeout = 2 * time.Second

func init() {
	resource.RegisterComponent(sensor.API, Model,
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
	coffee resource.Resource
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

	armComp, err := arm.FromProvider(deps, conf.ArmName)
	if err != nil {
		return nil, fmt.Errorf("arm %q not found in dependencies: %w", conf.ArmName, err)
	}

	return &maintenanceSensor{
		name:   rawConf.ResourceName(),
		logger: logger,
		coffee: coffeeRes,
		arm:    armComp,
	}, nil
}

func (m *maintenanceSensor) Name() resource.Name {
	return m.name
}

func (m *maintenanceSensor) Status(ctx context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}

func (m *maintenanceSensor) Readings(ctx context.Context, extra map[string]any) (map[string]any, error) {
	ctx, span := trace.StartSpan(ctx, "maintenance-sensor::Readings")
	defer span.End()

	// A failed check is reported but never returned as an error: viam-server
	// refuses to reconfigure whenever this sensor's Readings fails, which would
	// lock out the config change needed to fix a broken arm or coffee service.
	readings := map[string]any{}

	isArmMoving, err := m.armMoving(ctx)
	if err != nil {
		m.logger.CWarnw(ctx, "maintenance sensor: arm check failed, treating arm as idle", "err", err)
		readings["arm_error"] = err.Error()
	}

	isBusy, queueCount, err := m.coffeeActivity(ctx)
	if err != nil {
		m.logger.CWarnw(ctx, "maintenance sensor: coffee check failed, treating coffee service as idle", "err", err)
		readings["coffee_error"] = err.Error()
	}

	isSafe := !isArmMoving && !isBusy && queueCount == 0
	m.logger.CDebugf(
		ctx, "is_safe debugging: is_safe: %v, arm_moving: %v, is_busy: %v, queue_count: %v",
		isSafe, isArmMoving, isBusy, queueCount,
	)

	readings["is_safe"] = isSafe
	return readings, nil
}

func (m *maintenanceSensor) armMoving(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dependencyCheckTimeout)
	defer cancel()
	moving, err := m.arm.IsMoving(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to check arm movement: %w", err)
	}
	return moving, nil
}

func (m *maintenanceSensor) coffeeActivity(ctx context.Context) (isBusy bool, queueCount float64, err error) {
	ctx, cancel := context.WithTimeout(ctx, dependencyCheckTimeout)
	defer cancel()
	resp, err := m.coffee.DoCommand(ctx, map[string]any{"get_queue": true})
	if err != nil {
		return false, 0, fmt.Errorf("failed to query coffee service: %w", err)
	}
	isBusy, _ = resp["is_busy"].(bool)
	queueCount, _ = resp["count"].(float64)
	return isBusy, queueCount, nil
}

func (m *maintenanceSensor) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	_, span := trace.StartSpan(ctx, "maintenance-sensor::DoCommand")
	defer span.End()
	return nil, nil
}

func (m *maintenanceSensor) Close(context.Context) error {
	return nil
}
