package coffee

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/referenceframe"
)

// fakePlan stands in for a real plan; the tests only care which step it is for.
type fakePlan struct {
	motionplan.Plan
	pose string
}

// fakeSegment is a fake arm and planner for runSegmentWith. Each step moves the
// one joint of the fake arm by 1, so the plan for step k starts at k and ends at
// k+1.
type fakeSegment struct {
	mu        sync.Mutex
	arm       float64
	plannedAt map[string]float64 // pose -> joint value the plan started from
	planned   []string           // poses run with a plan made ahead
	live      []string           // poses run live
	planErr   map[string]error   // pose -> planning error
	armOffset float64            // added to armNow, to fake an arm off its plan
}

func newFakeSegment() *fakeSegment {
	return &fakeSegment{plannedAt: map[string]float64{}, planErr: map[string]error{}}
}

func (f *fakeSegment) ops() segmentOps {
	return segmentOps{
		plan: func(_ context.Context, step Step, from []referenceframe.Input) (motionplan.Plan, []referenceframe.Input, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.planErr[step.PoseName]; err != nil {
				return nil, nil, err
			}
			f.plannedAt[step.PoseName] = from[0]
			return fakePlan{pose: step.PoseName}, []referenceframe.Input{from[0] + 1}, nil
		},
		armNow: func(context.Context) ([]referenceframe.Input, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return []referenceframe.Input{f.arm + f.armOffset}, nil
		},
		runPlanned: func(_ context.Context, step Step, plan motionplan.Plan) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if plan.(fakePlan).pose != step.PoseName {
				return errors.New("ran a plan made for another step")
			}
			f.planned = append(f.planned, step.PoseName)
			f.arm++
			return nil
		},
		runLive: func(_ context.Context, step Step) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.live = append(f.live, step.PoseName)
			f.arm++
			return nil
		},
	}
}

