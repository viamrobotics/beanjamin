package beanjamin

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// requireVecEqual fails the test unless got is within tol of want. Wraps the
// shared vecAlmostEqual helper (served_shelf_test.go).
func requireVecEqual(t *testing.T, got, want r3.Vector, tol float64) {
	t.Helper()
	if !vecAlmostEqual(got, want, tol) {
		t.Fatalf("point = %v, want %v (tol %g)", got, want, tol)
	}
}

// heldGeomService builds a tracking-enabled service around the given frame
// system.
func heldGeomService(t *testing.T, fs *referenceframe.FrameSystem) *beanjaminCoffee {
	t.Helper()
	return &beanjaminCoffee{
		cfg:      &Config{TrackHeldGeometry: true},
		logger:   logging.NewTestLogger(t),
		cachedFS: fs,
	}
}

// clawsStaticFS returns world -> coffee-claws-middle (static at clawsPose).
func clawsStaticFS(t *testing.T, clawsPose spatialmath.Pose) *referenceframe.FrameSystem {
	t.Helper()
	fs := referenceframe.NewEmptyFrameSystem("test")
	claws, err := referenceframe.NewStaticFrame(componentClaws, clawsPose)
	if err != nil {
		t.Fatalf("new claws frame: %v", err)
	}
	if err := fs.AddFrame(claws, fs.World()); err != nil {
		t.Fatalf("add claws frame: %v", err)
	}
	return fs
}

// clawsRevoluteFS returns world -> j0 (revolute about Z) -> coffee-claws-middle
// (static offset +100mm along X from j0), so the gripper position depends on j0.
func clawsRevoluteFS(t *testing.T) *referenceframe.FrameSystem {
	t.Helper()
	fs := referenceframe.NewEmptyFrameSystem("test")
	wide := referenceframe.Limit{Min: -2 * math.Pi, Max: 2 * math.Pi}
	j0, err := referenceframe.NewRotationalFrame("j0", spatialmath.R4AA{Theta: 0, RX: 0, RY: 0, RZ: 1}, wide)
	if err != nil {
		t.Fatalf("new j0: %v", err)
	}
	if err := fs.AddFrame(j0, fs.World()); err != nil {
		t.Fatalf("add j0: %v", err)
	}
	claws, err := referenceframe.NewStaticFrame(componentClaws, spatialmath.NewPoseFromPoint(r3.Vector{X: 100}))
	if err != nil {
		t.Fatalf("new claws frame: %v", err)
	}
	if err := fs.AddFrame(claws, j0); err != nil {
		t.Fatalf("add claws frame: %v", err)
	}
	return fs
}

func testBox(t *testing.T, pose spatialmath.Pose) spatialmath.Geometry {
	t.Helper()
	box, err := spatialmath.NewBox(pose, r3.Vector{X: 40, Y: 40, Z: 80}, "cup")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return box
}

// heldItemWorldCenter returns the world-frame center of the held-item geometry
// at the given inputs, failing if the frame or its geometry is absent.
func heldItemWorldCenter(t *testing.T, fs *referenceframe.FrameSystem, inputs referenceframe.FrameSystemInputs) r3.Vector {
	t.Helper()
	all, err := referenceframe.FrameSystemGeometries(fs, inputs)
	if err != nil {
		t.Fatalf("frame system geometries: %v", err)
	}
	gif, ok := all[heldItemFrameName]
	if !ok {
		t.Fatalf("held-item frame has no geometry in frame system")
	}
	geos := gif.Geometries()
	if len(geos) == 0 {
		t.Fatalf("held-item geometry empty")
	}
	return geos[0].Pose().Point()
}

// TestAttachRoundTrip exercises the same world->gripper->world math
// attachDetectedGeometry uses: a geometry given in world coordinates, expressed
// in the gripper frame and re-added as the held-item frame, reads back at the
// same inputs at its original world pose.
func TestAttachRoundTrip(t *testing.T) {
	// Non-trivial gripper pose (translation + 90° about Z).
	clawsPose := spatialmath.NewPose(
		r3.Vector{X: 200, Y: -50, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 90},
	)
	fs := clawsStaticFS(t, clawsPose)
	s := heldGeomService(t, fs)
	inputs := referenceframe.NewZeroInputs(fs)

	worldCenter := r3.Vector{X: 250, Y: 80, Z: 120}
	worldGeom := testBox(t, spatialmath.NewPoseFromPoint(worldCenter))

	gripperLocal, err := geometryToWorldInverse(fs, inputs, worldGeom)
	if err != nil {
		t.Fatalf("to gripper frame: %v", err)
	}
	if err := s.addHeldItemFrame(gripperLocal); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}

	got := heldItemWorldCenter(t, fs, inputs)
	requireVecEqual(t, got, worldCenter, 1e-4)
}

