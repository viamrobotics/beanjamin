package maintenancesensor

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
	fail       bool
}

func (f *fakeCoffeeService) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	if f.fail {
		return nil, errors.New("coffee service unavailable")
	}
	if _, ok := cmd["get_queue"]; ok {
		return map[string]any{
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

func TestReadings_DependencyFailures(t *testing.T) {
	tests := []struct {
		name          string
		opts          testOpts
		armFails      bool
		coffeeFails   bool
		wantSafe      bool
		wantArmErr    bool
		wantCoffeeErr bool
	}{
		{name: "arm fails", armFails: true, wantSafe: true, wantArmErr: true},
		{name: "coffee fails", coffeeFails: true, wantSafe: true, wantCoffeeErr: true},
		{name: "both fail", armFails: true, coffeeFails: true, wantSafe: true, wantArmErr: true, wantCoffeeErr: true},
		{name: "arm fails while coffee busy", opts: testOpts{isBusy: true}, armFails: true, wantSafe: false, wantArmErr: true},
		{name: "coffee fails while arm moving", opts: testOpts{armMoving: true}, coffeeFails: true, wantSafe: false, wantCoffeeErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, fakeArm, coffee := setupMaintenanceSensor(t, tt.opts)
			if tt.armFails {
				fakeArm.IsMovingFunc = func(ctx context.Context) (bool, error) {
					return false, errors.New("arm unreachable")
				}
			}
			coffee.fail = tt.coffeeFails

			readings, err := s.Readings(context.Background(), nil)
			if err != nil {
				t.Fatalf("Readings must not error on dependency failure, got %v", err)
			}
			if safe, ok := readings["is_safe"].(bool); !ok || safe != tt.wantSafe {
				t.Errorf("is_safe = %v, want %v", readings["is_safe"], tt.wantSafe)
			}
			if _, ok := readings["arm_error"]; ok != tt.wantArmErr {
				t.Errorf("arm_error present = %v, want %v", ok, tt.wantArmErr)
			}
			if _, ok := readings["coffee_error"]; ok != tt.wantCoffeeErr {
				t.Errorf("coffee_error present = %v, want %v", ok, tt.wantCoffeeErr)
			}
		})
	}
}
