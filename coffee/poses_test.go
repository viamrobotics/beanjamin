package coffee

import "testing"

func hasPose(poses []requiredPose, name string) bool {
	for _, p := range poses {
		if p.poseName == name {
			return true
		}
	}
	return false
}

// TestRequiredPosesConditional characterizes the config-driven pose contract:
// the base brew poses are always required, while the extra-grinder and
// iced/glass poses appear only when their feature flag is enabled.
func TestRequiredPosesConditional(t *testing.T) {
	base := (&beanjaminCoffee{cfg: &Config{}}).requiredPoses()
	for _, name := range []string{
		filterPoseGrinderApproach, filterPoseHome, camPoseCupObserve,
	} {
		if !hasPose(base, name) {
			t.Errorf("base requiredPoses missing always-on pose %q", name)
		}
	}
	// Default config drives the single-toggle machine.
	if !hasPose(base, clawPoseCoffeeButtonOn) || !hasPose(base, clawPoseCoffeeButtonOff) {
		t.Error("base config should require the toggle-machine button poses")
	}
	if hasPose(base, clawPoseEspressoButtonPress) {
		t.Error("base config should not require the separate-button poses")
	}
	if hasPose(base, "decaf_grinder_approach") {
		t.Error("base config should not require decaf grinder poses")
	}
	if hasPose(base, glassPoseObserve) {
		t.Error("base config should not require iced/glass poses")
	}

	beans := (&beanjaminCoffee{cfg: &Config{BeanOptions: []BeanOption{{Name: "decaf"}, {Name: "light_roast"}}}}).requiredPoses()
	for _, name := range []string{
		"decaf_grinder_approach", "decaf_grinder_activate", "decaf_grinder_collect",
		"light_roast_grinder_approach", "light_roast_grinder_activate", "light_roast_grinder_collect",
	} {
		if !hasPose(beans, name) {
			t.Errorf("bean_options should require %q", name)
		}
	}
	// The regular grinder, and decaf enabled through can_serve_decaf, need
	// their collect poses like any other grinder.
	legacy := (&beanjaminCoffee{cfg: &Config{CanServeDecaf: true}}).requiredPoses()
	for _, name := range []string{"grinder_collect", "decaf_grinder_activate", "decaf_grinder_collect"} {
		if !hasPose(legacy, name) {
			t.Errorf("should require %q", name)
		}
	}

	// The two machine styles are mutually exclusive: enabling the separate
	// buttons must also stop requiring the toggle poses, since a switch that
	// still carried them would be validating against hardware that's gone.
	buttons := (&beanjaminCoffee{cfg: &Config{HasSeparateBrewButtons: true}}).requiredPoses()
	for _, name := range []string{
		clawPoseEspressoButtonApproach, clawPoseEspressoButtonPress,
		clawPoseLungoButtonApproach, clawPoseLungoButtonPress,
	} {
		if !hasPose(buttons, name) {
			t.Errorf("has_separate_brew_buttons should require %q", name)
		}
	}
	for _, name := range []string{
		clawPoseCoffeeButtonApproach, clawPoseCoffeeButtonOn, clawPoseCoffeeButtonOff,
	} {
		if hasPose(buttons, name) {
			t.Errorf("has_separate_brew_buttons should not require toggle pose %q", name)
		}
	}

	iced := (&beanjaminCoffee{cfg: &Config{CanServeIced: true}}).requiredPoses()
	if !hasPose(iced, glassPoseObserve) || !hasPose(iced, clawPoseIceMachineApproach) {
		t.Error("can_serve_iced should require the glass and ice-machine poses")
	}
	// The milk pour poses belong to the latte alone: an iced-coffee machine has
	// no fridge and must not be asked to resolve them.
	if hasPose(iced, milkApproachPose("left")) {
		t.Error("can_serve_iced alone should not require the milk poses")
	}

	latte := (&beanjaminCoffee{cfg: &Config{CanServeIced: true, CanServeIcedLatte: true, MilkOptions: testMilks}}).requiredPoses()
	// Each spot's two poses are required, named after the spot, not the milk.
	for _, name := range []string{"milk_left_approach", "milk_left_grab", "milk_right_approach", "milk_right_grab"} {
		if !hasPose(latte, name) {
			t.Errorf("can_serve_iced_latte should require %q", name)
		}
	}
	// The pours are resolved against the staged glass, so no switch carries them.
	for _, name := range []string{clawPosePourApproach, clawPosePour, clawPoseMilkPourApproach, clawPoseMilkPour} {
		if hasPose(latte, name) {
			t.Errorf("%q is resolved against the staged glass and must not be required on a switch", name)
		}
	}
	if !hasPose(latte, glassPoseObserve) {
		t.Error("can_serve_iced_latte should still require the iced poses it builds on")
	}
}