// geometryToWorldInverse expresses a world-frame geometry in the gripper frame —
// the transform attachDetectedGeometry performs before caching. Defined here so
// the round-trip test doesn't need a live arm (attachDetectedGeometry's only
// extra step is reading current joint inputs from the arm).
func geometryToWorldInverse(fs *referenceframe.FrameSystem, inputs referenceframe.FrameSystemInputs, worldGeom spatialmath.Geometry) (spatialmath.Geometry, error) {
	tf, err := fs.Transform(
		inputs.ToLinearInputs(),
		referenceframe.NewGeometriesInFrame(referenceframe.World, []spatialmath.Geometry{worldGeom}),
		componentClaws,
	)
	if err != nil {
		return nil, err
	}
	return tf.(*referenceframe.GeometriesInFrame).Geometries()[0], nil
}

// TestHeldItemTracksGripper verifies the attached geometry moves with the gripper
// as the joint angle changes.
func TestHeldItemTracksGripper(t *testing.T) {
	fs := clawsRevoluteFS(t)
	s := heldGeomService(t, fs)

	// Geometry at the gripper origin (gripper-local zero pose).
	if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}

	// At j0=0 the gripper sits at world (100,0,0).
	at0 := referenceframe.NewZeroInputs(fs)
	requireVecEqual(t, heldItemWorldCenter(t, fs, at0), r3.Vector{X: 100}, 1e-4)

	// At j0=+90° the gripper (and held item) rotate to world (0,100,0).
	at90 := referenceframe.NewZeroInputs(fs)
	at90["j0"] = []referenceframe.Input{math.Pi / 2}
	requireVecEqual(t, heldItemWorldCenter(t, fs, at90), r3.Vector{Y: 100}, 1e-4)
}

func TestDetachRemovesFrame(t *testing.T) {
	fs := clawsStaticFS(t, spatialmath.NewZeroPose())
	s := heldGeomService(t, fs)
	if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}
	if !s.heldItemAttached {
		t.Fatalf("expected heldItemAttached after attach")
	}
	s.detachHeldGeometry()
	if s.heldItemAttached {
		t.Fatalf("expected !heldItemAttached after detach")
	}
	if fs.Frame(heldItemFrameName) != nil {
		t.Fatalf("expected held-item frame removed from frame system")
	}
	// Idempotent.
	s.detachHeldGeometry()
}

func TestReattachUsesCache(t *testing.T) {
	fs := clawsStaticFS(t, spatialmath.NewZeroPose())
	s := heldGeomService(t, fs)

	// Nothing cached -> no-op.
	if err := s.reattachGeometry(pickupLabelCup); err != nil {
		t.Fatalf("reattach with empty cache: %v", err)
	}
	if s.heldItemAttached {
		t.Fatalf("reattach must be a no-op when nothing is cached")
	}

	s.cacheHeldGeometry(pickupLabelCup, testBox(t, spatialmath.NewZeroPose()))
	if err := s.reattachGeometry(pickupLabelCup); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if !s.heldItemAttached || fs.Frame(heldItemFrameName) == nil {
		t.Fatalf("expected held-item frame present after reattach")
	}
}

func TestReattachNoopWhenTrackingOff(t *testing.T) {
	fs := clawsStaticFS(t, spatialmath.NewZeroPose())
	s := heldGeomService(t, fs)
	s.cfg.TrackHeldGeometry = false
	s.cacheHeldGeometry(pickupLabelCup, testBox(t, spatialmath.NewZeroPose()))
	if err := s.reattachGeometry(pickupLabelCup); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if s.heldItemAttached {
		t.Fatalf("reattach must be a no-op when tracking is off")
	}
}

func TestClearHeldGeometry(t *testing.T) {
	fs := clawsStaticFS(t, spatialmath.NewZeroPose())
	s := heldGeomService(t, fs)
	s.cacheHeldGeometry(pickupLabelCup, testBox(t, spatialmath.NewZeroPose()))
	s.cacheHeldGeometry(pickupLabelGlass, testBox(t, spatialmath.NewZeroPose()))
	if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}

	s.clearHeldGeometry()
	if s.heldItemAttached {
		t.Fatalf("expected !heldItemAttached after clear")
	}
	if s.heldCupGeom != nil || s.heldGlassGeom != nil {
		t.Fatalf("expected caches cleared, got cup=%v glass=%v", s.heldCupGeom, s.heldGlassGeom)
	}
}

