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

// attachDetectedGeometry is attachDetectedGeometries for a single-box item (the
// cup, the glass). No-op when geomWorld is nil (no detection geometry
// available).
func (s *beanjaminCoffee) attachDetectedGeometry(ctx context.Context, label string, geomWorld spatialmath.Geometry) error {
	if geomWorld == nil {
		return nil
	}
	return s.attachDetectedGeometries(ctx, label, []spatialmath.Geometry{geomWorld})
}

// attachDetectedGeometries records the vision-detected geometry of a freshly
// grabbed item (geomsWorld, in world coordinates as lifted in observeVantage;
// element 0 is the container body, and any further elements ride along with
// it) and attaches it to the gripper with the arm where the moves handed off
// so far leave it (expectedInputs, sceneState.attachHeldItem). No-op when
// geomsWorld is empty.
func (s *beanjaminCoffee) attachDetectedGeometries(ctx context.Context, label string, geomsWorld []spatialmath.Geometry) error {
	if len(geomsWorld) == 0 {
		return nil
	}
	at, err := s.expectedPlacement(ctx)
	if err != nil {
		return err
	}
	var gripperLocal []spatialmath.Geometry
	if err := s.editScene(func(sc *sceneState) error {
		gripperLocal, err = sc.attachHeldItem(at, label, geomsWorld)
		return err
	}); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("attached %s geometry to gripper (center rel. grip point %v)", label, gripperLocal[0].Pose().Point())
	return nil
}

