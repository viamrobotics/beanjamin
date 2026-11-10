package coffee

// Held-item geometry tracking.
//
// The cup or glass the gripper picks up is added to the scene's frame system as a
// static "held-item" frame parented to the grip point (gripPoint), carrying the
// detection geometry expressed in the grip-point frame ("gripper-local"
// throughout this file). While it is
// present, every motion plan routes around the held item — so the arm doesn't
// drive the cup into the machine, the shelf, or itself while carrying it.
//
// The frame is attached rotated (heldItemFramePose) so its own +Z is the
// container's vertical axis rather than the gripper's tool axis. No collision
// geometry depends on that rotation, but the no-spill carry commands this frame
// by name, and its orientation bound only bounds the drink's tilt when measured
// about the container's own axis (see noSpillOrientationToleranceDegs in
// motion.go).
//
// The geometry is attached on grab (attachDetectedGeometry for a fresh vision
// detection, reattachGeometry when re-grabbing an item whose geometry was already
// cached this order) and removed on release (detachHeldGeometry). The grasp is
// assumed consistent across release/re-grab — the item is released and re-grabbed
// at the same pose — so the gripper-local geometry cached at pickup is reused
// verbatim on the re-grab. The staged glass is the exception: it is seated on
// the table when released (stageHeldGlass), and its re-grab is modeled from
// where it stands (regrabStagedGlass).
//
// This mirrors lockFilterFrame's frame-system mutation in motion.go, but the
// parent is the moving gripper rather than the world, and the geometry comes
// from vision rather than a part config.

import (
	"context"
	"fmt"
	"sort"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"

	"beanjamin/coffee/geom"
)

// heldItemFrameName is the static frame added under gripPoint carrying the
// geometry of the cup/glass currently held by the gripper.
const heldItemFrameName = "held-item"

// stagedGlassFrameName is the static World-parented frame carrying a glass set
// down in the staging area, so it stays a collision obstacle once released.
const stagedGlassFrameName = "staged-glass"

// attachDetectedGeometry records the vision-detected geometry of a freshly
// grabbed item (geomWorld, in world coordinates as lifted in observeVantage)
// and attaches it to the gripper with the arm where the moves handed off so
// far leave it (expectedInputs, sceneState.attachHeldItem). No-op when
// geomWorld is nil (no detection geometry available).
func (s *beanjaminCoffee) attachDetectedGeometry(ctx context.Context, label string, geomWorld spatialmath.Geometry) error {
	if geomWorld == nil {
		return nil
	}
	fsInputs, err := s.expectedInputs(ctx)
	if err != nil {
		return err
	}
	var gripperLocal spatialmath.Geometry
	if err := s.editScene(func(sc *sceneState) error {
		gripperLocal, err = sc.attachHeldItem(fsInputs, label, geomWorld)
		return err
	}); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("attached %s geometry to gripper (center rel. grip point %v)", label, gripperLocal.Pose().Point())
	return nil
}

// attachHeldItem adds the held-item frame carrying geomWorld, a world-frame
// geometry, with the frames standing at fsInputs. The geometry is expressed in
// the grip-point frame and cached by label, so a later re-grab of the same item
// (reattachHeldItem) can restore it without re-detecting. Returns that
// gripper-local geometry.
func (sc *sceneState) attachHeldItem(
	fsInputs referenceframe.FrameSystemInputs, label string, geomWorld spatialmath.Geometry,
) (spatialmath.Geometry, error) {
	// Cached in grip-point coordinates, the geometry tracks the gripper as it
	// moves: FrameSystemGeometries places it at gripPoint_to_world ∘
	// gripperLocalPose, whatever transform the held-item frame itself carries.
	tf, err := sc.fs.Transform(
		fsInputs.ToLinearInputs(),
		referenceframe.NewGeometriesInFrame(referenceframe.World, []spatialmath.Geometry{geomWorld}),
		gripPoint,
	)
	if err != nil {
		return nil, fmt.Errorf("transform %s geometry into gripper frame: %w", label, err)
	}
	geos := tf.(*referenceframe.GeometriesInFrame).Geometries()
	if len(geos) == 0 {
		return nil, fmt.Errorf("no %s geometry after transform into gripper frame", label)
	}
	gripperLocal := geos[0]
	if err := sc.addHeldItemFrame(gripperLocal); err != nil {
		return nil, err
	}
	sc.cacheHeldGeometry(label, gripperLocal)
	return gripperLocal, nil
}

