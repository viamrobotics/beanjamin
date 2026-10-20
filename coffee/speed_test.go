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

// Only the empty cup leaves its shelf at the arm default; the glass and the
// full milk bottle retreat at the slow tier.
func TestPickupRetreatSpeed(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	if got := s.cupPickupTarget().retreatSpeed; got != nil {
		t.Errorf("cup retreat speed = %+v, want the arm default", got)
	}
	slow := *s.slowMoveOptions()
	for label, target := range map[string]*pickupTarget{
		"glass": s.glassPickupTarget(),
		"milk":  s.milkPickupTarget(),
	} {
		if target.retreatSpeed == nil || *target.retreatSpeed != slow {
			t.Errorf("%s retreat speed = %+v, want the slow tier %+v", label, target.retreatSpeed, slow)
		}
	}
}

// The contact moves of the grind, the brew-button poke and the purge press
// something, so each names the slow tier.
func TestContactStepsAreSlow(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	for label, seq := range map[string][]Step{
		"grind":  s.grindSteps(filterPoseGrinderApproach, filterPoseGrinderActivate, grinderButtonCollisions),
		"button": s.brewButtonSteps(clawPoseEspressoButtonApproach, clawPoseEspressoButtonPress),
		"purge":  s.purgeSteps(),
	} {
		for _, st := range seq {
			if st.LinearConstraint != nil && st.MoveOptions == nil {
				t.Errorf("%s: constrained step %q has no explicit speed", label, st.PoseName)
			}
		}
	}
}
