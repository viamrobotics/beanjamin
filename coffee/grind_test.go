package coffee

import "testing"

// TestGrindStepsCollisionScope locks down which grind moves may reach through
// a grinder's button shield. The approach is free-planned from wherever the arm is,
// and an allowance covers the whole plan, so granting it there would let the
// planner route the traverse through the button the shield protects.
func TestGrindStepsCollisionScope(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	steps := s.grindSteps(filterPoseGrinderApproach, filterPoseGrinderActivate, grinderButtonCollisions)

	if len(steps) != 4 {
		t.Fatalf("grindSteps returned %d steps, want 4 (approach, activate, retreat, rotate)", len(steps))
	}

	tests := []struct {
		name          string
		step          Step
		wantPose      string
		wantCircular  bool
		wantCollision bool
	}{
		{"approach", steps[0], filterPoseGrinderApproach, false, false},
		{"activate", steps[1], filterPoseGrinderActivate, false, true},
		{"retreat", steps[2], filterPoseGrinderApproach, false, true},
		{"rotate", steps[3], filterPoseGrinderApproach, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.step.PoseName != tt.wantPose {
				t.Errorf("PoseName = %q, want %q", tt.step.PoseName, tt.wantPose)
			}
			if gotCircular := tt.step.CircularRadiusMm > 0; gotCircular != tt.wantCircular {
				t.Errorf("circular = %v, want %v", gotCircular, tt.wantCircular)
			}
			if gotCollision := len(tt.step.AllowedCollisions) > 0; gotCollision != tt.wantCollision {
				t.Errorf("has AllowedCollisions = %v, want %v", gotCollision, tt.wantCollision)
			}
		})
	}
}