// reattachGeometry restores the held-item frame for an item being re-grabbed
// (the brewed cup from under the machine) using the geometry
// cached at its initial pickup. No-op when nothing was cached for this label
// (e.g. a manually-stepped serving that never ran the vision pickup, or a frame
// system reset that dropped the cached grasp).
func (s *beanjaminCoffee) reattachGeometry(label string) error {
	if s.scene().cachedHeldGeometry(label) == nil {
		s.activeOrderLogger().Debugf("reattach %s geometry: nothing cached, skipping", label)
		return nil
	}
	if err := s.editScene(func(sc *sceneState) error {
		_, err := sc.reattachHeldItem(label)
		return err
	}); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("re-attached cached %s geometry to gripper", label)
	return nil
}

// reattachHeldItem adds the held-item frame carrying the geometry cached for
// label, and reports whether one was cached.
func (sc *sceneState) reattachHeldItem(label string) (bool, error) {
	gripperLocal := sc.cachedHeldGeometry(label)
	if gripperLocal == nil {
		return false, nil
	}
	return true, sc.addHeldItemFrame(gripperLocal)
}

// attachConfiguredCupGeometry models the held cup from cup_dimensions and
// attaches it, for the case where reattachGeometry found nothing cached to
// restore — e.g. a manually-stepped serving that skipped the vision cup pickup,
// or a frame-system reset that dropped the cached grasp. Without it the pour (and
// the hot-drink shelf placement) would carry the full cup as an untracked
// grip-point move, so motion plans wouldn't route the cup around the staged glass
// or the machine.
func (s *beanjaminCoffee) attachConfiguredCupGeometry(ctx context.Context) error {
	return s.attachConfiguredGeometry(ctx, pickupLabelCup, s.cfg.CupDimensions, s.cfg.CupGrabRelativePose)
}

// attachConfiguredGlassGeometry is attachConfiguredCupGeometry for an iced glass,
// modeled from glass_dimensions. Its caller is swapFilterForGlass, not the iced
// flow — there the vision pickup attaches the glass it actually detected.
func (s *beanjaminCoffee) attachConfiguredGlassGeometry(ctx context.Context) error {
	return s.attachConfiguredGeometry(ctx, pickupLabelGlass, s.cfg.GlassDimensions, s.cfg.GlassGrabRelativePose)
}

// attachConfiguredGeometry models a held container of the configured size at the
// grasp centroid recovered from the current grip-point pose, and attaches it.
func (s *beanjaminCoffee) attachConfiguredGeometry(
	ctx context.Context, label string, dims *ContainerDimensions, grabRel *RelativePose,
) error {
	fsInputs, err := s.expectedInputs(ctx)
	if err != nil {
		return err
	}
	if err := s.editScene(func(sc *sceneState) error {
		box, err := sc.containerBox(fsInputs, label, dims, grabRel)
		if err != nil {
			return err
		}
		_, err = sc.attachHeldItem(fsInputs, label, box)
		return err
	}); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("modeled the held %s from the configured %.0f×%.0f mm for held-item tracking",
		label, dims.DiameterMm, dims.HeightMm)
	return nil
}

// containerBox builds the world-frame box of a container of the given
// dimensions held by the gripper, with the frames standing at fsInputs. It is
// modeled exactly as geom.ContainerBox models it at pickup: an upright box
// centered on the grasp centroid. The centroid is recovered by inverting the
// geom.PoseRelativeTo the grab used — the grab sends the grip point to
// centroid + grabRel, so the centroid is the grip-point world position minus
// that offset.
func (sc *sceneState) containerBox(
	fsInputs referenceframe.FrameSystemInputs, label string, dims *ContainerDimensions, grabRel *RelativePose,
) (spatialmath.Geometry, error) {
	gripPointPIF := referenceframe.NewPoseInFrame(gripPoint, spatialmath.NewZeroPose())
	tf, err := sc.fs.Transform(fsInputs.ToLinearInputs(), gripPointPIF, referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("transform grip point to world: %w", err)
	}
	gripPointWorld := tf.(*referenceframe.PoseInFrame).Pose().Point()

	// The grab poses are required by Validate wherever their pickup is configured,
	// so they are non-nil on any machine that reaches the matching flow.
	grabOffset := relativePoseToSpatial(grabRel).Point()
	return geom.ContainerBox(gripPointWorld.Sub(grabOffset), dims.boxDims(), containerYaw(grabRel), label)
}

