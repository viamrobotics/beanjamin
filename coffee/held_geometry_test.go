package coffee

import (
	"context"
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/testutils/inject"
)

// requireVecEqual fails the test unless got is within tol of want. Wraps the
// shared vecAlmostEqual helper (served_shelf_test.go).
func requireVecEqual(t *testing.T, got, want r3.Vector, tol float64) {
	t.Helper()
	if !vecAlmostEqual(got, want, tol) {
		t.Fatalf("point = %v, want %v (tol %g)", got, want, tol)
	}
}

// heldGeomService builds a service around the given frame system.
func heldGeomService(t *testing.T, fs *referenceframe.FrameSystem) *beanjaminCoffee {
	t.Helper()
	return &beanjaminCoffee{
		cfg:      &Config{},
		logger:   logging.NewTestLogger(t),
		cachedFS: fs,
	}
}

// gripPointOffsetMm is how far past the claws the grip point sits along the tool
// axis, as on the real machine (see gripperFSWithHeldItem).
const gripPointOffsetMm = 25.0

// addGripPoint hangs grip-point off the claws at gripPointOffsetMm along the
// tool axis — the frame addHeldItemFrame places the held item on.
func addGripPoint(t *testing.T, fs *referenceframe.FrameSystem) {
	t.Helper()
	gp, err := referenceframe.NewStaticFrame(gripPoint, spatialmath.NewPoseFromPoint(r3.Vector{Z: gripPointOffsetMm}))
	if err != nil {
		t.Fatalf("new grip-point frame: %v", err)
	}
	if err := fs.AddFrame(gp, fs.Frame(componentClaws)); err != nil {
		t.Fatalf("add grip-point frame: %v", err)
	}
}

// clawsStaticFS returns world -> coffee-claws-middle (static at clawsPose) ->
// grip-point.
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
	addGripPoint(t, fs)
	return fs
}

// clawsRevoluteFS returns world -> j0 (revolute about Z) -> coffee-claws-middle
// (static offset +100mm along X from j0) -> grip-point, so the gripper position
// depends on j0.
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
	addGripPoint(t, fs)
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

// geometryToWorldInverse expresses a world-frame geometry in the grip-point
// frame — the transform attachDetectedGeometry performs before caching. Defined
// here so the round-trip test doesn't need a live arm (attachDetectedGeometry's
// only extra step is reading current joint inputs from the arm).
func geometryToWorldInverse(fs *referenceframe.FrameSystem, inputs referenceframe.FrameSystemInputs, worldGeom spatialmath.Geometry) (spatialmath.Geometry, error) {
	tf, err := fs.Transform(
		inputs.ToLinearInputs(),
		referenceframe.NewGeometriesInFrame(referenceframe.World, []spatialmath.Geometry{worldGeom}),
		gripPoint,
	)
	if err != nil {
		return nil, err
	}
	return tf.(*referenceframe.GeometriesInFrame).Geometries()[0], nil
}

