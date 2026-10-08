package coffee

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/referenceframe"
)

// flowCoffee returns a service with plan_ahead on and a flow started.
func flowCoffee(t *testing.T) (*beanjaminCoffee, func() error) {
	t.Helper()
	s, _ := newTestCoffee(t, &Config{PlanAhead: true})
	end := s.startFlow(context.Background(), context.Background())
	t.Cleanup(func() { _ = end() })
	return s, end
}

// recorder collects the order actions ran in.
type recorder struct {
	mu  sync.Mutex
	ran []string
}

func (r *recorder) action(name string, err error) func(context.Context) error {
	return func(context.Context) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ran = append(r.ran, name)
		return err
	}
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ran)
}

func TestHandOffRunsNowWithoutAFlow(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	end := s.startFlow(context.Background(), context.Background())
	if s.flow != nil {
		t.Fatal("plan_ahead off must not start a flow")
	}
	var r recorder
	if err := s.handOff(context.Background(), "a", r.action("a", nil)); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("ran %v, want a to run immediately", got)
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
}

func TestHandOffDoesNotWaitAndRunsInOrder(t *testing.T) {
	s, _ := flowCoffee(t)
	var r recorder
	release := make(chan struct{})
	slow := func(context.Context) error { <-release; return nil }

	if err := s.handOff(context.Background(), "slow", slow); err != nil {
		t.Fatal(err)
	}
	// The planning thread is free while the slow action runs.
	for _, name := range []string{"a", "b"} {
		if err := s.handOff(context.Background(), name, r.action(name, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.got(); len(got) != 0 {
		t.Fatalf("ran %v before the slow action finished", got)
	}
	close(release)
	if err := s.settle(); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("ran %v, want a then b", got)
	}
}

func TestFailureSkipsTheRest(t *testing.T) {
	s, _ := flowCoffee(t)
	var r recorder
	boom := errors.New("arm faulted")
	ctx := context.Background()
	_ = s.handOff(ctx, "a", r.action("a", boom))
	_ = s.handOff(ctx, "b", r.action("b", nil))

	err := s.settle()
	if !errors.Is(err, boom) {
		t.Fatalf("settle err = %v, want the failure", err)
	}
	if got := r.got(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("ran %v, want only a", got)
	}
	// The error is sticky: nothing more is queued.
	if err := s.handOff(ctx, "c", r.action("c", nil)); !errors.Is(err, boom) {
		t.Errorf("handOff after a failure = %v, want the failure", err)
	}
	if got := r.got(); slices.Contains(got, "c") {
		t.Error("an action ran after the order failed")
	}
}

func TestSetStepFollowsTheArm(t *testing.T) {
	s, _ := flowCoffee(t)
	s.setStepNow(stepTamping)
	release := make(chan struct{})
	_ = s.handOff(context.Background(), "move", func(context.Context) error { <-release; return nil })

	s.setStep(stepLockingPortafilter)
	if got, _ := s.currentStep.Load().(string); got != stepTamping {
		t.Errorf("step = %q while the arm is still moving, want %q", got, stepTamping)
	}
	close(release)
	if err := s.settle(); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.currentStep.Load().(string); got != stepLockingPortafilter {
		t.Errorf("step = %q after the move, want %q", got, stepLockingPortafilter)
	}
}

func TestSyncRegionRunsNow(t *testing.T) {
	s, _ := flowCoffee(t)
	var r recorder
	release, err := s.syncRegion()
	if err != nil {
		t.Fatal(err)
	}
	_ = s.handOff(context.Background(), "a", r.action("a", nil))
	if got := r.got(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("ran %v, want a to run immediately inside the region", got)
	}
	release()
	if s.activeFlow() == nil {
		t.Error("the flow should be active again after the region")
	}
}

func TestEndFlowReportsAQueuedFailure(t *testing.T) {
	s, end := flowCoffee(t)
	boom := errors.New("gripper faulted")
	_ = s.handOff(context.Background(), "open gripper", func(context.Context) error {
		time.Sleep(10 * time.Millisecond)
		return boom
	})
	if err := end(); !errors.Is(err, boom) {
		t.Fatalf("end = %v, want the queued failure", err)
	}
	if s.flow != nil {
		t.Error("end should clear the flow")
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

func TestIsDirectMove(t *testing.T) {
	for _, tc := range []struct {
		step Step
		want bool
	}{
		{Step{PoseName: "a"}, true},
		{Step{PoseName: "a", LinearConstraint: defaultApproachConstraint}, true},
		{Step{PoseName: "a", CircularRadiusMm: 3}, false},
		{Step{PoseName: "a", PivotFromPose: "b"}, false},
		{Step{PoseName: "a", NoSpill: true}, false},
	} {
		if got := isDirectMove(tc.step); got != tc.want {
			t.Errorf("isDirectMove(%+v) = %v, want %v", tc.step, got, tc.want)
		}
	}
}