// containerYaw returns the heading, in radians about world Z, that the box for
// a container grabbed at grabRel is built at: the grab's approach heading
// (geom.GraspYaw). geom.PoseRelativeTo composes grabRel onto an identity-oriented
// anchor, so the grab's world orientation is grabRel's own wherever the
// container stands. 0 when no grab pose is configured.
func containerYaw(grabRel *RelativePose) float64 {
	if grabRel == nil {
		return 0
	}
	return geom.GraspYaw(relativePoseToSpatial(grabRel).Orientation())
}

// withGlassActions are the actions with_glass may be passed to: the steps run
// with a glass loaded into the jaws by hand and the portafilter lifted off.
//
// An allowlist rather than a per-action opt-out, because the flag is an
// assertion about the physical world that only some steps can be making. An
// action that does its own pickup starts with empty jaws by definition, and
// hanging a stand-in where the real glass is about to be grasped puts a phantom
// in the free approach plan — inside the clean-area shield, which is hard
// there — so every candidate fails to plan. It would also hand the grasp descent
// the held-item shield exemption that pickupAreaShieldCollisions withholds until
// after the grab. Use lock_portafilter + release_filter to reach empty jaws.
var withGlassActions = map[string]bool{
	"move_to_ice_dispense": true,
	"dispense_ice":         true,
	"stage_glass":          true,
	// These plan no motion, so the flag does nothing for them. Allowed anyway:
	// they belong to the same stepping sequence, and making them its two
	// exceptions is a trap for an operator told to pass the flag on every call.
	"pulse_ice_pin":   true,
	"check_ice_level": true,
}

// checkWithGlassAllowed refuses with_glass on an action outside
// withGlassActions, naming the ones that accept it.
func checkWithGlassAllowed(name string) error {
	if withGlassActions[name] {
		return nil
	}
	allowed := make([]string, 0, len(withGlassActions))
	for k := range withGlassActions {
		allowed = append(allowed, k)
	}
	sort.Strings(allowed)
	return fmt.Errorf("with_glass is not accepted by action %q: it asserts a glass loaded into the jaws by hand, which only these steps can be run with: %v", name, allowed)
}

// swapFilterForGlass models the claws as holding a glass instead of the
// portafilter for the span of one action, and returns the restore for the caller
// to defer. It is what an operator stepping the iced sequence by hand needs: the
// frame system still models the filter they lifted off at rest, and the filter
// cannot follow a glass in under the ice chute.
//
// A glass already modeled in the gripper — the vision pickup's, from a
// fetch_glass earlier in the same session — is left exactly as it is, and only
// the filter comes out. A real detection beats a box built from config, and the
// restore must not take a held item away from the caller that grabbed it.
func (s *beanjaminCoffee) swapFilterForGlass(ctx context.Context) (func(), error) {
	restoreFilter, err := s.dropFilter()
	if err != nil {
		return nil, fmt.Errorf("with_glass: %w", err)
	}
	if s.scene().heldItemAttached {
		s.activeOrderLogger().Infof("with_glass: the gripper already models a held item, dropping only the portafilter")
		return restoreFilter, nil
	}
	if s.cfg.GlassDimensions == nil || s.cfg.GlassGrabRelativePose == nil {
		restoreFilter()
		return nil, fmt.Errorf("with_glass: nothing is modeled in the gripper and there is no glass to model one from; set glass_dimensions and glass_grab_relative_pose (can_serve_iced)")
	}
	cachedGlass := s.scene().heldGlassGeom
	if err := s.attachConfiguredGlassGeometry(ctx); err != nil {
		restoreFilter()
		return nil, fmt.Errorf("with_glass: %w", err)
	}
	// The box is fabricated from config, not detected, so put the cache back the
	// way the attach found it: a grasp a vision pickup recorded is what a re-grab
	// during this action should restore, and this box must outlive nothing.
	if err := s.editScene(func(sc *sceneState) error {
		sc.heldGlassGeom = cachedGlass
		return nil
	}); err != nil {
		s.detachHeldGeometry()
		restoreFilter()
		return nil, fmt.Errorf("with_glass: %w", err)
	}
	// Frames are shared between scenes, so the stand-in is recognized in any
	// later one.
	standIn := s.scene().fs.Frame(heldItemFrameName)
	return func() {
		// Only the stand-in comes down. An action that attached a held item of its
		// own — a re-grab, or a pickup — leaves the arm really holding something,
		// and that has to stay modeled after the swap unwinds.
		if s.scene().fs.Frame(heldItemFrameName) == standIn {
			s.detachHeldGeometry()
		}
		restoreFilter()
	}, nil
}

