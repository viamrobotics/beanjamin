package beanjamin

import (
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

func TestRankCupCentroids_Empty(t *testing.T) {
	got := rankCupCentroids(nil, r3.Vector{}, 100)
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestRankCupCentroids_SingleInRange(t *testing.T) {
	c := []r3.Vector{{X: 110, Y: 0, Z: 0}}
	got := rankCupCentroids(c, r3.Vector{X: 100, Y: 0, Z: 0}, 50)
	if len(got) != 1 || got[0] != c[0] {
		t.Fatalf("expected [%v], got %v", c[0], got)
	}
}

func TestRankCupCentroids_SingleOutOfRange(t *testing.T) {
	c := []r3.Vector{{X: 1000, Y: 0, Z: 0}}
	got := rankCupCentroids(c, r3.Vector{}, 100)
	if len(got) != 0 {
		t.Fatalf("expected empty slice (out of range), got %v", got)
	}
}

func TestRankCupCentroids_SortsClosestFirst(t *testing.T) {
	c := []r3.Vector{
		{X: 200, Y: 0, Z: 0}, // 100mm from target
		{X: 110, Y: 0, Z: 0}, // 10mm
		{X: 150, Y: 0, Z: 0}, // 50mm
	}
	target := r3.Vector{X: 100, Y: 0, Z: 0}
	got := rankCupCentroids(c, target, 300)
	want := []r3.Vector{c[1], c[2], c[0]}
	if len(got) != len(want) {
		t.Fatalf("expected %d candidates, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRankCupCentroids_AllOutOfRange(t *testing.T) {
	c := []r3.Vector{
		{X: 1000, Y: 0, Z: 0},
		{X: 2000, Y: 0, Z: 0},
	}
	got := rankCupCentroids(c, r3.Vector{}, 100)
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestRankCupCentroids_DropsOutOfRangeKeepsRest(t *testing.T) {
	c := []r3.Vector{
		{X: 1000, Y: 0, Z: 0}, // out
		{X: 110, Y: 0, Z: 0},  // in (10mm)
		{X: 200, Y: 0, Z: 0},  // in (100mm)
	}
	target := r3.Vector{X: 100, Y: 0, Z: 0}
	got := rankCupCentroids(c, target, 150)
	want := []r3.Vector{c[1], c[2]}
	if len(got) != len(want) {
		t.Fatalf("expected %d candidates, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRankCupCentroids_ZeroMaxMeansNoCutoff(t *testing.T) {
	c := []r3.Vector{
		{X: 1e6, Y: 0, Z: 0},
		{X: 100, Y: 0, Z: 0},
	}
	got := rankCupCentroids(c, r3.Vector{}, 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates with maxDistMm=0, got %d", len(got))
	}
	if got[0] != c[1] || got[1] != c[0] {
		t.Fatalf("expected closer-first ordering, got %v", got)
	}
}

func TestRankCupCentroids_TiesStable(t *testing.T) {
	c := []r3.Vector{
		{X: 110, Y: 0, Z: 0}, // 10mm
		{X: 90, Y: 0, Z: 0},  // 10mm — tie
	}
	target := r3.Vector{X: 100, Y: 0, Z: 0}
	got := rankCupCentroids(c, target, 50)
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(got))
	}
	if got[0] != c[0] || got[1] != c[1] {
		t.Fatalf("expected stable order on ties, got %v", got)
	}
}

func TestRankCupCentroids_DoesNotMutateInput(t *testing.T) {
	c := []r3.Vector{
		{X: 200, Y: 0, Z: 0},
		{X: 110, Y: 0, Z: 0},
		{X: 150, Y: 0, Z: 0},
	}
	orig := append([]r3.Vector(nil), c...)
	_ = rankCupCentroids(c, r3.Vector{X: 100, Y: 0, Z: 0}, 300)
	for i := range orig {
		if c[i] != orig[i] {
			t.Fatalf("input mutated at index %d: got %v, want %v", i, c[i], orig[i])
		}
	}
}

func TestMergeNearbyCentroids_Empty(t *testing.T) {
	got := mergeNearbyCentroids(nil, 40)
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestMergeNearbyCentroids_NoNearDuplicates(t *testing.T) {
	in := []r3.Vector{{X: 0}, {X: 100}, {X: 200}}
	got := mergeNearbyCentroids(in, 40)
	if len(got) != 3 {
		t.Fatalf("expected all kept, got %v", got)
	}
}

func TestMergeNearbyCentroids_AveragesWithinRadius(t *testing.T) {
	in := []r3.Vector{
		{X: 0, Y: 0, Z: 0},
		{X: 30, Y: 0, Z: 0}, // within 40 of cluster 0 -> merged
		{X: 100, Y: 0, Z: 0},
		{X: 110, Y: 0, Z: 0}, // within 40 of cluster 1 -> merged
	}
	got := mergeNearbyCentroids(in, 40)
	// Each cluster collapses to the mean of its members, not the first hit.
	want := []r3.Vector{{X: 15}, {X: 105}}
	if len(got) != len(want) {
		t.Fatalf("expected %d, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMergeNearbyCentroids_SeparatesBeyondRadius(t *testing.T) {
	// 0, 35, 70: 35 is within 40 of cluster {0}, so it merges and the
	// running mean moves to 17.5; 70 is >40 from 17.5, so it stays separate.
	in := []r3.Vector{{X: 0}, {X: 35}, {X: 70}}
	got := mergeNearbyCentroids(in, 40)
	want := []r3.Vector{{X: 17.5}, {X: 70}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestMergeNearbyCentroids_ZeroRadiusDisables(t *testing.T) {
	in := []r3.Vector{{X: 0}, {X: 0}, {X: 0}}
	got := mergeNearbyCentroids(in, 0)
	if len(got) != 3 {
		t.Fatalf("zero radius should keep all, got %v", got)
	}
}

func TestMergeNearbyCentroids_DoesNotMutateInput(t *testing.T) {
	in := []r3.Vector{{X: 0}, {X: 10}, {X: 100}}
	orig := append([]r3.Vector(nil), in...)
	_ = mergeNearbyCentroids(in, 40)
	for i := range orig {
		if in[i] != orig[i] {
			t.Fatalf("input mutated at %d: %v != %v", i, in[i], orig[i])
		}
	}
}

func TestComposeCupPose_IdentityRelative(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 300}
	relative := spatialmath.NewZeroPose()
	got := composeCupPose(centroid, relative)
	if got.Point() != centroid {
		t.Fatalf("expected centroid preserved %v, got %v", centroid, got.Point())
	}
	if !spatialmath.OrientationAlmostEqual(got.Orientation(), spatialmath.NewZeroOrientation()) {
		t.Fatalf("expected zero orientation, got %v", got.Orientation())
	}
}

func TestComposeCupPose_PureTranslation(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 300}
	relative := spatialmath.NewPoseFromPoint(r3.Vector{X: 10, Y: 0, Z: 0})
	got := composeCupPose(centroid, relative)
	want := r3.Vector{X: 110, Y: 200, Z: 300}
	if got.Point() != want {
		t.Fatalf("expected %v, got %v", want, got.Point())
	}
}

func TestComposeCupPose_PureRotation(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 300}
	orient := &spatialmath.OrientationVectorDegrees{OX: 1, OY: 0, OZ: 0, Theta: 90}
	relative := spatialmath.NewPose(r3.Vector{}, orient)
	got := composeCupPose(centroid, relative)
	if got.Point() != centroid {
		t.Fatalf("expected centroid preserved %v, got %v", centroid, got.Point())
	}
	if !spatialmath.OrientationAlmostEqual(got.Orientation(), orient) {
		t.Fatalf("expected %v, got %v", orient, got.Orientation())
	}
}

func cameraToWorldTestFS(t *testing.T, camPose spatialmath.Pose) *referenceframe.FrameSystem {
	t.Helper()
	fs := referenceframe.NewEmptyFrameSystem("test")
	camFrame, err := referenceframe.NewStaticFrame("camera", camPose)
	if err != nil {
		t.Fatalf("create camera frame: %v", err)
	}
	if err := fs.AddFrame(camFrame, fs.World()); err != nil {
		t.Fatalf("add camera frame: %v", err)
	}
	return fs
}

func TestCameraToWorld_Identity(t *testing.T) {
	fs := cameraToWorldTestFS(t, spatialmath.NewZeroPose())
	fsInputs := referenceframe.NewZeroInputs(fs)
	point := r3.Vector{X: 50, Y: 60, Z: 70}
	got, err := cameraToWorld(fs, fsInputs, "camera", point)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != point {
		t.Fatalf("expected %v unchanged, got %v", point, got)
	}
}

func TestCameraToWorld_Translated(t *testing.T) {
	camPose := spatialmath.NewPose(r3.Vector{X: 100, Y: 0, Z: 0}, spatialmath.NewZeroOrientation())
	fs := cameraToWorldTestFS(t, camPose)
	fsInputs := referenceframe.NewZeroInputs(fs)
	got, err := cameraToWorld(fs, fsInputs, "camera", r3.Vector{X: 10, Y: 0, Z: 0})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	want := r3.Vector{X: 110, Y: 0, Z: 0}
	if got != want {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestCameraToWorld_MissingFrame(t *testing.T) {
	fs := referenceframe.NewEmptyFrameSystem("test")
	fsInputs := referenceframe.NewZeroInputs(fs)
	_, err := cameraToWorld(fs, fsInputs, "no-such-camera", r3.Vector{})
	if err == nil {
		t.Fatalf("expected error for missing camera frame")
	}
}
