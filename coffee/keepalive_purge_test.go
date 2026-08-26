package coffee

import (
	"testing"

	"go.viam.com/rdk/testutils/inject"
)

// TestPurgeStepsCollisionScope locks down which moves of the keep-alive purge may
// carry filterCoffeeButtonCollisions.
//
// An allowed-collision entry applies to the whole plan, not just near its goal.
// The approach is free-planned from home, so allowing coffee-machine-buffer-front
// there lets the planner route the portafilter straight through the machine's
// front face. Only the two short linear moves, which genuinely sit inside that
// obstacle, may allow it. This mirrors TestBrewButtonStepsCollisionScope.
func TestPurgeStepsCollisionScope(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{
		HasSeparateBrewButtons: true,
		KeepAlive:              &KeepAlive{AutoStart: "07:45", End: "17:00", Timezone: "UTC"},
	}}
	steps := s.purgeSteps()

	if len(steps) != 3 {
		t.Fatalf("purgeSteps returned %d steps, want 3 (approach, press, retreat)", len(steps))
	}

	tests := []struct {
		name          string
		step          Step
		wantPose      string
		wantLinear    bool
		wantCollision bool
	}{
		{"approach", steps[0], filterPosePurgeApproach, false, false},
		{"press", steps[1], filterPosePurgePress, true, true},
		{"retreat", steps[2], filterPosePurgeApproach, true, true},
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

	// Only the press dwells. A dwell on the retreat would hold the arm against the
	// machine face for no reason; a dwell on the approach would do nothing at all.
	if steps[1].Pause != s.cfg.KeepAlive.hold() {
		t.Errorf("press Pause = %v, want the configured hold %v", steps[1].Pause, s.cfg.KeepAlive.hold())
	}
	if steps[0].Pause != 0 || steps[2].Pause != 0 {
		t.Errorf("approach/retreat Pause = %v/%v, want 0", steps[0].Pause, steps[2].Pause)
	}
}

// TestPurgeStepsUseFilterSwitch guards the reason these poses exist on the filter
// switch: the arm holds the portafilter through the whole purge, so the
// portafilter is the frame being positioned. Reading them off the claws switch
// would drive the claw geometry to filter-frame coordinates.
func TestPurgeStepsUseFilterSwitch(t *testing.T) {
	// Two distinct switches so the assertion is about identity, not just
	// non-nil-ness. inject.Switch is the RDK's test double, as inject.Arm is in
	// the maintenancesensor tests; no method on it is called here.
	filterSw := &inject.Switch{}
	clawsSw := &inject.Switch{}
	s := &beanjaminCoffee{
		cfg: &Config{
			HasSeparateBrewButtons: true,
			KeepAlive:              &KeepAlive{AutoStart: "07:45", End: "17:00", Timezone: "UTC"},
		},
		filterSw: filterSw,
		clawsSw:  clawsSw,
	}
	for i, step := range s.purgeSteps() {
		if step.PoseSwitch != filterSw {
			t.Errorf("step %d (%s) reads from the wrong switch, want the filter switch", i, step.PoseName)
		}
	}
}
