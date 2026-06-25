package beanjamin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/golang/geo/r3"
	viz "github.com/viam-labs/motion-tools/client/client"
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

const defaultSlowMovementVelDegsPerSec = 25.0

// slowMovementMoveOptions returns the MoveOptions used whenever a step carries
// a LinearConstraint (or for pivot/circular moves) but no explicit per-step
// MoveOptions. Velocity is configurable via Config.SlowMovementVelDegsPerSec.
func (s *beanjaminCoffee) slowMovementMoveOptions() *arm.MoveOptions {
	velDegs := s.cfg.SlowMovementVelDegsPerSec
	if velDegs <= 0 {
		velDegs = defaultSlowMovementVelDegsPerSec
	}
	return &arm.MoveOptions{
		MaxVelRads: velDegs * math.Pi / 180.0,
	}
}

// moveToPose fetches a named pose and moves to it.
func (s *beanjaminCoffee) moveToPose(ctx context.Context, step Step) error {
	pd, err := s.fetchPose(ctx, step.Component, step.PoseName)
	if err != nil {
		return err
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

// fetchPose retrieves a named pose from the switch determined by component.
func (s *beanjaminCoffee) fetchPose(ctx context.Context, component, poseName string) (*poseData, error) {
	sw, err := s.switchForComponent(component)
	if err != nil {
		return nil, err
	}
	resp, err := sw.DoCommand(ctx, map[string]interface{}{
		"get_pose_by_name": poseName,
	})
	if err != nil {
		return nil, fmt.Errorf("get pose %q: %w", poseName, err)
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
		done <- viz.DrawFrameSystem(s.cachedFS, fsInputs)
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

	logger.Infof("locked filter frame at world pose %v (%d descendants preserved)", worldPose.Point(), len(descendants))
	return nil
}

// resetFrameSystem rebuilds the cached frame system from the service, discarding
// any in-flight mutations (e.g. a filter frame that was reparented to world by
// lockFilterFrame). Shared by unlockFilterFrame during the normal brew cycle and
// by the reset_world operator command to recover from a mid-cycle cancel.
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
	// re-attached after a cancel/reset.
	s.clearHeldGeometry()
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

// savePlanRequest persists a PlanRequest to the configured directory. It is a
// no-op when SaveMotionRequestsDir is empty.
func (s *beanjaminCoffee) savePlanRequest(req *armplanning.PlanRequest, label string) {
	logger := s.activeOrderLogger()
	dir := s.cfg.SaveMotionRequestsDir
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warnf("save motion request: create dir: %v", err)
		return
	}
	filename := filepath.Join(dir, fmt.Sprintf("%s_%s.json", time.Now().Format("20060102_150405.000"), label))
	if err := req.WriteToFile(filename); err != nil {
		logger.Warnf("save motion request: %v", err)
		return
	}
	logger.Infof("saved motion request to %s", filename)
}

// frameSystemWithGeometries returns a deep copy of the cached frame system with
// each world-frame geometry added as a static frame parented to world, named
// "<label>_<i>". The geometries are expected to already be in world coordinates;
// each is attached at a zero-pose static frame so the frame system resolves it
// back at its world pose (the parent→world transform is identity, sidestepping
// the "GeometriesInFrame skips the frame's own transform" convention). The input
// geometries are not mutated — a copy is relabeled.
func (s *beanjaminCoffee) frameSystemWithGeometries(label string, geoms []spatialmath.Geometry) (*referenceframe.FrameSystem, error) {
	fs, err := s.cachedFS.Clone()
	if err != nil {
		return nil, fmt.Errorf("clone frame system: %w", err)
	}
	for i, g := range geoms {
		if g == nil {
			continue
		}
		name := fmt.Sprintf("%s_%d", label, i)
		geom := g.Transform(spatialmath.NewZeroPose())
		geom.SetLabel(name)
		frame, err := referenceframe.NewStaticFrameWithGeometry(name, spatialmath.NewZeroPose(), geom)
		if err != nil {
			return nil, fmt.Errorf("create static frame %q: %w", name, err)
		}
		if err := fs.AddFrame(frame, fs.World()); err != nil {
			return nil, fmt.Errorf("add frame %q: %w", name, err)
		}
	}
	return fs, nil
}

// saveObservedItemsFrameSystem persists a snapshot of the frame system augmented
// with the detected item geometries (cups/glasses) to SaveMotionRequestsDir, so
// it can be read back into a referenceframe.FrameSystem and drawn in a local
// motion-tools visualizer. It is a no-op when SaveMotionRequestsDir is empty or
// no geometries are given.
func (s *beanjaminCoffee) saveObservedItemsFrameSystem(label string, geoms []spatialmath.Geometry) {
	logger := s.activeOrderLogger()
	dir := s.cfg.SaveMotionRequestsDir
	if dir == "" || len(geoms) == 0 {
		return
	}
	fs, err := s.frameSystemWithGeometries(label, geoms)
	if err != nil {
		logger.Warnf("save observed %s frame system: %v", label, err)
		return
	}
	data, err := json.MarshalIndent(fs, "", "  ")
	if err != nil {
		logger.Warnf("save observed %s frame system: marshal: %v", label, err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warnf("save observed %s frame system: create dir: %v", label, err)
		return
	}
	filename := filepath.Join(dir, fmt.Sprintf("%s_%s_framesystem.json", time.Now().Format("20060102_150405.000"), label))
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		logger.Warnf("save observed %s frame system: %v", label, err)
		return
	}
	logger.Infof("saved observed-%s frame system (%d geometries) to %s", label, len(geoms), filename)
}

// savePlanResponse persists a Plan's path and trajectory to the configured
// directory. It is a no-op when SaveMotionRequestsDir is empty.
func (s *beanjaminCoffee) savePlanResponse(plan motionplan.Plan, label string) {
	logger := s.activeOrderLogger()
	dir := s.cfg.SaveMotionRequestsDir
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warnf("save motion response: create dir: %v", err)
		return
	}
	resp := struct {
		Path       motionplan.Path       `json:"path"`
		Trajectory motionplan.Trajectory `json:"trajectory"`
	}{
		Path:       plan.Path(),
		Trajectory: plan.Trajectory(),
	}
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		logger.Warnf("save motion response: marshal: %v", err)
		return
	}
	filename := filepath.Join(dir, fmt.Sprintf("%s_%s_response.json", time.Now().Format("20060102_150405.000"), label))
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		logger.Warnf("save motion response: %v", err)
		return
	}
	logger.Infof("saved motion response to %s", filename)
}

