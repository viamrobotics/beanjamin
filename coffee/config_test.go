package coffee

import "testing"

func TestOrDefault(t *testing.T) {
	if got := orDefault(0, 5); got != 5 {
		t.Errorf("orDefault(0, 5) = %d, want 5", got)
	}
	if got := orDefault(3, 5); got != 3 {
		t.Errorf("orDefault(3, 5) = %d, want 3", got)
	}
	if got := orDefault(-2, 5); got != 5 {
		t.Errorf("orDefault(-2, 5) = %d, want 5 (non-positive falls to default)", got)
	}
	if got := orDefault(2.5, 1.0); got != 2.5 {
		t.Errorf("orDefault(2.5, 1.0) = %v, want 2.5", got)
	}
}

func TestPickupGetters(t *testing.T) {
	if got := pickupMaxAttempts(0); got != defaultCupPickupMaxAttempts {
		t.Errorf("pickupMaxAttempts(0) = %d, want %d", got, defaultCupPickupMaxAttempts)
	}
	if got := pickupMaxAttempts(7); got != 7 {
		t.Errorf("pickupMaxAttempts(7) = %d, want 7", got)
	}
}
