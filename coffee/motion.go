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
	viz "github.com/viam-labs/motion-tools/client/api"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/spatialmath"

	"go.viam.com/rdk/components/arm"
	toggleswitch "go.viam.com/rdk/components/switch"
)

// errMotionPlanning is wrapped around armplanning.PlanMotion failures in
// moveToRawPose so callers with a recovery path (e.g. dynamic cup pickup
// falling back to another candidate cup) can use errors.Is to distinguish
// planning failures from execution errors.
var errMotionPlanning = errors.New("motion planning failed")

var defaultApproachConstraint = &StepLinearConstraint{
	LineToleranceMm:          1,
	OrientationToleranceDegs: 2,
}

// mergedCancelContext derives a context cancelled by either ctx or the shared
// cancelCtx, so an operator cancel interrupts planning and execution alike. The
// returned func must be deferred.
func mergedCancelContext(ctx, cancelCtx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
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
	if meta == nil || meta.GoalsProcessed >= goals {
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

// worldGoals turns poses authored in refFrame into plan states commanding
// componentName, each transformed into the world frame.
func worldGoals(
	fs *referenceframe.FrameSystem,
	inputs *referenceframe.LinearInputs,
	refFrame, componentName string,
	poses []spatialmath.Pose,
) ([]*armplanning.PlanState, error) {
	goals := make([]*armplanning.PlanState, 0, len(poses))
	for _, pose := range poses {
		tf, err := fs.Transform(inputs, referenceframe.NewPoseInFrame(refFrame, pose), referenceframe.World)
		if err != nil {
			return nil, fmt.Errorf("transform waypoint to world: %w", err)
		}
		goals = append(goals, armplanning.NewPlanState(
			referenceframe.FrameSystemPoses{componentName: tf.(*referenceframe.PoseInFrame)}, nil,
		))
	}
	return goals, nil
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

// Arm moves fall into three speeds. Most run at the arm's own DEFAULT speed
// (no override) — an ordinary free traverse with an empty gripper, or a tamped
// puck. SLOW (slowMoveOptions, below) is for the careful moves: a
// constrained/contact step, a pivot or circular motion, and the spill- or
// scatter-prone carries — the no-spill cup/glass carry, the milk bottle, and
// loose grounds on the way to the tamper. POUR (pourMoveOptions) is the fast
// tilt of a filled container over the cup/glass. An explicit Step.MoveOptions
// overrides the default.
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

// moveToPose fetches a named pose and moves to it.
func (s *beanjaminCoffee) moveToPose(ctx, cancelCtx context.Context, step Step) error {
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	pd, err := s.fetchPose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return err
	}
	// A filled-container traverse (NoSpill) routes through the level carry so the
	// drink doesn't slosh. The carry adds an orientation constraint in place of a
	// LinearConstraint, but still honors the step's AllowedCollisions and
	// MoveOptions. For every ordinary step, plan straight to the pose.
	if step.NoSpill {
		if err := s.carryHeldLevel(ctx, pd, step.AllowedCollisions, step.MoveOptions); err != nil {
			return fmt.Errorf("no-spill carry to %q failed: %w", step.PoseName, err)
		}
		return nil
	}
	if err := s.moveToRawPose(ctx, pd, step.LinearConstraint, step.AllowedCollisions, step.MoveOptions); err != nil {
		return fmt.Errorf("move to %q failed: %w", step.PoseName, err)
	}
	return nil
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

	if s.vizEnabled {
		s.drawViz(fsInputs)
	}

	return s.cachedFS, fsInputs, nil
}

const (
	vizTimeout     = 2 * time.Second
	vizMaxFailures = 3
)

// drawViz sends the current frame system to the visualizer with a timeout.
// After vizMaxFailures consecutive failures the visualizer is automatically
// disabled so that an unreachable server does not slow down every motion call.
func (s *beanjaminCoffee) drawViz(fsInputs referenceframe.FrameSystemInputs) {
	logger := s.activeOrderLogger()
	done := make(chan error, 1)
	go func() {
		_, err := viz.DrawFrameSystem(viz.DrawFrameSystemOptions{
			FrameSystem: s.cachedFS,
			Inputs:      fsInputs,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			s.vizConsecutiveFailures++
			logger.Warnf("viz: failed to draw frame system (%d/%d): %v",
				s.vizConsecutiveFailures, vizMaxFailures, err)
		} else {
			s.vizConsecutiveFailures = 0
		}
	case <-time.After(vizTimeout):
		s.vizConsecutiveFailures++
		logger.Warnf("viz: draw timed out after %v (%d/%d)",
			vizTimeout, s.vizConsecutiveFailures, vizMaxFailures)
	}

	if s.vizConsecutiveFailures >= vizMaxFailures {
		logger.Warnf("viz: disabling visualizer after %d consecutive failures", vizMaxFailures)
		s.vizEnabled = false
	}
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

// buildMoveOptions converts step-level move options into arm.MoveOptions.
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

// savePlanRequestAndResponse persists a PlanRequest together with the plan it
// produced — nil on a planning failure — to a single JSON file, using RDK's
// WriteRequestAndResponseToFile so the pair round-trips through
// ReadRequestAndResponseFromFile. It is a no-op when SaveMotionRequestsDir is
// empty.
func (s *beanjaminCoffee) savePlanRequestAndResponse(req *armplanning.PlanRequest, plan motionplan.Plan, label string, planErr error) {
	logger := s.activeOrderLogger()
	dir := s.cfg.SaveMotionRequestsDir
	if dir == "" {
		return
	}
	outcome := tagPlanningSuccess
	if planErr != nil {
		outcome = tagPlanningFailure
	}
	orderID, _ := s.currentOrderID.Load().(string)
	step, _ := s.currentStep.Load().(string)
	tagDir := planRequestTagDir(dir, orderID, step, label, outcome)
	if err := os.MkdirAll(tagDir, 0o755); err != nil {
		logger.Warnf("save plan request: create dir: %v", err)
		return
	}
	filename := filepath.Join(tagDir, fmt.Sprintf("%s_%s.json", time.Now().Format("20060102_150405.000"), label))
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
	var b strings.Builder
	pendingUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(step)) {
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
	if b.Len() == 0 {
		return ""
	}
	return "step_" + b.String()
}

// moveToRawPose plans a motion using armplanning and executes it on the arm.
func (s *beanjaminCoffee) moveToRawPose(ctx context.Context, pd *poseData, lc *StepLinearConstraint, allowedCollisions []AllowedCollision, moveOpts *StepMoveOptions) error {
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	plan, err := s.planToRawPose(ctx, fs, fsInputs, pd, lc, allowedCollisions)
	if err != nil {
		return err
	}
	return s.executePlan(ctx, plan, lc, moveOpts)
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
	fs *referenceframe.FrameSystem,
	startInputs referenceframe.FrameSystemInputs,
	pd *poseData,
	lc *StepLinearConstraint,
	allowedCollisions []AllowedCollision,
) (motionplan.Plan, error) {
	logger := s.activeOrderLogger()

	// Transform destination to world frame.
	destination := referenceframe.NewPoseInFrame(pd.refFrame, pd.pose)
	tf, err := fs.Transform(startInputs.ToLinearInputs(), destination, referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("transform destination to world: %w", err)
	}
	goalPose := tf.(*referenceframe.PoseInFrame)

	allowedCollisions = s.filterFakeModeCollisions(s.appendHeldItemCollisions(allowedCollisions))
	constraints := buildConstraints(lc, allowedCollisions)
	plannerOpts := plannerOptionsForConstraint(lc)
	if lc != nil {
		logger.Infof("applying linear constraint (line=%.1fmm, orient=%.1f°)",
			lc.LineToleranceMm, lc.OrientationToleranceDegs)
	} else {
		logger.Infof("applying %.1fmm collision buffer", plannerOpts.CollisionBufferMM)
	}
	if len(allowedCollisions) > 0 {
		logger.Infof("allowing %d collision pair(s)", len(allowedCollisions))
	}

	req := &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals: []*armplanning.PlanState{
			armplanning.NewPlanState(referenceframe.FrameSystemPoses{pd.componentName: goalPose}, nil),
		},
		StartState:     armplanning.NewPlanState(nil, startInputs),
		Constraints:    constraints,
		PlannerOptions: plannerOpts,
	}
	return s.planMotion(ctx, req, "move")
}

// executePlan sends a planned trajectory to the arm.
func (s *beanjaminCoffee) executePlan(ctx context.Context, plan motionplan.Plan, lc *StepLinearConstraint, moveOpts *StepMoveOptions) error {
	positions, err := s.armInputs(plan, "move")
	if err != nil {
		return err
	}
	// A constrained move with no explicit speed defaults to the slow tier; a free
	// traverse keeps the arm's own default speed (nil).
	opts := buildMoveOptions(moveOpts)
	if opts == nil && lc != nil {
		opts = buildMoveOptions(s.slowMoveOptions())
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, opts, nil)
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

// stepPipelineable reports whether a step's motion can be planned before the arm
// arrives at its start — from a prior plan's end configuration rather than from
// the arm's live position.
//
// Three kinds of step must observe the arm itself, so none of them can be
// planned ahead:
//
//   - A pivot seeds its arc from where the arm ACTUALLY is and refuses to run
//     when that is more than pivotStartToleranceMm off the pivot point
//     (executePivot). The check exists because the portafilter is engaged in the
//     bayonet while the rotation happens, and it is worthless against a
//     predicted configuration.
//   - A circular motion builds its revolution against the live inputs the same way.
//   - The no-spill carry bounds the drink's tilt about the container's start
//     orientation (carryHeldLevel). Since the carry was simplified to a single
//     orientation-constrained goal that orientation is a pure function of the
//     start inputs, so this one is pipelineable in the mechanical sense — but it
//     is measured on a full cup, and settling error between a plan's end and
//     where the arm really stops would feed straight into the tilt bound. It
//     keeps reading the arm.
//
// Everything else is a plain move, with or without a linear constraint, that
// planToRawPose derives entirely from its start inputs — so it can be planned
// while the previous plan is still executing.
func (s *beanjaminCoffee) stepPipelineable(step Step) bool {
	if step.PivotFromPose != "" || step.CircularRadiusMm > 0 {
		return false
	}
	return !step.NoSpill
}

// planStepMove resolves a step's pose and plans its move from startInputs
// without touching the arm — the planning half of moveToPose, split out so it
// can run against a predicted configuration.
func (s *beanjaminCoffee) planStepMove(
	ctx context.Context,
	fs *referenceframe.FrameSystem,
	startInputs referenceframe.FrameSystemInputs,
	step Step,
) (motionplan.Plan, error) {
	pd, err := s.fetchPose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return nil, err
	}
	plan, err := s.planToRawPose(ctx, fs, startInputs, pd, step.LinearConstraint, step.AllowedCollisions)
	if err != nil {
		return nil, fmt.Errorf("plan move to %q: %w", step.PoseName, err)
	}
	return plan, nil
}

// pipelinedPlan carries a plan-ahead result back from the planning goroutine.
type pipelinedPlan struct {
	plan motionplan.Plan
	err  error
}

// runStepsPipelined executes a run of pipelineable steps, overlapping each
// move's execution with the planning of the next. The first step is planned
// from the arm's actual configuration; every subsequent step is planned from the
// previous plan's end configuration while that plan executes — the same chaining
// planToRawPose already documents for tryGrab, but concurrent, so planning
// latency hides inside execution time instead of stalling the arm between moves.
//
// Planning only reads the frame system and never touches the arm, and only one
// plan is ever in flight, so nothing here races the running trajectory (or
// planMotion's own request dump, which is sequential for the same reason).
//
// A plan-ahead failure never interrupts a move already underway: the in-flight
// execution and the step's pause finish first, leaving the arm exactly where the
// sequential path would have left it, and the planning error surfaces afterwards.
//
// The one real difference from planning on arrival: a pipelined plan starts at
// the previous plan's NOMINAL end, not at where the arm physically settled, so
// its first waypoint can sit a servo tolerance away from the arm's actual
// configuration and MoveThroughJointPositions closes that gap un-collision-
// checked. Over the free-space moves this runs on, that gap is noise. It is
// exactly why the steps that work against something — the portafilter in the
// bayonet, a full cup — are held out by stepPipelineable instead.
func (s *beanjaminCoffee) runStepsPipelined(ctx, cancelCtx context.Context, steps []Step) error {
	logger := s.activeOrderLogger()
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	// The frame system is captured once for the whole run. Nothing inside a step
	// sequence re-parents a frame or attaches held geometry — lockFilterFrame,
	// attachHeldGeometry and the staged-glass obstacle all happen between
	// sequences — so every step plans against the same world, and only the arm's
	// configuration advances. That is exactly what withArmInputs substitutes.
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	plan, err := s.planStepMove(ctx, fs, fsInputs, steps[0])
	if err != nil {
		return err
	}

	for i, step := range steps {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("cancelled before %q: %w", step.PoseName, err)
		}
		stepCtx, span := trace.StartSpan(ctx, "beanjamin::executeStep::"+step.PoseName)
		logger.Infof("moving to %q", step.PoseName)

		// Hand the next step to a planner goroutine before committing this move
		// to the arm. The channel is buffered so an early return here never
		// leaks it: the deferred done() cancels stepCtx, the planner unblocks on
		// its send, and the goroutine exits.
		var ahead chan pipelinedPlan
		if i+1 < len(steps) {
			ahead = make(chan pipelinedPlan, 1)
			go func(from motionplan.Plan, next Step) {
				end, err := s.planEndArmInputs(from)
				if err != nil {
					ahead <- pipelinedPlan{err: err}
					return
				}
				p, err := s.planStepMove(stepCtx, fs, s.withArmInputs(fsInputs, end), next)
				ahead <- pipelinedPlan{plan: p, err: err}
			}(plan, steps[i+1])
		}

		execErr := s.executePlan(stepCtx, plan, step.LinearConstraint, step.MoveOptions)
		span.End()
		if execErr != nil {
			return fmt.Errorf("move to %q failed: %w", step.PoseName, execErr)
		}

		// The pause runs before the plan-ahead is collected, so planning overlaps
		// the dwell as well as the move.
		if step.Pause > 0 {
			logger.Infof("pausing %s after %q", step.Pause, step.PoseName)
			select {
			case <-time.After(step.Pause):
			case <-ctx.Done():
				return fmt.Errorf("cancelled during pause after %q: %w", step.PoseName, ctx.Err())
			}
		}
		if ahead == nil {
			break
		}
		result := <-ahead
		if result.err != nil {
			return fmt.Errorf("plan ahead to %q: %w", steps[i+1].PoseName, result.err)
		}
		plan = result.plan
	}
	return nil
}

// executePivot fetches start and end poses, computes interpolated waypoints,
// plans a single multi-goal trajectory through all of them, and executes it
// in one MoveThroughJointPositions call.
func (s *beanjaminCoffee) executePivot(ctx, cancelCtx context.Context, step Step) error {
	logger := s.activeOrderLogger()
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	startPD, err := s.fetchPose(ctx, step.PoseSwitch, step.PivotFromPose)
	if err != nil {
		return fmt.Errorf("pivot start: %w", err)
	}
	endPD, err := s.fetchPose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return fmt.Errorf("pivot end: %w", err)
	}

	if startPD.componentName != endPD.componentName {
		return fmt.Errorf("pivot %q → %q: component mismatch (%q vs %q)",
			step.PivotFromPose, step.PoseName, startPD.componentName, endPD.componentName)
	}
	// The authored poses must describe the same point (a pivot is a pure
	// rotation), and the arm must already be standing on it.
	const (
		pivotPositionToleranceMm = 0.5
		pivotStartToleranceMm    = 2.0
	)
	if dist := startPD.pose.Point().Sub(endPD.pose.Point()).Norm(); dist > pivotPositionToleranceMm {
		return fmt.Errorf("pivot %q → %q: positions differ by %.2f mm (max %.1f mm) — pivot assumes a fixed point",
			step.PivotFromPose, step.PoseName, dist, pivotPositionToleranceMm)
	}

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	linearInputs := fsInputs.ToLinearInputs()

	// Seed the arc from where the arm ACTUALLY is rather than from the authored
	// start pose. Each segment's first waypoint is dropped on the assumption
	// we're already standing on it, so an arm that ended the previous step off
	// the authored pose would get one coarse planner segment to climb back onto
	// the arc — precisely the free-form rotation the fine waypoints exist to
	// prevent, and with a portafilter engaged in the bayonet while it happens.
	// The authored *position* is kept so the pivot stays a rotation about the
	// intended fixed point even if the arm has drifted a fraction of a mm.
	curTF, err := fs.Transform(linearInputs,
		referenceframe.NewPoseInFrame(startPD.componentName, spatialmath.NewZeroPose()),
		startPD.refFrame)
	if err != nil {
		return fmt.Errorf("pivot %q: current pose of %q: %w", step.PivotFromPose, startPD.componentName, err)
	}
	actual := curTF.(*referenceframe.PoseInFrame).Pose()
	if dist := actual.Point().Sub(startPD.pose.Point()).Norm(); dist > pivotStartToleranceMm {
		return fmt.Errorf("pivot %q → %q: arm is %.2f mm off the pivot start (max %.1f mm) — refusing to pivot about the wrong point",
			step.PivotFromPose, step.PoseName, dist, pivotStartToleranceMm)
	}
	fromPose := spatialmath.NewPose(startPD.pose.Point(), actual.Orientation())

	// Rotate straight onto the goal, or past it and back when the step asks for
	// an overshoot. Both segments are planned together and run as one
	// trajectory, so the arm never stops at the overshot pose.
	targets := []spatialmath.Pose{endPD.pose}
	if step.PivotExtraDegrees != 0 {
		// Axis from the authored pair, not fromPose: the arm's drift should seed
		// where the arc starts, never tilt the axis it turns about.
		over, err := pivotOvershootPose(startPD.pose, endPD.pose, step.PivotExtraDegrees)
		if err != nil {
			return fmt.Errorf("pivot %q → %q: %w", step.PivotFromPose, step.PoseName, err)
		}
		targets = []spatialmath.Pose{over, endPD.pose}
	}

	var poses []spatialmath.Pose
	segStart := fromPose
	for _, target := range targets {
		seg := computePivotPoses(logger, segStart, target, step.PivotDegreesPerStep)
		poses = append(poses, seg[1:]...) // seg[0] is where the previous segment left us
		segStart = target
	}
	logger.Infof("pivot %q → %q: %d waypoints (%.1f°/step, %.1f° overshoot)",
		step.PivotFromPose, step.PoseName, len(poses), step.PivotDegreesPerStep, step.PivotExtraDegrees)

	goals, err := worldGoals(fs, linearInputs, startPD.refFrame, startPD.componentName, poses)
	if err != nil {
		return err
	}

	// Every waypoint is planned in a single call and run as one trajectory, so
	// the arm never stops partway through the arc.
	positions, err := s.planTrajectory(ctx, &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       goals,
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: buildConstraints(step.LinearConstraint, s.filterFakeModeCollisions(s.appendHeldItemCollisions(step.AllowedCollisions))),
	}, "pivot")
	if err != nil {
		return err
	}
	opts := buildMoveOptions(step.MoveOptions)
	if opts == nil {
		opts = buildMoveOptions(s.slowMoveOptions())
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, opts, nil)
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

// executeCircularMotion fetches the center pose, computes one revolution of
// circular waypoints, plans the trajectory once, then executes it in a loop
// until the configured duration is exceeded.
func (s *beanjaminCoffee) executeCircularMotion(ctx, cancelCtx context.Context, step Step) error {
	logger := s.activeOrderLogger()
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	centerPD, err := s.fetchPose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return fmt.Errorf("circular center: %w", err)
	}

	pointsPerRev := step.CircularPointsPerRev
	if pointsPerRev < 4 {
		pointsPerRev = 8
	}

	poses := computeCircularPoses(centerPD.pose, step.CircularRadiusMm, pointsPerRev)
	logger.Infof("circular motion around %q: radius=%.1fmm, %d pts/rev",
		step.PoseName, step.CircularRadiusMm, pointsPerRev)

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	linearInputs := fsInputs.ToLinearInputs()

	// One revolution is planned once and replayed until the duration is up.
	goals, err := worldGoals(fs, linearInputs, centerPD.refFrame, centerPD.componentName, poses)
	if err != nil {
		return err
	}
	positions, err := s.planTrajectory(ctx, &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       goals,
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: buildConstraints(step.LinearConstraint, s.filterFakeModeCollisions(s.appendHeldItemCollisions(step.AllowedCollisions))),
	}, "circular")
	if err != nil {
		return err
	}

	// Execute revolutions until the duration is exceeded.
	deadline := time.Now().Add(time.Duration(step.CircularDurationSec * float64(time.Second)))
	for rev := 0; time.Now().Before(deadline); rev++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled during circular motion: %w", ctx.Err())
		default:
		}
		logger.Debugf("circular revolution %d", rev+1)
		circOpts := buildMoveOptions(step.MoveOptions)
		if circOpts == nil {
			circOpts = buildMoveOptions(s.slowMoveOptions())
		}
		if err := s.arm.MoveThroughJointPositions(ctx, positions, circOpts, nil); err != nil {
			return fmt.Errorf("execute circular revolution %d: %w", rev+1, err)
		}
	}
	return nil
}

