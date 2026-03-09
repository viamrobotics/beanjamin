package statemachine

import (
	"testing"
)

// defaultTransitions returns the default state machine graph.
// Each key is a unique state name; values are the states directly reachable from that state.
func defaultTransitions() map[string][]string {
	return map[string][]string{
		"home":                {"grinder_approach", "tamper_approach", "coffee_approach", "dump_grounds", "pre_dump_grounds"},
		"grinder_approach":    {"home", "grinder_activate", "tamper_approach", "coffee_approach", "pre_dump_grounds"},
		"grinder_activate":    {"grinder_approach"},
		"tamper_approach":     {"home", "grinder_approach", "tamper_activate", "coffee_approach", "pre_dump_grounds"},
		"tamper_activate":     {"tamper_approach"},
		"coffee_approach":     {"home", "grinder_approach", "tamper_approach", "coffee_in", "pre_dump_grounds"},
		"coffee_in":           {"coffee_approach", "coffee_locked_final"},
		"coffee_locked_final": {"coffee_in"},
		"dump_grounds":        {"home", "pre_dump_grounds"},
		"pre_dump_grounds":    {"home", "grinder_approach", "tamper_approach", "coffee_approach", "dump_grounds"},
	}
}

func TestDefaultTransitions(t *testing.T) {
	tr := defaultTransitions()
	expectedKeys := []string{
		"home", "grinder_approach", "grinder_activate",
		"tamper_approach", "tamper_activate",
		"coffee_approach", "coffee_in", "coffee_locked_final",
		"dump_grounds", "pre_dump_grounds",
	}
	if len(tr) != len(expectedKeys) {
		t.Errorf("expected %d states, got %d", len(expectedKeys), len(tr))
	}
	for _, key := range expectedKeys {
		if _, ok := tr[key]; !ok {
			t.Errorf("missing expected state %q", key)
		}
	}
	// No dangling targets.
	for from, targets := range tr {
		for _, to := range targets {
			if _, ok := tr[to]; !ok {
				t.Errorf("state %q has dangling target %q", from, to)
			}
		}
	}
}

func TestIsDirectTransition(t *testing.T) {
	tr := defaultTransitions()

	t.Run("valid", func(t *testing.T) {
		pairs := []struct {
			from, to string
		}{
			{"home", "grinder_approach"},
			{"home", "coffee_approach"},
			{"home", "dump_grounds"},
			{"home", "pre_dump_grounds"},
			{"grinder_activate", "grinder_approach"},
			{"tamper_activate", "tamper_approach"},
			{"coffee_approach", "coffee_in"},
			{"coffee_in", "coffee_locked_final"},
			{"coffee_locked_final", "coffee_in"},
			{"dump_grounds", "pre_dump_grounds"},
			{"pre_dump_grounds", "dump_grounds"},
		}
		for _, p := range pairs {
			t.Run(p.from+"→"+p.to, func(t *testing.T) {
				if !isDirectTransition(tr, p.from, p.to) {
					t.Errorf("expected direct transition %q→%q", p.from, p.to)
				}
			})
		}
	})

	t.Run("invalid", func(t *testing.T) {
		pairs := []struct {
			from, to string
		}{
			{"coffee_in", "home"},
			{"coffee_locked_final", "home"},
			{"grinder_activate", "tamper_activate"},
			{"tamper_activate", "grinder_activate"},
			{"coffee_locked_final", "coffee_approach"},
		}
		for _, p := range pairs {
			t.Run(p.from+"→"+p.to, func(t *testing.T) {
				if isDirectTransition(tr, p.from, p.to) {
					t.Errorf("expected no direct transition %q→%q", p.from, p.to)
				}
			})
		}
	})
}

