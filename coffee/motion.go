package coffee

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/spatialmath"

	"go.viam.com/rdk/components/arm"
	toggleswitch "go.viam.com/rdk/components/switch"
)

// errMotionPlanning is wrapped around armplanning.PlanMotion failures in
// planMotion so callers with a recovery path (e.g. dynamic cup pickup
// falling back to another candidate cup) can use errors.Is to distinguish
// planning failures from execution errors.
var errMotionPlanning = errors.New("motion planning failed")

var defaultApproachConstraint = &StepLinearConstraint{
	LineToleranceMm:          1,
	OrientationToleranceDegs: 2,
}

// pourConstraint keeps a pour pivot's held container within 5mm of the
// straight line between consecutive waypoints. Beyond bounding the drift, any linear constraint whose tighter
// tolerance is under 10 (mm or degrees) stops armplanning from falling back to
// cBiRTT between waypoints, and cBiRTT's free-form detours are what swing a
// full cup off the glass: a waypoint the planner cannot reach directly fails
// the pour instead. The 5° orientation bound is twice the deviation the
// 5°-per-waypoint slerp ever needs.
var pourConstraint = &StepLinearConstraint{
	LineToleranceMm:          5,
	OrientationToleranceDegs: 5,
}

// mergedCancelContext derives a context cancelled by either ctx or the shared
// cancelCtx, so an operator cancel interrupts planning and execution alike. The
// returned func must be deferred.
func mergedCancelContext(ctx, cancelCtx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	// AfterFunc runs cancel on its own goroutine even when cancelCtx is already
	// done, so without this check the first call made with the merged context
	// could still see it live after the operator has cancelled.
	if cancelCtx.Err() != nil {
		cancel()
		return ctx, cancel
	}
	stop := context.AfterFunc(cancelCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// motionPlanTimeout bounds a single planning attempt. armplanning's own default
// is 300s, long enough that an operator cannot tell a plan that will never
// succeed from one still searching, and long enough to hold the brew cycle open
// past the point where the drink is worth serving. Every plan here runs against
// a fixed cell whose reachable poses solve in well under a second, so overrunning
// this means the pose is unreachable from where the arm stands, not that the
// planner needs longer.
const motionPlanTimeout = 15 * time.Second

// withPlanTimeout bounds opts by motionPlanTimeout, substituting armplanning's
// own defaults when the caller passed none — the same options PlanMotion would
// have filled in, minus its 300s timeout.
func withPlanTimeout(opts *armplanning.PlannerOptions) *armplanning.PlannerOptions {
	if opts == nil {
		opts = armplanning.NewBasicPlannerOptions()
	}
	opts.Timeout = motionPlanTimeout.Seconds()
	return opts
}

// incompletePlanErr reports a plan covering only a prefix of the requested goals.
// armplanning returns one of those with a *nil* error when its deadline expires
// between goals, so without this check a timed-out multi-waypoint plan (pivot,
// circular) would execute as though complete and stop the arm short of the pose
// the next step assumes it reached.
func incompletePlanErr(meta *armplanning.PlanMeta, goals int) error {
	if meta == nil {
		return nil
	}
	if meta.GoalsProcessed >= goals {
		return nil
	}
	return fmt.Errorf("planner solved %d of %d goals before the %s timeout",
		meta.GoalsProcessed, goals, motionPlanTimeout)
}

// planDuration renders how long a plan took, for logs and error messages that
// have to distinguish "gave up at the timeout" from "failed immediately".
func planDuration(meta *armplanning.PlanMeta) time.Duration {
	if meta == nil {
		return 0
	}
	return meta.Duration.Round(time.Millisecond)
}

// planMotion plans req under motionPlanTimeout, persisting the request/response
// pair under label for offline debugging.
//
// Planning failures are wrapped in errMotionPlanning: a plan that never ran
// leaves the arm where it stood, which is what lets callers with a recovery path
// (dynamic pickup falling back to another candidate, placeHeldInServingArea
// trying the next slot) tell them apart from execution errors via errors.Is.
func (s *beanjaminCoffee) planMotion(ctx context.Context, req *armplanning.PlanRequest, label string) (motionplan.Plan, error) {
	logger := s.activeOrderLogger()
	req.PlannerOptions = withPlanTimeout(req.PlannerOptions)

	plan, meta, err := armplanning.PlanMotion(ctx, logger, req)
	if err == nil {
		err = incompletePlanErr(meta, len(req.Goals))
	}
	s.savePlanRequestAndResponse(req, plan, label, err)
	if err != nil {
		return nil, fmt.Errorf("%w (%s, after %s): %w", errMotionPlanning, label, planDuration(meta), err)
	}
	logger.Infof("planned %s in %s", label, planDuration(meta))
	return plan, nil
}

// armInputs extracts a plan's joint waypoints for the arm frame — not the
// end-effector component name the goal poses are commanded against.
func (s *beanjaminCoffee) armInputs(plan motionplan.Plan, label string) ([][]referenceframe.Input, error) {
	positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
	if err != nil {
		return nil, fmt.Errorf("get frame inputs from %s plan: %w", label, err)
	}
	return positions, nil
}

// planTrajectory plans req and returns the arm joint waypoints to execute.
func (s *beanjaminCoffee) planTrajectory(ctx context.Context, req *armplanning.PlanRequest, label string) ([][]referenceframe.Input, error) {
	plan, err := s.planMotion(ctx, req, label)
	if err != nil {
		return nil, err
	}
	return s.armInputs(plan, label)
}

// freeMoveCollisionBufferMM is the clearance the planner must keep between any
// two geometries that did not begin the motion in collision. It applies to free
// traverses — both the ordinary direct plans and the no-spill level carry — but
// not to steps carrying a LinearConstraint: those have to close the last
// millimetres onto hardware (portafilter into the group head, claws onto a cup)
// and so keep armplanning's own hair-thin default.
const freeMoveCollisionBufferMM = 2.0

// freeMovePlannerOptions returns planner options carrying the free-traverse
// collision buffer.
func freeMovePlannerOptions() *armplanning.PlannerOptions {
	opts := armplanning.NewBasicPlannerOptions()
	opts.CollisionBufferMM = freeMoveCollisionBufferMM
	return opts
}

// plannerOptionsForConstraint returns the planner options for a step's move, or
// nil to let armplanning apply its defaults. A move with no linear constraint is
// a free traverse and gets the extra collision buffer.
func plannerOptionsForConstraint(lc *StepLinearConstraint) *armplanning.PlannerOptions {
	if lc != nil {
		return nil
	}
	return freeMovePlannerOptions()
}

const (
	defaultSlowMovementVelDegsPerSec  = 25.0
	defaultSlowMovementAccDegsPerSec2 = 25.0
)

// Arm moves fall into three speeds, and a move runs at the one it names: nil
// MoveOptions is the arm's own DEFAULT speed, whatever the move's shape. SLOW
// (slowMoveOptions, below) is for the careful moves — contact (pressing a
// button, seating the portafilter, tamping, cleaning), the spill- or
// scatter-prone carries (a filled cup or glass, the milk bottle, loose grounds),
// and the pivots and circles that do either. POUR (pourMoveOptions) is the fast
// tilt of a filled container over the cup/glass. A linear constraint only keeps
// the path straight and does not slow the move, so a straight move with neither
// contact nor a load, such as a retreat with an empty gripper, stays at default.
//
// slowMoveOptions is the config-facing StepMoveOptions (degrees) for the slow
// tier, attached to a Step or converted with buildMoveOptions at an execution
// site. Velocity and acceleration fall back to their defaults when unset — the
// acceleration to a gentle value below the arm's own, since a hard acceleration
// can slosh a full cup even at a low top speed.
func (s *beanjaminCoffee) slowMoveOptions() *StepMoveOptions {
	return &StepMoveOptions{
		MaxVelDegsPerSec:  orDefault(s.cfg.SlowMovementVelDegsPerSec, defaultSlowMovementVelDegsPerSec),
		MaxAccDegsPerSec2: orDefault(s.cfg.SlowMovementAccDegsPerSec2, defaultSlowMovementAccDegsPerSec2),
	}
}

type poseData struct {
	pose          spatialmath.Pose
	refFrame      string
	componentName string
}

// fetchPose retrieves a named pose from the given switch. The returned
// poseData.componentName is the frame the goal pose is commanded against — the
// switch's configured component_name.
func (s *beanjaminCoffee) fetchPose(ctx context.Context, sw toggleswitch.Switch, poseName string) (*poseData, error) {
	if sw == nil {
		return nil, fmt.Errorf("get pose %q: no pose switch configured", poseName)
	}
	resp, err := sw.DoCommand(ctx, map[string]any{
		"get_pose_by_name": poseName,
	})
	if err != nil {
		return nil, fmt.Errorf("get pose %q from %q: %w", poseName, sw.Name().ShortName(), err)
	}

	x, _ := resp["x"].(float64)
	y, _ := resp["y"].(float64)
	z, _ := resp["z"].(float64)
	oX, _ := resp["o_x"].(float64)
	oY, _ := resp["o_y"].(float64)
	oZ, _ := resp["o_z"].(float64)
	theta, _ := resp["theta"].(float64)
	refFrame, _ := resp["reference_frame"].(string)
	if refFrame == "" {
		refFrame = referenceframe.World
	}
	componentName, _ := resp["component_name"].(string)

	pose := spatialmath.NewPose(
		r3.Vector{X: x, Y: y, Z: z},
		&spatialmath.OrientationVectorDegrees{OX: oX, OY: oY, OZ: oZ, Theta: theta},
	)

	return &poseData{pose: pose, refFrame: refFrame, componentName: componentName}, nil
}

// currentInputs settles, then returns the scene's frame system and fresh joint
// inputs.
// We build the inputs directly from the arm rather than calling fsSvc.CurrentInputs,
// which iterates all resources and can fail on modular arms whose kinematics
// proto round-trip produces KINEMATICS_FILE_FORMAT_UNSPECIFIED.
func (s *beanjaminCoffee) currentInputs(ctx context.Context) (*referenceframe.FrameSystem, referenceframe.FrameSystemInputs, error) {
	// Reading the arm is a stopping point.
	if err := s.settle(); err != nil {
		return nil, nil, err
	}
	logger := s.activeOrderLogger()
	fsInputs := referenceframe.NewZeroInputs(s.scene().fs)

	// Get current joint positions directly from the arm.
	armInputs, err := s.arm.CurrentInputs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get current inputs: %w", err)
	}

	// Use the config arm name as the key — this matches the frame name in the cached
	// frame system built from FrameSystemConfig.
	logger.Debugf("currentInputs: arm=%q, armInputsLen=%d", s.cfg.ArmName, len(armInputs))
	fsInputs[s.cfg.ArmName] = armInputs

	return s.scene().fs, fsInputs, nil
}

// expectedInputs returns where the frames will stand once every move handed off
// so far has run: the planned inputs of the run's scene, or, when the run has
// not planned where the arm ends up, the arm read now (currentInputs, a
// stopping point). It is what a scene edit is applied at, so an edit made while
// moves are still in flight lands where the arm will be when it takes effect.
// Motions recorded in an open chunk are planned and handed off first, since
// until then nothing says where they leave the arm.
func (s *beanjaminCoffee) expectedInputs(ctx context.Context) (referenceframe.FrameSystemInputs, error) {
	if s.run != nil && s.run.chunk.hasMotions() {
		if err := s.flushChunk(); err != nil {
			return nil, err
		}
	}
	if inputs := s.scene().inputs; inputs != nil {
		return inputs, nil
	}
	_, inputs, err := s.currentInputs(ctx)
	return inputs, err
}

// lockFilterFrame locks the filter frame into the machine with the arm where the
// moves handed off so far leave it (expectedInputs, sceneState.lockFilter). Call this after physically locking the
// portafilter, so all subsequent planning calls see the filter at its locked
// position.
func (s *beanjaminCoffee) lockFilterFrame(ctx context.Context) error {
	fsInputs, err := s.expectedInputs(ctx)
	if err != nil {
		return err
	}
	var worldPose spatialmath.Pose
	var descendants int
	if err := s.editScene(func(sc *sceneState) error {
		var err error
		worldPose, descendants, err = sc.lockFilter(fsInputs)
		return err
	}); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("locked filter frame at world pose %v (%d descendants preserved)", worldPose.Point(), descendants)
	return nil
}

// lockFilter re-parents the "filter" frame from the arm subtree to the world at
// the pose it has with the frames standing at fsInputs. Returns that pose and
// how many descendant frames moved with it.
func (sc *sceneState) lockFilter(fsInputs referenceframe.FrameSystemInputs) (spatialmath.Pose, int, error) {
	const filterFrameName = componentFilter

	filterFrame := sc.fs.Frame(filterFrameName)
	if filterFrame == nil {
		return nil, 0, fmt.Errorf("frame %q not found in frame system", filterFrameName)
	}

	// 1. Compute filter's world pose at fsInputs.
	filterPIF := referenceframe.NewPoseInFrame(filterFrameName, spatialmath.NewZeroPose())
	tf, err := sc.fs.Transform(fsInputs.ToLinearInputs(), filterPIF, referenceframe.World)
	if err != nil {
		return nil, 0, fmt.Errorf("transform filter to world: %w", err)
	}
	worldPose := tf.(*referenceframe.PoseInFrame).Pose()

	// 2. Get the filter's geometry in world coordinates.
	//    The RDK places part geometry on the "<name>_origin" frame (a
	//    tailGeometryStaticFrame), not on the model frame. We read it from there
	//    and use the frame system's Transform to convert it to world coordinates,
	//    which correctly applies only the parent-to-world transform (the RDK
	//    skips the frame's own transform for GeometriesInFrame objects).
	filterOriginFrameName := filterFrameName + "_origin"
	originFrame := sc.fs.Frame(filterOriginFrameName)
	if originFrame == nil {
		return nil, 0, fmt.Errorf("frame %q not found in frame system", filterOriginFrameName)
	}
	originGeos, err := originFrame.Geometries([]referenceframe.Input{})
	if err != nil {
		return nil, 0, fmt.Errorf("get geometries from %q: %w", filterOriginFrameName, err)
	}
	geos := originGeos.Geometries()
	if len(geos) == 0 {
		return nil, 0, fmt.Errorf("no geometry found on frame %q", filterOriginFrameName)
	}
	// Transform the geometry to world coordinates via the frame system so that
	// the parent-to-world transform is applied correctly.  We cannot simply call
	// geom.Transform(worldPose) because Geometries() on a tailGeometryStaticFrame
	// already pre-applies the origin offset — composing worldPose on top would
	// double-count it.
	worldGeoTF, err := sc.fs.Transform(
		fsInputs.ToLinearInputs(),
		referenceframe.NewGeometriesInFrame(filterOriginFrameName, geos),
		referenceframe.World,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("transform filter geometry to world: %w", err)
	}
	worldGeos := worldGeoTF.(*referenceframe.GeometriesInFrame).Geometries()
	if len(worldGeos) == 0 {
		return nil, 0, fmt.Errorf("no geometry after transforming %q to world", filterOriginFrameName)
	}
	worldGeom := worldGeos[0]

	// 3. Collect filter's descendants in BFS order before removal.
	descendants := collectDescendants(sc.fs, filterFrameName)

	// 4. Remove filter (and all descendants) from the arm subtree.
	//    Also remove the companion "filter_origin" frame that the RDK creates
	//    for every part — it carries the collision geometry and must not remain
	//    attached to the arm.
	sc.fs.RemoveFrame(filterFrame)
	if filterOriginFrame := sc.fs.Frame(filterOriginFrameName); filterOriginFrame != nil {
		sc.fs.RemoveFrame(filterOriginFrame)
	}

	// 5. Re-add filter as a static frame parented to world at the locked position.
	//    The geometry is already in world coordinates (from step 2). Since the
	//    planner uses the parent-to-world transform for geometry positioning and
	//    the parent is world (identity), this places the collision volume correctly.
	newFrame, err := referenceframe.NewStaticFrameWithGeometry(filterFrameName, worldPose, worldGeom)
	if err != nil {
		return nil, 0, fmt.Errorf("create static filter frame: %w", err)
	}
	if err := sc.fs.AddFrame(newFrame, sc.fs.World()); err != nil {
		return nil, 0, fmt.Errorf("add filter frame to world: %w", err)
	}

	// 6. Re-attach descendants under the new static filter, preserving subtree structure.
	for _, d := range descendants {
		parent := sc.fs.Frame(d.parentName)
		if err := sc.fs.AddFrame(d.frame, parent); err != nil {
			return nil, 0, fmt.Errorf("re-add descendant %q under %q: %w", d.frame.Name(), d.parentName, err)
		}
	}

	sc.filterFrameLocked = true
	return worldPose, len(descendants), nil
}

// resetFrameSystem rebuilds the scene from the service, discarding any
// in-flight mutations (e.g. a filter frame that was reparented to world by
// lockFilterFrame). Shared by unlockFilterFrame during the normal brew cycle and
// by refreshFrameSystemIfClean.
//
// The fridge door is the exception: a rebuild puts the panel back at its authored
// shut transform, but rebuilding a model does not close a real door, so the
// recorded doorOpenDegs is re-applied afterward. Clearing that record is an
// operator's call, not a rebuild's — reset_world and proceed rebuild with the
// door shut (rebuildScene), and both say so in their response.
func (s *beanjaminCoffee) resetFrameSystem(ctx context.Context) error {
	return s.rebuildScene(ctx, s.scene().doorOpenDegs)
}

// rebuildScene replaces the scene with one built from the service, with the
// fridge door standing doorOpenDegs open. The rebuilt frame system has no
// held-item frame, and any cached grasp no longer corresponds to reality, so
// none is carried over and a stale geometry can't be re-attached after a reset.
// It also has the filter frame back in the arm subtree and no staged-glass
// obstacle. On failure the scene is left as it was.
func (s *beanjaminCoffee) rebuildScene(ctx context.Context, doorOpenDegs float64) error {
	logger := s.activeOrderLogger()
	fs, err := framesystem.NewFromService(ctx, s.fsSvc, nil)
	if err != nil {
		return fmt.Errorf("rebuild frame system: %w", err)
	}
	if err := applyJointLimits(logger, fs, s.cfg.InputRangeOverride); err != nil {
		return fmt.Errorf("re-apply joint limits: %w", err)
	}
	if doorOpenDegs != 0 {
		base, berr := doorBasePose(fs)
		if berr != nil {
			return fmt.Errorf("re-apply fridge door angle: %w", berr)
		}
		if derr := setDoorTheta(fs, frameFridgeDoor, base, doorOpenDegs); derr != nil {
			return fmt.Errorf("re-apply fridge door angle: %w", derr)
		}
		logger.Infof("frame system rebuilt with fridge door held open at %.1f°", doorOpenDegs)
	}
	s.publishScene(&sceneState{fs: fs, doorOpenDegs: doorOpenDegs})
	return nil
}

// refreshFrameSystemIfClean rebuilds the scene from the service when no in-flight
// state would be lost — i.e. nothing is held, no milk bottle is out of the
// fridge, the filter frame is not locked, and no glass is staged as an obstacle
// — so a manually-invoked action picks up out-of-band config edits (e.g. the
// portafilter handle geometry being changed during calibration) instead of
// planning against a stale snapshot. Otherwise the scene carries state that must
// persist across separate DoCommand calls, so it is left untouched.
func (s *beanjaminCoffee) refreshFrameSystemIfClean(ctx context.Context) error {
	// heldMilk on its own: the rebuild would clear it, and return_milk would no
	// longer know which spot the bottle in the gripper goes back on.
	if s.scene().heldItemAttached || s.scene().heldMilk != "" || s.scene().filterFrameLocked || s.scene().stagedGlassPlaced {
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
	if s.scene().filterFrameLocked {
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

// detachFilterFrame removes the portafilter from the scene
// (sceneState.detachFilter). Returns what it removed, for reattachFilterFrame.
func (s *beanjaminCoffee) detachFilterFrame() ([]descendantEntry, error) {
	var removed []descendantEntry
	if err := s.editScene(func(sc *sceneState) error {
		var err error
		removed, err = sc.detachFilter()
		return err
	}); err != nil {
		return nil, err
	}
	if len(removed) == 0 {
		return nil, nil
	}
	s.activeOrderLogger().Infof("detached %q from the frame system (%d frames) for this action", componentFilter, len(removed))
	return removed, nil
}

// detachFilter removes the portafilter from the frame system, for driving the
// arm after it has physically been taken off the gripper. Returns what it
// removed, for reattachFilter.
//
// Every filterSw step fails to plan while it is gone — motion goals are keyed by
// frame name — so this is for claw-only sequences.
func (sc *sceneState) detachFilter() ([]descendantEntry, error) {
	// A locked filter models the real portafilter in the machine, not one on the
	// arm; dropping it would let the arm plan straight through it.
	if sc.filterFrameLocked {
		return nil, fmt.Errorf("detach filter: the filter frame is locked into the machine, so it is not on the arm; unlock_portafilter first")
	}
	// RemoveFrame takes descendants with it, and the RDK parents the model frame
	// under its "_origin", so the origin carries the whole part away.
	var removed []descendantEntry
	for _, name := range []string{componentFilter + "_origin", componentFilter} {
		root := sc.fs.Frame(name)
		if root == nil {
			continue
		}
		parent, err := sc.fs.Parent(root)
		if err != nil {
			return nil, fmt.Errorf("detach filter: parent of %q: %w", name, err)
		}
		// Root first, then BFS descendants, so reattaching in order always finds
		// the parent already present.
		removed = append(removed, descendantEntry{root, parent.Name()})
		removed = append(removed, collectDescendants(sc.fs, name)...)
		sc.fs.RemoveFrame(root)
	}
	return removed, nil
}

// reattachFilterFrame puts back what detachFilterFrame removed
// (sceneState.reattachFilter).
func (s *beanjaminCoffee) reattachFilterFrame(removed []descendantEntry) error {
	if len(removed) == 0 {
		return nil
	}
	if err := s.editScene(func(sc *sceneState) error { return sc.reattachFilter(removed) }); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("reattached %q to the frame system (%d frames)", componentFilter, len(removed))
	return nil
}

// reattachFilter puts back what detachFilter removed, parents first. It
// restores the exact frames rather than rebuilding from the service because the
// action may have attached a held item, which refreshFrameSystemIfClean
// declines to rebuild around.
func (sc *sceneState) reattachFilter(removed []descendantEntry) error {
	for _, e := range removed {
		if sc.fs.Frame(e.frame.Name()) != nil {
			continue // already back
		}
		parent := sc.fs.Frame(e.parentName)
		if parent == nil {
			return fmt.Errorf("reattach filter: parent %q is gone, cannot restore %q", e.parentName, e.frame.Name())
		}
		if err := sc.fs.AddFrame(e.frame, parent); err != nil {
			return fmt.Errorf("reattach filter: add %q under %q: %w", e.frame.Name(), e.parentName, err)
		}
	}
	return nil
}

// unlockFilterFrame rebuilds the scene from the service,
// restoring the filter frame to its original position in the arm subtree.
func (s *beanjaminCoffee) unlockFilterFrame(ctx context.Context) error {
	if err := s.settle(); err != nil {
		return err
	}
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

// fakeMissingFrames are gripper sub-geometries that only exist on the real
// ufactory gripper. When running against a fake barista (FakeMode=true),
// AllowedCollision entries referencing these frames are dropped so motion
// planning doesn't fail on unknown frames.
var fakeMissingFrames = []string{"gripper:claws", "gripper:case-gripper"}

// filterFakeModeCollisions drops AllowedCollision entries that reference a
// frame in fakeMissingFrames. Returns the input unchanged when FakeMode is off.
func (s *beanjaminCoffee) filterFakeModeCollisions(acs []AllowedCollision) []AllowedCollision {
	logger := s.activeOrderLogger()
	if !s.cfg.FakeMode {
		return acs
	}
	out := make([]AllowedCollision, 0, len(acs))
	for _, ac := range acs {
		if slices.Contains(fakeMissingFrames, ac.Frame1) || slices.Contains(fakeMissingFrames, ac.Frame2) {
			logger.Debugf("fake mode: dropping allowed collision %s <-> %s", ac.Frame1, ac.Frame2)
			continue
		}
		out = append(out, ac)
	}
	return out
}

// buildConstraints converts step-level linear constraints and allowed collisions
// into the motionplan.Constraints structure used by armplanning.
func buildConstraints(lc *StepLinearConstraint, allowedCollisions []AllowedCollision) *motionplan.Constraints {
	if lc == nil && len(allowedCollisions) == 0 {
		return nil
	}
	constraints := &motionplan.Constraints{}
	if lc != nil {
		constraints.LinearConstraint = []motionplan.LinearConstraint{
			{
				LineToleranceMm:          lc.LineToleranceMm,
				OrientationToleranceDegs: lc.OrientationToleranceDegs,
			},
		}
	}
	if len(allowedCollisions) > 0 {
		allows := make([]motionplan.CollisionSpecificationAllowedFrameCollisions, len(allowedCollisions))
		for i, ac := range allowedCollisions {
			allows[i] = motionplan.CollisionSpecificationAllowedFrameCollisions{
				Frame1: ac.Frame1,
				Frame2: ac.Frame2,
			}
		}
		constraints.CollisionSpecification = []motionplan.CollisionSpecification{
			{Allows: allows},
		}
	}
	return constraints
}

// withPathOrientationConstraint bounds the moving frame's orientation to within
// degs of the slerp between the plan's start and goal along the whole path. It
// bounds all three rotation axes, unlike the no-spill carry's, because a step's
// goal frame need not have +Z along the axis spin about which is harmless.
// Returns constraints unchanged when degs is not positive.
func withPathOrientationConstraint(constraints *motionplan.Constraints, degs float64) *motionplan.Constraints {
	if degs <= 0 {
		return constraints
	}
	if constraints == nil {
		constraints = &motionplan.Constraints{}
	}
	constraints.OrientationConstraint = append(constraints.OrientationConstraint,
		motionplan.OrientationConstraint{OrientationToleranceDegs: degs})
	return constraints
}

// buildMoveOptions converts step-level move options into arm.MoveOptions; nil
// leaves the arm at its default speed.
func buildMoveOptions(opts *StepMoveOptions) *arm.MoveOptions {
	if opts == nil {
		return nil
	}
	return &arm.MoveOptions{
		MaxVelRads: opts.MaxVelDegsPerSec * math.Pi / 180.0,
		MaxAccRads: opts.MaxAccDegsPerSec2 * math.Pi / 180.0,
	}
}

// Planning-outcome tag values for synced plan-request files (see planRequestTagDir).
const (
	tagPlanningSuccess = "planning_success"
	tagPlanningFailure = "planning_failure"
)

// planStampLayout starts every saved plan file's name. The web app's
// motion-plan panel (web-app/app/home/data.ts, PLAN_STAMP) reads the time back
// out of it and lists an order's plans sorted by file name.
const planStampLayout = "20060102_150405.000"

// savePlanRequestAndResponse persists a PlanRequest together with the plan it
// produced — nil on a planning failure — to a single JSON file named
// <stamp>_<label>.json (savePlanFile).
func (s *beanjaminCoffee) savePlanRequestAndResponse(req *armplanning.PlanRequest, plan motionplan.Plan, label string, planErr error) {
	s.savePlanFile(req, plan, label, planErr, time.Now().Format(planStampLayout)+"_"+label)
}

// savePlanFile persists a PlanRequest together with the plan it produced — nil
// on a planning failure — to name.json, using RDK's
// WriteRequestAndResponseToFile so the pair round-trips through
// ReadRequestAndResponseFromFile. It is a no-op when SaveMotionRequestsDir is
// empty.
func (s *beanjaminCoffee) savePlanFile(req *armplanning.PlanRequest, plan motionplan.Plan, label string, planErr error, name string) {
	logger := s.activeOrderLogger()
	dir := s.cfg.SaveMotionRequestsDir
	if dir == "" {
		return
	}
	outcome := tagPlanningSuccess
	if planErr != nil {
		outcome = tagPlanningFailure
	}
	orderID := s.queue.CurrentID()
	step, _ := s.currentStep.Load().(string)
	tagDir := planRequestTagDir(dir, orderID, step, label, outcome)
	if err := os.MkdirAll(tagDir, 0o755); err != nil {
		logger.Warnf("save plan request: create dir: %v", err)
		return
	}
	filename := filepath.Join(tagDir, name+".json")
	if err := req.WriteRequestAndResponseToFile(filename, plan); err != nil {
		logger.Warnf("save plan request: %v", err)
		return
	}
	logger.Infof("saved plan request+response (%s) to %s", outcome, filename)
}

// planRequestTagDir nests the file under tag=<value> directories — order ID,
// step, motion label, and planning outcome — which the Viam data manager reads
// on sync to tag the uploaded file (see inferTagsAndDatasetIDsFromPath), making
// it filterable on the data page. Empty values (e.g. a plan issued outside an
// order) are skipped.
//
// These tag values are a cross-repo contract: the web app's motion-plan panel
// (web-app/app/home/data.ts, loadPlanRequestsForOrder) parses the "step_",
// "motion_" and "planning_" prefixes to group and label plans. It deploys
// separately, so renaming a prefix here empties that panel with no build
// failure on either side.
func planRequestTagDir(baseDir, orderID, step, label, outcome string) string {
	parts := []string{baseDir}
	for _, tag := range []string{orderID, stepTag(step), "motion_" + label, outcome} {
		if tag == "" {
			continue
		}
		parts = append(parts, "tag="+tag)
	}
	return filepath.Join(parts...)
}

// stepTag slugifies a step label ("Locking portafilter") into a tag-safe token
// ("step_locking_portafilter"), or "" when there is no active step.
func stepTag(step string) string {
	if slug := slugify(step); slug != "" {
		return "step_" + slug
	}
	return ""
}

// slugify lowercases s and keeps only its letters and digits, with one
// underscore between each run of them: "Fetch, pour and return milk" becomes
// "fetch_pour_and_return_milk".
func slugify(s string) string {
	var b strings.Builder
	pendingUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingUnderscore && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingUnderscore = false
			b.WriteRune(r)
			continue
		}
		pendingUnderscore = true
	}
	return b.String()
}

// moveToRawPose moves the arm straight to pd: recorded in the open chunk, or
// planned and run as a chunk of its own (addMotion).
func (s *beanjaminCoffee) moveToRawPose(ctx context.Context, pd *poseData, lc *StepLinearConstraint, allowedCollisions []AllowedCollision, moveOpts *StepMoveOptions) error {
	return s.addMotion(ctx, context.Background(), "raw pose", chunkOp{},
		func(start referenceframe.FrameSystemInputs) (*motionRequest, error) {
			return s.rawMotion("raw pose", pd, lc, 0, allowedCollisions, moveOpts, start)
		})
}

// planToRawPose plans a motion to pd starting from startInputs, without touching
// the arm. Planning failures are wrapped in errMotionPlanning so callers with a
// recovery path (e.g. dynamic pickup falling back to another candidate) can
// distinguish them from execution errors. Both the destination transform and the
// plan begin from startInputs, so passing a prior plan's end configuration
// (planEndArmInputs) chains a second plan onto the first — letting a caller
// prepare two moves and commit to executing them only if both plan successfully
// (see tryGrab).
func (s *beanjaminCoffee) planToRawPose(
	ctx context.Context,
	startInputs referenceframe.FrameSystemInputs,
	pd *poseData,
	lc *StepLinearConstraint,
	orientTolDegs float64,
	allowedCollisions []AllowedCollision,
) (motionplan.Plan, error) {
	m, err := s.rawMotion("raw pose", pd, lc, orientTolDegs, allowedCollisions, nil, startInputs)
	if err != nil {
		return nil, err
	}
	return s.planMotion(ctx, m.req, m.label)
}

// executePlan sends a planned trajectory to the arm.
func (s *beanjaminCoffee) executePlan(ctx context.Context, plan motionplan.Plan, moveOpts *StepMoveOptions) error {
	positions, err := s.armInputs(plan, "move")
	if err != nil {
		return err
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, buildMoveOptions(moveOpts), nil)
}

// planEndArmInputs returns the arm's joint configuration at the end of a plan's
// trajectory. Errors on an empty trajectory.
func (s *beanjaminCoffee) planEndArmInputs(plan motionplan.Plan) ([]referenceframe.Input, error) {
	positions, err := s.armInputs(plan, "move")
	if err != nil {
		return nil, err
	}
	if len(positions) == 0 {
		return nil, fmt.Errorf("plan has an empty trajectory")
	}
	return positions[len(positions)-1], nil
}

// withArmInputs returns a copy of base with the arm frame's inputs replaced by
// armInputs, leaving base unmutated. Used to build a chained plan's start state:
// the same frame-system configuration currentInputs produces (all frames zeroed,
// arm set), but with the arm advanced to a prior plan's end.
func (s *beanjaminCoffee) withArmInputs(base referenceframe.FrameSystemInputs, armInputs []referenceframe.Input) referenceframe.FrameSystemInputs {
	out := make(referenceframe.FrameSystemInputs, len(base))
	maps.Copy(out, base)
	out[s.cfg.ArmName] = armInputs
	return out
}

// pivotMmPerStep caps how far a translating pivot's position advances between
// waypoints, so a long shift paired with a small tilt is still sampled finely
// enough that the planner cannot wander off the straight line between them.
const pivotMmPerStep = 5.0

// pivotSeedPoint returns the point on the segment from start to end closest to
// actual, and actual's distance from it. A pivot may begin anywhere along that
// segment rather than only at start, so the check measures drift off the
// pivot line rather than progress along it.
func pivotSeedPoint(start, end, actual r3.Vector) (r3.Vector, float64) {
	seg := end.Sub(start)
	seed := start
	if lenSq := seg.Norm2(); lenSq > 0 {
		t := min(max(actual.Sub(start).Dot(seg)/lenSq, 0), 1)
		seed = start.Add(seg.Mul(t))
	}
	return seed, actual.Sub(seed).Norm()
}

// computeCircularPoses generates waypoints evenly spaced around a circle in
// the XY plane of the given center pose. Orientation is kept constant.
// It returns pointsPerRev poses forming one full revolution (the closing
// point at 360° equals the opening point at 0° and is omitted).
func computeCircularPoses(centerPose spatialmath.Pose, radiusMm float64, pointsPerRev int) []spatialmath.Pose {
	center := centerPose.Point()
	poses := make([]spatialmath.Pose, pointsPerRev)
	for i := range pointsPerRev {
		angle := 2 * math.Pi * float64(i) / float64(pointsPerRev)
		offset := r3.Vector{X: radiusMm * math.Cos(angle), Y: radiusMm * math.Sin(angle), Z: 0}
		poses[i] = spatialmath.NewPose(center.Add(offset), centerPose.Orientation())
	}
	return poses
}

// noSpillOrientationToleranceDegs caps how far the carried container's
// orientation may stray from the slerp between the carry's start and its goal.
// RDK checks this along the whole path as a tube of this width around that
// direct reorientation, so a single start-to-goal segment is enough to hold the
// drink near level for the entire traverse.
//
// The bound is on the excursion off the slerp, not on the commanded rotation:
// both endpoints are upright container poses, so the slerp itself stays level
// and 15° leaves the drink well inside a full cup's static spill angle.
//
// The constraint ignores theta — spin about the moving frame's own +Z, which is
// the container's vertical axis (heldItemFramePose). A cup or bottle spills when
// tipped, not when spun about its axis, so bounding spin would only shrink the
// planner's reachable space without keeping the drink any more level.
const noSpillOrientationToleranceDegs = 15.0

// withNoSpillOrientationConstraint adds the carry's path orientation bound,
// allocating the Constraints when the caller has none (no linear constraint and
// no allowed collisions, so buildConstraints returned nil).
func withNoSpillOrientationConstraint(constraints *motionplan.Constraints) *motionplan.Constraints {
	if constraints == nil {
		constraints = &motionplan.Constraints{}
	}
	constraints.OrientationConstraint = append(constraints.OrientationConstraint,
		motionplan.OrientationConstraint{OrientationToleranceDegs: noSpillOrientationToleranceDegs, IgnoreTheta: true})
	return constraints
}

// carryHeldLevel carries the held container to dest, level (carryMotion):
// recorded in the open chunk, or planned and run as a chunk of its own
// (addMotion).
func (s *beanjaminCoffee) carryHeldLevel(ctx context.Context, dest *poseData, allowedCollisions []AllowedCollision, moveOpts *StepMoveOptions) error {
	return s.addMotion(ctx, context.Background(), "carry", chunkOp{},
		func(start referenceframe.FrameSystemInputs) (*motionRequest, error) {
			return s.carryMotion("carry", dest, allowedCollisions, moveOpts, start)
		})
}

// minPivotThetaRads is the smallest authored rotation an overshoot direction
// can be derived from. Below it QuatToR4AA's axis is numerical noise (it falls
// back to a hardcoded +Z), so continuing "along the same axis" is meaningless.
const minPivotThetaRads = 1e-3

// pivotOvershootPose returns endPose rotated a further degrees about the same
// axis that carries startPose's orientation to endPose's, with the position
// left untouched. Positive degrees continues past endPose in the direction of
// travel, so callers do not have to know the pivot's handedness.
//
// The extra rotation pre-multiplies because QuatBetween is a left difference
// (q2 * conj(q1)) — the axis is expressed in the poses' shared reference frame,
// not the tool's body frame.
func pivotOvershootPose(startPose, endPose spatialmath.Pose, degrees float64) (spatialmath.Pose, error) {
	aa := spatialmath.OrientationBetween(startPose.Orientation(), endPose.Orientation()).AxisAngles()
	// AxisAngles reports the same rotation as either (axis, +θ) or (-axis, -θ).
	// Normalize to a positive angle so `degrees` continues the travel direction
	// instead of silently reversing into it. Same signed-Theta trap that
	// computePivotPoses guards against with math.Abs.
	if aa.Theta < 0 {
		aa = &spatialmath.R4AA{Theta: -aa.Theta, RX: -aa.RX, RY: -aa.RY, RZ: -aa.RZ}
	}
	if aa.Theta < minPivotThetaRads {
		return nil, fmt.Errorf("rotation is %.5f rad, too small to derive an overshoot axis", aa.Theta)
	}
	extra := spatialmath.NewPoseFromOrientation(&spatialmath.R4AA{
		Theta: degrees * math.Pi / 180.0, RX: aa.RX, RY: aa.RY, RZ: aa.RZ,
	})
	rotated := spatialmath.Compose(extra, spatialmath.NewPoseFromOrientation(endPose.Orientation()))
	return spatialmath.NewPose(endPose.Point(), rotated.Orientation()), nil
}

// computePivotPoses returns interpolated poses between startPose and endPose:
// the position moves along the straight line between them while the
// orientation slerps, in lockstep. The step count is the larger of the
// rotation over degreesPerStep and the translation over pivotMmPerStep.
func computePivotPoses(logger logging.Logger, startPose, endPose spatialmath.Pose, degreesPerStep float64) []spatialmath.Pose {
	diff := spatialmath.OrientationBetween(startPose.Orientation(), endPose.Orientation())
	// AxisAngles().Theta is signed: the axis/angle pair can come back as
	// (axis, +θ) or (-axis, -θ) depending on the rotation. Use the magnitude so a
	// negative angle doesn't collapse numSteps to 1 (max(1, round(negative)) == 1),
	// which would degenerate the pivot into a single straight-to-goal waypoint.
	totalRadians := math.Abs(diff.AxisAngles().Theta)
	totalDegrees := totalRadians * 180.0 / math.Pi

	totalMm := endPose.Point().Sub(startPose.Point()).Norm()

	numSteps := max(1, int(math.Round(totalDegrees/degreesPerStep)), int(math.Round(totalMm/pivotMmPerStep)))

	logger.Infof("pivot: %.1f° rotation, %.1f mm translation (%d steps, max %.1f°/step, %.1f mm/step)",
		totalDegrees, totalMm, numSteps, degreesPerStep, pivotMmPerStep)

	poses := make([]spatialmath.Pose, numSteps+1)
	for i := 0; i <= numSteps; i++ {
		t := float64(i) / float64(numSteps)
		poses[i] = spatialmath.Interpolate(startPose, endPose, t)
	}
	return poses
}
