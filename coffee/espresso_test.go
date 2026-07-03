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
		filterPoseGrinderApproach, clawPoseCoffeeButtonOn, filterPoseHome, camPoseCupObserve,
	} {
		if !hasPose(base, name) {
			t.Errorf("base requiredPoses missing always-on pose %q", name)
		}
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

	iced := (&beanjaminCoffee{cfg: &Config{CanServeIced: true}}).requiredPoses()
	if !hasPose(iced, glassPoseObserve) || !hasPose(iced, clawPoseIceMachineApproach) {
		t.Error("can_serve_iced should require the glass and ice-machine poses")
	}
}
