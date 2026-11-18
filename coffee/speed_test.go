package coffee

import "testing"

// A move runs at the speed it names: nothing is slowed by its shape alone.
func TestBuildMoveOptions(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	if got := buildMoveOptions(nil); got != nil {
		t.Errorf("buildMoveOptions(nil) = %+v, want the arm default", got)
	}
	slow := buildMoveOptions(s.slowMoveOptions())
	if slow == nil || slow.MaxVelRads <= 0 || slow.MaxAccRads <= 0 {
		t.Errorf("slow tier = %+v, want velocity and acceleration caps", slow)
	}
}

// Only the empty cup leaves its shelf at the arm default; the glass retreats
// at the slow tier.
func TestPickupRetreatSpeed(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	if got := s.cupPickupTarget().retreatSpeed; got != nil {
		t.Errorf("cup retreat speed = %+v, want the arm default", got)
	}
	slow := *s.slowMoveOptions()
	if got := s.glassPickupTarget().retreatSpeed; got == nil || *got != slow {
		t.Errorf("glass retreat speed = %+v, want the slow tier %+v", got, slow)
	}
}

// The contact moves of the grind and the purge press something, so each names
// the slow tier.
func TestContactStepsAreSlow(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	for label, seq := range map[string][]Step{
		"grind": s.grindSteps(grinderPoses("regular"), grinderButtonCollisions("regular")),
		"purge": s.purgeSteps(),
	} {
		for _, st := range seq {
			if st.LinearConstraint != nil && st.MoveOptions == nil {
				t.Errorf("%s: constrained step %q has no explicit speed", label, st.PoseName)
			}
		}
	}
}

// The spillage-prevention tier follows the slow tier until it is configured on
// its own, so the slow tier can be sped up without touching the carries.
func TestSpillagePreventionMoveOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want StepMoveOptions
	}{
		{"defaults", Config{}, StepMoveOptions{
			MaxVelDegsPerSec: defaultSlowMovementVelDegsPerSec, MaxAccDegsPerSec2: defaultSlowMovementAccDegsPerSec2}},
		{"follows slow tier", Config{SlowMovementVelDegsPerSec: 60, SlowMovementAccDegsPerSec2: 150},
			StepMoveOptions{MaxVelDegsPerSec: 60, MaxAccDegsPerSec2: 150}},
		{"configured", Config{SlowMovementVelDegsPerSec: 60, SlowMovementAccDegsPerSec2: 150,
			SpillagePreventionVelDegsPerSec: 40, SpillagePreventionAccDegsPerSec2: 25},
			StepMoveOptions{MaxVelDegsPerSec: 40, MaxAccDegsPerSec2: 25}},
		{"velocity only", Config{SlowMovementVelDegsPerSec: 60, SlowMovementAccDegsPerSec2: 150,
			SpillagePreventionVelDegsPerSec: 40},
			StepMoveOptions{MaxVelDegsPerSec: 40, MaxAccDegsPerSec2: 150}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			s, _ := newTestCoffee(t, &cfg)
			if got := *s.spillagePreventionMoveOptions(); got != tc.want {
				t.Errorf("spillagePreventionMoveOptions() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Once the grinder has dispensed, the filter holds loose grounds: the circle
// under the chute and the retreat from it run at the spillage-prevention tier,
// while the empty filter's moves in stay at the slow tier.
func TestGrindStepsSpeeds(t *testing.T) {
	s, _ := newTestCoffee(t, &Config{SlowMovementAccDegsPerSec2: 150, SpillagePreventionAccDegsPerSec2: 25})
	slow, spill := *s.slowMoveOptions(), *s.spillagePreventionMoveOptions()
	steps := s.grindSteps(grinderPoses("regular"), grinderButtonCollisions("regular"))
	want := []*StepMoveOptions{nil, &slow, &slow, &spill, &spill}
	if len(steps) != len(want) {
		t.Fatalf("grindSteps has %d steps, want %d", len(steps), len(want))
	}
	for i, st := range steps {
		switch {
		case want[i] == nil && st.MoveOptions != nil:
			t.Errorf("step %d (%q) = %+v, want the arm default", i, st.PoseName, *st.MoveOptions)
		case want[i] != nil && (st.MoveOptions == nil || *st.MoveOptions != *want[i]):
			t.Errorf("step %d (%q) = %+v, want %+v", i, st.PoseName, st.MoveOptions, *want[i])
		}
	}
}
