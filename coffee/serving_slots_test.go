package coffee

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
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
