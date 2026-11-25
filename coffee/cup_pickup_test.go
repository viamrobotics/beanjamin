package coffee

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/spatialmath"

	"beanjamin/coffee/geom"
)

func TestPickupGetters(t *testing.T) {
	if got := pickupMaxAttempts(0); got != defaultCupPickupMaxAttempts {
		t.Errorf("pickupMaxAttempts(0) = %d, want %d", got, defaultCupPickupMaxAttempts)
	}
	if got := pickupMaxAttempts(7); got != 7 {
		t.Errorf("pickupMaxAttempts(7) = %d, want 7", got)
	}
}

func TestHeldGeometriesCupIsOneBox(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	box, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{X: 100, Y: 200, Z: 50}), r3.Vector{X: 70, Y: 70, Z: 90}, "cup")
	if err != nil {
		t.Fatal(err)
	}
	cand := geom.Candidate{Geom: box}
	got, err := s.cupPickupTarget().heldGeometries(cand)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != box {
		t.Fatalf("got %d geometries, want the candidate's own box", len(got))
	}
}

func TestHeldGeometriesMugAddsHandle(t *testing.T) {
	s, _ := newTestCoffee(t, &Config{
		MugDimensions:       &ContainerDimensions{DiameterMm: 80, HeightMm: 97},
		MugHandleDimensions: &HandleDimensions{DepthMm: 39, WidthMm: 15},
		MugGrabRelativePose: &RelativePose{Y: -14, OX: 1},
	})
	body, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{X: 100, Y: 200, Z: 50}), r3.Vector{X: 80, Y: 80, Z: 97}, "mug")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.mugPickupTarget().heldGeometries(geom.Candidate{Geom: body})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d geometries, want body and handle", len(got))
	}
	// The body moves by the grab's sideways offset, and the handle with it.
	if b := got[0].Pose().Point(); math.Abs(b.X-100) > 1e-6 || math.Abs(b.Y-186) > 1e-6 {
		t.Errorf("body center = %+v, want X=100, Y=186", b)
	}
	h := got[1].Pose().Point()
	if math.Abs(h.X-100) > 1e-6 || math.Abs(h.Y-(186+59.5)) > 1e-6 {
		t.Errorf("handle center = %+v, want X=100, Y=%v", h, 186+59.5)
	}
}

// TestPickupBlockedBy: a back-row mug waits while a mug stands in front of it,
// even one whose grab failed and is still on the shelf.
func TestPickupBlockedBy(t *testing.T) {
	s, _ := newTestCoffee(t, &Config{
		MugDimensions:           &ContainerDimensions{DiameterMm: 80, HeightMm: 97},
		MugApproachRelativePose: &RelativePose{X: -145, Y: -14, OX: 1},
		MugGrabRelativePose:     &RelativePose{X: 15, Y: -14, OX: 1},
	})
	mug := s.mugPickupTarget()
	front := geom.Candidate{Centroid: r3.Vector{X: 760, Y: 250, Z: 42}}
	back := geom.Candidate{Centroid: r3.Vector{X: 870, Y: 248, Z: 42}}
	backClear := geom.Candidate{Centroid: r3.Vector{X: 870, Y: 40, Z: 42}}
	all := []geom.Candidate{front, back, backClear}

	if blocker, ok := mug.blockedBy(back, all); !ok || blocker != front.Centroid {
		t.Errorf("back mug: blockedBy = %v, %v; want the front mug", blocker, ok)
	}
	if _, ok := mug.blockedBy(backClear, all); ok {
		t.Error("back mug with a clear lane reported blocked")
	}
	if _, ok := mug.blockedBy(front, all); ok {
		t.Error("front mug reported blocked")
	}

	// A straight-down grab never blocks.
	s.cfg.MugApproachRelativePose = &RelativePose{Z: 100, OZ: -1}
	s.cfg.MugGrabRelativePose = &RelativePose{OZ: -1}
	if _, ok := s.mugPickupTarget().blockedBy(back, all); ok {
		t.Error("straight-down grab reported blocked")
	}
}
