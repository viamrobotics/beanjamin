package statemachine

import (
	"testing"
)

func TestInferIndex(t *testing.T) {
	tests := []struct {
		pose    string
		wantIdx int
	}{
		{"home", 0},
		{"grinder_approach", 1}, // lowest index for duplicate
		{"grinder_activate", 2},
		{"tamper_approach", 4}, // lowest index for duplicate
		{"tamper_activate", 5},
		{"coffee_approach", 7},
		{"coffee_in", 8},
		{"coffee_locked_final", 9},
		{"dump_grounds", 10},
		{"pre_dump_grounds", 11},
		{"not_a_pose", -1},
	}
	for _, tc := range tests {
		t.Run(tc.pose, func(t *testing.T) {
			got := InferIndex(tc.pose)
			if got != tc.wantIdx {
				t.Errorf("got %d, want %d", got, tc.wantIdx)
			}
		})
	}
}

func TestPoseNameAt(t *testing.T) {
	tests := []struct {
		idx      int
		wantName string
	}{
		{0, "home"},
		{1, "grinder_approach"},
		{3, "grinder_approach"},
		{9, "coffee_locked_final"},
		{10, "dump_grounds"},
		{11, "pre_dump_grounds"},
	}
	for _, tc := range tests {
		t.Run(tc.wantName, func(t *testing.T) {
			got := PoseNameAt(tc.idx)
			if got != tc.wantName {
				t.Errorf("PoseNameAt(%d) = %q, want %q", tc.idx, got, tc.wantName)
			}
		})
	}
}

func TestIsDirectTransition(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		pairs := []struct {
			from, to int
			label    string
		}{
			{0, 1, "home→grinder_approach"},
			{0, 7, "home→coffee_approach"},
			{0, 10, "home→dump_grounds"},
			{0, 11, "home→pre_dump_grounds"},
			{7, 8, "coffee_approach→coffee_in"},
			{8, 9, "coffee_in→coffee_locked_final"},
			{9, 8, "coffee_locked_final→coffee_in"},
			{10, 11, "dump_grounds→pre_dump_grounds"},
			{11, 10, "pre_dump_grounds→dump_grounds"},
		}
		for _, p := range pairs {
			t.Run(p.label, func(t *testing.T) {
				if !isDirectTransition(p.from, p.to) {
					t.Errorf("expected direct transition %d→%d", p.from, p.to)
				}
			})
		}
	})

	t.Run("invalid", func(t *testing.T) {
		pairs := []struct {
			from, to int
			label    string
		}{
			{8, 0, "coffee_in→home"},
			{9, 0, "coffee_locked_final→home"},
			{2, 5, "grinder_activate→tamper_activate"},
			{5, 2, "tamper_activate→grinder_activate"},
			{9, 7, "coffee_locked_final→coffee_approach"},
		}
		for _, p := range pairs {
			t.Run(p.label, func(t *testing.T) {
				if isDirectTransition(p.from, p.to) {
					t.Errorf("expected no direct transition %d→%d", p.from, p.to)
				}
			})
		}
	})
}

func TestValidatePath(t *testing.T) {
	t.Run("valid sequence", func(t *testing.T) {
		poses := []string{"home", "grinder_approach", "grinder_activate", "grinder_approach"}
		if err := ValidatePath(poses); err != nil {
			t.Errorf("expected valid path, got error: %v", err)
		}
	})

	t.Run("unknown poses skipped", func(t *testing.T) {
		// Poses not in the state machine should be ignored without error.
		poses := []string{"home", "custom_approach_step", "coffee_approach"}
		if err := ValidatePath(poses); err != nil {
			t.Errorf("expected unknown poses to be skipped, got error: %v", err)
		}
	})

	t.Run("invalid transition", func(t *testing.T) {
		// coffee_in → home is not a direct transition.
		poses := []string{"coffee_in", "home"}
		if err := ValidatePath(poses); err == nil {
			t.Error("expected error for invalid transition coffee_in→home, got nil")
		}
	})

	t.Run("single pose", func(t *testing.T) {
		if err := ValidatePath([]string{"home"}); err != nil {
			t.Errorf("single-pose path should be valid, got: %v", err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if err := ValidatePath(nil); err != nil {
			t.Errorf("empty path should be valid, got: %v", err)
		}
	})
}

func TestResolvePath(t *testing.T) {
	t.Run("uninitialized returns error", func(t *testing.T) {
		_, _, err := ResolvePath(-1, "home")
		if err == nil {
			t.Error("expected error for uninitialized state, got nil")
		}
	})

	t.Run("direct adjacent transition has no intermediates", func(t *testing.T) {
		// home (0) → coffee_approach (7) is a direct edge.
		intermediates, finalIdx, err := ResolvePath(0, "coffee_approach")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if finalIdx != 7 {
			t.Errorf("finalIdx = %d, want 7", finalIdx)
		}
		if len(intermediates) != 0 {
			t.Errorf("expected no intermediates, got %v", intermediates)
		}
	})

	t.Run("retrace through coffee locked state", func(t *testing.T) {
		// coffee_locked_final (9) → coffee_approach (7) must retrace: 9 → 8 → 7.
		intermediates, finalIdx, err := ResolvePath(9, "coffee_approach")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if finalIdx != 7 {
			t.Errorf("finalIdx = %d, want 7", finalIdx)
		}
		want := []int{8}
		if len(intermediates) != len(want) {
			t.Fatalf("intermediates = %v, want %v", intermediates, want)
		}
		for i, v := range want {
			if intermediates[i] != v {
				t.Errorf("intermediates[%d] = %d, want %d", i, intermediates[i], v)
			}
		}
	})

	t.Run("unreachable pose returns error", func(t *testing.T) {
		_, _, err := ResolvePath(0, "nonexistent_pose")
		if err == nil {
			t.Error("expected error for unreachable pose, got nil")
		}
	})

	t.Run("resolving current pose is a no-op", func(t *testing.T) {
		// Already at home — no moves needed, no error.
		intermediates, finalIdx, err := ResolvePath(0, "home")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if finalIdx != 0 {
			t.Errorf("finalIdx = %d, want 0", finalIdx)
		}
		if len(intermediates) != 0 {
			t.Errorf("expected no intermediates, got %v", intermediates)
		}
	})
}
