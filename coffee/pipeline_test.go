package coffee

import "testing"

// TestStepPipelineable locks down which steps may have their plan computed
// ahead of time, while the previous plan is still executing. Pivots, circular
// motions, and the no-spill carry all read the arm's (or the held container's)
// actual pose before planning, so they must break the pipeline; everything else
// is a plain move that plans purely from its start inputs.
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

// TestPipelineRun checks how runSteps carves a sequence into pipelined runs and
// the standalone steps between them. The partition drives which moves overlap
// their planning, so an off-by-one here would either pipeline a step that must
// observe the arm or leave an idle gap that did not need to be there.
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

// TestPipelineRunPartitionsCleanPortafilter walks the real clean_portafilter
// sequence — the longest runSteps call in the brew cycle — to show the runs the
// two scrubbing motions carve it into, and that every step lands in exactly one
// of them.
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