// attachHeldItem adds the held-item frame carrying geomsWorld, world-frame
// geometries with the container body first, with the frames placed by at. The
// geometries are expressed in the grip-point frame and cached by label, so a
// later re-grab of the same item (reattachHeldItem) can restore them without
// re-detecting. Returns those gripper-local geometries.
func (sc *sceneState) attachHeldItem(
	at placement, label string, geomsWorld []spatialmath.Geometry,
) ([]spatialmath.Geometry, error) {
	if len(geomsWorld) == 0 {
		return nil, fmt.Errorf("no %s geometry to attach", label)
	}
	// Cached in grip-point coordinates, the geometry tracks the gripper as it
	// moves: FrameSystemGeometries places it at gripPoint_to_world ∘
	// gripperLocalPose, whatever transform the held-item frame itself carries.
	tf, err := at.transform(sc.fs, referenceframe.NewGeometriesInFrame(referenceframe.World, geomsWorld),
		gripPoint,
	)
	if err != nil {
		return nil, fmt.Errorf("transform %s geometry into gripper frame: %w", label, err)
	}
	gripperLocal := tf.(*referenceframe.GeometriesInFrame).Geometries()
	if len(gripperLocal) != len(geomsWorld) {
		return nil, fmt.Errorf("%d %s geometries after transform into gripper frame, want %d", len(gripperLocal), label, len(geomsWorld))
	}
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
	if len(s.scene().cachedHeldGeometry(label)) == 0 {
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
	if len(gripperLocal) == 0 {
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
	at, err := s.expectedPlacement(ctx)
	if err != nil {
		return err
	}
	if err := s.editScene(func(sc *sceneState) error {
		box, err := sc.containerBox(at, label, dims, grabRel)
		if err != nil {
			return err
		}
		_, err = sc.attachHeldItem(at, label, []spatialmath.Geometry{box})
		return err
	}); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("modeled the held %s from the configured %.0f×%.0f mm for held-item tracking",
		label, dims.DiameterMm, dims.HeightMm)
	return nil
}

// containerBox builds the world-frame box of a container of the given
// dimensions held by the gripper, with the frames placed by at. It is
// modeled exactly as geom.ContainerBox models it at pickup: an upright box
// centered on the grasp centroid. The centroid is recovered by inverting the
// geom.PoseRelativeTo the grab used — the grab sends the grip point to
// centroid + grabRel, so the centroid is the grip-point world position minus
// that offset.
func (sc *sceneState) containerBox(
	at placement, label string, dims *ContainerDimensions, grabRel *RelativePose,
) (spatialmath.Geometry, error) {
	centroid, err := sc.graspCentroid(at, grabRel)
	if err != nil {
		return nil, err
	}
	return geom.ContainerBox(centroid, dims.boxDims(), containerYaw(grabRel), label)
}

// graspCentroid recovers the grasp centroid in world coordinates, with the
// frames placed by at: the grip-point world position minus the grab offset.
func (sc *sceneState) graspCentroid(at placement, grabRel *RelativePose) (r3.Vector, error) {
	gripPointPIF := referenceframe.NewPoseInFrame(gripPoint, spatialmath.NewZeroPose())
	tf, err := at.transform(sc.fs, gripPointPIF, referenceframe.World)
	if err != nil {
		return r3.Vector{}, fmt.Errorf("transform grip point to world: %w", err)
	}
	gripPointWorld := tf.(*referenceframe.PoseInFrame).Pose().Point()

	// The grab poses are required by Validate wherever their pickup is configured,
	// so they are non-nil on any machine that reaches the matching flow.
	grabOffset := relativePoseToSpatial(grabRel).Point()
	return gripPointWorld.Sub(grabOffset), nil
}

// attachConfiguredMugGeometry is attachConfiguredCupGeometry for a mug: the body
// from mug_dimensions plus the handle box beside it, so a mug re-grabbed with
// nothing cached is still carried as a tracked held item.
func (s *beanjaminCoffee) attachConfiguredMugGeometry(ctx context.Context) error {
	at, err := s.expectedPlacement(ctx)
	if err != nil {
		return err
	}
	if err := s.editScene(func(sc *sceneState) error {
		geoms, err := sc.configuredMugGeometries(at, s.cfg)
		if err != nil {
			return err
		}
		_, err = sc.attachHeldItem(at, pickupLabelMug, geoms)
		return err
	}); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("modeled the held mug from the configured %.0f×%.0f mm body and handle for held-item tracking",
		s.cfg.MugDimensions.DiameterMm, s.cfg.MugDimensions.HeightMm)
	return nil
}

// configuredMugGeometries builds the world-frame mug body and handle around the
// grasp centroid recovered with the frames placed by at.
func (sc *sceneState) configuredMugGeometries(at placement, cfg *Config) ([]spatialmath.Geometry, error) {
	if cfg.MugDimensions == nil || cfg.MugHandleDimensions == nil || cfg.MugGrabRelativePose == nil {
		return nil, fmt.Errorf("mug_dimensions, mug_handle_dimensions and mug_grab_relative_pose must be configured")
	}
	grabRel := cfg.MugGrabRelativePose
	centroid, err := sc.graspCentroid(at, grabRel)
	if err != nil {
		return nil, err
	}
	h := cfg.MugHandleDimensions
	return geom.MugGeometries(centroid, cfg.MugDimensions.boxDims(), h.DepthMm, h.WidthMm, containerYaw(grabRel), pickupLabelMug)
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
// the held-item shield exemption that areaShieldCollisions withholds until
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
// carrying gripperLocal (geometries already expressed in grip-point coordinates,
// body first) and rotated onto the body's axes (heldItemFramePose). Any existing held-item frame is removed first so attach
// is idempotent. Sets heldItemAttached on success.
func (sc *sceneState) addHeldItemFrame(gripperLocal []spatialmath.Geometry) error {
	if len(gripperLocal) == 0 {
		return fmt.Errorf("held item has no geometry")
	}
	parent := sc.fs.Frame(gripPoint)
	if parent == nil {
		return fmt.Errorf("grip-point frame %q not found in frame system", gripPoint)
	}
	if existing := sc.fs.Frame(heldItemFrameName); existing != nil {
		sc.fs.RemoveFrame(existing)
	}
	frame, err := referenceframe.NewStaticFrameWithGeometries(heldItemFrameName, heldItemFramePose(gripperLocal[0]), gripperLocal)
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

// stagedItem names one kind of item that is set down and kept as a static world
// obstacle (the staged glass, the staged mug): its obstacle frame and its pickup
// label (also the noun in messages). Whether it is placed lives in the scene
// (sceneState.stagedPlaced). The staging helpers below are shared across kinds
// through it.
type stagedItem struct {
	frame string
	label string
}

var (
	stagedGlass = stagedItem{frame: stagedGlassFrameName, label: pickupLabelGlass}
	stagedMug   = stagedItem{frame: stagedMugFrameName, label: pickupLabelMug}
)

// stagedPlaced reports whether it is staged in sc (stagedGlassPlaced,
// stagedMugPlaced).
func (sc *sceneState) stagedPlaced(it stagedItem) bool {
	if it.frame == stagedMugFrameName {
		return sc.stagedMugPlaced
	}
	return it.frame == stagedGlassFrameName && sc.stagedGlassPlaced
}

// setStagedPlaced records whether it is staged in sc.
func (sc *sceneState) setStagedPlaced(it stagedItem, placed bool) {
	if it.frame == stagedMugFrameName {
		sc.stagedMugPlaced = placed
	}
	if it.frame == stagedGlassFrameName {
		sc.stagedGlassPlaced = placed
	}
}

// stageGlassAsObstacle is stageGlass's release path: it stages the held glass
// with the arm where the moves handed off so far leave it (expectedInputs,
// sceneState.stageHeldGlass), so the released glass stays a collision obstacle.
// No-op when nothing is attached, like detachHeldGeometry.
func (s *beanjaminCoffee) stageGlassAsObstacle(ctx context.Context) error {
	return s.stageAsObstacle(ctx, stagedGlass)
}

// stageAsObstacle is the release path of staging an item: it stages the held
// item with the arm where the moves handed off so far leave it
// (sceneState.stageHeldItem), so the released item stays a collision obstacle.
// No-op when nothing is attached.
func (s *beanjaminCoffee) stageAsObstacle(ctx context.Context, it stagedItem) error {
	if !s.scene().heldItemAttached {
		return nil
	}
	at, err := s.expectedPlacement(ctx)
	if err != nil {
		return err
	}
	var staged itemStaging
	if err := s.editScene(func(sc *sceneState) error {
		staged, err = sc.stageHeldItem(at, it)
		return err
	}); err != nil {
		return err
	}
	logger := s.activeOrderLogger()
	if staged.seated {
		logger.Infof("staged %s seated on the surface beneath it: released at Z=%.1f, shifted %.1fmm", it.label, staged.released.Z, staged.shiftMm)
	} else {
		logger.Warnf("no surface modeled beneath the staged %s at (x=%.1f, y=%.1f); staging it where it was released", it.label, staged.released.X, staged.released.Y)
	}
	logger.Infof("staged %s as world obstacle at %v", it.label, staged.at)
	return nil
}

// itemStaging is where stageHeldItem put the item.
type itemStaging struct {
	// released is the body's center where the gripper let go of it, and at its
	// center once staged.
	released, at r3.Vector
	// seated is whether a surface was found beneath it, and shiftMm how far the
	// item moved to stand on it.
	seated  bool
	shiftMm float64
}

// stageHeldGlass moves the held glass from the gripper to a static World frame
// with the frames placed by at (stageHeldItem).
func (sc *sceneState) stageHeldGlass(at placement) (itemStaging, error) {
	return sc.stageHeldItem(at, stagedGlass)
}

// stageHeldItem moves the held item from the gripper to its static World
// frame with the frames placed by at.
//
// The released item is seated on the surface beneath it (geom.SeatOnSurface on
// the body, geometry 0) rather than left where the gripper held it: the staging
// pose releases it slightly above the table, so it drops onto it, and every
// geometry must follow — every geometry after the body shifts by the same
// dz so the shape stays rigid. The pours resolve against the glass rim,
// and the re-grab models the grasp from where the item stands
// (regrabStagedItem). When no surface is modeled beneath it, the item is staged
// where it was released.
func (sc *sceneState) stageHeldItem(at placement, it stagedItem) (itemStaging, error) {
	heldFrame := sc.fs.Frame(heldItemFrameName)
	if heldFrame == nil {
		sc.heldItemAttached = false
		return itemStaging{}, nil
	}
	gif, err := heldFrame.Geometries([]referenceframe.Input{})
	if err != nil {
		return itemStaging{}, fmt.Errorf("get held-item geometry: %w", err)
	}
	geos := gif.Geometries()
	if len(geos) == 0 {
		return itemStaging{}, fmt.Errorf("held-item frame carries no geometry to stage")
	}
	// Lift the gripper-local geometry into world coordinates, placed by at.
	// Transforming a GeometriesInFrame resolves through the source frame's parent,
	// so this composes gripPoint_to_world with the geometry's own gripper-local pose
	// and rightly ignores the held-item frame's container rotation — the geometry
	// was never expressed in that frame's coordinates.
	worldTF, err := at.transform(sc.fs, referenceframe.NewGeometriesInFrame(heldItemFrameName, geos),
		referenceframe.World,
	)
	if err != nil {
		return itemStaging{}, fmt.Errorf("transform staged %s geometry to world: %w", it.label, err)
	}
	worldGeos := worldTF.(*referenceframe.GeometriesInFrame).Geometries()
	if len(worldGeos) == 0 {
		return itemStaging{}, fmt.Errorf("no %s geometry after transform to world", it.label)
	}

	// Surfaces are enumerated while the item is still gripper-parented, so the
	// item itself is never taken for the surface it stands on.
	surfaces, err := geom.WorldAnchoredSurfaceBoxes(sc.fs, at.fullInputs(sc.fs))
	if err != nil {
		return itemStaging{}, fmt.Errorf("find the surface under the staged %s: %w", it.label, err)
	}
	staged := itemStaging{released: worldGeos[0].Pose().Point()}
	if seated, dz, ok := geom.SeatOnSurface(surfaces, worldGeos[0], surfaceRestClearanceMm); ok {
		worldGeos[0] = seated
		shift := spatialmath.NewPoseFromPoint(r3.Vector{Z: dz})
		for i := 1; i < len(worldGeos); i++ {
			worldGeos[i] = worldGeos[i].Transform(shift)
		}
		staged.seated, staged.shiftMm = true, dz
	}

	sc.detachHeldItem()

	// Re-add as a static world obstacle: identity frame transform, geometries
	// carry their own world pose (like addHeldItemFrame, but parented to World).
	if existing := sc.fs.Frame(it.frame); existing != nil {
		sc.fs.RemoveFrame(existing)
	}
	obstacle, err := referenceframe.NewStaticFrameWithGeometries(it.frame, spatialmath.NewZeroPose(), worldGeos)
	if err != nil {
		return itemStaging{}, fmt.Errorf("create %s obstacle frame: %w", it.frame, err)
	}
	if err := sc.fs.AddFrame(obstacle, sc.fs.World()); err != nil {
		return itemStaging{}, fmt.Errorf("add %s obstacle to world: %w", it.frame, err)
	}
	sc.setStagedPlaced(it, true)
	staged.at = worldGeos[0].Pose().Point()
	return staged, nil
}

// regrabStagedGlass moves the staged glass from the world back into the gripper
// once the jaws have closed on it (sceneState.regrabStagedGlass), with the arm
// where the moves handed off so far leave it (expectedInputs).
func (s *beanjaminCoffee) regrabStagedGlass(ctx context.Context) error {
	return s.regrabStaged(ctx, stagedGlass)
}

// regrabStaged moves a staged item from the world back into the gripper once
// the jaws have closed on it (sceneState.regrabStagedItem), with the arm where
// the moves handed off so far leave it (expectedInputs).
func (s *beanjaminCoffee) regrabStaged(ctx context.Context, it stagedItem) error {
	var at placement
	if s.scene().stagedPlaced(it) {
		var err error
		if at, err = s.expectedPlacement(ctx); err != nil {
			return err
		}
	}
	var fromStaged bool
	if err := s.editScene(func(sc *sceneState) error {
		var err error
		fromStaged, err = sc.regrabStagedItem(at, it)
		return err
	}); err != nil {
		return err
	}
	if fromStaged {
		s.activeOrderLogger().Infof("re-grabbed the staged %s where it stands", it.label)
	} else {
		s.activeOrderLogger().Infof("no %s staged; restored the cached %s grasp, if any", it.label, it.label)
	}
	return nil
}

// regrabStagedItem moves the staged item from the world into the gripper, with
// the frames placed by at, and reports whether there was one. The grasp is
// modeled from where the item stands rather than restored from the pickup:
// seating it on the surface (stageHeldItem) moved it relative to the grip
// point, and the item really is held at that new height now. With none staged
// (e.g. a manually stepped grab_staged_glass) it restores the cached pickup
// grasp instead, and at is unused.
func (sc *sceneState) regrabStagedItem(at placement, it stagedItem) (bool, error) {
	geos, err := sc.stagedGeometry(it)
	// The obstacle comes out before the held item goes in, so the held item never
	// collides with its double.
	sc.removeStaged(it)
	if err != nil {
		_, err := sc.reattachHeldItem(it.label)
		return false, err
	}
	_, err = sc.attachHeldItem(at, it.label, geos)
	return true, err
}

// removeStaged drops a staged item's obstacle created by stageHeldItem, if any.
func (sc *sceneState) removeStaged(it stagedItem) {
	if existing := sc.fs.Frame(it.frame); existing != nil {
		sc.fs.RemoveFrame(existing)
	}
	sc.setStagedPlaced(it, false)
}

// stagedGlassGrabCollisions allows the gripper jaws to contact the staged-glass
// obstacle while re-grabbing it (the rest of the arm still routes around it).
// Returns nil when no glass is staged. The gripper sub-frames only exist on the
// real gripper; filterFakeModeCollisions drops them under FakeMode.
func (sc *sceneState) stagedGlassGrabCollisions() []AllowedCollision {
	return sc.stagedGrabCollisions(stagedGlass)
}

// stagedGrabCollisions allows the gripper jaws to contact a staged item's
// obstacle while re-grabbing it. Returns nil when none is staged.
func (sc *sceneState) stagedGrabCollisions(it stagedItem) []AllowedCollision {
	if !sc.stagedPlaced(it) {
		return nil
	}
	return []AllowedCollision{
		{Frame1: it.frame, Frame2: componentClaws},
		{Frame1: it.frame, Frame2: "gripper:claws"},
		{Frame1: it.frame, Frame2: "gripper:case-gripper"},
	}
}

// cachedHeldGeometry returns the cached gripper-local geometries (body first)
// for the given item label, or nil if none has been recorded this order. Each
// label gets its own slot: an iced latte carries a cup, a glass and a bottle
// within one order, and a shared slot would hand a re-grab the shape of
// whichever came last. Unknown labels share the cup's slot.
func (sc *sceneState) cachedHeldGeometry(label string) []spatialmath.Geometry {
	switch label {
	case pickupLabelGlass:
		return sc.heldGlassGeom
	case pickupLabelMilk:
		return sc.heldMilkGeom
	case pickupLabelMug:
		return sc.heldMugGeom
	case pickupLabelSpice:
		return sc.heldSpiceGeom
	default:
		return sc.heldCupGeom
	}
}

// cacheHeldGeometry stores the gripper-local geometries for the given item label.
func (sc *sceneState) cacheHeldGeometry(label string, gripperLocal []spatialmath.Geometry) {
	switch label {
	case pickupLabelGlass:
		sc.heldGlassGeom = gripperLocal
	case pickupLabelMilk:
		sc.heldMilkGeom = gripperLocal
	case pickupLabelMug:
		sc.heldMugGeom = gripperLocal
	case pickupLabelSpice:
		sc.heldSpiceGeom = gripperLocal
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
	_, _, heightMm, ok := sc.heldItemBox()
	if !ok {
		return 0, false
	}
	return heightMm / 2, true
}

// heldItemBottomDepthMm returns how far the bottom of the held container lies
// below the held-item frame's origin, measured along the frame's +Z (the
// container's axis), and true when an item is attached and modeled as a Box.
// A held-item goal placed this far above a surface, upright, sets the container
// down on it whatever its height and wherever along it the gripper holds it:
// the depth is about 36 mm for an espresso cup and 80 mm for an iced glass.
func (sc *sceneState) heldItemBottomDepthMm() (float64, bool) {
	frame, box, heightMm, ok := sc.heldItemBox()
	if !ok {
		return 0, false
	}
	framePose, err := frame.Transform([]referenceframe.Input{})
	if err != nil {
		return 0, false
	}
	// The box is stored in grip-point coordinates (addHeldItemFrame), so its
	// center is brought into the held-item frame, whose +Z is the box's own
	// vertical axis (heldItemFramePose).
	center := spatialmath.Compose(spatialmath.PoseInverse(framePose), box.Pose()).Point()
	return heightMm/2 - center.Z, true
}

// heldItemBox returns the held-item frame, its box geometry (in grip-point
// coordinates) and the box's vertical extent: its Z dimension, since
// geom.ContainerBox builds it upright and a Box keeps its dims under transform.
// False when nothing is attached or the geometry is not a Box.
func (sc *sceneState) heldItemBox() (referenceframe.Frame, spatialmath.Geometry, float64, bool) {
	if !sc.heldItemAttached {
		return nil, nil, 0, false
	}
	frame := sc.fs.Frame(heldItemFrameName)
	if frame == nil {
		return nil, nil, 0, false
	}
	gif, err := frame.Geometries([]referenceframe.Input{})
	if err != nil {
		return nil, nil, 0, false
	}
	geos := gif.Geometries()
	if len(geos) == 0 {
		return nil, nil, 0, false
	}
	box := geos[0].ToProtobuf().GetBox()
	if box == nil || box.DimsMm == nil {
		return nil, nil, 0, false
	}
	return frame, geos[0], box.DimsMm.Z, true
}
