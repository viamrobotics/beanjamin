package coffee

// Held-item geometry tracking.
//
// When track_held_geometry is enabled, the cup or glass the gripper picks up is
// added to the cached frame system as a static "held-item" frame parented to the
// gripper frame (componentClaws), carrying the vision-detected geometry expressed
// relative to the gripper. While it is present, every motion plan routes around
// the held item — so the arm doesn't drive the cup into the machine, the shelf,
// or itself while carrying it.
//
// The geometry is attached on grab (attachDetectedGeometry for a fresh vision
// detection, reattachGeometry when re-grabbing an item whose geometry was already
// cached this order) and removed on release (detachHeldGeometry). The grasp is
// assumed consistent across release/re-grab — the item is released and re-grabbed
// at the same pose — so the gripper-local geometry cached at pickup is reused
// verbatim on the re-grab.
//
// This mirrors lockFilterFrame's frame-system mutation in motion.go, but the
// parent is the moving gripper rather than the world, and the geometry comes
// from vision rather than a part config.

import (
	"context"
	"fmt"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// heldItemFrameName is the static frame added under componentClaws carrying the
// geometry of the cup/glass currently held by the gripper.
const heldItemFrameName = "held-item"

// stagedGlassFrameName is the static World-parented frame carrying a glass set
// down in the staging area, so it stays a collision obstacle once released.
const stagedGlassFrameName = "staged-glass"

// attachDetectedGeometry records the vision-detected geometry of a freshly
// grabbed item and adds the held-item frame under the gripper so subsequent
// motion plans account for it. geomWorld is the detection in world coordinates
// (as lifted in observeVantage). It is expressed relative to the gripper frame
// at the current (grab) pose and cached by label so a later re-grab of the same
// item (reattachGeometry) can restore it without re-detecting. No-op when
// track_held_geometry is off or geomWorld is nil (no detection geometry
// available).
func (s *beanjaminCoffee) attachDetectedGeometry(ctx context.Context, label string, geomWorld spatialmath.Geometry) error {
	if !s.cfg.TrackHeldGeometry || geomWorld == nil {
		return nil
	}
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	// Express the world-frame geometry in the gripper frame at the current pose.
	// Cached in gripper-local coordinates, the held-item frame can be added with
	// an identity transform and the geometry will track the gripper as it moves
	// (FrameSystemGeometries places it at claws_to_world ∘ gripperLocalPose).
	tf, err := fs.Transform(
		fsInputs.ToLinearInputs(),
		referenceframe.NewGeometriesInFrame(referenceframe.World, []spatialmath.Geometry{geomWorld}),
		componentClaws,
	)
	if err != nil {
		return fmt.Errorf("transform %s geometry into gripper frame: %w", label, err)
	}
	geos := tf.(*referenceframe.GeometriesInFrame).Geometries()
	if len(geos) == 0 {
		return fmt.Errorf("no %s geometry after transform into gripper frame", label)
	}
	gripperLocal := geos[0]

	if err := s.addHeldItemFrame(gripperLocal); err != nil {
		return err
	}
	s.cacheHeldGeometry(label, gripperLocal)
	s.activeOrderLogger().Infof("attached %s geometry to gripper (center rel. claws %v)", label, gripperLocal.Pose().Point())
	return nil
}

// reattachGeometry restores the held-item frame for an item being re-grabbed
// (the brewed cup from under the machine, or the staged glass) using the geometry
// cached at its initial pickup. No-op when tracking is off or nothing was cached
// for this label (the item was first grabbed before tracking, or via the static
// pickup path).
func (s *beanjaminCoffee) reattachGeometry(label string) error {
	if !s.cfg.TrackHeldGeometry {
		return nil
	}
	gripperLocal := s.cachedHeldGeometry(label)
	if gripperLocal == nil {
		s.activeOrderLogger().Debugf("reattach %s geometry: nothing cached, skipping", label)
		return nil
	}
	if err := s.addHeldItemFrame(gripperLocal); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("re-attached cached %s geometry to gripper", label)
	return nil
}

// addHeldItemFrame adds the held-item static frame under the gripper frame,
// carrying gripperLocal (geometry already expressed in gripper-local
// coordinates). Any existing held-item frame is removed first so attach is
// idempotent. Sets heldItemAttached on success.
func (s *beanjaminCoffee) addHeldItemFrame(gripperLocal spatialmath.Geometry) error {
	gripperFrame := s.cachedFS.Frame(componentClaws)
	if gripperFrame == nil {
		return fmt.Errorf("gripper frame %q not found in frame system", componentClaws)
	}
	if existing := s.cachedFS.Frame(heldItemFrameName); existing != nil {
		s.cachedFS.RemoveFrame(existing)
	}
	// Identity frame transform: the geometry carries its own gripper-local pose,
	// so the planner places it relative to the gripper and it tracks the arm.
	frame, err := referenceframe.NewStaticFrameWithGeometry(heldItemFrameName, spatialmath.NewZeroPose(), gripperLocal)
	if err != nil {
		return fmt.Errorf("create held-item frame: %w", err)
	}
	if err := s.cachedFS.AddFrame(frame, gripperFrame); err != nil {
		return fmt.Errorf("add held-item frame under %q: %w", componentClaws, err)
	}
	s.heldItemAttached = true
	return nil
}

// detachHeldGeometry removes the held-item frame from the cached frame system on
// release. The cached gripper-local geometry is retained so a re-grab of the same
// item can restore it (reattachGeometry). No-op when nothing is attached.
func (s *beanjaminCoffee) detachHeldGeometry() {
	if !s.heldItemAttached {
		return
	}
	if existing := s.cachedFS.Frame(heldItemFrameName); existing != nil {
		s.cachedFS.RemoveFrame(existing)
	}
	s.heldItemAttached = false
	s.activeOrderLogger().Infof("detached held-item geometry from gripper")
}

// stageGlassAsObstacle is stageGlass's release path: it lifts the held glass
// geometry into world coordinates and re-parents it from the gripper to a static
// World frame, so the released glass stays a collision obstacle. No-op when nothing
// is attached (e.g. track_held_geometry is off), like detachHeldGeometry.
func (s *beanjaminCoffee) stageGlassAsObstacle(ctx context.Context) error {
	if !s.heldItemAttached {
		return nil
	}
	_, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	heldFrame := s.cachedFS.Frame(heldItemFrameName)
	if heldFrame == nil {
		s.heldItemAttached = false
		return nil
	}
	gif, err := heldFrame.Geometries([]referenceframe.Input{})
	if err != nil {
		return fmt.Errorf("get held-item geometry: %w", err)
	}
	geos := gif.Geometries()
	if len(geos) == 0 {
		return fmt.Errorf("held-item frame carries no geometry to stage")
	}
	// Lift the gripper-local geometry into world coordinates at the current pose.
	worldTF, err := s.cachedFS.Transform(
		fsInputs.ToLinearInputs(),
		referenceframe.NewGeometriesInFrame(heldItemFrameName, geos),
		referenceframe.World,
	)
	if err != nil {
		return fmt.Errorf("transform staged glass geometry to world: %w", err)
	}
	worldGeos := worldTF.(*referenceframe.GeometriesInFrame).Geometries()
	if len(worldGeos) == 0 {
		return fmt.Errorf("no glass geometry after transform to world")
	}
	worldGeom := worldGeos[0]

	// Release the gripper-parented held-item frame.
	s.cachedFS.RemoveFrame(heldFrame)
	s.heldItemAttached = false

	// Re-add as a static world obstacle: identity frame transform, geometry carries
	// its own world pose (like addHeldItemFrame, but parented to World).
	if existing := s.cachedFS.Frame(stagedGlassFrameName); existing != nil {
		s.cachedFS.RemoveFrame(existing)
	}
	obstacle, err := referenceframe.NewStaticFrameWithGeometry(stagedGlassFrameName, spatialmath.NewZeroPose(), worldGeom)
	if err != nil {
		return fmt.Errorf("create staged-glass obstacle frame: %w", err)
	}
	if err := s.cachedFS.AddFrame(obstacle, s.cachedFS.World()); err != nil {
		return fmt.Errorf("add staged-glass obstacle to world: %w", err)
	}
	s.stagedGlassPlaced = true
	s.activeOrderLogger().Infof("staged glass as world obstacle at %v", worldGeom.Pose().Point())
	return nil
}

// removeStagedGlassObstacle drops the static world obstacle created by
// stageGlassAsObstacle (e.g. once the glass is re-grabbed). No-op when none is set.
func (s *beanjaminCoffee) removeStagedGlassObstacle() {
	if !s.stagedGlassPlaced {
		return
	}
	if existing := s.cachedFS.Frame(stagedGlassFrameName); existing != nil {
		s.cachedFS.RemoveFrame(existing)
	}
	s.stagedGlassPlaced = false
	s.activeOrderLogger().Infof("removed staged-glass world obstacle")
}

// stagedGlassGrabCollisions allows the gripper jaws to contact the staged-glass
// obstacle while re-grabbing it (the rest of the arm still routes around it).
// Returns nil when no glass is staged. The gripper sub-frames only exist on the
// real gripper; filterFakeModeCollisions drops them under FakeMode.
func (s *beanjaminCoffee) stagedGlassGrabCollisions() []AllowedCollision {
	if !s.stagedGlassPlaced {
		return nil
	}
	return []AllowedCollision{
		{Frame1: stagedGlassFrameName, Frame2: componentClaws},
		{Frame1: stagedGlassFrameName, Frame2: "gripper:claws"},
		{Frame1: stagedGlassFrameName, Frame2: "gripper:case-gripper"},
	}
}

// clearHeldGeometry forgets all cached item geometry and clears the attached
// flag. Called from resetFrameSystem: rebuilding the cached frame system from the
// service already drops the held-item frame, and any cached grasp no longer
// corresponds to reality.
func (s *beanjaminCoffee) clearHeldGeometry() {
	s.heldItemAttached = false
	s.heldCupGeom = nil
	s.heldGlassGeom = nil
}

// cachedHeldGeometry returns the cached gripper-local geometry for the given item
// label, or nil if none has been recorded this order.
func (s *beanjaminCoffee) cachedHeldGeometry(label string) spatialmath.Geometry {
	if label == pickupLabelGlass {
		return s.heldGlassGeom
	}
	return s.heldCupGeom
}

// cacheHeldGeometry stores the gripper-local geometry for the given item label.
func (s *beanjaminCoffee) cacheHeldGeometry(label string, gripperLocal spatialmath.Geometry) {
	if label == pickupLabelGlass {
		s.heldGlassGeom = gripperLocal
		return
	}
	s.heldCupGeom = gripperLocal
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
func (s *beanjaminCoffee) heldItemSelfCollisions() []AllowedCollision {
	if !s.heldItemAttached {
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
func (s *beanjaminCoffee) appendHeldItemCollisions(acs []AllowedCollision) []AllowedCollision {
	self := s.heldItemSelfCollisions()
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
func (s *beanjaminCoffee) heldItemSurfaceCollisions(pairs []AllowedCollision) []AllowedCollision {
	if !s.heldItemAttached {
		return nil
	}
	return pairs
}

// heldItemHalfHeightMm returns half the vertical (Z) extent, in mm, of the
// currently tracked held-item geometry, and true when an item is attached and
// its geometry is a Box. The held-item box is modeled upright (overriddenBox /
// worldBoundingBox build it with OZ=1 and Z = container height) and a Box keeps
// its dims under the transform into the gripper frame, so its Z dimension is the
// container's vertical extent once placed upright at the drop pose. Returns false
// when nothing is attached or the geometry is not a Box, so callers fall back to
// a fixed offset.
func (s *beanjaminCoffee) heldItemHalfHeightMm() (float64, bool) {
	if !s.heldItemAttached {
		return 0, false
	}
	frame := s.cachedFS.Frame(heldItemFrameName)
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
