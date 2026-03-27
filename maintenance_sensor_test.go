package beanjamin

import (
	"context"
	"errors"
	"testing"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/testutils/inject"
)

func setupMaintenanceSensor(t *testing.T, armMoving bool, sequenceRunning bool) (sensor.Sensor, *inject.Arm) {
	t.Helper()

	logger := logging.NewTestLogger(t)

	fakeArm := &inject.Arm{}
	fakeArm.IsMovingFunc = func(ctx context.Context) (bool, error) {
		return armMoving, nil
	}

	coffee := &beanjaminCoffee{
		name:   resource.NewName(generic.API, "coffee"),
		logger: logger,
		cfg:    &Config{},
		queue:  NewOrderQueue(),
	}
	if sequenceRunning {
		coffee.running.Store(true)
	}

	s := &maintenanceSensor{
		name:   resource.NewName(sensor.API, "maintenance"),
		logger: logger,
		coffee: coffee,
		arm:    fakeArm,
	}
	return s, fakeArm
}

func TestReadings_Safe_WhenIdle(t *testing.T) {
	s, _ := setupMaintenanceSensor(t, false, false)

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe, ok := readings["is_safe"].(bool); !ok || !safe {
		t.Errorf("expected is_safe=true when idle, got %v", readings["is_safe"])
	}
}

func TestReadings_Unsafe_WhenArmMoving(t *testing.T) {
	s, _ := setupMaintenanceSensor(t, true, false)

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe, ok := readings["is_safe"].(bool); !ok || safe {
		t.Errorf("expected is_safe=false when arm is moving, got %v", readings["is_safe"])
	}
}

func TestReadings_Unsafe_WhenSequenceRunning(t *testing.T) {
	s, _ := setupMaintenanceSensor(t, false, true)

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe, ok := readings["is_safe"].(bool); !ok || safe {
		t.Errorf("expected is_safe=false when sequence is running, got %v", readings["is_safe"])
	}
}

func TestReadings_Unsafe_WhenBothActive(t *testing.T) {
	s, _ := setupMaintenanceSensor(t, true, true)

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe, ok := readings["is_safe"].(bool); !ok || safe {
		t.Errorf("expected is_safe=false when both active, got %v", readings["is_safe"])
	}
}

func TestReadings_Error_WhenArmFails(t *testing.T) {
	s, fakeArm := setupMaintenanceSensor(t, false, false)
	fakeArm.IsMovingFunc = func(ctx context.Context) (bool, error) {
		return false, errors.New("arm unreachable")
	}

	_, err := s.Readings(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when arm.IsMoving fails")
	}
}