// moveToRawPose plans a motion using armplanning and executes it on the arm.
func (s *beanjaminCoffee) moveToRawPose(ctx context.Context, pd *poseData, lc *StepLinearConstraint, allowedCollisions []AllowedCollision, moveOpts *StepMoveOptions) error {
	logger := s.activeOrderLogger()
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}

	// Transform destination to world frame.
	destination := referenceframe.NewPoseInFrame(pd.refFrame, pd.pose)
	tf, err := fs.Transform(fsInputs.ToLinearInputs(), destination, referenceframe.World)
	if err != nil {
		return fmt.Errorf("transform destination to world: %w", err)
	}
	goalPose := tf.(*referenceframe.PoseInFrame)

	allowedCollisions = s.filterFakeModeCollisions(s.appendHeldItemCollisions(allowedCollisions))
	constraints := buildConstraints(lc, allowedCollisions)
	if lc != nil {
		logger.Infof("applying linear constraint (line=%.1fmm, orient=%.1f°)",
			lc.LineToleranceMm, lc.OrientationToleranceDegs)
	}
	if len(allowedCollisions) > 0 {
		logger.Infof("allowing %d collision pair(s)", len(allowedCollisions))
	}

	// Plan.
	req := &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals: []*armplanning.PlanState{
			armplanning.NewPlanState(referenceframe.FrameSystemPoses{pd.componentName: goalPose}, nil),
		},
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: constraints,
	}
	s.savePlanRequest(req, "move")
	plan, _, err := armplanning.PlanMotion(ctx, logger, req)
	if err != nil {
		return fmt.Errorf("%w: %w", errMotionPlanning, err)
	}
	s.savePlanResponse(plan, "move")

	// Execute — extract joint positions for the arm frame (not the end-effector
	// component name used for the goal pose) and send to arm.
	positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
	if err != nil {
		return fmt.Errorf("get frame inputs from plan: %w", err)
	}
	opts := buildMoveOptions(moveOpts)
	if opts == nil && lc != nil {
		opts = s.slowMovementMoveOptions()
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, opts, nil)
}

