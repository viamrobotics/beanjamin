package coffee

import "testing"

// TestBrewButtonStepsCollisionScope locks down which moves of a brew-button poke
// may carry clawCoffeeButtonCollisions.
//
// An allowed-collision entry applies to the whole plan, not just near its goal.
// The approach is free-planned from wherever the arm is mid-cycle, so allowing
// coffee-machine-buffer-front there lets the planner route the entire traverse
// through the machine's front face and the claw hits the machine. Only the two
// short linear moves, which genuinely sit inside that obstacle, may allow it.
func TestBrewButtonStepsCollisionScope(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	steps := s.brewButtonSteps("espresso_button_approach", "espresso_button_press")

	if len(steps) != 3 {
		t.Fatalf("brewButtonSteps returned %d steps, want 3 (approach, press, retreat)", len(steps))
	}

	tests := []struct {
		name          string
		step          Step
		wantPose      string
		wantLinear    bool
		wantCollision bool
	}{
		{"approach", steps[0], "espresso_button_approach", false, false},
		{"press", steps[1], "espresso_button_press", true, true},
		{"retreat", steps[2], "espresso_button_approach", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.step.PoseName != tt.wantPose {
				t.Errorf("PoseName = %q, want %q", tt.step.PoseName, tt.wantPose)
			}
			if gotLinear := tt.step.LinearConstraint != nil; gotLinear != tt.wantLinear {
				t.Errorf("has LinearConstraint = %v, want %v", gotLinear, tt.wantLinear)
			}
			gotCollision := len(tt.step.AllowedCollisions) > 0
			if gotCollision != tt.wantCollision {
				t.Errorf("has AllowedCollisions = %v, want %v — a free-planned move must not "+
					"allow coffee-machine-buffer-front, the allowance covers the whole trajectory",
					gotCollision, tt.wantCollision)
			}
		})
	}

	// The press dwells; the approach and retreat must not, or the arm sits on a
	// momentary button past the point the machine has already started dosing.
	if steps[1].Pause <= 0 {
		t.Errorf("press step Pause = %v, want the configured button hold", steps[1].Pause)
	}
	if steps[0].Pause != 0 || steps[2].Pause != 0 {
		t.Errorf("approach/retreat Pause = %v/%v, want 0", steps[0].Pause, steps[2].Pause)
	}
}
