package coffee

import (
	"context"
	"errors"
	"testing"

	"beanjamin/coffee/order"

	"go.viam.com/rdk/testutils/inject"
)

// faultingGripper is a gripper whose position read always fails, so
// prepareDrink faults at normalizeGripperAtStart — before any motion — with
// whatever world state the test recorded still in place.
func faultingGripper() *inject.Gripper {
	g := inject.NewGripper("g")
	g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
		return nil, errors.New("gripper unreachable")
	}
	return g
}

// TestFaultWithStrandedStatePausesQueue pins that a genuine fault leaving the
// machine mid-cycle holds the next order back for rewind → proceed, instead of
// letting it start from the stranded state.
func TestFaultWithStrandedStatePausesQueue(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, nil)
	s.portafilterInMachine.Store(true)
	s.gripper = faultingGripper()
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())

	if err := s.prepareDrink(context.Background(), order.NewOrder("espresso", "Alice", "", "")); err == nil {
		t.Fatal("prepareDrink should fail on the unreadable gripper")
	}
	if !s.paused.Load() {
		t.Error("a fault that stranded state must pause the queue")
	}
	if s.running.Load() {
		t.Error("running must be released after the fault")
	}
}

// TestFaultWithCleanWorldPausesQueue covers a fault with no mid-cycle state
// recorded: the queue still pauses, because the recorded state does not show
// everything a fault can leave behind.
func TestFaultWithCleanWorldPausesQueue(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, nil)
	s.heldItemAttached = false
	s.filterFrameLocked = false
	s.stagedGlassPlaced = false
	s.gripper = faultingGripper()
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())

	if err := s.prepareDrink(context.Background(), order.NewOrder("espresso", "Alice", "", "")); err == nil {
		t.Fatal("prepareDrink should fail on the unreadable gripper")
	}
	if !s.paused.Load() {
		t.Error("a fault must pause the queue even with no mid-cycle state recorded")
	}
}

// TestOperatorCancelLeavesPauseToCancel pins that an interrupted order is not
// treated as a fault: the pause belongs to the cancel (or reset_world, which
// releases it), so the fault path must not raise one of its own.
func TestOperatorCancelLeavesPauseToCancel(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, nil)
	s.portafilterHasGrounds.Store(true)
	s.gripper = faultingGripper()
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())
	s.cancelFunc()

	if err := s.prepareDrink(context.Background(), order.NewOrder("espresso", "Alice", "", "")); err == nil {
		t.Fatal("prepareDrink should fail on the unreadable gripper")
	}
	if s.paused.Load() {
		t.Error("the fault path must leave an operator-cancelled order's pause to the cancel")
	}
}

// TestStrandedStateNamesEachFlag checks that each piece of mid-cycle state is
// named in the fault log on its own, and that a clean machine names nothing.
func TestStrandedStateNamesEachFlag(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *beanjaminCoffee)
		want  string
	}{
		{name: "clean", setup: func(*beanjaminCoffee) {}},
		{name: "portafilter in machine", setup: func(s *beanjaminCoffee) { s.portafilterInMachine.Store(true) }, want: "portafilter in machine"},
		{name: "grounds in portafilter", setup: func(s *beanjaminCoffee) { s.portafilterHasGrounds.Store(true) }, want: "grounds in portafilter"},
		{name: "filter frame locked", setup: func(s *beanjaminCoffee) { s.filterFrameLocked = true }, want: "filter frame locked"},
		{name: "item in gripper", setup: func(s *beanjaminCoffee) { s.heldItemAttached = true }, want: "item in gripper"},
		{name: "glass staged", setup: func(s *beanjaminCoffee) { s.stagedGlassPlaced = true }, want: "glass staged"},
		{name: "fridge door open", setup: func(s *beanjaminCoffee) { s.doorOpenDegs = 90 }, want: "fridge door open 90°"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestCoffee(t, nil)
			tc.setup(s)
			got := s.strandedState()
			if tc.want == "" {
				if len(got) != 0 {
					t.Errorf("strandedState() = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("strandedState() = %v, want [%s]", got, tc.want)
			}
		})
	}
}
