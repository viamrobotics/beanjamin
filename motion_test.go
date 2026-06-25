package beanjamin

import (
	"encoding/json"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// TestFrameSystemWithGeometries verifies that detected world-frame geometries
// are added to a clone of the cached frame system as static world frames, that
// they survive a JSON round-trip (as the local motion-tools reader does), and
// that they resolve back at their original world pose via the same path
// motion-tools uses to draw a frame system (FrameSystemGeometries). The cached
// frame system and the input geometries must not be mutated.
func TestFrameSystemWithGeometries(t *testing.T) {
	fs := referenceframe.NewEmptyFrameSystem("test")
	cam, err := referenceframe.NewStaticFrame("camera", spatialmath.NewPoseFromPoint(r3.Vector{X: 100}))
	if err != nil {
		t.Fatalf("new camera frame: %v", err)
	}
	if err := fs.AddFrame(cam, fs.World()); err != nil {
		t.Fatalf("add camera frame: %v", err)
	}
	s := &beanjaminCoffee{cachedFS: fs}

	worldPose := spatialmath.NewPose(
		r3.Vector{X: 300, Y: -150, Z: 50},
		&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 30},
	)
	dims := r3.Vector{X: 40, Y: 40, Z: 80}
	box, err := spatialmath.NewBox(worldPose, dims, "")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	out, err := s.frameSystemWithGeometries("cup", []spatialmath.Geometry{box})
	if err != nil {
		t.Fatalf("frameSystemWithGeometries: %v", err)
	}

	// The cached frame system must be left untouched (we clone, not mutate).
	if s.cachedFS.Frame("cup_0") != nil {
		t.Errorf("cached frame system was mutated: cup_0 leaked into it")
	}
	// The input geometry must not be relabeled (the held-item tracker reuses it).
	if box.Label() != "" {
		t.Errorf("input geometry was mutated: label = %q", box.Label())
	}

	// Round-trip through JSON exactly as the local motion-tools reader will.
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal frame system: %v", err)
	}
	var loaded referenceframe.FrameSystem
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal frame system: %v", err)
	}

	// Pre-existing frames survive the clone + round-trip.
	if loaded.Frame("camera") == nil {
		t.Errorf("camera frame lost after round-trip")
	}

	// The cup geometry resolves at its original world pose via the same path
	// motion-tools uses to render a frame system.
	all, err := referenceframe.FrameSystemGeometries(&loaded, referenceframe.NewZeroInputs(&loaded))
	if err != nil {
		t.Fatalf("frame system geometries: %v", err)
	}
	gif, ok := all["cup_0"]
	if !ok {
		t.Fatalf("cup_0 has no geometry after round-trip")
	}
	geos := gif.Geometries()
	if len(geos) != 1 {
		t.Fatalf("want 1 geometry for cup_0, got %d", len(geos))
	}
	if got := geos[0].Label(); got != "cup_0" {
		t.Errorf("geometry label = %q, want %q", got, "cup_0")
	}
	want, err := spatialmath.NewBox(worldPose, dims, "cup_0")
	if err != nil {
		t.Fatalf("new want box: %v", err)
	}
	if !spatialmath.GeometriesAlmostEqual(geos[0], want) {
		t.Errorf("resolved geometry mismatch:\n got %v\nwant %v", geos[0], want)
	}
}

// TestFrameSystemWithGeometries_SkipsNil ensures nil geometries are skipped and
// indices follow input order.
func TestFrameSystemWithGeometries_SkipsNil(t *testing.T) {
	s := &beanjaminCoffee{cachedFS: referenceframe.NewEmptyFrameSystem("test")}
	box, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 10, Y: 10, Z: 10}, "")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	out, err := s.frameSystemWithGeometries("glass", []spatialmath.Geometry{nil, box})
	if err != nil {
		t.Fatalf("frameSystemWithGeometries: %v", err)
	}
	if out.Frame("glass_0") != nil {
		t.Errorf("glass_0 should not exist (index 0 was nil)")
	}
	if out.Frame("glass_1") == nil {
		t.Errorf("glass_1 missing for the non-nil geometry at index 1")
	}
}