func (s *beanjaminCoffee) switchForComponent(componentName string) (toggleswitch.Switch, error) {
	switch componentName {
	case componentFilter:
		return s.filterSw, nil
	case componentClaws:
		return s.clawsSw, nil
	case componentCam:
		if s.cameraObserveSw == nil {
			return nil, fmt.Errorf("camera observe switch not configured")
		}
		return s.cameraObserveSw, nil
	case componentGlassCam:
		if s.glassObserveSw == nil {
			return nil, fmt.Errorf("glass observe switch not configured")
		}
		return s.glassObserveSw, nil
	default:
		return nil, fmt.Errorf("unknown reference frame %q", componentName)
	}
}

// executePivot fetches start and end poses, computes interpolated waypoints,
// plans a single multi-goal trajectory through all of them, and executes it
// in one MoveThroughJointPositions call.
func (s *beanjaminCoffee) executePivot(ctx, cancelCtx context.Context, step Step) error {
	logger := s.activeOrderLogger()
	// Merge both contexts so cancellation from either stops planning and execution.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(cancelCtx, func() { cancel() })
	defer stop()
	defer cancel()

	startPD, err := s.fetchPose(ctx, step.Component, step.PivotFromPose)
	if err != nil {
		return fmt.Errorf("pivot start: %w", err)
	}
	endPD, err := s.fetchPose(ctx, step.Component, step.PoseName)
	if err != nil {
		return fmt.Errorf("pivot end: %w", err)
	}

	if startPD.componentName != endPD.componentName {
		return fmt.Errorf("pivot %q → %q: component mismatch (%q vs %q)",
			step.PivotFromPose, step.PoseName, startPD.componentName, endPD.componentName)
	}
	const pivotPositionToleranceMm = 0.5
	if dist := startPD.pose.Point().Sub(endPD.pose.Point()).Norm(); dist > pivotPositionToleranceMm {
		return fmt.Errorf("pivot %q → %q: positions differ by %.2f mm (max %.1f mm) — pivot assumes a fixed point",
			step.PivotFromPose, step.PoseName, dist, pivotPositionToleranceMm)
	}

	poses := computePivotPoses(startPD.pose, endPD.pose, step.PivotDegreesPerStep)
	logger.Infof("pivot %q → %q: %d waypoints (%.1f°/step)",
		step.PivotFromPose, step.PoseName, len(poses)-1, step.PivotDegreesPerStep)

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	linearInputs := fsInputs.ToLinearInputs()

	// Build a goal state for each waypoint (skip poses[0] — we're already there).
	goals := make([]*armplanning.PlanState, 0, len(poses)-1)
	for _, pose := range poses[1:] {
		pif := referenceframe.NewPoseInFrame(startPD.refFrame, pose)
		tf, err := fs.Transform(linearInputs, pif, referenceframe.World)
		if err != nil {
			return fmt.Errorf("transform pivot waypoint to world: %w", err)
		}
		goalPose := tf.(*referenceframe.PoseInFrame)
		goals = append(goals, armplanning.NewPlanState(
			referenceframe.FrameSystemPoses{startPD.componentName: goalPose}, nil,
		))
	}

	// Build constraints.
	constraints := buildConstraints(step.LinearConstraint, s.filterFakeModeCollisions(s.appendHeldItemCollisions(step.AllowedCollisions)))

	// Plan all waypoints in a single call.
	req := &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       goals,
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: constraints,
	}
	s.savePlanRequest(req, "pivot")
	plan, _, err := armplanning.PlanMotion(ctx, logger, req)
	if err != nil {
		return fmt.Errorf("plan pivot motion: %w", err)
	}
	s.savePlanResponse(plan, "pivot")

	// Execute the full trajectory in one call — extract joint positions for the
	// arm frame, not the end-effector component name used for goal poses.
	positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
	if err != nil {
		return fmt.Errorf("get frame inputs from pivot plan: %w", err)
	}
	opts := buildMoveOptions(step.MoveOptions)
	if opts == nil {
		opts = s.slowMovementMoveOptions()
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
	// Merge both contexts so cancellation from either stops planning and execution.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(cancelCtx, func() { cancel() })
	defer stop()
	defer cancel()

	centerPD, err := s.fetchPose(ctx, step.Component, step.PoseName)
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

	// Build goal states for one revolution.
	goals := make([]*armplanning.PlanState, 0, len(poses))
	for _, pose := range poses {
		pif := referenceframe.NewPoseInFrame(centerPD.refFrame, pose)
		tf, err := fs.Transform(linearInputs, pif, referenceframe.World)
		if err != nil {
			return fmt.Errorf("transform circular waypoint to world: %w", err)
		}
		goalPose := tf.(*referenceframe.PoseInFrame)
		goals = append(goals, armplanning.NewPlanState(
			referenceframe.FrameSystemPoses{centerPD.componentName: goalPose}, nil,
		))
	}

	constraints := buildConstraints(step.LinearConstraint, s.filterFakeModeCollisions(s.appendHeldItemCollisions(step.AllowedCollisions)))

	req := &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       goals,
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: constraints,
	}
	s.savePlanRequest(req, "circular")
	plan, _, err := armplanning.PlanMotion(ctx, logger, req)
	if err != nil {
		return fmt.Errorf("plan circular motion: %w", err)
	}
	s.savePlanResponse(plan, "circular")

	positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
	if err != nil {
		return fmt.Errorf("get frame inputs from circular plan: %w", err)
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
			circOpts = s.slowMovementMoveOptions()
		}
		if err := s.arm.MoveThroughJointPositions(ctx, positions, circOpts, nil); err != nil {
			return fmt.Errorf("execute circular revolution %d: %w", rev+1, err)
		}
	}
	return nil
}

