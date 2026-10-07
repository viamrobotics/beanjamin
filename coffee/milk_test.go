package coffee

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/testutils/inject"

	"beanjamin/coffee/geom"
)

// Returning with no milk out must fail rather than drive an empty gripper
// at a shelf where the bottle may already stand.
func TestReturnMilkBottleWithoutPickupFails(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{CanServeIcedLatte: true}, gripper: inject.NewGripper("g")}
	err := s.returnMilkBottle(context.Background(), context.Background())
	if err == nil || !strings.Contains(err.Error(), "no milk is out") {
		t.Fatalf("expected a no-milk-out error, got %v", err)
	}
}

// Every milk action refuses to run on a machine that isn't configured for milk,
// rather than failing partway through a motion on nil config.
func TestMilkActionsRequireConfiguredMilk(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	// Through the registered actions, so the wrappers that read execute_action's
	// "milk" are covered too.
	registered := s.actionFuncs()
	for _, name := range []string{"add_milk", "fetch_milk", "pour_milk", "return_milk", "serve_iced_latte"} {
		run := registered[name]
		t.Run(name, func(t *testing.T) {
			err := run(context.Background(), context.Background())
			if err == nil || !strings.Contains(err.Error(), "can_serve_iced_latte") {
				t.Fatalf("expected a can_serve_iced_latte error, got %v", err)
			}
		})
	}
}

// The milk actions are on the execute_action surface so each bottle's poses can
// be stepped through one at a time on hardware.
func TestMilkActionsRegistered(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	actions := s.actionFuncs()
	for _, name := range []string{"fetch_milk", "pour_milk", "return_milk", "add_milk", "serve_iced_latte"} {
		if _, ok := actions[name]; !ok {
			t.Errorf("execute_action %q not registered", name)
		}
	}
}

// A milk bottle in the gripper must not overwrite the cup or glass geometry: an
// iced latte carries all three within one order, and a re-grab restores the
// cached shape by label.
func TestHeldGeometryCachePerLabel(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	cup, err := geom.ContainerBox(r3.Vector{}, (&ContainerDimensions{DiameterMm: 80, HeightMm: 95}).boxDims(), pickupLabelCup)
	if err != nil {
		t.Fatal(err)
	}
	milk, err := geom.ContainerBox(r3.Vector{}, (&ContainerDimensions{DiameterMm: 90, HeightMm: 250}).boxDims(), pickupLabelMilk)
	if err != nil {
		t.Fatal(err)
	}
	s.cacheHeldGeometry(pickupLabelCup, cup)
	s.cacheHeldGeometry(pickupLabelMilk, milk)

	if got := s.cachedHeldGeometry(pickupLabelCup); got != cup {
		t.Error("caching milk geometry clobbered the cup's")
	}
	if got := s.cachedHeldGeometry(pickupLabelMilk); got != milk {
		t.Error("milk geometry did not round-trip through the cache")
	}
}

// A frame-system reset forgets which milk is out along with the cached geometry
// — it describes a bottle the gripper is no longer known to hold.
func TestClearHeldGeometryForgetsHeldMilk(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}, heldMilk: "oat"}
	s.clearHeldGeometry()
	if s.heldMilk != "" {
		t.Errorf("heldMilk = %q, want empty after clearHeldGeometry", s.heldMilk)
	}
}

// A hand-run milk action takes the milk execute_action names, defaults to the
// first configured milk, and refuses one that isn't configured.
func TestActionMilk(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{CanServeIcedLatte: true, MilkOptions: testMilks}}
	// Required by hand, unlike on an order: the error lists the milks to pick from.
	if _, err := s.actionMilk(context.Background(), "fetch_milk"); err == nil || !strings.Contains(err.Error(), "whole, oat") {
		t.Errorf("actionMilk with no milk = %v, want an error listing whole, oat", err)
	}
	if got, err := s.actionMilk(withMilkChoice(context.Background(), "oat"), "fetch_milk"); err != nil || got != "oat" {
		t.Errorf("actionMilk(oat) = %q, %v; want oat", got, err)
	}
	if _, err := s.actionMilk(withMilkChoice(context.Background(), "soy"), "fetch_milk"); err == nil {
		t.Error("actionMilk(soy) should fail for an unconfigured milk")
	}
}

// testMilks is a two-milk fridge: whole on the left, oat on the right.
var testMilks = []MilkOption{{Name: "whole", Spot: "left"}, {Name: "oat", Spot: "right"}}

// Each milk is looked up to the spot it stands at; a milk that isn't configured
// has no spot.
func TestMilkSpot(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{MilkOptions: testMilks}}
	if spot, err := s.milkSpot("oat"); err != nil || spot != "right" {
		t.Errorf("milkSpot(oat) = %q, %v; want right", spot, err)
	}
	if _, err := s.milkSpot("soy"); err == nil {
		t.Error("milkSpot(soy) should fail for an unconfigured milk")
	}
}