// heldItemFramePose returns the transform the held-item frame is attached at,
// relative to the grip point, for a container geometry already expressed in
// grip-point coordinates.
//
// The frame adopts the geometry's orientation turned about its vertical axis
// onto the grab's approach heading (geom.HeadingAligned). geom.ContainerBox
// builds the container box upright with its height on Z, so this makes the
// frame's +Z the container's vertical axis — upright in the world at the moment
// of the grab, and tilting with the container afterwards as the wrist moves —
// and its +X the direction the gripper points, flattened onto the container's
// horizontal. A held-item theta of 0 therefore means the gripper points toward
// world +X, whatever heading the box was built at or the grab came from.
//
// carryHeldLevel depends on that — the no-spill orientation bound only bounds
// the drink's tilt if the frame it is measured against is the container's own —
// and so do the pour poses, which are authored as the container's orientation
// (glass_relative_poses.go).
//
// The translation is zero, so the frame sits on the grip point: a held-item pose
// places the same point a grip-point pose would, and a pivot of the held item
// turns about the grip point.
//
// The geometry stays in grip-point coordinates and is deliberately not
// re-expressed in this frame. RDK attaches a frame's geometry at the frame's
// parent, never at the frame itself (the GeometriesInFrame special case in
// referenceframe.FrameSystem.Transform, and FrameSystemGeometriesForFrames), so
// this rotation cannot move the collision box; it only changes the pose the
// frame system reports for — and the planner drives — the held-item frame.
func heldItemFramePose(gripperLocal spatialmath.Geometry) spatialmath.Pose {
	return spatialmath.NewPoseFromOrientation(geom.HeadingAligned(gripperLocal.Pose().Orientation()))
}

// addHeldItemFrame adds the held-item static frame under the grip point,
// carrying gripperLocal (geometry already expressed in grip-point coordinates)
// and rotated onto the container's axes (heldItemFramePose). Any existing held-item frame is removed first so attach
// is idempotent. Sets heldItemAttached on success.
func (sc *sceneState) addHeldItemFrame(gripperLocal spatialmath.Geometry) error {
	parent := sc.fs.Frame(gripPoint)
	if parent == nil {
		return fmt.Errorf("grip-point frame %q not found in frame system", gripPoint)
	}
	if existing := sc.fs.Frame(heldItemFrameName); existing != nil {
		sc.fs.RemoveFrame(existing)
	}
	frame, err := referenceframe.NewStaticFrameWithGeometry(heldItemFrameName, heldItemFramePose(gripperLocal), gripperLocal)
	if err != nil {
		return fmt.Errorf("create held-item frame: %w", err)
	}
	if err := sc.fs.AddFrame(frame, parent); err != nil {
		return fmt.Errorf("add held-item frame under %q: %w", gripPoint, err)
	}
	sc.heldItemAttached = true
	return nil
}

// detachHeldGeometry removes the held-item frame on release
// (sceneState.detachHeldItem). No-op when nothing is attached.
func (s *beanjaminCoffee) detachHeldGeometry() {
	if !s.scene().heldItemAttached {
		return
	}
	if err := s.editScene(func(sc *sceneState) error {
		sc.detachHeldItem()
		return nil
	}); err != nil {
		s.activeOrderLogger().Errorf("detach held-item geometry: %v", err)
		return
	}
	s.activeOrderLogger().Infof("detached held-item geometry from gripper")
}

