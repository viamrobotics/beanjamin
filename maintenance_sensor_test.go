package beanjamin

import (
	"context"
	"errors"
	"testing"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/testutils/inject"
)

type fakeCoffeeService struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	isBusy     bool
	queueCount float64
}

func (f *fakeCoffeeService) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if _, ok := cmd["get_queue"]; ok {
		return map[string]interface{}{
			"is_busy": f.isBusy,
			"count":   f.queueCount,
		}, nil
	}
	return nil, errors.New("unknown command")
}

type testOpts struct {
	armMoving  bool
	isBusy     bool
	queueCount float64
}

func setupMaintenanceSensor(t *testing.T, opts testOpts) (sensor.Sensor, *inject.Arm, *fakeCoffeeService) {
	t.Helper()

	logger := logging.NewTestLogger(t)

	fakeArm := &inject.Arm{}
	fakeArm.IsMovingFunc = func(ctx context.Context) (bool, error) {
		return opts.armMoving, nil
	}

	coffee := &fakeCoffeeService{
		Named:      resource.NewName(sensor.API, "coffee").AsNamed(),
		isBusy:     opts.isBusy,
		queueCount: opts.queueCount,
	}

	s := &maintenanceSensor{
		name:   resource.NewName(sensor.API, "maintenance"),
		logger: logger,
		coffee: coffee,
		arm:    fakeArm,
	}
	return s, fakeArm, coffee
}

func TestReadings_Safe_WhenIdle(t *testing.T) {
	s, _, _ := setupMaintenanceSensor(t, testOpts{})

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe, ok := readings["is_safe"].(bool); !ok || !safe {
		t.Errorf("expected is_safe=true when idle, got %v", readings["is_safe"])
	}
}

func TestReadings_Unsafe_WhenArmMoving(t *testing.T) {
	s, _, _ := setupMaintenanceSensor(t, testOpts{armMoving: true})

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe, ok := readings["is_safe"].(bool); !ok || safe {
		t.Errorf("expected is_safe=false when arm is moving, got %v", readings["is_safe"])
	}
}

func TestReadings_Unsafe_WhenSequenceRunning(t *testing.T) {
	s, _, _ := setupMaintenanceSensor(t, testOpts{isBusy: true})

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe, ok := readings["is_safe"].(bool); !ok || safe {
		t.Errorf("expected is_safe=false when sequence is running, got %v", readings["is_safe"])
	}
}

func TestReadings_Unsafe_WhenQueueHasOrders(t *testing.T) {
	s, _, _ := setupMaintenanceSensor(t, testOpts{queueCount: 2})

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe, ok := readings["is_safe"].(bool); !ok || safe {
		t.Errorf("expected is_safe=false when queue has orders, got %v", readings["is_safe"])
	}
}

func TestReadings_Unsafe_WhenAllActive(t *testing.T) {
	s, _, _ := setupMaintenanceSensor(t, testOpts{armMoving: true, isBusy: true, queueCount: 1})

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if safe, ok := readings["is_safe"].(bool); !ok || safe {
		t.Errorf("expected is_safe=false when all active, got %v", readings["is_safe"])
	}
}

func TestReadings_Error_WhenArmFails(t *testing.T) {
	s, fakeArm, _ := setupMaintenanceSensor(t, testOpts{})
	fakeArm.IsMovingFunc = func(ctx context.Context) (bool, error) {
		return false, errors.New("arm unreachable")
	}

	_, err := s.Readings(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when arm.IsMoving fails")
	}
}