// Fetching a second bottle while one is out would leave nothing to say which
// spot the first goes back to.
func TestFetchMilkWhileOneIsOutFails(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{CanServeIcedLatte: true}, gripper: inject.NewGripper("g"), heldMilk: "whole"}
	err := s.fetchMilkBottle(context.Background(), context.Background(), "oat")
	if err == nil || !strings.Contains(err.Error(), "already out") {
		t.Fatalf("expected an already-out error, got %v", err)
	}
}

// "milk" only goes to the actions that pick a bottle; elsewhere it is refused
// rather than silently ignored.
func TestCheckMilkChoiceAllowed(t *testing.T) {
	for _, name := range []string{"fetch_milk", "add_milk", "serve_iced_latte"} {
		if err := checkMilkChoiceAllowed(name); err != nil {
			t.Errorf("checkMilkChoiceAllowed(%q) = %v, want allowed", name, err)
		}
	}
	for _, name := range []string{"return_milk", "pour_milk", "grind_coffee"} {
		if err := checkMilkChoiceAllowed(name); err == nil {
			t.Errorf("checkMilkChoiceAllowed(%q) = nil, want refused", name)
		}
	}
}

// A bottle out of the fridge keeps the frame system from being rebuilt between
// hand-run actions, even when its geometry never attached: the rebuild would
// clear heldMilk and return_milk would no longer know where it goes.
func TestRefreshFrameSystemSkippedWhileMilkIsOut(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}, heldMilk: "oat"}
	// No frame system service is configured, so a rebuild attempt would fail.
	if err := s.refreshFrameSystemIfClean(context.Background()); err != nil {
		t.Fatalf("refreshFrameSystemIfClean = %v, want a skipped rebuild", err)
	}
	if s.heldMilk != "oat" {
		t.Errorf("heldMilk = %q, want it kept", s.heldMilk)
	}
}

// gripperAt fakes a gripper whose jaws read pos.
func gripperAt(pos float64) *inject.Gripper {
	g := inject.NewGripper("g")
	g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"pos": pos}, nil
	}
	return g
}

// rewind must not open the jaws on a milk bottle wherever the arm is; once the
// operator has taken it out, rewind goes ahead.
func TestRefuseDropWhileMilkHeld(t *testing.T) {
	cfg := &Config{CanServeIcedLatte: true, MilkOptions: testMilks}
	holding := &beanjaminCoffee{cfg: cfg, gripper: gripperAt(500), heldMilk: "oat"}
	err := holding.refuseDropWhileMilkHeld(context.Background())
	if err == nil || !strings.Contains(err.Error(), "oat milk") || !strings.Contains(err.Error(), "right spot") {
		t.Fatalf("holding the oat bottle: got %v, want a refusal naming the oat milk and its right spot", err)
	}
	removed := &beanjaminCoffee{cfg: cfg, gripper: gripperAt(0), heldMilk: "oat"}
	if err := removed.refuseDropWhileMilkHeld(context.Background()); err != nil {
		t.Errorf("bottle already taken out: got %v, want rewind allowed", err)
	}
	noMilk := &beanjaminCoffee{cfg: cfg, gripper: gripperAt(500)}
	if err := noMilk.refuseDropWhileMilkHeld(context.Background()); err != nil {
		t.Errorf("holding a cup, no milk out: got %v, want rewind allowed", err)
	}
}

// A failure with the fridge open names where the bottle goes back.
func TestMilkStepErrNamesTheSpot(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{MilkOptions: testMilks}, doorOpenDegs: 90}
	err := s.milkStepErr("oat", errors.New("boom"))
	if !strings.Contains(err.Error(), "oat milk") || !strings.Contains(err.Error(), "right spot") {
		t.Errorf("milkStepErr = %v, want it to name the oat milk's right spot", err)
	}
}

// A bottle out of the fridge is reported as stranded state on a fault.
func TestStrandedStateReportsMilkOut(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}, heldMilk: "oat"}
	if got := s.strandedState(); len(got) != 1 || got[0] != "oat milk out of the fridge" {
		t.Errorf("strandedState = %v, want [oat milk out of the fridge]", got)
	}
}

// execute_action refuses "milk" on an action that doesn't pick a bottle before
// it takes the run gate, so a rejected flag costs no state.
func TestExecuteActionRefusesMilkOnOtherActions(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	_, err := s.executeAction(withMilkChoice(context.Background(), "oat"), "grind_coffee", false)
	if err == nil || !strings.Contains(err.Error(), "milk is not accepted") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if s.lease.busy() {
		t.Errorf("arm claimed by a refused call (holder %q)", s.lease.holderName())
	}
}
