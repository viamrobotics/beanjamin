package beanjamin

import (
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

func TestRankCentroidsByProximity_Empty(t *testing.T) {
	got := rankCentroidsByProximity(nil, r3.Vector{})
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestRankCentroidsByProximity_Single(t *testing.T) {
	c := []r3.Vector{{X: 110, Y: 0, Z: 0}}
	got := rankCentroidsByProximity(c, r3.Vector{X: 100, Y: 0, Z: 0})
	if len(got) != 1 || got[0] != c[0] {
		t.Fatalf("expected [%v], got %v", c[0], got)
	}
}

func TestRankCentroidsByProximity_SortsClosestFirst(t *testing.T) {
	c := []r3.Vector{
		{X: 200, Y: 0, Z: 0}, // 100mm from gripper
		{X: 110, Y: 0, Z: 0}, // 10mm
		{X: 150, Y: 0, Z: 0}, // 50mm
	}
	gripper := r3.Vector{X: 100, Y: 0, Z: 0}
	got := rankCentroidsByProximity(c, gripper)
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

func TestRankCentroidsByProximity_KeepsAllNoCutoff(t *testing.T) {
	c := []r3.Vector{
		{X: 1e6, Y: 0, Z: 0},
		{X: 100, Y: 0, Z: 0},
	}
	got := rankCentroidsByProximity(c, r3.Vector{})
	if len(got) != 2 {
		t.Fatalf("expected all candidates kept (no cutoff), got %d", len(got))
	}
	if got[0] != c[1] || got[1] != c[0] {
		t.Fatalf("expected closer-first ordering, got %v", got)
	}
}

func TestRankCentroidsByProximity_TiesStable(t *testing.T) {
	c := []r3.Vector{
		{X: 110, Y: 0, Z: 0}, // 10mm
		{X: 90, Y: 0, Z: 0},  // 10mm — tie
	}
	gripper := r3.Vector{X: 100, Y: 0, Z: 0}
	got := rankCentroidsByProximity(c, gripper)
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(got))
	}
	if got[0] != c[0] || got[1] != c[1] {
		t.Fatalf("expected stable order on ties, got %v", got)
	}
}

func TestRankCentroidsByProximity_DoesNotMutateInput(t *testing.T) {
	c := []r3.Vector{
		{X: 200, Y: 0, Z: 0},
		{X: 110, Y: 0, Z: 0},
		{X: 150, Y: 0, Z: 0},
	}
	orig := append([]r3.Vector(nil), c...)
	_ = rankCentroidsByProximity(c, r3.Vector{X: 100, Y: 0, Z: 0})
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

func TestCentroidsOf(t *testing.T) {
	box, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 10, Y: 10, Z: 10}, "c")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	cands := []pickupCandidate{
		{centroid: r3.Vector{X: 1}, geom: box},
		{centroid: r3.Vector{X: 2}},
	}
	got := centroidsOf(cands)
	want := []r3.Vector{{X: 1}, {X: 2}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("centroidsOf = %v, want %v", got, want)
	}
}

func TestNearestGeometry(t *testing.T) {
	near, _ := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{X: 100}), r3.Vector{X: 10, Y: 10, Z: 10}, "near")
	far, _ := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{X: 200}), r3.Vector{X: 10, Y: 10, Z: 10}, "far")
	originals := []pickupCandidate{
		{centroid: r3.Vector{X: 200}, geom: far},
		{centroid: r3.Vector{X: 100}, geom: near},
		{centroid: r3.Vector{X: 150}, geom: nil}, // skipped — no geometry
	}
	// Closest original to (110) is the one at (100) -> "near".
	got := nearestGeometry(r3.Vector{X: 110}, originals)
	if got == nil || got.Label() != "near" {
		t.Fatalf("expected nearest geometry 'near', got %v", got)
	}

	// No originals carry geometry -> nil.
	if g := nearestGeometry(r3.Vector{}, []pickupCandidate{{centroid: r3.Vector{X: 1}}}); g != nil {
		t.Fatalf("expected nil when no geometry available, got %v", g)
	}
}

func TestCandidatesForCentroids(t *testing.T) {
	a, _ := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{X: 0}), r3.Vector{X: 10, Y: 10, Z: 10}, "a")
	b, _ := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{X: 100}), r3.Vector{X: 10, Y: 10, Z: 10}, "b")
	originals := []pickupCandidate{
		{centroid: r3.Vector{X: 0}, geom: a},
		{centroid: r3.Vector{X: 100}, geom: b},
	}
	// Ranked order swaps the two; each ranked centroid should keep its own geom.
	ranked := []r3.Vector{{X: 100}, {X: 0}}
	got := candidatesForCentroids(ranked, originals)
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(got))
	}
	if got[0].centroid != (r3.Vector{X: 100}) || got[0].geom.Label() != "b" {
		t.Fatalf("candidate[0] = %v/%v, want centroid (100) geom 'b'", got[0].centroid, got[0].geom)
	}
	if got[1].centroid != (r3.Vector{X: 0}) || got[1].geom.Label() != "a" {
		t.Fatalf("candidate[1] = %v/%v, want centroid (0) geom 'a'", got[1].centroid, got[1].geom)
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