// TestHeldItemFrameIsContainerAligned verifies the frame is attached on the grip
// point and rotated onto the container's axes — world-aligned, +Z up, at the
// grab — while the geometry stays exactly where it was. The rotation is what the
// no-spill goal cloud measures tilt against and the placement is what the pour
// poses command; neither may move the collision box.
func TestHeldItemFrameIsContainerAligned(t *testing.T) {
	// Claws well away from world-aligned: tool axis along world +X, plus a roll.
	clawsPose := spatialmath.NewPose(
		r3.Vector{X: 200, Y: -50, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 1, Theta: 30},
	)
	fs := clawsStaticFS(t, clawsPose)
	s := heldGeomService(t, fs)
	inputs := referenceframe.NewZeroInputs(fs)

	worldCenter := r3.Vector{X: 250, Y: 80, Z: 120}
	gripperLocal, err := geometryToWorldInverse(fs, inputs, testBox(t, spatialmath.NewPoseFromPoint(worldCenter)))
	if err != nil {
		t.Fatalf("to gripper frame: %v", err)
	}
	if err := s.addHeldItemFrame(gripperLocal); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}

	tf, err := fs.Transform(inputs.ToLinearInputs(),
		referenceframe.NewPoseInFrame(heldItemFrameName, spatialmath.NewZeroPose()), referenceframe.World)
	if err != nil {
		t.Fatalf("transform held-item to world: %v", err)
	}
	framePose := tf.(*referenceframe.PoseInFrame).Pose()

	// The frame reads back world-aligned even though the claws are not.
	if !spatialmath.OrientationAlmostEqual(framePose.Orientation(), spatialmath.NewZeroOrientation()) {
		t.Errorf("held-item world orientation = %v, want world-aligned",
			framePose.Orientation().OrientationVectorDegrees())
	}
	// Its origin is the grip point's, so a held-item pose places the same point a
	// grip-point pose would.
	gripPointWorld := spatialmath.Compose(clawsPose, spatialmath.NewPoseFromPoint(r3.Vector{Z: gripPointOffsetMm})).Point()
	requireVecEqual(t, framePose.Point(), gripPointWorld, 1e-6)
	// And the geometry is unmoved by the frame's pose: RDK places a frame's
	// geometry at the frame's parent, so it stays where the gripper-local pose
	// puts it.
	requireVecEqual(t, heldItemWorldCenter(t, fs, inputs), worldCenter, 1e-4)
}

// TestHeldItemTracksGripper verifies the attached geometry moves with the gripper
// as the joint angle changes.
func TestHeldItemTracksGripper(t *testing.T) {
	fs := clawsRevoluteFS(t)
	s := heldGeomService(t, fs)

	// Geometry at the grip point (grip-point-local zero pose).
	if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}

	// At j0=0 the claws sit at world (100,0,0), the grip point above them.
	at0 := referenceframe.NewZeroInputs(fs)
	requireVecEqual(t, heldItemWorldCenter(t, fs, at0), r3.Vector{X: 100, Z: gripPointOffsetMm}, 1e-4)

	// At j0=+90° the gripper (and held item) rotate to world (0,100,·).
	at90 := referenceframe.NewZeroInputs(fs)
	at90["j0"] = []referenceframe.Input{math.Pi / 2}
	requireVecEqual(t, heldItemWorldCenter(t, fs, at90), r3.Vector{Y: 100, Z: gripPointOffsetMm}, 1e-4)
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

// gripPointStaticFS returns world -> grip-point (static at gpPose).
func gripPointStaticFS(t *testing.T, gpPose spatialmath.Pose) *referenceframe.FrameSystem {
	t.Helper()
	fs := referenceframe.NewEmptyFrameSystem("test")
	gp, err := referenceframe.NewStaticFrame(gripPoint, gpPose)
	if err != nil {
		t.Fatalf("new grip-point frame: %v", err)
	}
	if err := fs.AddFrame(gp, fs.World()); err != nil {
		t.Fatalf("add grip-point frame: %v", err)
	}
	return fs
}