// defaultCarryWaypointSpacingMm is the straight-line spacing between the
// waypoints inserted along a no-spill carry move (see carryHeldLevel). 200 mm
// keeps consecutive goals close enough that the planner has little room to tilt
// the held drink between them.
const defaultCarryWaypointSpacingMm = 200.0

// noSpillGoalCloud loosens the goal at each carry waypoint so IK has room to
// solve while still keeping the held container close to level. A PoseCloud only
// ever relaxes a goal — a candidate inside the cloud scores as a perfect match,
// otherwise the standard weighted metric applies. X/Y/Z are small translational
// leeways (mm) that give a little positional slack; the orientation leeways are
// deviations from the goal orientation — OX/OY allow a small tilt of the
// container's axis, Theta a wider twist about it.
//
// These are conservative defaults. Which axis is genuinely "safe" to open up
// depends on how the cup sits in the gripper (its vertical axis relative to the
// goal orientation vector); opening the wrong one can tip the drink (see the
// referenceframe.PoseCloud docs). Tune on hardware before widening them.
var noSpillGoalCloud = &referenceframe.PoseCloud{
	X: 2, Y: 2, Z: 2,
	OX: 0.2, OY: 0.2, OZ: 0.1, Theta: 45,
}

// computeLevelCarryWaypoints returns the ordered goal poses for a straight-line
// carry from startPose to endPose. Each waypoint is spaced at most spacingMm
// apart along the line.
func computeLevelCarryWaypoints(startPose, endPose spatialmath.Pose, spacingMm float64) []spatialmath.Pose {
	startPt := startPose.Point()
	endPt := endPose.Point()
	delta := endPt.Sub(startPt)
	dist := delta.Norm()

	// Number of straight-line segments: at least 1, otherwise ceil(dist/spacing)
	// so no segment exceeds spacingMm.
	segments := 1
	if spacingMm > 0 && dist > spacingMm {
		segments = int(math.Ceil(dist / spacingMm))
	}

	poses := make([]spatialmath.Pose, 0, segments)
	for i := 1; i <= segments; i++ {
		t := float64(i) / float64(segments)
		poses = append(poses, spatialmath.Interpolate(startPose, endPose, t))
	}
	return poses
}

