package coffee

import (
	"context"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
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
	actions := map[string]func(ctx, cancelCtx context.Context) error{
		"add_milk":    func(ctx, cancelCtx context.Context) error { return s.addMilk(ctx, cancelCtx, "whole") },
		"fetch_milk":  func(ctx, cancelCtx context.Context) error { return s.fetchMilkBottle(ctx, cancelCtx, "whole") },
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
	cup, err := geom.ContainerBox(r3.Vector{}, (&ContainerDimensions{DiameterMm: 80, HeightMm: 95}).boxDims(), 0, pickupLabelCup)
	if err != nil {
		t.Fatal(err)
	}
	milk, err := geom.ContainerBox(r3.Vector{}, (&ContainerDimensions{DiameterMm: 90, HeightMm: 250}).boxDims(), 0, pickupLabelMilk)
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

// Every iced latte and hand-run milk action uses the first milk for now.
func TestFirstMilk(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{MilkOptions: testMilks}}
	if got := s.firstMilk(); got != "whole" {
		t.Errorf("firstMilk = %q, want whole", got)
	}
}

// Fetching a second bottle while one is out would leave nothing to say which
// spot the first goes back to.
func TestFetchMilkWhileOneIsOutFails(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{CanServeIcedLatte: true, MilkOptions: testMilks}, gripper: inject.NewGripper("g"), heldMilk: "whole"}
	err := s.fetchMilkBottle(context.Background(), context.Background(), "oat")
	if err == nil || !strings.Contains(err.Error(), "already out") {
		t.Fatalf("expected an already-out error, got %v", err)
	}
}

// fridgeFloorFS is a grip point at gripZ above a fridge floor whose top face is
// at Z=200 and spans the bottle's spot.
func fridgeFloorFS(t *testing.T, gripZ float64) *referenceframe.FrameSystem {
	t.Helper()
	fs := gripPointStaticFS(t, spatialmath.NewPoseFromPoint(r3.Vector{X: 400, Y: 100, Z: gripZ}))
	floorBox, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{X: 400, Y: 100, Z: 190}),
		r3.Vector{X: 400, Y: 400, Z: 20}, "fridge-floor")
	if err != nil {
		t.Fatalf("new floor box: %v", err)
	}
	floor, err := referenceframe.NewStaticFrameWithGeometry("fridge-floor", spatialmath.NewZeroPose(), floorBox)
	if err != nil {
		t.Fatalf("new floor frame: %v", err)
	}
	if err := fs.AddFrame(floor, fs.World()); err != nil {
		t.Fatalf("add floor frame: %v", err)
	}
	return fs
}

// The bottle model stands on the fridge floor wherever up the bottle the grab
// pose was taught, keeping the grip point's X/Y.
func TestMilkBottleBoxRestsOnSurface(t *testing.T) {
	dims := &ContainerDimensions{DiameterMm: 90, HeightMm: 250}
	wantCenter := r3.Vector{X: 400, Y: 100, Z: 200 + surfaceRestClearanceMm + dims.HeightMm/2}
	for _, gripZ := range []float64{240, 325, 430} {
		fs := fridgeFloorFS(t, gripZ)
		s := heldGeomService(t, fs)
		s.cfg.MilkBottleDimensions = dims
		box, err := s.milkBottleBox(fs, referenceframe.NewZeroInputs(fs))
		if err != nil {
			t.Fatalf("grip Z=%.0f: milkBottleBox: %v", gripZ, err)
		}
		requireVecEqual(t, box.Pose().Point(), wantCenter, 1e-6)
	}
}

func TestMilkBottleBoxRejectsUnseatableBottle(t *testing.T) {
	dims := &ContainerDimensions{DiameterMm: 90, HeightMm: 250}
	for name, fs := range map[string]*referenceframe.FrameSystem{
		// Nothing modeled under the bottle.
		"no surface": gripPointStaticFS(t, spatialmath.NewPoseFromPoint(r3.Vector{X: 400, Y: 100, Z: 325})),
		// The grip point is above the top of a bottle standing on the floor.
		"grab above the bottle": fridgeFloorFS(t, 470),
	} {
		s := heldGeomService(t, fs)
		s.cfg.MilkBottleDimensions = dims
		if _, err := s.milkBottleBox(fs, referenceframe.NewZeroInputs(fs)); err == nil {
			t.Errorf("%s: milkBottleBox succeeded, want an error", name)
		}
	}
}