// TestConfiguredContainerBox verifies the modeled box is centered on the grasp
// centroid — the grip-point world position minus the grab offset, inverting
// composeCupPose — and sized from the configured dimensions. The glass case is
// what the with_glass override models.
func TestConfiguredContainerBox(t *testing.T) {
	for _, tc := range []struct {
		name         string
		label        string
		gripPoint    r3.Vector
		dims         *ContainerDimensions
		grab         *RelativePose
		wantCentroid r3.Vector
	}{
		{
			name:      "cup",
			label:     pickupLabelCup,
			gripPoint: r3.Vector{X: 170, Y: -300, Z: 250},
			dims:      &ContainerDimensions{DiameterMm: 60, HeightMm: 90},
			// The grab sends the grip point 5mm +X and 30mm -Z off the centroid.
			grab:         &RelativePose{X: 5, Z: -30},
			wantCentroid: r3.Vector{X: 165, Y: -300, Z: 280},
		},
		{
			name:         "glass",
			label:        pickupLabelGlass,
			gripPoint:    r3.Vector{X: 100, Y: -200, Z: 300},
			dims:         &ContainerDimensions{DiameterMm: 75, HeightMm: 140},
			grab:         &RelativePose{X: 10, Z: -40},
			wantCentroid: r3.Vector{X: 90, Y: -200, Z: 340},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := gripPointStaticFS(t, spatialmath.NewPoseFromPoint(tc.gripPoint))
			s := heldGeomService(t, fs)

			box, err := s.configuredContainerBox(fs, referenceframe.NewZeroInputs(fs), tc.label, tc.dims, tc.grab)
			if err != nil {
				t.Fatalf("configuredContainerBox: %v", err)
			}

			requireVecEqual(t, box.Pose().Point(), tc.wantCentroid, 1e-6)

			dims := box.ToProtobuf().GetBox().GetDimsMm()
			if dims == nil {
				t.Fatalf("expected a box geometry, got %v", box)
			}
			requireVecEqual(t, r3.Vector{X: dims.X, Y: dims.Y, Z: dims.Z},
				r3.Vector{X: tc.dims.DiameterMm, Y: tc.dims.DiameterMm, Z: tc.dims.HeightMm}, 1e-6)
		})
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

func TestHeldItemHalfHeightMm(t *testing.T) {
	fs := clawsStaticFS(t, spatialmath.NewZeroPose())
	s := heldGeomService(t, fs)

	// Nothing attached: no height available, callers fall back to a fixed offset.
	if _, ok := s.heldItemHalfHeightMm(); ok {
		t.Fatalf("expected ok=false when nothing is attached")
	}

	// Attached: half of the test box's 80mm Z extent, regardless of how the box
	// is rotated into the gripper frame (a Box keeps its dims under transform).
	rotated := spatialmath.NewPose(
		r3.Vector{X: 10, Y: 20, Z: 30},
		&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 37},
	)
	if err := s.addHeldItemFrame(testBox(t, rotated)); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}
	got, ok := s.heldItemHalfHeightMm()
	if !ok {
		t.Fatalf("expected ok=true when attached")
	}
	if math.Abs(got-40) > 1e-6 {
		t.Fatalf("half-height = %g, want 40", got)
	}
}

func TestServingAreaShieldCollisions(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}

	// Detached (the retreat): gripper + claws pairs only, no held-item pair.
	got := s.servingAreaShieldCollisions()
	if len(got) != 3 {
		t.Fatalf("expected 3 pairs when detached, got %d (%v)", len(got), got)
	}
	for _, ac := range got {
		if ac.Frame2 != servingAreaShieldFrameName {
			t.Fatalf("expected Frame2=%q, got %q", servingAreaShieldFrameName, ac.Frame2)
		}
		if ac.Frame1 == heldItemFrameName {
			t.Fatalf("held-item pair must be omitted when detached, got %v", got)
		}
	}

	// Attached (the descent): adds the held-item↔shield pair.
	s.heldItemAttached = true
	got = s.servingAreaShieldCollisions()
	if len(got) != 4 {
		t.Fatalf("expected 4 pairs when attached, got %d (%v)", len(got), got)
	}
	var hasHeld bool
	for _, ac := range got {
		if ac.Frame1 == heldItemFrameName && ac.Frame2 == servingAreaShieldFrameName {
			hasHeld = true
		}
	}
	if !hasHeld {
		t.Fatalf("expected held-item↔shield pair when attached, got %v", got)
	}
}

