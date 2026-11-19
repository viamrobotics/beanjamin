package geom

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

func TestRankCentroidsByProximity_Empty(t *testing.T) {
	got := RankCentroidsByProximity(nil, r3.Vector{})
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestRankCentroidsByProximity_Single(t *testing.T) {
	c := []r3.Vector{{X: 110, Y: 0, Z: 0}}
	got := RankCentroidsByProximity(c, r3.Vector{X: 100, Y: 0, Z: 0})
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
	got := RankCentroidsByProximity(c, gripper)
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
	got := RankCentroidsByProximity(c, r3.Vector{})
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
	got := RankCentroidsByProximity(c, gripper)
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
	_ = RankCentroidsByProximity(c, r3.Vector{X: 100, Y: 0, Z: 0})
	for i := range orig {
		if c[i] != orig[i] {
			t.Fatalf("input mutated at index %d: got %v, want %v", i, c[i], orig[i])
		}
	}
}

func TestMergeNearbyCentroids_Empty(t *testing.T) {
	got := MergeNearbyCentroids(nil, 40)
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestMergeNearbyCentroids_NoNearDuplicates(t *testing.T) {
	in := []r3.Vector{{X: 0}, {X: 100}, {X: 200}}
	got := MergeNearbyCentroids(in, 40)
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
	got := MergeNearbyCentroids(in, 40)
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
	got := MergeNearbyCentroids(in, 40)
	want := []r3.Vector{{X: 17.5}, {X: 70}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestMergeNearbyCentroids_ZeroRadiusDisables(t *testing.T) {
	in := []r3.Vector{{X: 0}, {X: 0}, {X: 0}}
	got := MergeNearbyCentroids(in, 0)
	if len(got) != 3 {
		t.Fatalf("zero radius should keep all, got %v", got)
	}
}

func TestMergeNearbyCentroids_DoesNotMutateInput(t *testing.T) {
	in := []r3.Vector{{X: 0}, {X: 10}, {X: 100}}
	orig := append([]r3.Vector(nil), in...)
	_ = MergeNearbyCentroids(in, 40)
	for i := range orig {
		if in[i] != orig[i] {
			t.Fatalf("input mutated at %d: %v != %v", i, in[i], orig[i])
		}
	}
}

func TestPoseRelativeTo_IdentityRelative(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 300}
	relative := spatialmath.NewZeroPose()
	got := PoseRelativeTo(centroid, relative)
	if got.Point() != centroid {
		t.Fatalf("expected centroid preserved %v, got %v", centroid, got.Point())
	}
	if !spatialmath.OrientationAlmostEqual(got.Orientation(), spatialmath.NewZeroOrientation()) {
		t.Fatalf("expected zero orientation, got %v", got.Orientation())
	}
}

func TestPoseRelativeTo_PureTranslation(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 300}
	relative := spatialmath.NewPoseFromPoint(r3.Vector{X: 10, Y: 0, Z: 0})
	got := PoseRelativeTo(centroid, relative)
	want := r3.Vector{X: 110, Y: 200, Z: 300}
	if got.Point() != want {
		t.Fatalf("expected %v, got %v", want, got.Point())
	}
}

func TestPoseRelativeTo_PureRotation(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 300}
	orient := &spatialmath.OrientationVectorDegrees{OX: 1, OY: 0, OZ: 0, Theta: 90}
	relative := spatialmath.NewPose(r3.Vector{}, orient)
	got := PoseRelativeTo(centroid, relative)
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
	cands := []Candidate{
		{Centroid: r3.Vector{X: 1}, Geom: box},
		{Centroid: r3.Vector{X: 2}},
	}
	got := Centroids(cands)
	want := []r3.Vector{{X: 1}, {X: 2}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Centroids = %v, want %v", got, want)
	}
}

func TestContainerCandidates(t *testing.T) {
	dims := r3.Vector{X: 70, Y: 70, Z: 60}
	centroids := []r3.Vector{{X: 100, Y: 5, Z: 50}, {X: 0}}
	got, err := ContainerCandidates(centroids, dims, 0, "cup")
	if err != nil {
		t.Fatalf("ContainerCandidates: %v", err)
	}
	if len(got) != len(centroids) {
		t.Fatalf("expected %d candidates, got %d", len(centroids), len(got))
	}
	for i, c := range got {
		if c.Centroid != centroids[i] {
			t.Fatalf("candidate[%d] centroid = %v, want %v", i, c.Centroid, centroids[i])
		}
		if c.Geom == nil || c.Geom.Label() != "cup" {
			t.Fatalf("candidate[%d] geom = %v, want a box labeled cup", i, c.Geom)
		}
		if p := c.Geom.Pose().Point(); p != centroids[i] {
			t.Fatalf("candidate[%d] box center = %v, want its centroid %v", i, p, centroids[i])
		}
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

func TestCameraToWorldPose_Identity(t *testing.T) {
	fs := cameraToWorldTestFS(t, spatialmath.NewZeroPose())
	fsInputs := referenceframe.NewZeroInputs(fs)
	camToWorld, err := CameraToWorldPose(fs, fsInputs, "camera")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	point := r3.Vector{X: 50, Y: 60, Z: 70}
	got := spatialmath.Compose(camToWorld, spatialmath.NewPoseFromPoint(point)).Point()
	if got != point {
		t.Fatalf("expected %v unchanged, got %v", point, got)
	}
}

func TestCameraToWorldPose_Translated(t *testing.T) {
	camPose := spatialmath.NewPose(r3.Vector{X: 100, Y: 0, Z: 0}, spatialmath.NewZeroOrientation())
	fs := cameraToWorldTestFS(t, camPose)
	fsInputs := referenceframe.NewZeroInputs(fs)
	camToWorld, err := CameraToWorldPose(fs, fsInputs, "camera")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	got := spatialmath.Compose(camToWorld, spatialmath.NewPoseFromPoint(r3.Vector{X: 10})).Point()
	want := r3.Vector{X: 110, Y: 0, Z: 0}
	if got != want {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestCameraToWorldPose_MissingFrame(t *testing.T) {
	fs := referenceframe.NewEmptyFrameSystem("test")
	fsInputs := referenceframe.NewZeroInputs(fs)
	_, err := CameraToWorldPose(fs, fsInputs, "no-such-camera")
	if err == nil {
		t.Fatalf("expected error for missing camera frame")
	}
}

// TestContainerBox verifies that ContainerBox builds an upright box of the
// configured size centered on the grasp centroid — not on any
// point-cloud-derived midpoint — turned the given yaw about world Z.
func TestContainerBox(t *testing.T) {
	centroid := r3.Vector{X: 110, Y: 20, Z: 40}
	dims := r3.Vector{X: 70, Y: 70, Z: 140}

	for _, yawDeg := range []float64{0, -161.4} {
		box, err := ContainerBox(centroid, dims, yawDeg*math.Pi/180, "glass")
		if err != nil {
			t.Fatalf("ContainerBox: %v", err)
		}
		want, err := spatialmath.NewBox(
			spatialmath.NewPose(centroid, &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: yawDeg}),
			r3.Vector{X: 70, Y: 70, Z: 140},
			"glass",
		)
		if err != nil {
			t.Fatalf("new want box: %v", err)
		}
		if !spatialmath.GeometriesAlmostEqual(box, want) {
			t.Errorf("yaw %v°: box mismatch:\n got %v\nwant %v", yawDeg, box, want)
		}
	}
}

// TestGraspYaw verifies the heading is read off the tool axis for a side grasp,
// whatever the roll about that axis, and off the tool +X for a top-down one.
func TestGraspYaw(t *testing.T) {
	cases := []struct {
		name    string
		grasp   spatialmath.Orientation
		wantDeg float64
	}{
		{"side grasp from -161.4°", &spatialmath.OrientationVectorDegrees{OX: -0.948, OY: -0.319, Theta: -180}, -161.4},
		{"side grasp, rolled", &spatialmath.OrientationVectorDegrees{OX: -0.948, OY: -0.319, Theta: 30}, -161.4},
		{"side grasp along -Y", &spatialmath.OrientationVectorDegrees{OY: -1, Theta: -180}, -90},
		{"tilted grasp", &spatialmath.OrientationVectorDegrees{OX: 0.7, OY: 0.7, OZ: -0.2}, 45},
		{"top-down grasp", &spatialmath.OrientationVectorDegrees{OZ: -1, Theta: 30}, 150},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := GraspYaw(tc.grasp) * 180 / math.Pi
			if math.Abs(got-tc.wantDeg) > 0.1 {
				t.Fatalf("GraspYaw = %.2f°, want %.2f°", got, tc.wantDeg)
			}
		})
	}
}

// TestHeadingAligned verifies an upright container, expressed in the grasp's
// coordinates, is turned to read the grasp's own heading (GraspYaw) in the world
// — whatever yaw the container was built at, and for a top-down grasp too.
func TestHeadingAligned(t *testing.T) {
	grasps := []spatialmath.Orientation{
		&spatialmath.OrientationVectorDegrees{OX: 1, Theta: 30},
		&spatialmath.OrientationVectorDegrees{OY: 1, Theta: -180},
		&spatialmath.OrientationVectorDegrees{OX: -0.948, OY: -0.319, Theta: -180},
		&spatialmath.OrientationVectorDegrees{OX: 0.7, OY: 0.7, OZ: -0.2},
		&spatialmath.OrientationVectorDegrees{OZ: -1, Theta: 30},
	}
	for _, o := range grasps {
		for _, builtDeg := range []float64{0, -161.4, 37} {
			grasp := spatialmath.NewPoseFromOrientation(o)
			container := spatialmath.NewPoseFromOrientation(&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: builtDeg})
			local := spatialmath.PoseBetween(grasp, container).Orientation()

			world := spatialmath.Compose(grasp, spatialmath.NewPoseFromOrientation(HeadingAligned(local))).Orientation()
			want := &spatialmath.OrientationVector{OZ: 1, Theta: GraspYaw(o)}
			if !spatialmath.OrientationAlmostEqualEps(world, want, 1e-6) {
				t.Errorf("grasp %v, built at %v°: world %v, want %v",
					o, builtDeg, world.OrientationVectorDegrees(), want.Degrees())
			}
		}
	}
}

// TestContainerBoxFollowsGrasp verifies the point of the yaw: expressed in the
// grasp frame, the box built at a grasp's GraspYaw has the same orientation
// whichever heading the grasp approaches from.
func TestContainerBoxFollowsGrasp(t *testing.T) {
	dims := r3.Vector{X: 70, Y: 70, Z: 60}
	var first spatialmath.Pose
	for _, ox := range []r3.Vector{{X: -0.948, Y: -0.319}, {X: -0.707, Y: -0.707}, {Y: -1}} {
		grasp := spatialmath.NewPoseFromOrientation(&spatialmath.OrientationVectorDegrees{OX: ox.X, OY: ox.Y, Theta: -180})
		box, err := ContainerBox(r3.Vector{}, dims, GraspYaw(grasp.Orientation()), "cup")
		if err != nil {
			t.Fatalf("ContainerBox: %v", err)
		}
		local := spatialmath.PoseBetween(grasp, box.Pose())
		if first == nil {
			first = local
			continue
		}
		if !spatialmath.OrientationAlmostEqual(local.Orientation(), first.Orientation()) {
			t.Fatalf("grasp %v: box in grasp frame %v, want %v", ox, local.Orientation(), first.Orientation())
		}
	}
}

func TestMugGeometriesHandleOnLeft(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 50}
	body := r3.Vector{X: 80, Y: 80, Z: 97}
	for _, yaw := range []float64{0, math.Pi / 2, math.Pi, -math.Pi / 4} {
		geos, err := MugGeometries(centroid, body, 39, 15, yaw, "mug")
		if err != nil {
			t.Fatalf("yaw %v: %v", yaw, err)
		}
		if len(geos) != 2 {
			t.Fatalf("yaw %v: got %d geometries, want 2", yaw, len(geos))
		}
		if got := geos[0].Pose().Point(); got.Sub(centroid).Norm() > 1e-6 {
			t.Errorf("yaw %v: body center %v, want %v", yaw, got, centroid)
		}
		// The arm's left is the heading turned +90 degrees.
		want := centroid.Add(r3.Vector{X: -math.Sin(yaw), Y: math.Cos(yaw)}.Mul(40 + 19.5))
		if got := geos[1].Pose().Point(); got.Sub(want).Norm() > 1e-6 {
			t.Errorf("yaw %v: handle center %v, want %v", yaw, got, want)
		}
		if got := geos[1].Pose().Point().Z; math.Abs(got-50) > 1e-6 {
			t.Errorf("yaw %v: handle Z %v, want 50", yaw, got)
		}
		// Width 15 along the heading, depth 39 out to the left, in the body's frame.
		dims := geos[1].ToProtobuf().GetBox().GetDimsMm()
		if dims.X != 15 || dims.Y != 39 || dims.Z != 97 {
			t.Errorf("yaw %v: handle dims %v, want (15,39,97)", yaw, dims)
		}
		if !spatialmath.OrientationAlmostEqual(geos[1].Pose().Orientation(), geos[0].Pose().Orientation()) {
			t.Errorf("yaw %v: handle orientation %v, want body's %v", yaw, geos[1].Pose().Orientation(), geos[0].Pose().Orientation())
		}
		if geos[1].Label() != "mug-handle" {
			t.Errorf("yaw %v: handle label %q", yaw, geos[1].Label())
		}
	}
	// GraspYaw is the approach heading (tool +Z): yaw 0 approaches along +X,
	// so the arm's left is +Y.
	yaw := GraspYaw(&spatialmath.OrientationVector{OX: 1})
	if yaw != 0 {
		t.Fatalf("GraspYaw of +X approach = %v, want 0", yaw)
	}
	geos, err := MugGeometries(r3.Vector{}, body, 39, 15, yaw, "mug")
	if err != nil {
		t.Fatal(err)
	}
	if got := geos[1].Pose().Point(); got.Y <= 0 || math.Abs(got.X) > 1e-6 {
		t.Errorf("handle at yaw 0 should be at +Y, got %v", got)
	}
	geos, err = MugGeometries(r3.Vector{}, body, 39, 15, math.Pi/2, "mug")
	if err != nil {
		t.Fatal(err)
	}
	if got := geos[1].Pose().Point(); got.X >= 0 || math.Abs(got.Y) > 1e-6 {
		t.Errorf("handle at yaw pi/2 should be at -X, got %v", got)
	}
}
