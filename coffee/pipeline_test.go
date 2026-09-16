package coffee

import (
	"strings"
	"testing"
)

// Pivots, circular motions and the no-spill carry read the arm's live pose, so
// they cannot be planned ahead. Everything else can.
func TestStepPipelineable(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}

	tests := []struct {
		name string
		step Step
		want bool
	}{
		{"plain move", Step{PoseName: "home"}, true},
		{"linear constraint", Step{PoseName: "coffee_in", LinearConstraint: defaultApproachConstraint}, true},
		{"allowed collisions", Step{PoseName: "coffee_in", AllowedCollisions: coffeeBrewingCollisions}, true},
		{"with pause", Step{PoseName: "tamper_approach", Pause: shortPause}, true},
		{"slow move options", Step{PoseName: "tamper_approach", MoveOptions: &StepMoveOptions{MaxVelDegsPerSec: 10}}, true},
		{"pivot", Step{PoseName: "locked_final", PivotFromPose: "coffee_in"}, false},
		{"pivot with overshoot", Step{PoseName: "locked_final", PivotFromPose: "coffee_in", PivotExtraDegrees: 4}, false},
		{"circular", Step{PoseName: "grinder", CircularRadiusMm: 3, CircularDurationSec: 2.5}, false},
		{"no-spill carry", Step{PoseName: "serving", NoSpill: true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.stepPipelineable(tc.step); got != tc.want {
				t.Errorf("stepPipelineable = %v, want %v", got, tc.want)
			}
		})
	}
}

// The partition decides which moves overlap their planning. An off-by-one would
// either pipeline a step that needs the live pose or leave a needless idle gap.
func TestPipelineRun(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	move := func(name string) Step { return Step{PoseName: name} }
	circle := func(name string) Step { return Step{PoseName: name, CircularRadiusMm: 3} }
	pivot := func(name string) Step { return Step{PoseName: name, PivotFromPose: "coffee_in"} }

	tests := []struct {
		name  string
		steps []Step
		want  int
	}{
		{"empty", nil, 0},
		{"single move", []Step{move("home")}, 1},
		{"tamp_ground's three moves", []Step{move("approach"), move("activate"), move("approach")}, 3},
		{"stops at the circular motion", []Step{move("a"), move("b"), circle("b"), move("a")}, 2},
		{"leads with a pivot", []Step{pivot("locked"), move("approach")}, 0},
		{"single trailing move", []Step{move("close_to_cleaning")}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.pipelineRun(tc.steps); got != tc.want {
				t.Errorf("pipelineRun = %d, want %d", got, tc.want)
			}
		})
	}
}

// clean_portafilter is the longest sequence in the brew cycle. Check how its two
// scrubs split it, and that every step lands in exactly one run.
func TestPipelineRunPartitionsCleanPortafilter(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	steps := []Step{
		{PoseName: filterPoseCloseToCleaning},
		{PoseName: filterPoseApproachToCleaningScrapper},
		{PoseName: filterPoseCleaningScrapperActive, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseCleaningScrapperActive, CircularRadiusMm: 3},
		{PoseName: filterPoseApproachToCleaningScrapper, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseApproachToCleaningBrush, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseCleaningBrushActive, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseCleaningBrushActive, CircularRadiusMm: 3},
		{PoseName: filterPoseApproachToCleaningBrush, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseCloseToCleaning},
	}

	var runs []int
	var standalone, covered int
	for rest := steps; len(rest) > 0; {
		if n := s.pipelineRun(rest); n > 0 {
			runs = append(runs, n)
			covered += n
			rest = rest[n:]
			continue
		}
		standalone++
		covered++
		rest = rest[1:]
	}

	if want := []int{3, 3, 2}; len(runs) != len(want) {
		t.Fatalf("runs = %v, want %v", runs, want)
	} else {
		for i := range want {
			if runs[i] != want[i] {
				t.Fatalf("runs = %v, want %v", runs, want)
			}
		}
	}
	if standalone != 2 {
		t.Errorf("standalone steps = %d, want 2 (the two circular scrubs)", standalone)
	}
	if covered != len(steps) {
		t.Errorf("partition covered %d steps, want all %d", covered, len(steps))
	}
	// Each run of n hides n-1 plans behind a move already underway.
	hidden := 0
	for _, n := range runs {
		hidden += n - 1
	}
	if hidden != 5 {
		t.Errorf("plans hidden behind execution = %d, want 5", hidden)
	}
}

// planStepMove builds a direct plan. Given a step that needs a different motion —
// above all a NoSpill carry — it must fail rather than plan the wrong one.
func TestPlanStepMoveRefusesNonPipelineable(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	for _, step := range []Step{
		{PoseName: "serving", NoSpill: true},
		{PoseName: "locked_final", PivotFromPose: "coffee_in"},
		{PoseName: "grinder", CircularRadiusMm: 3},
	} {
		// A nil pose switch would make fetchPose fail too, so the refusal has to
		// come first for this to prove anything.
		_, err := s.planStepMove(t.Context(), nil, nil, step)
		if err == nil {
			t.Fatalf("planStepMove(%+v) succeeded, want refusal", step)
		}
		if !strings.Contains(err.Error(), "planned on arrival") {
			t.Errorf("planStepMove(%+v) = %v, want the on-arrival refusal", step, err)
		}
	}
}
