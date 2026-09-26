package coffee

// Cached frame-system state: the snapshot every plan runs against, and the
// in-place mutations (locking or detaching the portafilter) and rebuilds that
// keep it in step with what the arm is physically holding.

import (
	"context"
	"fmt"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/spatialmath"
)

// currentInputs returns the cached frame system and fresh joint inputs.
// We build the inputs directly from the arm rather than calling fsSvc.CurrentInputs,
// which iterates all resources and can fail on modular arms whose kinematics
// proto round-trip produces KINEMATICS_FILE_FORMAT_UNSPECIFIED.
func (s *beanjaminCoffee) currentInputs(ctx context.Context) (*referenceframe.FrameSystem, referenceframe.FrameSystemInputs, error) {
	logger := s.activeOrderLogger()
	fsInputs := referenceframe.NewZeroInputs(s.cachedFS)

	// Get current joint positions directly from the arm.
	armInputs, err := s.arm.CurrentInputs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get current inputs: %w", err)
	}

	// Use the config arm name as the key — this matches the frame name in the cached
	// frame system built from FrameSystemConfig.
	logger.Debugf("currentInputs: arm=%q, armInputsLen=%d", s.cfg.ArmName, len(armInputs))
	fsInputs[s.cfg.ArmName] = armInputs

	return s.cachedFS, fsInputs, nil
}

// lockFilterFrame re-parents the "filter" frame from the arm subtree to the
// world at its current pose. Call this after physically locking the portafilter.
// The cached frame system is mutated in place so all subsequent planning calls
// see the filter at its locked position.
func (s *beanjaminCoffee) lockFilterFrame(ctx context.Context) error {
	logger := s.activeOrderLogger()
	const filterFrameName = componentFilter

	_, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}

	filterFrame := s.cachedFS.Frame(filterFrameName)
	if filterFrame == nil {
		return fmt.Errorf("frame %q not found in frame system", filterFrameName)
	}

	// 1. Compute filter's world pose using current joint inputs.
	filterPIF := referenceframe.NewPoseInFrame(filterFrameName, spatialmath.NewZeroPose())
	tf, err := s.cachedFS.Transform(fsInputs.ToLinearInputs(), filterPIF, referenceframe.World)
	if err != nil {
		return fmt.Errorf("transform filter to world: %w", err)
	}
	worldPose := tf.(*referenceframe.PoseInFrame).Pose()

	// 2. Get the filter's geometry in world coordinates.
	//    The RDK places part geometry on the "<name>_origin" frame (a
	//    tailGeometryStaticFrame), not on the model frame. We read it from there
	//    and use the frame system's Transform to convert it to world coordinates,
	//    which correctly applies only the parent-to-world transform (the RDK
	//    skips the frame's own transform for GeometriesInFrame objects).
	filterOriginFrameName := filterFrameName + "_origin"
	originFrame := s.cachedFS.Frame(filterOriginFrameName)
	if originFrame == nil {
		return fmt.Errorf("frame %q not found in frame system", filterOriginFrameName)
	}
	originGeos, err := originFrame.Geometries([]referenceframe.Input{})
	if err != nil {
		return fmt.Errorf("get geometries from %q: %w", filterOriginFrameName, err)
	}
	geos := originGeos.Geometries()
	if len(geos) == 0 {
		return fmt.Errorf("no geometry found on frame %q", filterOriginFrameName)
	}
	// Transform the geometry to world coordinates via the frame system so that
	// the parent-to-world transform is applied correctly.  We cannot simply call
	// geom.Transform(worldPose) because Geometries() on a tailGeometryStaticFrame
	// already pre-applies the origin offset — composing worldPose on top would
	// double-count it.
	worldGeoTF, err := s.cachedFS.Transform(
		fsInputs.ToLinearInputs(),
		referenceframe.NewGeometriesInFrame(filterOriginFrameName, geos),
		referenceframe.World,
	)
	if err != nil {
		return fmt.Errorf("transform filter geometry to world: %w", err)
	}
	worldGeos := worldGeoTF.(*referenceframe.GeometriesInFrame).Geometries()
	if len(worldGeos) == 0 {
		return fmt.Errorf("no geometry after transforming %q to world", filterOriginFrameName)
	}
	worldGeom := worldGeos[0]

	// 3. Collect filter's descendants in BFS order before removal.
	descendants := collectDescendants(s.cachedFS, filterFrameName)

	// 4. Remove filter (and all descendants) from the arm subtree.
	//    Also remove the companion "filter_origin" frame that the RDK creates
	//    for every part — it carries the collision geometry and must not remain
	//    attached to the arm.
	s.cachedFS.RemoveFrame(filterFrame)
	if filterOriginFrame := s.cachedFS.Frame(filterOriginFrameName); filterOriginFrame != nil {
		s.cachedFS.RemoveFrame(filterOriginFrame)
	}

	// 5. Re-add filter as a static frame parented to world at the locked position.
	//    The geometry is already in world coordinates (from step 2). Since the
	//    planner uses the parent-to-world transform for geometry positioning and
	//    the parent is world (identity), this places the collision volume correctly.
	newFrame, err := referenceframe.NewStaticFrameWithGeometry(filterFrameName, worldPose, worldGeom)
	if err != nil {
		return fmt.Errorf("create static filter frame: %w", err)
	}
	if err := s.cachedFS.AddFrame(newFrame, s.cachedFS.World()); err != nil {
		return fmt.Errorf("add filter frame to world: %w", err)
	}

	// 6. Re-attach descendants under the new static filter, preserving subtree structure.
	for _, d := range descendants {
		parent := s.cachedFS.Frame(d.parentName)
		if err := s.cachedFS.AddFrame(d.frame, parent); err != nil {
			return fmt.Errorf("re-add descendant %q under %q: %w", d.frame.Name(), d.parentName, err)
		}
	}

	s.filterFrameLocked = true
	logger.Infof("locked filter frame at world pose %v (%d descendants preserved)", worldPose.Point(), len(descendants))
	return nil
}

