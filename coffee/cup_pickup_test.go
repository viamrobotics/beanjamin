package coffee

import (
	"testing"
)

func TestPickupGetters(t *testing.T) {
	if got := pickupMaxAttempts(0); got != defaultCupPickupMaxAttempts {
		t.Errorf("pickupMaxAttempts(0) = %d, want %d", got, defaultCupPickupMaxAttempts)
	}
	if got := pickupMaxAttempts(7); got != 7 {
		t.Errorf("pickupMaxAttempts(7) = %d, want 7", got)
	}
}