func TestCacheRoutingByLabel(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	cup := testBox(t, spatialmath.NewZeroPose())
	glass := testBox(t, spatialmath.NewPoseFromPoint(r3.Vector{Z: 10}))
	s.cacheHeldGeometry(pickupLabelCup, cup)
	s.cacheHeldGeometry(pickupLabelGlass, glass)
	if s.cachedHeldGeometry(pickupLabelCup) != cup {
		t.Fatalf("cup cache mismatch")
	}
	if s.cachedHeldGeometry(pickupLabelGlass) != glass {
		t.Fatalf("glass cache mismatch")
	}
}

func TestHeldItemSelfCollisions(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	if got := s.heldItemSelfCollisions(); got != nil {
		t.Fatalf("expected nil when not attached, got %v", got)
	}
	s.heldItemAttached = true
	got := s.heldItemSelfCollisions()
	if len(got) != 3 {
		t.Fatalf("expected 3 self-collision pairs, got %d (%v)", len(got), got)
	}
	for _, ac := range got {
		if ac.Frame1 != heldItemFrameName {
			t.Fatalf("expected Frame1=%q, got %q", heldItemFrameName, ac.Frame1)
		}
	}
}

func TestAppendHeldItemCollisions(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	base := []AllowedCollision{{Frame1: "a", Frame2: "b"}}

	// Not attached: returned unchanged.
	if got := s.appendHeldItemCollisions(base); len(got) != 1 {
		t.Fatalf("expected passthrough when not attached, got %v", got)
	}

	// Attached: base + 3 self pairs, and input is not mutated.
	s.heldItemAttached = true
	got := s.appendHeldItemCollisions(base)
	if len(got) != 4 {
		t.Fatalf("expected 4 pairs, got %d (%v)", len(got), got)
	}
	if len(base) != 1 {
		t.Fatalf("input slice was mutated: %v", base)
	}
}

func TestHeldItemSurfaceCollisions(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	pairs := []AllowedCollision{{Frame1: heldItemFrameName, Frame2: "serving-area"}}
	if got := s.heldItemSurfaceCollisions(pairs); got != nil {
		t.Fatalf("expected nil when not attached, got %v", got)
	}
	s.heldItemAttached = true
	if got := s.heldItemSurfaceCollisions(pairs); len(got) != 1 {
		t.Fatalf("expected the pairs when attached, got %v", got)
	}
}

func TestGeometryToWorld(t *testing.T) {
	// world -> camera at (100,0,0); a geometry at camera-local (10,0,0) lifts to
	// world (110,0,0) with dimensions preserved.
	fs := referenceframe.NewEmptyFrameSystem("test")
	cam, err := referenceframe.NewStaticFrame("camera", spatialmath.NewPoseFromPoint(r3.Vector{X: 100}))
	if err != nil {
		t.Fatalf("new camera frame: %v", err)
	}
	if err := fs.AddFrame(cam, fs.World()); err != nil {
		t.Fatalf("add camera frame: %v", err)
	}
	inputs := referenceframe.NewZeroInputs(fs)

	local := testBox(t, spatialmath.NewPoseFromPoint(r3.Vector{X: 10}))
	world, err := geometryToWorld(fs, inputs, "camera", local)
	if err != nil {
		t.Fatalf("geometryToWorld: %v", err)
	}
	requireVecEqual(t, world.Pose().Point(), r3.Vector{X: 110}, 1e-4)

	box := world.ToProtobuf().GetBox()
	if box == nil || box.DimsMm == nil {
		t.Fatalf("expected a box geometry, got %v", world.ToProtobuf())
	}
	if box.DimsMm.X != 40 || box.DimsMm.Y != 40 || box.DimsMm.Z != 80 {
		t.Fatalf("dimensions not preserved: %v", box.DimsMm)
	}
}

func TestGeometryToWorld_MissingFrame(t *testing.T) {
	fs := referenceframe.NewEmptyFrameSystem("test")
	inputs := referenceframe.NewZeroInputs(fs)
	_, err := geometryToWorld(fs, inputs, "no-such-camera", testBox(t, spatialmath.NewZeroPose()))
	if err == nil {
		t.Fatalf("expected error for missing camera frame")
	}
}
