package coffee

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/spatialmath"
)

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
	if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}
	if got := s.servingAreaDropZOffset(); math.Abs(got-40) > 1e-6 {
		t.Fatalf("height-derived offset = %g, want 40", got)
	}
}

func TestServingSlotStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)
	logger := logging.NewTestLogger(t)

	first := newServingSlotStore(logger)
	if got := first.get(); got != 0 {
		t.Errorf("get() on an empty store = %d, want 0", got)
	}

	first.set(logger, 7)
	if got := first.get(); got != 7 {
		t.Errorf("get() after set = %d, want 7", got)
	}

	// A new store reads the persisted value back — this is the reconfigure case.
	second := newServingSlotStore(logger)
	if got := second.get(); got != 7 {
		t.Errorf("get() on a reloaded store = %d, want 7", got)
	}
}

func TestServingSlotStoreWithoutModuleData(t *testing.T) {
	t.Setenv("VIAM_MODULE_DATA", "")
	logger := logging.NewTestLogger(t)

	st := newServingSlotStore(logger)
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
	if got := newServingSlotStore(logger).get(); got != 0 {
		t.Errorf("get() with a corrupt file = %d, want 0", got)
	}
}
