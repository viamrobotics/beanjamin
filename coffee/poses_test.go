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
// the base brew poses are always required, while the decaf-grinder and
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
	if hasPose(base, filterPoseDecafGrinderApproach) {
		t.Error("base config should not require decaf grinder poses")
	}
	if hasPose(base, glassPoseObserve) {
		t.Error("base config should not require iced/glass poses")
	}

	decaf := (&beanjaminCoffee{cfg: &Config{CanServeDecaf: true}}).requiredPoses()
	if !hasPose(decaf, filterPoseDecafGrinderApproach) || !hasPose(decaf, filterPoseDecafGrinderActivate) {
		t.Error("can_serve_decaf should require the decaf grinder poses")
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
	// With milk_options, each spot's two poses are required too, named after the
	// spot rather than the milk.
	spots := (&beanjaminCoffee{cfg: &Config{CanServeIced: true, CanServeIcedLatte: true, MilkOptions: testMilks}}).requiredPoses()
	for _, name := range []string{"milk_left_approach", "milk_left_grab", "milk_right_approach", "milk_right_grab"} {
		if !hasPose(spots, name) {
			t.Errorf("milk_options should require %q", name)
		}
		if hasPose(latte, name) {
			t.Errorf("%q required without milk_options", name)
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