// resetFrameSystem rebuilds the cached frame system from the service, discarding
// any in-flight mutations (e.g. a filter frame that was reparented to world by
// lockFilterFrame). Shared by unlockFilterFrame during the normal brew cycle and
// by the reset_world, rewind and proceed operator commands to recover from a
// mid-cycle cancel.
//
// The fridge door is the exception: a rebuild puts the panel back at its authored
// shut transform, but rebuilding a model does not close a real door, so the
// recorded doorOpenDegs is re-applied afterward. Clearing that record is an
// operator's call, not a rebuild's — reset_world and proceed each clear it
// before calling this, and both say so in their response.
func (s *beanjaminCoffee) resetFrameSystem(ctx context.Context) error {
	logger := s.activeOrderLogger()
	fs, err := framesystem.NewFromService(ctx, s.fsSvc, nil)
	if err != nil {
		return fmt.Errorf("rebuild frame system: %w", err)
	}
	if err := applyJointLimits(logger, fs, s.cfg.InputRangeOverride); err != nil {
		return fmt.Errorf("re-apply joint limits: %w", err)
	}
	s.cachedFS = fs
	// The rebuilt frame system has no held-item frame, and any cached grasp no
	// longer corresponds to reality — forget it so a stale geometry can't be
	// re-attached after a rewind/reset. The rebuilt frame system also restores the
	// filter frame to the arm subtree (undoing any lockFilterFrame mutation) and
	// drops the staged-glass obstacle (undoing any stageGlassAsObstacle mutation).
	s.clearHeldGeometry()
	s.filterFrameLocked = false
	s.stagedGlassPlaced = false
	if s.doorOpenDegs != 0 {
		base, berr := doorBasePose(fs)
		if berr != nil {
			return fmt.Errorf("re-apply fridge door angle: %w", berr)
		}
		if derr := setDoorTheta(fs, frameFridgeDoor, base, s.doorOpenDegs); derr != nil {
			return fmt.Errorf("re-apply fridge door angle: %w", derr)
		}
		logger.Infof("frame system rebuilt with fridge door held open at %.1f°", s.doorOpenDegs)
	}
	return nil
}

// refreshFrameSystemIfClean rebuilds cachedFS from the service when no in-flight
// state would be lost — i.e. nothing is held, the filter frame is not locked, and
// no glass is staged as an obstacle — so a manually-invoked action picks up
// out-of-band config edits (e.g. the portafilter handle geometry being changed
// during calibration) instead of planning against a stale snapshot. When an item
// is held, the filter is locked, or a glass is staged, cachedFS carries state that
// must persist across separate DoCommand calls, so it is left untouched. Must be
// called on the motion sequence goroutine (gated by the running flag), like
// resetFrameSystem.
func (s *beanjaminCoffee) refreshFrameSystemIfClean(ctx context.Context) error {
	if s.heldItemAttached || s.filterFrameLocked || s.stagedGlassPlaced {
		return nil
	}
	if err := s.resetFrameSystem(ctx); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("refreshed frame system from service")
	return nil
}