// detachHeldItem removes the held-item frame. The cached gripper-local geometry
// is retained so a re-grab of the same item can restore it (reattachHeldItem).
func (sc *sceneState) detachHeldItem() {
	if existing := sc.fs.Frame(heldItemFrameName); existing != nil {
		sc.fs.RemoveFrame(existing)
	}
	sc.heldItemAttached = false
}

// stageGlassAsObstacle is stageGlass's release path: it stages the held glass
// with the arm where the moves handed off so far leave it (expectedInputs,
// sceneState.stageHeldGlass), so the released glass stays a collision obstacle.
// No-op when nothing is attached, like detachHeldGeometry.
func (s *beanjaminCoffee) stageGlassAsObstacle(ctx context.Context) error {
	if !s.scene().heldItemAttached {
		return nil
	}
	fsInputs, err := s.expectedInputs(ctx)
	if err != nil {
		return err
	}
	var staged glassStaging
	if err := s.editScene(func(sc *sceneState) error {
		staged, err = sc.stageHeldGlass(fsInputs)
		return err
	}); err != nil {
		return err
	}
	logger := s.activeOrderLogger()
	if staged.seated {
		logger.Infof("staged glass seated on the surface beneath it: released at Z=%.1f, shifted %.1fmm", staged.released.Z, staged.shiftMm)
	} else {
		logger.Warnf("no surface modeled beneath the staged glass at (x=%.1f, y=%.1f); staging it where it was released", staged.released.X, staged.released.Y)
	}
	logger.Infof("staged glass as world obstacle at %v", staged.at)
	return nil
}

// glassStaging is where stageHeldGlass put the glass.
type glassStaging struct {
	// released is the glass's center where the gripper let go of it, and at its
	// center once staged.
	released, at r3.Vector
	// seated is whether a surface was found beneath it, and shiftMm how far the
	// glass moved to stand on it.
	seated  bool
	shiftMm float64
}

// stageHeldGlass moves the held glass from the gripper to a static World frame
// with the frames standing at fsInputs.
//
// The released glass is seated on the surface beneath it (geom.SeatOnSurface)
// rather than left where the gripper held it: the staging pose releases the
// glass slightly above the table, so the glass drops onto it, and the box must
// follow — the pours resolve against its rim, and the re-grab models the grasp
// from where it stands (regrabStagedGlass). When no surface is modeled beneath
// it, the glass is staged where it was released.
func (sc *sceneState) stageHeldGlass(fsInputs referenceframe.FrameSystemInputs) (glassStaging, error) {
	heldFrame := sc.fs.Frame(heldItemFrameName)
	if heldFrame == nil {
		sc.heldItemAttached = false
		return glassStaging{}, nil
	}
	gif, err := heldFrame.Geometries([]referenceframe.Input{})
	if err != nil {
		return glassStaging{}, fmt.Errorf("get held-item geometry: %w", err)
	}
	geos := gif.Geometries()
	if len(geos) == 0 {
		return glassStaging{}, fmt.Errorf("held-item frame carries no geometry to stage")
	}
	// Lift the gripper-local geometry into world coordinates at fsInputs.
	// Transforming a GeometriesInFrame resolves through the source frame's parent,
	// so this composes gripPoint_to_world with the geometry's own gripper-local pose
	// and rightly ignores the held-item frame's container rotation — the geometry
	// was never expressed in that frame's coordinates.
	worldTF, err := sc.fs.Transform(
		fsInputs.ToLinearInputs(),
		referenceframe.NewGeometriesInFrame(heldItemFrameName, geos),
		referenceframe.World,
	)
	if err != nil {
		return glassStaging{}, fmt.Errorf("transform staged glass geometry to world: %w", err)
	}
	worldGeos := worldTF.(*referenceframe.GeometriesInFrame).Geometries()
	if len(worldGeos) == 0 {
		return glassStaging{}, fmt.Errorf("no glass geometry after transform to world")
	}
	worldGeom := worldGeos[0]

	// Surfaces are enumerated while the glass is still gripper-parented, so the
	// glass itself is never taken for the surface it stands on.
	surfaces, err := geom.WorldAnchoredSurfaceBoxes(sc.fs, fsInputs)
	if err != nil {
		return glassStaging{}, fmt.Errorf("find the surface under the staged glass: %w", err)
	}
	staged := glassStaging{released: worldGeom.Pose().Point()}
	if seated, dz, ok := geom.SeatOnSurface(surfaces, worldGeom, surfaceRestClearanceMm); ok {
		worldGeom = seated
		staged.seated, staged.shiftMm = true, dz
	}

	sc.detachHeldItem()

	// Re-add as a static world obstacle: identity frame transform, geometry carries
	// its own world pose (like addHeldItemFrame, but parented to World).
	if existing := sc.fs.Frame(stagedGlassFrameName); existing != nil {
		sc.fs.RemoveFrame(existing)
	}
	obstacle, err := referenceframe.NewStaticFrameWithGeometry(stagedGlassFrameName, spatialmath.NewZeroPose(), worldGeom)
	if err != nil {
		return glassStaging{}, fmt.Errorf("create staged-glass obstacle frame: %w", err)
	}
	if err := sc.fs.AddFrame(obstacle, sc.fs.World()); err != nil {
		return glassStaging{}, fmt.Errorf("add staged-glass obstacle to world: %w", err)
	}
	sc.stagedGlassPlaced = true
	staged.at = worldGeom.Pose().Point()
	return staged, nil
}