// noSpillOrientationToleranceDegs caps how far the carried container's
// orientation may stray from the slerp between the carry's start and its goal.
// RDK checks this along the whole path as a tube of this width around that
// direct reorientation, so a single start-to-goal segment is enough to hold the
// drink near level for the entire traverse.
//
// The bound is on the excursion off the slerp, not on the commanded rotation:
// both endpoints are upright container poses, so the slerp itself stays level
// and 20° leaves the drink well inside a full cup's static spill angle.
const noSpillOrientationToleranceDegs = 20.0

// withNoSpillOrientationConstraint adds the carry's path orientation bound,
// allocating the Constraints when the caller has none (no linear constraint and
// no allowed collisions, so buildConstraints returned nil).
func withNoSpillOrientationConstraint(constraints *motionplan.Constraints) *motionplan.Constraints {
	if constraints == nil {
		constraints = &motionplan.Constraints{}
	}
	constraints.OrientationConstraint = append(constraints.OrientationConstraint,
		motionplan.OrientationConstraint{OrientationToleranceDegs: noSpillOrientationToleranceDegs})
	return constraints
}

// carryGoalForMoveFrame converts dest — a pose authored for dest.componentName —
// into the world pose moveFrame must reach for that component to land on dest.
// Returns dest's world pose unchanged when moveFrame is the authored component.
//
// The two frames are rigidly linked but neither coincident nor co-oriented:
// held-item hangs off the claws, short of the grip point along the tool axis and
// rotated onto the container's axes (heldItemFramePose). Commanding the container
// straight at a grip-point goal leaves the gripper past it and mis-rotated — the
// offset alone trips executePivot's start-position check on the following step.
func carryGoalForMoveFrame(
	fs *referenceframe.FrameSystem,
	inputs *referenceframe.LinearInputs,
	dest *poseData,
	moveFrame string,
) (spatialmath.Pose, error) {
	destTF, err := fs.Transform(inputs,
		referenceframe.NewPoseInFrame(dest.refFrame, dest.pose), referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("transform carry destination to world: %w", err)
	}
	destPose := destTF.(*referenceframe.PoseInFrame).Pose()
	if moveFrame == dest.componentName {
		return destPose, nil
	}
	// moveFrame expressed in the authored component's frame — constant, since both
	// hang rigidly off the gripper.
	offTF, err := fs.Transform(inputs,
		referenceframe.NewPoseInFrame(moveFrame, spatialmath.NewZeroPose()), dest.componentName)
	if err != nil {
		return nil, fmt.Errorf("transform %q into %q: %w", moveFrame, dest.componentName, err)
	}
	return spatialmath.Compose(destPose, offTF.(*referenceframe.PoseInFrame).Pose()), nil
}

