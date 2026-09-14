package coffee

import "testing"

// The no-spill carry exists to keep a drink level, so it only earns its cost
// when there is a drink in the container. The iced flow places the espresso cup
// on the shelf *after* pouring it over the ice, and that cup is empty.
func TestUsesNoSpillCarry(t *testing.T) {
	cases := []struct {
		name     string
		configed bool
		contents heldContents
		want     bool
	}{
		{"configured, filled -> level carry", true, heldFilled, true},
		{"configured, empty -> free plan", true, heldEmpty, false},
		{"not configured, filled -> free plan", false, heldFilled, false},
		{"not configured, empty -> free plan", false, heldEmpty, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &beanjaminCoffee{cfg: &Config{NoSpillCarry: c.configed}}
			if got := s.usesNoSpillCarry(c.contents); got != c.want {
				t.Errorf("usesNoSpillCarry = %v, want %v", got, c.want)
			}
		})
	}
}