// dropFilter takes the portafilter out of the frame system and returns the
// restore, to be deferred by the caller. A filter locked into the machine models
// the real portafilter in the bayonet rather than one on the claws, so it is
// left in place and the restore is a no-op.
func (s *beanjaminCoffee) dropFilter() (func(), error) {
	if s.filterFrameLocked {
		return func() {}, nil
	}
	detached, err := s.detachFilterFrame()
	if err != nil {
		return nil, err
	}
	return func() {
		if err := s.reattachFilterFrame(detached); err != nil {
			s.activeOrderLogger().Errorf("%v — the frame system no longer models the portafilter; run reset_world before brewing", err)
		}
	}, nil
}

// detachFilterFrame removes the portafilter from the cached frame system, for
// driving the arm after it has physically been taken off the gripper. Returns
// what it removed, for reattachFilterFrame.
//
// Every filterSw step fails to plan while it is gone — motion goals are keyed by
// frame name — so this is for claw-only sequences.
func (s *beanjaminCoffee) detachFilterFrame() ([]descendantEntry, error) {
	// A locked filter models the real portafilter in the machine, not one on the
	// arm; dropping it would let the arm plan straight through it.
	if s.filterFrameLocked {
		return nil, fmt.Errorf("detach filter: the filter frame is locked into the machine, so it is not on the arm; unlock_portafilter first")
	}
	// RemoveFrame takes descendants with it, and the RDK parents the model frame
	// under its "_origin", so the origin carries the whole part away.
	var removed []descendantEntry
	for _, name := range []string{componentFilter + "_origin", componentFilter} {
		root := s.cachedFS.Frame(name)
		if root == nil {
			continue
		}
		parent, err := s.cachedFS.Parent(root)
		if err != nil {
			return nil, fmt.Errorf("detach filter: parent of %q: %w", name, err)
		}
		// Root first, then BFS descendants, so reattaching in order always finds
		// the parent already present.
		removed = append(removed, descendantEntry{root, parent.Name()})
		removed = append(removed, collectDescendants(s.cachedFS, name)...)
		s.cachedFS.RemoveFrame(root)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	s.activeOrderLogger().Infof("detached %q from the frame system (%d frames) for this action", componentFilter, len(removed))
	return removed, nil
}

// reattachFilterFrame puts back what detachFilterFrame removed, parents first.
// It restores the exact frames rather than rebuilding from the service because
// the action may have attached a held item, which refreshFrameSystemIfClean
// declines to rebuild around.
func (s *beanjaminCoffee) reattachFilterFrame(removed []descendantEntry) error {
	for _, e := range removed {
		if s.cachedFS.Frame(e.frame.Name()) != nil {
			continue // already back
		}
		parent := s.cachedFS.Frame(e.parentName)
		if parent == nil {
			return fmt.Errorf("reattach filter: parent %q is gone, cannot restore %q", e.parentName, e.frame.Name())
		}
		if err := s.cachedFS.AddFrame(e.frame, parent); err != nil {
			return fmt.Errorf("reattach filter: add %q under %q: %w", e.frame.Name(), e.parentName, err)
		}
	}
	if len(removed) > 0 {
		s.activeOrderLogger().Infof("reattached %q to the frame system (%d frames)", componentFilter, len(removed))
	}
	return nil
}

// unlockFilterFrame rebuilds the cached frame system from the service,
// restoring the filter frame to its original position in the arm subtree.
func (s *beanjaminCoffee) unlockFilterFrame(ctx context.Context) error {
	logger := s.activeOrderLogger()
	if err := s.resetFrameSystem(ctx); err != nil {
		return err
	}
	logger.Infof("unlocked filter frame, frame system restored from service")
	return nil
}

type descendantEntry struct {
	frame      referenceframe.Frame
	parentName string
}

// collectDescendants returns all descendants of the given frame in BFS order.
// BFS guarantees parents appear before children, so re-adding in order will
// always find the parent frame already present.
func collectDescendants(fs *referenceframe.FrameSystem, rootName string) []descendantEntry {
	var descendants []descendantEntry
	queue := []string{rootName}
	for len(queue) > 0 {
		parentName := queue[0]
		queue = queue[1:]
		for _, name := range fs.FrameNames() {
			f := fs.Frame(name)
			p, err := fs.Parent(f)
			if err != nil || p.Name() != parentName {
				continue
			}
			descendants = append(descendants, descendantEntry{f, parentName})
			queue = append(queue, name)
		}
	}
	return descendants
}