// regrabStagedGlass moves the staged glass from the world back into the gripper
// once the jaws have closed on it (sceneState.regrabStagedGlass), with the arm
// where the moves handed off so far leave it (expectedInputs).
func (s *beanjaminCoffee) regrabStagedGlass(ctx context.Context) error {
	var fsInputs referenceframe.FrameSystemInputs
	if s.scene().stagedGlassPlaced {
		var err error
		if fsInputs, err = s.expectedInputs(ctx); err != nil {
			return err
		}
	}
	var fromStaged bool
	if err := s.editScene(func(sc *sceneState) error {
		var err error
		fromStaged, err = sc.regrabStagedGlass(fsInputs)
		return err
	}); err != nil {
		return err
	}
	if fromStaged {
		s.activeOrderLogger().Infof("re-grabbed the staged glass where it stands")
	} else {
		s.activeOrderLogger().Infof("no glass staged; restored the cached glass grasp, if any")
	}
	return nil
}

// regrabStagedGlass moves the staged glass from the world into the gripper,
// with the frames standing at fsInputs, and reports whether there was one. The
// grasp is modeled from where the glass stands rather than restored from the
// pickup: seating it on the surface (stageHeldGlass) moved it relative to the
// grip point, and the glass really is held at that new height now. With no
// glass staged (e.g. a manually stepped grab_staged_glass) it restores the
// cached pickup grasp instead, and fsInputs is unused.
func (sc *sceneState) regrabStagedGlass(fsInputs referenceframe.FrameSystemInputs) (bool, error) {
	glass, err := sc.stagedGlassGeometry()
	// The obstacle comes out before the held item goes in, so the held glass never
	// collides with its double.
	sc.removeStagedGlass()
	if err != nil {
		_, err := sc.reattachHeldItem(pickupLabelGlass)
		return false, err
	}
	_, err = sc.attachHeldItem(fsInputs, pickupLabelGlass, glass)
	return true, err
}

// removeStagedGlass drops the staged-glass obstacle created by stageHeldGlass,
// if any.
func (sc *sceneState) removeStagedGlass() {
	if existing := sc.fs.Frame(stagedGlassFrameName); existing != nil {
		sc.fs.RemoveFrame(existing)
	}
	sc.stagedGlassPlaced = false
}

// stagedGlassGrabCollisions allows the gripper jaws to contact the staged-glass
// obstacle while re-grabbing it (the rest of the arm still routes around it).
// Returns nil when no glass is staged. The gripper sub-frames only exist on the
// real gripper; filterFakeModeCollisions drops them under FakeMode.
func (sc *sceneState) stagedGlassGrabCollisions() []AllowedCollision {
	if !sc.stagedGlassPlaced {
		return nil
	}
	return []AllowedCollision{
		{Frame1: stagedGlassFrameName, Frame2: componentClaws},
		{Frame1: stagedGlassFrameName, Frame2: "gripper:claws"},
		{Frame1: stagedGlassFrameName, Frame2: "gripper:case-gripper"},
	}
}

