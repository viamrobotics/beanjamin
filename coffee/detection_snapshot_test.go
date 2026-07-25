package coffee

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang/geo/r3"
	"github.com/viam-labs/motion-tools/draw"
	drawv1 "github.com/viam-labs/motion-tools/draw/v1"
	commonv1 "go.viam.com/api/common/v1"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	"google.golang.org/protobuf/proto"
)

// detectionSnapshotTestFS builds a frame system with one world-anchored box
// ("shelf") and one prismatic frame ("arm") carrying a box, so a test can move
// the arm by supplying inputs.
func detectionSnapshotTestFS(t *testing.T) *referenceframe.FrameSystem {
	t.Helper()
	fs := referenceframe.NewEmptyFrameSystem("test")

	shelfGeom, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 500, Y: 500, Z: 20}, "shelf")
	if err != nil {
		t.Fatalf("new shelf box: %v", err)
	}
	shelf, err := referenceframe.NewStaticFrameWithGeometry("shelf", spatialmath.NewZeroPose(), shelfGeom)
	if err != nil {
		t.Fatalf("new shelf frame: %v", err)
	}
	if err := fs.AddFrame(shelf, fs.World()); err != nil {
		t.Fatalf("add shelf frame: %v", err)
	}

	armGeom, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 50, Y: 50, Z: 50}, "arm")
	if err != nil {
		t.Fatalf("new arm box: %v", err)
	}
	arm, err := referenceframe.NewTranslationalFrameWithGeometry(
		"arm", r3.Vector{X: 1}, referenceframe.Limit{Min: -1000, Max: 1000}, armGeom)
	if err != nil {
		t.Fatalf("new arm frame: %v", err)
	}
	if err := fs.AddFrame(arm, fs.World()); err != nil {
		t.Fatalf("add arm frame: %v", err)
	}
	return fs
}

// transformByName finds the snapshot transform drawn under the given entity name.
func transformByName(snapshot *draw.Snapshot, name string) *commonv1.Transform {
	for _, tf := range snapshot.Transforms() {
		if tf.GetReferenceFrame() == name {
			return tf
		}
	}
	return nil
}

// TestBuildDetectionSnapshot covers what the snapshot exists for: the frame
// system is drawn at the inputs the arm actually held during the observation
// (not at rest), and each detection contributes both its point cloud — anchored
// by the camera's world pose — and its world-frame bounding box.
func TestBuildDetectionSnapshot(t *testing.T) {
	fs := detectionSnapshotTestFS(t)
	fsInputs := referenceframe.NewZeroInputs(fs)
	fsInputs["arm"] = []referenceframe.Input{300}

	camToWorld := spatialmath.NewPose(
		r3.Vector{X: 100, Y: -50, Z: 400},
		&spatialmath.OrientationVectorDegrees{OZ: -1},
	)

	cloud := pointcloud.NewBasicEmpty()
	for _, p := range []r3.Vector{{X: 0, Y: 0, Z: 0}, {X: 10, Y: 10, Z: 10}} {
		if err := cloud.Set(p, pointcloud.NewBasicData()); err != nil {
			t.Fatalf("set point: %v", err)
		}
	}
	boxPose := spatialmath.NewPose(r3.Vector{X: 250, Y: 40, Z: 120}, &spatialmath.OrientationVectorDegrees{OZ: 1})
	box, err := spatialmath.NewBox(boxPose, r3.Vector{X: 70, Y: 70, Z: 100}, "cup")
	if err != nil {
		t.Fatalf("new detection box: %v", err)
	}

	snapshot, err := buildDetectionSnapshot("cup", fs, fsInputs, camToWorld, []detectionSnapshotItem{
		{cloud: cloud, box: box},
	})
	if err != nil {
		t.Fatalf("buildDetectionSnapshot: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot is not well-formed: %v", err)
	}

	// The arm's geometry must be drawn where the inputs put it (+300 in X), not at
	// its zero-input rest pose — the whole reason the snapshot carries inputs.
	armTF := transformByName(snapshot, "arm")
	if armTF == nil {
		t.Fatalf("arm geometry missing from snapshot")
	}
	armCenter := spatialmath.NewPoseFromProtobuf(armTF.GetPhysicalObject().GetCenter()).Point()
	if want := (r3.Vector{X: 300}); !spatialmath.R3VectorAlmostEqual(armCenter, want, 1e-6) {
		t.Errorf("arm drawn at %v, want %v (frame system was drawn at rest)", armCenter, want)
	}
	if transformByName(snapshot, "shelf") == nil {
		t.Errorf("shelf geometry missing from snapshot")
	}

	// The detection's bounding box carries its own world pose, so it is drawn at
	// the identity pose of the world frame.
	boxTF := transformByName(snapshot, "cup_0")
	if boxTF == nil {
		t.Fatalf("detection bounding box missing from snapshot")
	}
	if got := boxTF.GetPoseInObserverFrame().GetReferenceFrame(); got != referenceframe.World {
		t.Errorf("bounding box parent = %q, want %q", got, referenceframe.World)
	}
	gotBoxPose := spatialmath.NewPoseFromProtobuf(boxTF.GetPhysicalObject().GetCenter())
	if !spatialmath.PoseAlmostEqual(gotBoxPose, boxPose) {
		t.Errorf("bounding box drawn at %v, want %v", gotBoxPose, boxPose)
	}

	// The cloud keeps its camera-frame coordinates and is placed by the camera's
	// world pose.
	cloudTF := transformByName(snapshot, "cup_0_cloud")
	if cloudTF == nil {
		t.Fatalf("detection point cloud missing from snapshot")
	}
	if got := cloudTF.GetPoseInObserverFrame().GetReferenceFrame(); got != referenceframe.World {
		t.Errorf("point cloud parent = %q, want %q", got, referenceframe.World)
	}
	gotCloudPose := spatialmath.NewPoseFromProtobuf(cloudTF.GetPoseInObserverFrame().GetPose())
	if !spatialmath.PoseAlmostEqual(gotCloudPose, camToWorld) {
		t.Errorf("point cloud anchored at %v, want camera pose %v", gotCloudPose, camToWorld)
	}
	if cloudTF.GetPhysicalObject().GetPointcloud() == nil {
		t.Errorf("point cloud transform carries no point data")
	}
}