func poses(names ...string) []Step {
	steps := make([]Step, len(names))
	for i, n := range names {
		steps[i] = Step{PoseName: n}
	}
	return steps
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSplitIntoRuns: consecutive direct moves group into a segment; a pivot, a
// circular motion or a no-spill carry stands alone and ends the segment.
func TestSplitIntoRuns(t *testing.T) {
	steps := []Step{
		{PoseName: "a"},
		{PoseName: "b", LinearConstraint: defaultApproachConstraint},
		{PoseName: "scrub", CircularRadiusMm: 3},
		{PoseName: "c"},
		{PoseName: "twist", PivotFromPose: "c"},
		{PoseName: "carry", NoSpill: true},
		{PoseName: "d"},
		{PoseName: "e"},
	}
	runs := splitIntoRuns(steps)
	type want struct {
		names   []string
		segment bool
	}
	wants := []want{
		{[]string{"a", "b"}, true},
		{[]string{"scrub"}, false},
		{[]string{"c"}, true},
		{[]string{"twist"}, false},
		{[]string{"carry"}, false},
		{[]string{"d", "e"}, true},
	}
	if len(runs) != len(wants) {
		t.Fatalf("got %d runs, want %d: %+v", len(runs), len(wants), runs)
	}
	for i, w := range wants {
		var got []string
		for _, s := range runs[i].steps {
			got = append(got, s.PoseName)
		}
		if !equalStrings(got, w.names) || runs[i].segment != w.segment {
			t.Errorf("run %d = %v segment=%v, want %v segment=%v", i, got, runs[i].segment, w.names, w.segment)
		}
	}
}

// TestRunSegmentChainsPlans: every step runs with its plan made ahead, and each
// plan starts where the previous one ends.
func TestRunSegmentChainsPlans(t *testing.T) {
	f := newFakeSegment()
	if err := runSegmentWith(context.Background(), poses("a", "b", "c"), []referenceframe.Input{0}, f.ops()); err != nil {
		t.Fatalf("runSegmentWith: %v", err)
	}
	if !equalStrings(f.planned, []string{"a", "b", "c"}) || len(f.live) != 0 {
		t.Fatalf("planned=%v live=%v, want every step planned ahead", f.planned, f.live)
	}
	for i, p := range []string{"a", "b", "c"} {
		if got := f.plannedAt[p]; got != float64(i) {
			t.Errorf("plan for %q started at %v, want %d (the previous plan's end)", p, got, i)
		}
	}
}

// TestRunSegmentPlansWhileTheArmMoves: the next step is planned while the
// current one is still executing, which is the whole point.
func TestRunSegmentPlansWhileTheArmMoves(t *testing.T) {
	f := newFakeSegment()
	ops := f.ops()
	plannedB := make(chan struct{})
	basePlan := ops.plan
	ops.plan = func(ctx context.Context, step Step, from []referenceframe.Input) (motionplan.Plan, []referenceframe.Input, error) {
		plan, end, err := basePlan(ctx, step, from)
		if step.PoseName == "b" {
			close(plannedB)
		}
		return plan, end, err
	}
	baseRun := ops.runPlanned
	ops.runPlanned = func(ctx context.Context, step Step, plan motionplan.Plan) error {
		if step.PoseName == "a" {
			// Step a "moves" until b has been planned. Without planning ahead,
			// b would only be planned after a returns, and this would time out.
			select {
			case <-plannedB:
			case <-time.After(time.Second):
				return errors.New("b was not planned while a was executing")
			}
		}
		return baseRun(ctx, step, plan)
	}
	if err := runSegmentWith(context.Background(), poses("a", "b"), []referenceframe.Input{0}, ops); err != nil {
		t.Fatal(err)
	}
}

// TestRunSegmentPlansLiveWhenTheArmIsElsewhere: a plan made ahead is only used
// if the arm is where it starts; otherwise the step is planned on the spot.
func TestRunSegmentPlansLiveWhenTheArmIsElsewhere(t *testing.T) {
	f := newFakeSegment()
	f.armOffset = 0.5 // far past planAheadStartToleranceRad
	if err := runSegmentWith(context.Background(), poses("a", "b"), []referenceframe.Input{0}, f.ops()); err != nil {
		t.Fatal(err)
	}
	if len(f.planned) != 0 || !equalStrings(f.live, []string{"a", "b"}) {
		t.Errorf("planned=%v live=%v, want both steps run live", f.planned, f.live)
	}
}

// TestRunSegmentPlansLiveAfterAPlanningFailure: when planning ahead fails, that
// step and the rest of the segment are planned on the spot, where a real
// failure is reported as it is today.
func TestRunSegmentPlansLiveAfterAPlanningFailure(t *testing.T) {
	f := newFakeSegment()
	f.planErr["b"] = errMotionPlanning
	if err := runSegmentWith(context.Background(), poses("a", "b", "c"), []referenceframe.Input{0}, f.ops()); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(f.planned, []string{"a"}) || !equalStrings(f.live, []string{"b", "c"}) {
		t.Errorf("planned=%v live=%v, want a ahead and b, c live", f.planned, f.live)
	}
}

// TestRunSegmentStopsPlanningWhenAStepFails: a failed move ends the segment, and
// runSegmentWith returns only once the planning goroutine has exited, so nothing
// is still reading the frame system when the caller moves on.
func TestRunSegmentStopsPlanningWhenAStepFails(t *testing.T) {
	f := newFakeSegment()
	ops := f.ops()
	var plannerExited atomic.Bool
	basePlan := ops.plan
	ops.plan = func(ctx context.Context, step Step, from []referenceframe.Input) (motionplan.Plan, []referenceframe.Input, error) {
		if step.PoseName == "c" {
			// A slow plan: blocks until the segment gives up on it.
			<-ctx.Done()
			plannerExited.Store(true)
			return nil, nil, ctx.Err()
		}
		return basePlan(ctx, step, from)
	}
	moveErr := errors.New("arm faulted")
	ops.runPlanned = func(_ context.Context, step Step, _ motionplan.Plan) error {
		if step.PoseName == "b" {
			return moveErr
		}
		f.arm++
		return nil
	}
	err := runSegmentWith(context.Background(), poses("a", "b", "c"), []referenceframe.Input{0}, ops)
	if !errors.Is(err, moveErr) {
		t.Fatalf("err = %v, want the move failure", err)
	}
	if !plannerExited.Load() {
		t.Error("runSegmentWith returned while the planner was still running")
	}
}

// TestRunSegmentStopsOnCancel: a cancel ends the segment between steps and stops
// the planning goroutine.
func TestRunSegmentStopsOnCancel(t *testing.T) {
	f := newFakeSegment()
	ops := f.ops()
	ctx, cancel := context.WithCancel(context.Background())
	baseRun := ops.runPlanned
	ops.runPlanned = func(c context.Context, step Step, plan motionplan.Plan) error {
		if step.PoseName == "a" {
			cancel()
		}
		return baseRun(c, step, plan)
	}
	err := runSegmentWith(ctx, poses("a", "b", "c"), []referenceframe.Input{0}, ops)
	if err == nil {
		t.Fatal("want a cancellation error")
	}
	if len(f.planned)+len(f.live) > 2 {
		t.Errorf("planned=%v live=%v; the cancel should stop the segment", f.planned, f.live)
	}
}

// TestArmNear checks the per-joint start tolerance.
func TestArmNear(t *testing.T) {
	want := []referenceframe.Input{0, 1, 2}
	if !armNear([]referenceframe.Input{0.005, 1, 1.995}, want) {
		t.Error("within tolerance should be near")
	}
	if armNear([]referenceframe.Input{0.05, 1, 2}, want) {
		t.Error("one joint past tolerance should not be near")
	}
	if armNear([]referenceframe.Input{0, 1}, want) {
		t.Error("a different joint count should not be near")
	}
}
