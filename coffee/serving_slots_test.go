package coffee

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

func TestEmptyCupAreaPrefersDirtyContainerArea(t *testing.T) {
	s := sceneCoffee(t)
	s.servingSlot = &servingSlotStore{}
	s.dirtyContainerSlot = &servingSlotStore{}

	// No dirty-container area: the empty cup goes to the serving area.
	if got := s.emptyCupArea(); got.frame != servingAreaFrameName || got.slots != s.servingSlot {
		t.Fatalf("without a dirty-container area: got %q, want %q with the serving counter", got.frame, servingAreaFrameName)
	}

	for _, name := range []string{dirtyContainerAreaFrameName, dirtyContainerAreaOriginFrameName} {
		t.Run(name, func(t *testing.T) {
			s := sceneCoffee(t)
			s.servingSlot = &servingSlotStore{}
			s.dirtyContainerSlot = &servingSlotStore{}
			frame, err := referenceframe.NewStaticFrame(name, spatialmath.NewZeroPose())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.editScene(func(sc *sceneState) error { return sc.fs.AddFrame(frame, sc.fs.World()) }); err != nil {
				t.Fatal(err)
			}
			got := s.emptyCupArea()
			if got.frame != dirtyContainerAreaFrameName || got.shieldFrame != dirtyContainerAreaShieldFrameName || got.slots != s.dirtyContainerSlot {
				t.Fatalf("got area %+v, want the dirty-container area with its own counter", got)
			}
		})
	}
}

func vecAlmostEqual(a, b r3.Vector, eps float64) bool {
	return math.Abs(a.X-b.X) < eps && math.Abs(a.Y-b.Y) < eps && math.Abs(a.Z-b.Z) < eps
}

func TestServingAreaDropZOffset(t *testing.T) {
	fs := clawsStaticFS(t, spatialmath.NewZeroPose())
	s := heldGeomService(t, fs)

	// Nothing held: the fixed fallback offset (tuned for the cup).
	if got := s.servingAreaDropZOffset(); got != shelfDropZOffsetMm {
		t.Fatalf("fallback offset = %g, want %g", got, shelfDropZOffsetMm)
	}

	// Container held: half its height (the 80mm test box -> 40), so its bottom
	// lands on the shelf rather than being driven in like a taller glass was.
	addHeldItem(t, s, testBox(t, spatialmath.NewZeroPose()))
	if got := s.servingAreaDropZOffset(); math.Abs(got-40) > 1e-6 {
		t.Fatalf("height-derived offset = %g, want 40", got)
	}
}

func TestServingSlotStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)
	logger := logging.NewTestLogger(t)

	first := newServingSlotStore(logger, servingAreaFrameName, servingSlotFile)
	if got := first.get(); got != 0 {
		t.Errorf("get() on an empty store = %d, want 0", got)
	}

	first.set(logger, 7)
	if got := first.get(); got != 7 {
		t.Errorf("get() after set = %d, want 7", got)
	}

	// A new store reads the persisted value back — this is the reconfigure case.
	second := newServingSlotStore(logger, servingAreaFrameName, servingSlotFile)
	if got := second.get(); got != 7 {
		t.Errorf("get() on a reloaded store = %d, want 7", got)
	}
}

func TestServingSlotStoreWithoutModuleData(t *testing.T) {
	t.Setenv("VIAM_MODULE_DATA", "")
	logger := logging.NewTestLogger(t)

	st := newServingSlotStore(logger, servingAreaFrameName, servingSlotFile)
	st.set(logger, 3)
	if got := st.get(); got != 3 {
		t.Errorf("in-memory get() = %d, want 3", got)
	}
}

func TestServingSlotStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)
	logger := logging.NewTestLogger(t)

	if err := os.WriteFile(filepath.Join(dir, servingSlotFile), []byte("not a number"), 0o644); err != nil {
		t.Fatalf("seeding a corrupt file: %v", err)
	}

	// Unreadable content must degrade to slot 0 rather than failing construction.
	if got := newServingSlotStore(logger, servingAreaFrameName, servingSlotFile).get(); got != 0 {
		t.Errorf("get() with a corrupt file = %d, want 0", got)
	}
}

// holdLabeled attaches a box labeled label as the held item.
func holdLabeled(t *testing.T, s *beanjaminCoffee, label string) {
	t.Helper()
	box, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 40, Y: 40, Z: 80}, label)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	if err := s.editScene(func(sc *sceneState) error { return sc.addHeldItemFrame([]spatialmath.Geometry{box}) }); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}
}

func TestDropZOffsetForMug(t *testing.T) {
	s := heldGeomService(t, clawsStaticFS(t, spatialmath.NewZeroPose()))
	s.cfg.MugDimensions = &ContainerDimensions{HeightMm: 97}
	s.cfg.MugGrabRelativePose = &RelativePose{Z: 5}
	holdLabeled(t, s, pickupLabelMug)
	// Like every other container, a mug drops at the held item's half-height.
	if got := s.servingAreaDropZOffset(); got != 40 {
		t.Fatalf("mug drop offset = %g, want the held half-height 40", got)
	}
}