func TestValidatePath(t *testing.T) {
	tr := defaultTransitions()

	t.Run("valid sequence", func(t *testing.T) {
		poses := []string{"home", "grinder_approach", "grinder_activate", "grinder_approach"}
		if err := validatePath(tr, poses, ""); err != nil {
			t.Errorf("expected valid path, got error: %v", err)
		}
	})

	t.Run("unknown pose returns error", func(t *testing.T) {
		poses := []string{"home", "custom_approach_step", "coffee_approach"}
		if err := validatePath(tr, poses, ""); err == nil {
			t.Error("expected error for unknown pose, got nil")
		}
	})

	t.Run("invalid transition", func(t *testing.T) {
		// coffee_in → home is not a direct transition.
		poses := []string{"coffee_in", "home"}
		if err := validatePath(tr, poses, ""); err == nil {
			t.Error("expected error for invalid transition coffee_in→home, got nil")
		}
	})

	t.Run("single pose", func(t *testing.T) {
		if err := validatePath(tr, []string{"home"}, ""); err != nil {
			t.Errorf("single-pose path should be valid, got: %v", err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if err := validatePath(tr, nil, ""); err != nil {
			t.Errorf("empty path should be valid, got: %v", err)
		}
	})

	t.Run("rewind with seeded start", func(t *testing.T) {
		// Rewind of a grinder sequence starting from grinder_approach,
		// going back through grinder_activate then grinder_approach.
		poses := []string{"grinder_activate", "grinder_approach"}
		if err := validatePath(tr, poses, "grinder_approach"); err != nil {
			t.Errorf("expected valid rewind path, got error: %v", err)
		}
	})
}

func TestResolvePath(t *testing.T) {
	tr := defaultTransitions()

	t.Run("uninitialized returns error", func(t *testing.T) {
		_, _, err := resolvePath(tr, "", "home")
		if err == nil {
			t.Error("expected error for uninitialized state, got nil")
		}
	})

	t.Run("direct adjacent transition has no intermediates", func(t *testing.T) {
		intermediates, finalPose, err := resolvePath(tr, "home", "coffee_approach")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if finalPose != "coffee_approach" {
			t.Errorf("finalPose = %q, want %q", finalPose, "coffee_approach")
		}
		if len(intermediates) != 0 {
			t.Errorf("expected no intermediates, got %v", intermediates)
		}
	})

	t.Run("retrace through coffee locked state", func(t *testing.T) {
		// coffee_locked_final → coffee_approach must retrace: coffee_locked_final → coffee_in → coffee_approach.
		intermediates, finalPose, err := resolvePath(tr, "coffee_locked_final", "coffee_approach")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if finalPose != "coffee_approach" {
			t.Errorf("finalPose = %q, want %q", finalPose, "coffee_approach")
		}
		want := []string{"coffee_in"}
		if len(intermediates) != len(want) {
			t.Fatalf("intermediates = %v, want %v", intermediates, want)
		}
		for i, v := range want {
			if intermediates[i] != v {
				t.Errorf("intermediates[%d] = %q, want %q", i, intermediates[i], v)
			}
		}
	})

	t.Run("unreachable pose returns error", func(t *testing.T) {
		_, _, err := resolvePath(tr, "home", "nonexistent_pose")
		if err == nil {
			t.Error("expected error for unreachable pose, got nil")
		}
	})

	t.Run("resolving current pose is a no-op", func(t *testing.T) {
		intermediates, finalPose, err := resolvePath(tr, "home", "home")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if finalPose != "home" {
			t.Errorf("finalPose = %q, want %q", finalPose, "home")
		}
		if len(intermediates) != 0 {
			t.Errorf("expected no intermediates, got %v", intermediates)
		}
	})
}

func TestConfigProvidedTransitions(t *testing.T) {
	// Minimal two-state custom graph.
	customTransitions := map[string][]string{
		"state_a": {"state_b"},
		"state_b": {"state_a"},
	}

	t.Run("direct transition", func(t *testing.T) {
		intermediates, finalPose, err := resolvePath(customTransitions, "state_a", "state_b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if finalPose != "state_b" {
			t.Errorf("finalPose = %q, want %q", finalPose, "state_b")
		}
		if len(intermediates) != 0 {
			t.Errorf("expected no intermediates, got %v", intermediates)
		}
	})

	t.Run("unknown target returns error", func(t *testing.T) {
		_, _, err := resolvePath(customTransitions, "state_a", "state_c")
		if err == nil {
			t.Error("expected error for unknown target, got nil")
		}
	})
}