// cachedHeldGeometry returns the cached gripper-local geometry for the given item
// label, or nil if none has been recorded this order. Each label gets its own
// slot: an iced latte carries a cup, a glass and a bottle within one order, and
// a shared slot would hand a re-grab the shape of whichever came last.
func (sc *sceneState) cachedHeldGeometry(label string) spatialmath.Geometry {
	switch label {
	case pickupLabelGlass:
		return sc.heldGlassGeom
	case pickupLabelMilk:
		return sc.heldMilkGeom
	default:
		return sc.heldCupGeom
	}
}

// cacheHeldGeometry stores the gripper-local geometry for the given item label.
func (sc *sceneState) cacheHeldGeometry(label string, gripperLocal spatialmath.Geometry) {
	switch label {
	case pickupLabelGlass:
		sc.heldGlassGeom = gripperLocal
	case pickupLabelMilk:
		sc.heldMilkGeom = gripperLocal
	default:
		sc.heldCupGeom = gripperLocal
	}
}

// heldItemSelfCollisions returns the allowed-collision pairs between the tracked
// held-item geometry and the gripper frames it necessarily overlaps. Because the
// cup/glass is grasped by the gripper, its geometry intersects the gripper
// bodies; without these allowances every plan would fail immediately on that
// overlap. They are auto-injected into every plan (moveToRawPose, pivots,
// circular motion) while an item is attached. Returns nil when nothing is held.
//
// The gripper sub-frames (gripper:claws, gripper:case-gripper) only exist on the
// real gripper; filterFakeModeCollisions drops them under FakeMode.
func (sc *sceneState) heldItemSelfCollisions() []AllowedCollision {
	if !sc.heldItemAttached {
		return nil
	}
	return []AllowedCollision{
		{Frame1: heldItemFrameName, Frame2: componentClaws},
		{Frame1: heldItemFrameName, Frame2: "gripper:claws"},
		{Frame1: heldItemFrameName, Frame2: "gripper:case-gripper"},
	}
}

// appendHeldItemCollisions returns acs plus the held-item self-collision pairs
// (when an item is attached) as a new slice; acs is not mutated. When nothing is
// attached it returns acs unchanged, so behavior is identical to before tracking.
func (sc *sceneState) appendHeldItemCollisions(acs []AllowedCollision) []AllowedCollision {
	self := sc.heldItemSelfCollisions()
	if len(self) == 0 {
		return acs
	}
	out := make([]AllowedCollision, 0, len(acs)+len(self))
	out = append(out, acs...)
	out = append(out, self...)
	return out
}

// heldItemSurfaceCollisions returns the given held-item↔surface pairs only while
// an item is attached, so the pairs never reference a non-existent held-item
// frame (and motion is unchanged when tracking is off). Used at the contact
// phases where the held cup/glass legitimately approaches a modeled surface.
func (sc *sceneState) heldItemSurfaceCollisions(pairs []AllowedCollision) []AllowedCollision {
	if !sc.heldItemAttached {
		return nil
	}
	return pairs
}

// heldItemHalfHeightMm returns half the vertical (Z) extent, in mm, of the
// currently tracked held-item geometry, and true when an item is attached and
// its geometry is a Box. The held-item box is modeled upright (geom.ContainerBox
// builds it with OZ=1 and Z = container height) and a Box keeps
// its dims under the transform into the gripper frame, so its Z dimension is the
// container's vertical extent once placed upright at the drop pose. Returns false
// when nothing is attached or the geometry is not a Box, so callers fall back to
// a fixed offset.
func (sc *sceneState) heldItemHalfHeightMm() (float64, bool) {
	if !sc.heldItemAttached {
		return 0, false
	}
	frame := sc.fs.Frame(heldItemFrameName)
	if frame == nil {
		return 0, false
	}
	gif, err := frame.Geometries([]referenceframe.Input{})
	if err != nil {
		return 0, false
	}
	geos := gif.Geometries()
	if len(geos) == 0 {
		return 0, false
	}
	box := geos[0].ToProtobuf().GetBox()
	if box == nil || box.DimsMm == nil {
		return 0, false
	}
	return box.DimsMm.Z / 2, true
}