// TestBuildDetectionSnapshot_PartialItems verifies that a detection missing
// either half still contributes the half it has: a cloud with no usable geometry
// is drawn on its own, and a geometry with no cloud likewise.
func TestBuildDetectionSnapshot_PartialItems(t *testing.T) {
	fs := detectionSnapshotTestFS(t)
	fsInputs := referenceframe.NewZeroInputs(fs)

	cloud := pointcloud.NewBasicEmpty()
	if err := cloud.Set(r3.Vector{}, pointcloud.NewBasicData()); err != nil {
		t.Fatalf("set point: %v", err)
	}
	box, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 10, Y: 10, Z: 10}, "glass")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	snapshot, err := buildDetectionSnapshot("glass", fs, fsInputs, spatialmath.NewZeroPose(), []detectionSnapshotItem{
		{cloud: cloud},
		{box: box},
		{cloud: pointcloud.NewBasicEmpty()},
	})
	if err != nil {
		t.Fatalf("buildDetectionSnapshot: %v", err)
	}

	if transformByName(snapshot, "glass_0_cloud") == nil {
		t.Errorf("cloud-only detection 0 missing its point cloud")
	}
	if transformByName(snapshot, "glass_0") != nil {
		t.Errorf("detection 0 has no geometry, yet a bounding box was drawn")
	}
	if transformByName(snapshot, "glass_1") == nil {
		t.Errorf("box-only detection 1 missing its bounding box")
	}
	if transformByName(snapshot, "glass_1_cloud") != nil {
		t.Errorf("detection 1 has no cloud, yet a point cloud was drawn")
	}
	if transformByName(snapshot, "glass_2_cloud") != nil {
		t.Errorf("empty cloud of detection 2 should not be drawn")
	}
}

// TestSaveDetectionSnapshot writes a snapshot to disk and decodes it the way the
// visualizer does — gunzip, then parse the snapshot proto — so the on-disk
// artifact is verified, not just the in-memory scene.
func TestSaveDetectionSnapshot(t *testing.T) {
	dir := t.TempDir()
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t), cfg: &Config{SaveMotionRequestsDir: dir}}

	fs := detectionSnapshotTestFS(t)
	box, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 10, Y: 10, Z: 10}, "cup")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	c.saveDetectionSnapshot("cup", fs, referenceframe.NewZeroInputs(fs), spatialmath.NewZeroPose(),
		[]detectionSnapshotItem{{box: box}})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d files in %q, want 1", len(entries), dir)
	}
	if !strings.HasPrefix(entries[0].Name(), detectionSnapshotPrefix) {
		t.Errorf("wrote %q, which the visualizer's loader would reject", entries[0].Name())
	}

	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gunzip snapshot: %v", err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read gunzipped snapshot: %v", err)
	}
	var loaded drawv1.Snapshot
	if err := proto.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("parse snapshot proto: %v", err)
	}

	var names []string
	for _, tf := range loaded.GetTransforms() {
		names = append(names, tf.GetReferenceFrame())
	}
	for _, want := range []string{"arm", "shelf", "cup_0"} {
		if !slices.Contains(names, want) {
			t.Errorf("saved snapshot has transforms %v, missing %q", names, want)
		}
	}
}

// TestSaveDetectionSnapshot_Disabled verifies nothing is written when no
// motion-requests dir is configured.
func TestSaveDetectionSnapshot_Disabled(t *testing.T) {
	dir := t.TempDir()
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t), cfg: &Config{}}

	fs := detectionSnapshotTestFS(t)
	box, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 10, Y: 10, Z: 10}, "cup")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	c.saveDetectionSnapshot("cup", fs, referenceframe.NewZeroInputs(fs), spatialmath.NewZeroPose(),
		[]detectionSnapshotItem{{box: box}})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("wrote %d file(s) with no save dir configured", len(entries))
	}
}

// TestDetectionSnapshotPath checks the two things the visualizer's drag-and-drop
// loader cares about: the required filename prefix and an unambiguous extension
// (it splits the name on ".", so the timestamp must not contain one).
func TestDetectionSnapshotPath(t *testing.T) {
	at := time.Date(2026, 7, 25, 14, 3, 9, 456_000_000, time.UTC)
	got := detectionSnapshotPath("/tmp/motion", "cup", at)

	want := "/tmp/motion/visualization_snapshot_20260725_140309_456_cup.pb.gz"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	name := strings.TrimPrefix(got, "/tmp/motion/")
	if !strings.HasPrefix(name, detectionSnapshotPrefix) {
		t.Errorf("filename %q does not start with %q", name, detectionSnapshotPrefix)
	}
	if strings.Count(name, ".") != 2 {
		t.Errorf("filename %q must contain dots only in the .pb.gz extension", name)
	}
}
