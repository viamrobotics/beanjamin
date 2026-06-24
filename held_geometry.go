// Package beanjamin: held-item geometry tracking.
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
package beanjamin

import (
	"context"
	"fmt"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// heldItemFrameName is the static frame added under componentClaws carrying the
// geometry of the cup/glass currently held by the gripper.
const heldItemFrameName = "held-item"

// attachDetectedGeometry records the vision-detected geometry of a freshly
// grabbed item and adds the held-item frame under the gripper so subsequent
// motion plans account for it. geomWorld is the detection in world coordinates
// (as lifted in observeVantage). It is expressed relative to the gripper frame
// at the current (grab) pose and cached by label so a later re-grab of the same
// item (reattachGeometry) can restore it without re-detecting. No-op when
// track_held_geometry is off or geomWorld is nil (e.g. the static pickup path,
// which has no detection).
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

// geometryToWorld lifts a geometry given in cameraFrame coordinates into the
// world frame, preserving its dimensions and orientation. Mirrors cameraToWorld
// but for a full geometry rather than a single point.
//
// It deliberately does NOT use fs.Transform on a GeometriesInFrame: that path
// applies only the parent-to-world transform and skips the source frame's own
// transform (the "geometry tied to a frame" convention in FrameSystem.Transform),
// which would drop the camera's mount transform. Instead it resolves the full
// camera→world pose via a PoseInFrame (same as cameraToWorld) and applies it to
// the geometry.
func geometryToWorld(
	fs *referenceframe.FrameSystem,
	fsInputs referenceframe.FrameSystemInputs,
	cameraFrame string,
	geom spatialmath.Geometry,
) (spatialmath.Geometry, error) {
	pif := referenceframe.NewPoseInFrame(cameraFrame, spatialmath.NewZeroPose())
	tf, err := fs.Transform(fsInputs.ToLinearInputs(), pif, referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("transform %q to world: %w", cameraFrame, err)
	}
	cameraToWorldPose := tf.(*referenceframe.PoseInFrame).Pose()
	return geom.Transform(cameraToWorldPose), nil
}
