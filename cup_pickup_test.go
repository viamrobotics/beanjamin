package beanjamin

import (
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

func TestSelectCupCentroid_Empty(t *testing.T) {
	_, idx, err := selectCupCentroid(nil, r3.Vector{}, 100)
	if err == nil {
		t.Fatalf("expected error on empty input")
	}
	if idx != -1 {
		t.Fatalf("expected idx -1 on error, got %d", idx)
	}
}

func TestSelectCupCentroid_SingleInRange(t *testing.T) {
	c := []r3.Vector{{X: 110, Y: 0, Z: 0}}
	got, idx, err := selectCupCentroid(c, r3.Vector{X: 100, Y: 0, Z: 0}, 50)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 0 {
		t.Fatalf("expected index 0, got %d", idx)
	}
	if got != c[0] {
		t.Fatalf("expected centroid %v, got %v", c[0], got)
	}
}

func TestSelectCupCentroid_SingleOutOfRange(t *testing.T) {
	c := []r3.Vector{{X: 1000, Y: 0, Z: 0}}
	_, idx, err := selectCupCentroid(c, r3.Vector{}, 100)
	if err == nil || !strings.Contains(err.Error(), "within") {
		t.Fatalf("expected 'within' error, got %v", err)
	}
	if idx != -1 {
		t.Fatalf("expected idx -1 on error, got %d", idx)
	}
}

func TestSelectCupCentroid_PicksClosest(t *testing.T) {
	c := []r3.Vector{
		{X: 200, Y: 0, Z: 0}, // 100mm from target — farther
		{X: 110, Y: 0, Z: 0}, // 10mm from target — closer
		{X: 150, Y: 0, Z: 0}, // 50mm from target
	}
	target := r3.Vector{X: 100, Y: 0, Z: 0}
	got, idx, err := selectCupCentroid(c, target, 300)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	if got != c[1] {
		t.Fatalf("expected centroid %v, got %v", c[1], got)
	}
}

func TestSelectCupCentroid_AllOutOfRange(t *testing.T) {
	c := []r3.Vector{
		{X: 1000, Y: 0, Z: 0},
		{X: 2000, Y: 0, Z: 0},
	}
	_, idx, err := selectCupCentroid(c, r3.Vector{}, 100)
	if err == nil || !strings.Contains(err.Error(), "within") {
		t.Fatalf("expected 'within' error, got %v", err)
	}
	if idx != -1 {
		t.Fatalf("expected idx -1 on error, got %d", idx)
	}
}

func TestSelectCupCentroid_ZeroMaxMeansNoCutoff(t *testing.T) {
	c := []r3.Vector{
		{X: 1e6, Y: 0, Z: 0},
		{X: 100, Y: 0, Z: 0},
	}
	got, idx, err := selectCupCentroid(c, r3.Vector{}, 0)
	if err != nil {
		t.Fatalf("expected no error with maxDistMm=0, got %v", err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	if got != c[1] {
		t.Fatalf("expected closest centroid, got %v", got)
	}
}

func TestSelectCupCentroid_TieBreaksFirst(t *testing.T) {
	c := []r3.Vector{
		{X: 110, Y: 0, Z: 0},
		{X: 90, Y: 0, Z: 0}, // both 10mm from target
	}
	target := r3.Vector{X: 100, Y: 0, Z: 0}
	got, idx, err := selectCupCentroid(c, target, 50)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 0 {
		t.Fatalf("expected first-wins (index 0), got %d", idx)
	}
	if got != c[0] {
		t.Fatalf("expected first centroid, got %v", got)
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
