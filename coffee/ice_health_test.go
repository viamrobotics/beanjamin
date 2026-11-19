package coffee

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/testutils/inject"
)

// iceBoardAnswering is an ice board whose pin reads fail with readErr, or
// succeed when it is nil. Its pin panics if set: a probe must never drive it.
func iceBoardAnswering(readErr error) *inject.Board {
	pin := &inject.GPIOPin{}
	pin.GetFunc = func(context.Context, map[string]interface{}) (bool, error) { return false, readErr }
	pin.SetFunc = func(context.Context, bool, map[string]interface{}) error {
		panic("the ice probe drove the ice pin")
	}
	brd := inject.NewBoard("ice-board")
	brd.GPIOPinByNameFunc = func(string) (board.GPIOPin, error) { return pin, nil }
	return brd
}

// errRemoteDown is the remote's failure when the ice machine is off the network.
var errRemoteDown = errors.New("rpc error: code = Unknown desc = not connected")

func icedTestCoffee(t *testing.T, readErr error) (*beanjaminCoffee, *fakeSpeech) {
	t.Helper()
	c, sp := newTestCoffee(t, &Config{CanServeIced: true, IceDispensePinName: "11", Conversational: true})
	c.iceBoard = iceBoardAnswering(readErr)
	return c, sp
}

func TestEnqueueOrder_RefusesIcedWhileIceMachineDown(t *testing.T) {
	c, sp := icedTestCoffee(t, errRemoteDown)
	_, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "iced_coffee", "customer_name": "Jo"})
	if !errors.Is(err, errIceUnavailable) || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("err = %v, want errIceUnavailable carrying the remote's failure", err)
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue length = %d, want the iced order refused", c.queue.Len())
	}
	if calls := sp.calls(); len(calls) != 1 || !strings.Contains(calls[0], "ice machine") {
		t.Errorf("spoken = %v, want the ice-machine apology", calls)
	}

	// A hot drink does not need the ice machine.
	if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "espresso", "customer_name": "Jo"}); err != nil {
		t.Errorf("espresso refused while the ice machine is down: %v", err)
	}
}

func TestEnqueueOrder_AcceptsIcedOnceIceMachineAnswers(t *testing.T) {
	c, _ := icedTestCoffee(t, errRemoteDown)
	if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "iced_coffee"}); err == nil {
		t.Fatal("iced order accepted while the ice machine is down")
	}
	c.iceBoard = iceBoardAnswering(nil)
	if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "iced_coffee"}); err != nil {
		t.Fatalf("iced order refused after the ice machine came back: %v", err)
	}
}

func TestStatusReportsIceAvailability(t *testing.T) {
	c, _ := icedTestCoffee(t, errRemoteDown)
	ctx := context.Background()

	st, _ := c.Status(ctx)
	if st["iced_available"] != false || st["iced_unavailable_reason"] == "" {
		t.Errorf("before any probe: iced_available=%v reason=%q, want unavailable with a reason",
			st["iced_available"], st["iced_unavailable_reason"])
	}

	c.probeIce(ctx)
	st, _ = c.Status(ctx)
	reason, _ := st["iced_unavailable_reason"].(string)
	if st["iced_available"] != false || !strings.Contains(reason, "not connected") {
		t.Errorf("ice machine down: iced_available=%v reason=%q, want unavailable naming the failure", st["iced_available"], reason)
	}

	c.iceBoard = iceBoardAnswering(nil)
	c.probeIce(ctx)
	st, _ = c.Status(ctx)
	if st["iced_available"] != true || st["iced_unavailable_reason"] != "" {
		t.Errorf("ice machine back: iced_available=%v reason=%q, want available", st["iced_available"], st["iced_unavailable_reason"])
	}
}

func TestStatusOmitsIceAvailabilityWithoutIcedDrinks(t *testing.T) {
	c, _ := newTestCoffee(t, &Config{})
	st, _ := c.Status(context.Background())
	if _, ok := st["iced_available"]; ok {
		t.Error("iced_available reported on a machine that serves no iced drinks")
	}
}

// An iced order queued before the ice machine dropped is refused before the
// brew starts, so it strands nothing and does not pause the queue.
func TestQueuedIcedOrderRefusedBeforeStarting(t *testing.T) {
	c, _ := icedTestCoffee(t, nil)
	if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "iced_coffee"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	o, ok := c.queue.Start()
	if !ok {
		t.Fatal("no order queued")
	}
	c.iceBoard = iceBoardAnswering(errRemoteDown)
	err := c.executeQueuedOrder(context.Background(), context.Background(), o)
	if !errors.Is(err, errIceUnavailable) {
		t.Fatalf("err = %v, want errIceUnavailable", err)
	}
	if c.lease.isPaused() {
		t.Error("refusing an order that never started paused the queue")
	}
}

// A move failure surfaces at the planner's next hand-off, wrapped in the
// context of whatever step the planner reached; the order reports the failure
// itself.
func TestExecutionCauseDropsThePlannersLaterContext(t *testing.T) {
	s, _ := runCoffee(t)
	dispense := errors.New("dispense_ice: pulse_ice_pin: set pin \"11\" high: not connected")
	ctx := context.Background()

	_ = s.handOff(ctx, move{name: "dispense", run: func(context.Context) error { return dispense }})
	err := s.settle()
	planned := fmt.Errorf("place_in_dirty-container-area: open gripper to release cup: %w", err)

	if got := executionCause(planned); got != dispense {
		t.Errorf("executionCause = %v, want the dispense failure alone", got)
	}
	planning := errors.New("motion planning failed")
	if got := executionCause(planning); got != planning {
		t.Errorf("executionCause changed a planner-side error: %v", got)
	}
}
