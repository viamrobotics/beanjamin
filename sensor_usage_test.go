package beanjamin

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/resource"
)

// fakeUsageSensor is a minimal sensor.Sensor that returns a seeded Readings map
// and records every DoCommand it receives, with injectable errors.
type fakeUsageSensor struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	mu         sync.Mutex
	readings   map[string]interface{}
	readErr    error
	doErr      error
	doCommands []map[string]interface{}
}

func newFakeUsageSensor(readings map[string]interface{}) *fakeUsageSensor {
	return &fakeUsageSensor{
		Named:    resource.NewName(sensor.API, "usage-test").AsNamed(),
		readings: readings,
	}
}

func (f *fakeUsageSensor) Readings(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.readings, nil
}

func (f *fakeUsageSensor) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doCommands = append(f.doCommands, cmd)
	if f.doErr != nil {
		return nil, f.doErr
	}
	return map[string]interface{}{}, nil
}

// lastSet returns the value written by the most recent DoCommand({"set": {field: v}}).
func (f *fakeUsageSensor) lastSet(t *testing.T, field string) float64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.doCommands) == 0 {
		t.Fatalf("no DoCommand recorded")
	}
	cmd := f.doCommands[len(f.doCommands)-1]
	set, ok := cmd["set"].(map[string]interface{})
	if !ok {
		t.Fatalf("DoCommand missing set object: %#v", cmd)
	}
	v, ok := set[field].(float64)
	if !ok {
		t.Fatalf("set[%q] is not a float64: %#v", field, set[field])
	}
	return v
}

func (f *fakeUsageSensor) doCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.doCommands)
}

func TestIncrementSensorReading_ExistingValue(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	sen := newFakeUsageSensor(map[string]interface{}{"regular_grinds": float64(4)})
	s.incrementSensorReading(context.Background(), sen, "grinder", "regular_grinds", 1)
	if got := sen.lastSet(t, "regular_grinds"); got != 5 {
		t.Fatalf("grinds = %v, want 5", got)
	}
}

func TestIncrementSensorReading_MissingFieldStartsAtZero(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	sen := newFakeUsageSensor(map[string]interface{}{})
	s.incrementSensorReading(context.Background(), sen, "grinder", "regular_grinds", 1)
	if got := sen.lastSet(t, "regular_grinds"); got != 1 {
		t.Fatalf("grinds = %v, want 1", got)
	}
}

func TestIncrementSensorReading_IntFieldAccepted(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	sen := newFakeUsageSensor(map[string]interface{}{"regular_grinds": 7})
	s.incrementSensorReading(context.Background(), sen, "grinder", "regular_grinds", 1)
	if got := sen.lastSet(t, "regular_grinds"); got != 8 {
		t.Fatalf("grinds = %v, want 8", got)
	}
}

func TestIncrementSensorReading_NonNumericSkipsWrite(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	sen := newFakeUsageSensor(map[string]interface{}{"regular_grinds": "oops"})
	s.incrementSensorReading(context.Background(), sen, "grinder", "regular_grinds", 1)
	if n := sen.doCount(); n != 0 {
		t.Fatalf("expected no DoCommand for non-numeric field, got %d", n)
	}
}

func TestIncrementSensorReading_JSONNumberAccepted(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	sen := newFakeUsageSensor(map[string]interface{}{"regular_grinds": json.Number("4")})
	s.incrementSensorReading(context.Background(), sen, "grinder", "regular_grinds", 1)
	if got := sen.lastSet(t, "regular_grinds"); got != 5 {
		t.Fatalf("grinds = %v, want 5", got)
	}
}

func TestIncrementSensorReading_DistinctKeysOnOneSensor(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	// A single usage sensor carries every counter under its own key; each
	// update reads and writes only that key.
	sen := newFakeUsageSensor(map[string]interface{}{
		"regular_grinds":                float64(2),
		"decaf_grinds":                  float64(5),
		"usage":                         float64(10),
		"cleanings":                     float64(1),
		"successful_consecutive_orders": float64(3),
	})
	s.incrementSensorReading(context.Background(), sen, "grinder", "regular_grinds", 1)
	if got := sen.lastSet(t, "regular_grinds"); got != 3 {
		t.Fatalf("regular_grinds = %v, want 3", got)
	}
	s.incrementSensorReading(context.Background(), sen, "water", "usage", 1.5)
	if got := sen.lastSet(t, "usage"); got != 11.5 {
		t.Fatalf("usage = %v, want 11.5", got)
	}
	s.incrementSensorReading(context.Background(), sen, "decaf grinder", "decaf_grinds", 1)
	if got := sen.lastSet(t, "decaf_grinds"); got != 6 {
		t.Fatalf("decaf_grinds = %v, want 6", got)
	}
}

func TestIncrementSensorReading_ReadErrorSkipsWrite(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	sen := newFakeUsageSensor(map[string]interface{}{"regular_grinds": float64(4)})
	sen.readErr = errors.New("boom")
	s.incrementSensorReading(context.Background(), sen, "grinder", "regular_grinds", 1)
	if n := sen.doCount(); n != 0 {
		t.Fatalf("expected no DoCommand on read error, got %d", n)
	}
}

func TestIncrementSensorReading_WriteErrorIsBestEffort(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	sen := newFakeUsageSensor(map[string]interface{}{"regular_grinds": float64(4)})
	sen.doErr = errors.New("boom")
	// Must not panic or block; failure is swallowed.
	s.incrementSensorReading(context.Background(), sen, "grinder", "regular_grinds", 1)
	if n := sen.doCount(); n != 1 {
		t.Fatalf("expected one DoCommand attempt, got %d", n)
	}
}

func TestIncrementSensorReading_NilSensorNoOp(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	// Should not panic.
	s.incrementSensorReading(context.Background(), nil, "grinder", "regular_grinds", 1)
}

func TestSetSensorReading_NilSensorNoOp(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	s.setSensorReading(context.Background(), nil, "consecutive orders", "successful_consecutive_orders", 0)
}

func TestConsecutiveOrders_SuccessIncrementsResetOnFailure(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	sen := newFakeUsageSensor(map[string]interface{}{"successful_consecutive_orders": float64(3)})

	// Success bumps the streak.
	s.incrementSensorReading(context.Background(), sen, "consecutive orders", "successful_consecutive_orders", 1)
	if got := sen.lastSet(t, "successful_consecutive_orders"); got != 4 {
		t.Fatalf("after success = %v, want 4", got)
	}

	// Failure resets to 0 regardless of current value.
	s.setSensorReading(context.Background(), sen, "consecutive orders", "successful_consecutive_orders", 0)
	if got := sen.lastSet(t, "successful_consecutive_orders"); got != 0 {
		t.Fatalf("after failure = %v, want 0", got)
	}
}

func TestWaterDelta(t *testing.T) {
	for _, tc := range []struct {
		drink string
		want  float64
	}{
		{"espresso", 1},
		{"decaf", 1},
		{"lungo", 1.5},
		{"decaf_lungo", 1.5},
	} {
		if got := waterDelta(tc.drink); got != tc.want {
			t.Errorf("waterDelta(%q) = %v, want %v", tc.drink, got, tc.want)
		}
	}
}
