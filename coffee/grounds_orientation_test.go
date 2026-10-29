package coffee

import (
	"context"
	"math"
	"testing"

	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/spatialmath"

	"beanjamin/coffee/order"
)

// groundsConstrained reports whether step's move would be planned under the
// grounds orientation constraint: only direct moves go through planToRawPose,
// where it is applied.
func groundsConstrained(s *beanjaminCoffee, step Step) bool {
	return isDirectMove(step) && s.groundsOrientationConstraint(componentFilter, step.LinearConstraint) != nil
}

// TestGroundsOrientationConstraintScope pins which moves get the bound: free
// moves of the filter while it holds grounds, and nothing else.
func TestGroundsOrientationConstraintScope(t *testing.T) {
	for _, tt := range []struct {
		name      string
		component string
		lc        *StepLinearConstraint
		planned   bool // groundsPlanned
		atArm     bool // portafilterHasGrounds
		want      bool
	}{
		{"free filter move, grounds planned", componentFilter, nil, true, false, true},
		{"free filter move, grounds at the arm", componentFilter, nil, false, true, true},
		{"free filter move, no grounds", componentFilter, nil, false, false, false},
		{"linear filter move keeps its own bound", componentFilter, defaultApproachConstraint, true, true, false},
		{"claws move while the filter holds grounds", componentClaws, nil, true, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &beanjaminCoffee{cfg: &Config{}}
			s.groundsPlanned.Store(tt.planned)
			s.portafilterHasGrounds.Store(tt.atArm)
			oc := s.groundsOrientationConstraint(tt.component, tt.lc)
			if got := oc != nil; got != tt.want {
				t.Fatalf("constrained = %v, want %v", got, tt.want)
			}
			if oc == nil {
				return
			}
			if oc.IgnoreTheta {
				t.Error("the filter's theta is its roll — the constraint must not ignore it")
			}
			if oc.OrientationToleranceDegs != defaultGroundsOrientationToleranceDegs {
				t.Errorf("tolerance = %v, want the default %v", oc.OrientationToleranceDegs, defaultGroundsOrientationToleranceDegs)
			}
		})
	}
}

func TestGroundsOrientationToleranceConfigurable(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{GroundsOrientationToleranceDegs: 12}}
	s.groundsPlanned.Store(true)
	if got := s.groundsOrientationConstraint(componentFilter, nil).OrientationToleranceDegs; got != 12 {
		t.Errorf("tolerance = %v, want the configured 12", got)
	}
}

// TestGroundsOrientationConstraintBySteps walks the free filter moves of a
// brew cycle with the grounds state each one is planned under, and pins which
// get the constraint: every free move from the tamper until the cleaning is
// done, but not the grinder approach, where the filter is still empty.
func TestGroundsOrientationConstraintBySteps(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	poses := grinderPoses(order.BeansRegular)

	type checked struct {
		pose string
		got  bool
	}
	var all []checked
	for _, step := range s.grindSteps(poses, grinderButtonCollisions(order.BeansRegular)) {
		if step.PoseName == poses.activate {
			s.groundsPlanned.Store(true) // as grindBeans does
		}
		all = append(all, checked{step.PoseName, groundsConstrained(s, step)})
	}
	for _, step := range append(append(s.tampSteps(), s.lockSteps()...), s.cleanSteps()...) {
		all = append(all, checked{step.PoseName, groundsConstrained(s, step)})
	}

	want := []checked{
		{filterPoseGrinderApproach, false}, // empty filter
		{filterPoseGrinderActivate, false}, // linear
		{filterPoseGrinderCollect, false},  // linear
		{filterPoseGrinderCollect, false},  // circle
		{filterPoseGrinderApproach, false}, // linear
		{filterPoseTamperApproach, true},   // loose grounds to the tamper
		{filterPoseTamperActivate, false},
		{filterPoseTamperApproach, false},
		{filterPoseCoffeeApproach, true}, // tamper to group head: the spill
		{filterPoseCoffeeIn, false},
		{filterPoseCoffeeLockedFinal, false}, // pivot
		{filterPoseCloseToCleaning, true},
		{filterPoseApproachToCleaningScrapper, true},
		{filterPoseCleaningScrapperActive, false},
		{filterPoseCleaningScrapperActive, false}, // circle
		{filterPoseApproachToCleaningScrapper, false},
		{filterPoseApproachToCleaningBrush, false},
		{filterPoseCleaningBrushActive, false},
		{filterPoseCleaningBrushActive, false}, // circle
		{filterPoseApproachToCleaningBrush, false},
		{filterPoseCloseToCleaning, true},
	}
	if len(all) != len(want) {
		t.Fatalf("walked %d steps, want %d: %v", len(all), len(want), all)
	}
	for i := range want {
		if all[i] != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, all[i], want[i])
		}
	}
}