// carryHeldLevel carries the held container from its current pose to dest,
// free-planning the path but holding the container's orientation within
// noSpillOrientationToleranceDegs of the direct start-to-goal reorientation for
// the whole traverse — so the drink doesn't slosh.
//
// The goal commands the held-item frame rather than the gripper, because the
// orientation bound only bounds the drink's tilt when expressed about the
// container's own axis. That frame is neither coincident nor co-oriented with
// the one dest is authored for, so dest is converted into it
// (carryGoalForMoveFrame); with nothing attached it falls back to the gripper
// frame and the conversion is a no-op.
func (s *beanjaminCoffee) carryHeldLevel(ctx context.Context, dest *poseData, allowedCollisions []AllowedCollision, moveOpts *StepMoveOptions) error {
	logger := s.activeOrderLogger()
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	linearInputs := fsInputs.ToLinearInputs()

	// dest targets the grip-point frame. When an item is held, move the
	// held-item frame instead: it is the container that must stay level.
	moveFrame := gripPoint
	if s.heldItemAttached {
		moveFrame = heldItemFrameName
	}

	// Start: current world pose of the moving frame (the container is upright).
	startPIF := referenceframe.NewPoseInFrame(moveFrame, spatialmath.NewZeroPose())
	startTF, err := fs.Transform(linearInputs, startPIF, referenceframe.World)
	if err != nil {
		return fmt.Errorf("transform held-container start pose to world: %w", err)
	}
	startPose := startTF.(*referenceframe.PoseInFrame).Pose()

	// End: the goal moveFrame must reach for dest's component to land on dest.
	destPose, err := carryGoalForMoveFrame(fs, linearInputs, dest, moveFrame)
	if err != nil {
		return err
	}

	logger.Infof("no-spill carry: moving %q over %.0fmm (path±%.0f°, buffer: %.1fmm)",
		moveFrame, destPose.Point().Sub(startPose.Point()).Norm(),
		noSpillOrientationToleranceDegs, freeMoveCollisionBufferMM)

	goal := armplanning.NewPlanState(referenceframe.FrameSystemPoses{
		moveFrame: referenceframe.NewPoseInFrame(referenceframe.World, destPose),
	}, nil)

	constraints := withNoSpillOrientationConstraint(
		buildConstraints(nil, s.filterFakeModeCollisions(s.appendHeldItemCollisions(allowedCollisions))))

	positions, err := s.planTrajectory(ctx, &armplanning.PlanRequest{
		FrameSystem:    fs,
		Goals:          []*armplanning.PlanState{goal},
		StartState:     armplanning.NewPlanState(nil, fsInputs),
		Constraints:    constraints,
		PlannerOptions: freeMovePlannerOptions(),
	}, "carry")
	if err != nil {
		return err
	}
	// The level carry moves a filled container, so default to the slow tier.
	opts := buildMoveOptions(moveOpts)
	if opts == nil {
		opts = buildMoveOptions(s.slowMoveOptions())
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, opts, nil)
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

// computePivotPoses returns interpolated poses between startPose and endPose.
// The step count is derived from the total rotation angle divided by degreesPerStep.
func computePivotPoses(logger logging.Logger, startPose, endPose spatialmath.Pose, degreesPerStep float64) []spatialmath.Pose {
	diff := spatialmath.OrientationBetween(startPose.Orientation(), endPose.Orientation())
	// AxisAngles().Theta is signed: the axis/angle pair can come back as
	// (axis, +θ) or (-axis, -θ) depending on the rotation. Use the magnitude so a
	// negative angle doesn't collapse numSteps to 1 (max(1, round(negative)) == 1),
	// which would degenerate the pivot into a single straight-to-goal waypoint.
	totalRadians := math.Abs(diff.AxisAngles().Theta)
	totalDegrees := totalRadians * 180.0 / math.Pi

	numSteps := max(1, int(math.Round(totalDegrees/degreesPerStep)))

	logger.Infof("pivot rotation: %.1f° total (%d steps at %.1f°/step)", totalDegrees, numSteps, degreesPerStep)

	poses := make([]spatialmath.Pose, numSteps+1)
	for i := 0; i <= numSteps; i++ {
		t := float64(i) / float64(numSteps)
		poses[i] = spatialmath.Interpolate(startPose, endPose, t)
	}
	return poses
}
