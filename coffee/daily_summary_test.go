package coffee

import (
	"context"
	"testing"
)

func TestConsecutiveSuccesses(t *testing.T) {
	ctx := context.Background()

	// No usage sensor configured: the digest omits the field rather than
	// claiming a broken streak.
	s, _ := newTestCoffee(t, nil)
	if streak, ok := s.consecutiveSuccesses(ctx); ok {
		t.Errorf("consecutiveSuccesses() = (%d, true) with no usage sensor, want ok=false", streak)
	}

	s.usageSensor = newFakeUsageSensor(map[string]any{
		"successful_consecutive_orders": float64(12),
		"regular_grinds":                float64(40),
	})
	streak, ok := s.consecutiveSuccesses(ctx)
	if !ok || streak != 12 {
		t.Errorf("consecutiveSuccesses() = (%d, %v), want (12, true)", streak, ok)
	}

	// A sensor that answers but has never recorded a streak reads as 0, which is
	// a real value — the machine's last order failed.
	s.usageSensor = newFakeUsageSensor(map[string]any{"regular_grinds": float64(40)})
	if streak, ok := s.consecutiveSuccesses(ctx); !ok || streak != 0 {
		t.Errorf("consecutiveSuccesses() = (%d, %v) for an absent key, want (0, true)", streak, ok)
	}

	// A non-numeric value is unreadable, not zero.
	s.usageSensor = newFakeUsageSensor(map[string]any{"successful_consecutive_orders": "lots"})
	if streak, ok := s.consecutiveSuccesses(ctx); ok {
		t.Errorf("consecutiveSuccesses() = (%d, true) for a non-numeric value, want ok=false", streak)
	}
}