// TestEndFlowReconcilesGroundsPlanned: planning marks the grounds before the
// arm gets there, so if the order fails first the planned flag must fall back
// to the arm's.
func TestEndFlowReconcilesGroundsPlanned(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	end := s.startFlow(context.Background(), context.Background())
	s.groundsPlanned.Store(true)
	_ = end()
	if s.groundsPlanned.Load() {
		t.Error("groundsPlanned should follow the arm (no grounds) once the flow ends")
	}
	if s.filterHoldsGrounds() {
		t.Error("filterHoldsGrounds should be false once the arm's flag is")
	}
}

// TestGroundsOrientationConstraintGeometry checks the RDK behaviour the
// constraint relies on, with the filter's taught orientations: a legitimate
// heading change between upright working poses stays inside the tube, and
// rolling or tipping the basket leaves it. It also shows why theta is not
// ignored: with IgnoreTheta the flip scores zero.
func TestGroundsOrientationConstraintGeometry(t *testing.T) {
	ov := func(x, y, z, theta float64) spatialmath.Orientation {
		return &spatialmath.OrientationVectorDegrees{OX: x, OY: y, OZ: z, Theta: theta}
	}
	tamper := ov(0, -1, 0.06, -180)
	coffeeIn := ov(0.67, -0.74, 0.04, -180)
	grinder := ov(-0.57, -0.82, -0.03, -179.3)

	oc := motionplan.OrientationConstraint{OrientationToleranceDegs: defaultGroundsOrientationToleranceDegs}
	free := motionplan.OrientationConstraint{OrientationToleranceDegs: defaultGroundsOrientationToleranceDegs, IgnoreTheta: true}

	// Halfway along each move's slerp: on the arc, so in the tube.
	mid := func(from, to spatialmath.Orientation) spatialmath.Orientation {
		return spatialmath.Interpolate(spatialmath.NewPoseFromOrientation(from), spatialmath.NewPoseFromOrientation(to), 0.5).Orientation()
	}
	for _, tt := range []struct {
		name     string
		from, to spatialmath.Orientation
	}{
		{"grinder to tamper", grinder, tamper},
		{"tamper to group head", tamper, coffeeIn},
	} {
		if moved := motionplan.OrientDist(tt.from, tt.to); moved < 30 {
			t.Errorf("%s reorients %.0f°; the case is meant to exceed the tolerance", tt.name, moved)
		}
		if d := oc.Distance(tt.from, tt.to, mid(tt.from, tt.to)); d > 1 {
			t.Errorf("%s: midpoint is %.1f° off the slerp, want ~0", tt.name, d)
		}
	}

	// The same heading as the tamper, rolled over about the handle: upside down.
	flipped := ov(0, -1, 0.06, 0)
	if d := oc.Distance(tamper, coffeeIn, flipped); d < 90 {
		t.Errorf("flipped basket scores %.1f°, want far outside the tube", d)
	}
	if d := free.Distance(tamper, coffeeIn, flipped); d > 1 {
		t.Errorf("with IgnoreTheta the flip scores %.1f°; expected ~0, which is why theta is kept", d)
	}

	// Tipped 30° about the horizontal axis across the handle: grounds slide.
	tilt := &spatialmath.R4AA{Theta: 30 * math.Pi / 180, RX: 1}
	tipped := spatialmath.Compose(spatialmath.NewPoseFromOrientation(tilt), spatialmath.NewPoseFromOrientation(tamper)).Orientation()
	if d := oc.Distance(tamper, coffeeIn, tipped); d <= defaultGroundsOrientationToleranceDegs {
		t.Errorf("basket tipped 30° scores %.1f°, want outside the %.0f° tube", d, defaultGroundsOrientationToleranceDegs)
	}
}
