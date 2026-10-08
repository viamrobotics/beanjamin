package coffee

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/testutils/inject"
)

// flowCoffee returns a service with a flow started, as during an order.
func flowCoffee(t *testing.T) (*beanjaminCoffee, func() error) {
	t.Helper()
	s, _ := newTestCoffee(t, nil)
	end := s.startFlow(context.Background(), context.Background())
	t.Cleanup(func() { _ = end() })
	return s, end
}

// failingGripper is a gripper whose Open fails with err.
func failingGripper(err error) *inject.Gripper {
	g := inject.NewGripper("g")
	g.OpenFunc = func(context.Context, map[string]any) error { return err }
	return g
}

// recorder records the moves that ran, by name.
type recorder struct {
	mu  sync.Mutex
	ran []string
}

func (r *recorder) move(name string) move {
	return move{name: name, bookkeep: func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ran = append(r.ran, name)
	}}
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ran)
}

// blocking returns a move that runs until release is closed.
func blocking(release chan struct{}) move {
	return move{bookkeep: func() { <-release }}
}

// Outside an order (execute_action, rewind, keepalive) there is no flow.
func TestHandOffRunsNowWithoutAFlow(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	var r recorder
	if err := s.handOff(context.Background(), r.move("a")); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("ran %v, want a to run immediately", got)
	}
}

func TestHandOffDoesNotWaitAndRunsInOrder(t *testing.T) {
	s, _ := flowCoffee(t)
	var r recorder
	release := make(chan struct{})
	ctx := context.Background()

	if err := s.handOff(ctx, blocking(release)); err != nil {
		t.Fatal(err)
	}
	// The planning thread is free while the blocking move runs.
	for _, name := range []string{"a", "b"} {
		if err := s.handOff(ctx, r.move(name)); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.got(); len(got) != 0 {
		t.Fatalf("ran %v before the blocking move finished", got)
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
	boom := errors.New("gripper faulted")
	s.gripper = failingGripper(boom)
	var r recorder
	ctx := context.Background()

	_ = s.openGripper(ctx)
	_ = s.handOff(ctx, r.move("b"))
	if err := s.settle(); !errors.Is(err, boom) {
		t.Fatalf("settle err = %v, want the gripper failure", err)
	}
	if got := r.got(); len(got) != 0 {
		t.Errorf("ran %v after the failure", got)
	}
	// The error is sticky: nothing more is handed off.
	if err := s.handOff(ctx, r.move("c")); !errors.Is(err, boom) {
		t.Errorf("handOff after a failure = %v, want the failure", err)
	}
	if got := r.got(); len(got) != 0 {
		t.Error("a move ran after the order failed")
	}
}

func TestExecutionPanicFailsTheOrder(t *testing.T) {
	s, _ := flowCoffee(t)
	var r recorder
	ctx := context.Background()
	_ = s.handOff(ctx, move{name: "boom", bookkeep: func() { panic("boom") }})
	_ = s.handOff(ctx, r.move("b"))
	if err := s.settle(); err == nil {
		t.Fatal("settle err = nil, want the panic as an error")
	}
	if got := r.got(); len(got) != 0 {
		t.Errorf("ran %v after the panic", got)
	}
}

func TestRunMoveOrder(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	var order []string
	g := inject.NewGripper("g")
	g.OpenFunc = func(context.Context, map[string]any) error { order = append(order, "gripper"); return nil }
	s.gripper = g

	m := move{bookkeep: func() { order = append(order, "bookkeep") }, gripper: openGripperAction, sleep: time.Millisecond}
	if err := s.runMove(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"bookkeep", "gripper"}) {
		t.Errorf("ran %v, want bookkeep then gripper", order)
	}
}

func TestSetStepFollowsTheArm(t *testing.T) {
	s, _ := flowCoffee(t)
	s.setStepNow(stepTamping)
	release := make(chan struct{})
	_ = s.handOff(context.Background(), blocking(release))

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
	_ = s.handOff(context.Background(), r.move("a"))
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
	s.gripper = failingGripper(boom)
	_ = s.openGripper(context.Background())
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
