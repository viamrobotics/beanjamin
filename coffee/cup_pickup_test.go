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
		MugGrabRelativePose: &RelativePose{},
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
	h := got[1].Pose().Point()
	if math.Abs(h.X-100) > 1e-6 || math.Abs(h.Y-(200+59.5)) > 1e-6 {
		t.Errorf("handle center = %+v, want X=100, Y=%v", h, 200+59.5)
	}
}

// Only the mug lifts off its shelf before backing out: 15 mm unless
// mug_pickup_lift_mm says otherwise. Cups and glasses back straight out.
func TestPickupRetreatLift(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	if got := s.cupPickupTarget().retreatLiftMm; got != 0 {
		t.Errorf("cup lift = %v, want none", got)
	}
	if got := s.glassPickupTarget().retreatLiftMm; got != 0 {
		t.Errorf("glass lift = %v, want none", got)
	}
	if got := s.mugPickupTarget().retreatLiftMm; got != defaultMugPickupLiftMm {
		t.Errorf("mug lift = %v, want the default %v", got, defaultMugPickupLiftMm)
	}
	s.cfg.MugPickupLiftMm = 45
	if got := s.mugPickupTarget().retreatLiftMm; got != 45 {
		t.Errorf("configured mug lift = %v, want 45", got)
	}
}

// The lift moves the pose straight up and keeps its orientation.
func TestRaisedPose(t *testing.T) {
	orient := &spatialmath.OrientationVectorDegrees{OX: 1, Theta: 20}
	pd := &poseData{pose: spatialmath.NewPose(r3.Vector{X: 880, Y: 180, Z: 42}, orient), refFrame: "world", componentName: gripPoint}
	got := raisedPose(pd, 30)
	requireVecEqual(t, got.pose.Point(), r3.Vector{X: 880, Y: 180, Z: 72}, 1e-9)
	if !spatialmath.OrientationAlmostEqual(got.pose.Orientation(), orient) {
		t.Errorf("orientation = %v, want %v", got.pose.Orientation(), orient)
	}
	if got.refFrame != pd.refFrame || got.componentName != pd.componentName {
		t.Errorf("frames = %q/%q, want %q/%q", got.refFrame, got.componentName, pd.refFrame, pd.componentName)
	}
}
