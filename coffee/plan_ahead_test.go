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

// fakePlan records which step it was planned for.
type fakePlan struct {
	motionplan.Plan
	pose string
}

// fakeSegment is a one-joint fake arm and planner; each move advances it by 1.
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

func TestSegmentEnd(t *testing.T) {
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
	for _, tc := range []struct{ from, want int }{
		{0, 2}, // a, b
		{2, 2}, // scrub is not a direct move
		{3, 4}, // c alone, ended by the pivot
		{4, 4}, // pivot
		{5, 5}, // no-spill carry
		{6, 8}, // d, e to the end
	} {
		if got := segmentEnd(steps, tc.from); got != tc.want {
			t.Errorf("segmentEnd(steps, %d) = %d, want %d", tc.from, got, tc.want)
		}
	}
}

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
			// Without planning ahead, b isn't planned until a returns.
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

// TestRunSegmentStopsPlanningWhenAStepFails also checks the planner has exited
// before runSegmentWith returns.
func TestRunSegmentStopsPlanningWhenAStepFails(t *testing.T) {
	f := newFakeSegment()
	ops := f.ops()
	var plannerExited atomic.Bool
	basePlan := ops.plan
	ops.plan = func(ctx context.Context, step Step, from []referenceframe.Input) (motionplan.Plan, []referenceframe.Input, error) {
		if step.PoseName == "c" {
			// A slow plan that only ends when cancelled.
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