// carryHeldLevel carries the held container from its current pose to dest along
// the straight line between them, stepping through waypoints (one per
// defaultCarryWaypointSpacingMm). Each waypoint's pose is interpolated from the
// container's current (upright) pose to dest, so the orientation eases from
// upright to the approach pose while a goal pose cloud keeps it close to level —
// so the drink doesn't slosh.
//
// The goals command the held-item frame (the container) rather than the gripper:
// the held-item frame is coincident with the gripper (attached with an identity
// offset, geometry aside), so converting the gripper start/dest poses to it is
// the same world pose, but expressing the goals against the container frame keeps
// the upright goal and the relaxing pose cloud about the container itself. When
// no item is attached (tracking off, or a static pickup left nothing cached) it
// falls back to the gripper frame, which is equivalent. Each goal carries
// noSpillGoalCloud to loosen the orientation; held-item self-collisions are
// injected so the tracked geometry still routes around obstacles. Planning
// failures are wrapped in errMotionPlanning so placeHeldInServingArea can fall
// through to the next slot.
func (s *beanjaminCoffee) carryHeldLevel(ctx context.Context, dest *poseData) error {
	logger := s.activeOrderLogger()
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	linearInputs := fsInputs.ToLinearInputs()

	// Move the held-container frame when an item is attached; otherwise the
	// coincident gripper frame (same world pose).
	moveFrame := componentClaws
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

	// End: the gripper destination, converted to the moving frame (coincident, so
	// the same world position).
	destPIF := referenceframe.NewPoseInFrame(dest.refFrame, dest.pose)
	destTF, err := fs.Transform(linearInputs, destPIF, referenceframe.World)
	if err != nil {
		return fmt.Errorf("transform carry destination to world: %w", err)
	}
	destPose := destTF.(*referenceframe.PoseInFrame).Pose()

	waypoints := computeLevelCarryWaypoints(startPose, destPose, defaultCarryWaypointSpacingMm)
	logger.Infof("no-spill carry: moving %q through %d waypoint(s) over %.0fmm (cloud: tilt±%.2f, twist±%.0f°)",
		moveFrame, len(waypoints), destPose.Point().Sub(startPose.Point()).Norm(), noSpillGoalCloud.OX, noSpillGoalCloud.Theta)

	goals := make([]*armplanning.PlanState, 0, len(waypoints))
	for _, pose := range waypoints {
		pif := referenceframe.NewPoseInFrameWithGoalCloud(referenceframe.World, pose, noSpillGoalCloud)
		goals = append(goals, armplanning.NewPlanState(
			referenceframe.FrameSystemPoses{moveFrame: pif}, nil,
		))
	}

	constraints := buildConstraints(nil, s.filterFakeModeCollisions(s.appendHeldItemCollisions(nil)))

	req := &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       goals,
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: constraints,
	}
	s.savePlanRequest(req, "carry")
	plan, _, err := armplanning.PlanMotion(ctx, logger, req)
	if err != nil {
		return fmt.Errorf("%w: %w", errMotionPlanning, err)
	}
	s.savePlanResponse(plan, "carry")

	positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
	if err != nil {
		return fmt.Errorf("get frame inputs from carry plan: %w", err)
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, nil, nil)
}

// computePivotPoses returns interpolated poses between startPose and endPose.
// The step count is derived from the total rotation angle divided by degreesPerStep.
func computePivotPoses(startPose, endPose spatialmath.Pose, degreesPerStep float64) []spatialmath.Pose {
	diff := spatialmath.OrientationBetween(startPose.Orientation(), endPose.Orientation())
	totalRadians := diff.AxisAngles().Theta
	totalDegrees := totalRadians * 180.0 / math.Pi

	numSteps := max(1, int(math.Round(totalDegrees/degreesPerStep)))

	poses := make([]spatialmath.Pose, numSteps+1)
	for i := 0; i <= numSteps; i++ {
		t := float64(i) / float64(numSteps)
		poses[i] = spatialmath.Interpolate(startPose, endPose, t)
	}
	return poses
}