// Empty jaws and no glass configured: there is nothing to model the hand-loaded
// vessel from, and dropping the filter alone would leave the arm carrying an
// invisible glass.
func TestSwapFilterForGlassNeedsGlassConfig(t *testing.T) {
	s := heldGeomService(t, filterOnArmFS(t))

	if _, err := s.swapFilterForGlass(context.Background()); err == nil {
		t.Error("swapFilterForGlass succeeded without glass_dimensions, want an error")
	}
	if s.cachedFS.Frame(componentFilter) == nil {
		t.Error("filter was left detached after the refusal")
	}
}

// A glass the vision pickup already attached is the real detection; the swap
// drops only the filter and leaves it — and the restore must not take it away.
func TestSwapFilterForGlassKeepsAttachedItem(t *testing.T) {
	s := heldGeomService(t, filterOnArmFS(t))
	if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}
	s.cacheHeldGeometry(pickupLabelGlass, testBox(t, spatialmath.NewZeroPose()))

	restore, err := s.swapFilterForGlass(context.Background())
	if err != nil {
		t.Fatalf("swapFilterForGlass: %v", err)
	}
	if s.cachedFS.Frame(componentFilter) != nil {
		t.Error("filter still modeled during the swap")
	}
	if !s.heldItemAttached || s.cachedFS.Frame(heldItemFrameName) == nil {
		t.Error("the already-attached glass was dropped by the swap")
	}

	restore()
	if !s.heldItemAttached || s.cachedFS.Frame(heldItemFrameName) == nil {
		t.Error("the already-attached glass was dropped by the restore")
	}
	if s.heldGlassGeom == nil {
		t.Error("the cached glass grasp was cleared by the restore")
	}
	if s.cachedFS.Frame(componentFilter) == nil {
		t.Error("filter was not restored")
	}
}

// glassSwapService builds a service swapFilterForGlass can run against: a
// portafilter on the claws, a grip point to recover the grasp centroid from
// (clawsStaticFS's), and an arm with no joints to read inputs off.
func glassSwapService(t *testing.T) *beanjaminCoffee {
	t.Helper()
	s := heldGeomService(t, filterOnArmFS(t))
	s.cfg.GlassDimensions = &ContainerDimensions{DiameterMm: 75, HeightMm: 140}
	s.cfg.GlassGrabRelativePose = &RelativePose{}
	s.arm = &inject.Arm{CurrentInputsFunc: func(context.Context) ([]referenceframe.Input, error) {
		return nil, nil
	}}
	return s
}

// The stand-in must not displace a grasp the vision pickup cached, and must not
// survive the action that borrowed it.
func TestSwapFilterForGlassKeepsCachedGrasp(t *testing.T) {
	s := glassSwapService(t)
	cached := testBox(t, spatialmath.NewZeroPose())
	s.cacheHeldGeometry(pickupLabelGlass, cached)

	restore, err := s.swapFilterForGlass(context.Background())
	if err != nil {
		t.Fatalf("swapFilterForGlass: %v", err)
	}
	if s.heldGlassGeom != cached {
		t.Error("the stand-in displaced the cached glass grasp")
	}
	if !s.heldItemAttached {
		t.Error("the stand-in was not attached")
	}

	restore()
	if s.heldItemAttached {
		t.Error("the stand-in outlived the action")
	}
	if s.heldGlassGeom != cached {
		t.Error("the restore cleared the cached glass grasp")
	}
}

// An action that grabs something for real leaves the arm holding it, so the
// stand-in's restore must leave that attachment alone.
func TestSwapFilterForGlassKeepsItemAttachedByTheAction(t *testing.T) {
	s := glassSwapService(t)

	restore, err := s.swapFilterForGlass(context.Background())
	if err != nil {
		t.Fatalf("swapFilterForGlass: %v", err)
	}
	// Stand in for the action re-grabbing the real glass mid-call.
	if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}

	restore()
	if !s.heldItemAttached || s.cachedFS.Frame(heldItemFrameName) == nil {
		t.Error("the restore detached the item the action grabbed")
	}
}
