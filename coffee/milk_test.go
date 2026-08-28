package coffee

import (
	"context"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/testutils/inject"
)

// The return is the pickup replayed: the same offsets composed onto the recorded
// grasp centroid, so the bottle is set back down exactly where it was lifted
// from and the descent retraces the retreat.
func TestMilkReturnPosesReplayThePickupOffsets(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{
		MilkApproachRelativePose: &RelativePose{X: 10, Y: -20, Z: 150, OZ: -1},
		MilkGrabRelativePose:     &RelativePose{X: 10, Y: -20, Z: 5, OZ: -1},
	}}
	centroid := r3.Vector{X: 300, Y: -450, Z: 620}

	approach, place := s.milkReturnPoses(centroid)

	wantApproach := composeCupPose(centroid, relativePoseToSpatial(s.cfg.MilkApproachRelativePose))
	wantPlace := composeCupPose(centroid, relativePoseToSpatial(s.cfg.MilkGrabRelativePose))
	if approach.Point() != wantApproach.Point() {
		t.Errorf("approach point = %v, want %v", approach.Point(), wantApproach.Point())
	}
	if place.Point() != wantPlace.Point() {
		t.Errorf("place point = %v, want %v", place.Point(), wantPlace.Point())
	}
	// The standoff must sit above the set-down pose, or the "descend onto the
	// shelf" move would be a climb.
	if approach.Point().Z <= place.Point().Z {
		t.Errorf("approach Z %.1f should be above place Z %.1f", approach.Point().Z, place.Point().Z)
	}
}

// Returning with nothing recorded must fail rather than drive an empty gripper
// at a shelf where the bottle may already stand.
func TestReturnMilkBottleWithoutPickupFails(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{CanServeIcedLatte: true}, gripper: inject.NewGripper("g")}
	err := s.returnMilkBottle(context.Background(), context.Background())
	if err == nil || !strings.Contains(err.Error(), "no recorded pickup position") {
		t.Fatalf("expected a no-recorded-position error, got %v", err)
	}
}

// Every milk action refuses to run on a machine that isn't configured for milk,
// rather than failing partway through a motion on nil config.
func TestMilkActionsRequireConfiguredMilk(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	actions := map[string]func(ctx, cancelCtx context.Context) error{
		"add_milk":    s.addMilk,
		"fetch_milk":  s.fetchMilkBottle,
		"pour_milk":   s.pourMilk,
		"return_milk": s.returnMilkBottle,
	}
	for name, run := range actions {
		t.Run(name, func(t *testing.T) {
			err := run(context.Background(), context.Background())
			if err == nil || !strings.Contains(err.Error(), "can_serve_iced_latte") {
				t.Fatalf("expected a can_serve_iced_latte error, got %v", err)
			}
		})
	}
}

// The milk actions are on the execute_action surface so the poses and the grasp
// offsets can be stepped through one at a time on hardware.
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
	cup, err := containerBox(r3.Vector{}, &ContainerDimensions{DiameterMm: 80, HeightMm: 95}, pickupLabelCup)
	if err != nil {
		t.Fatal(err)
	}
	milk, err := containerBox(r3.Vector{}, &ContainerDimensions{DiameterMm: 90, HeightMm: 250}, pickupLabelMilk)
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

// A frame-system reset drops the recorded pickup position along with the cached
// geometry — it describes a bottle the gripper is no longer known to hold.
func TestClearHeldGeometryForgetsMilkPickup(t *testing.T) {
	centroid := r3.Vector{X: 1, Y: 2, Z: 3}
	s := &beanjaminCoffee{cfg: &Config{}, milkGraspCentroid: &centroid}
	s.clearHeldGeometry()
	if s.milkGraspCentroid != nil {
		t.Errorf("milkGraspCentroid = %v, want nil after clearHeldGeometry", *s.milkGraspCentroid)
	}
}
