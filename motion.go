package beanjamin

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/golang/geo/r3"
	viz "github.com/viam-labs/motion-tools/client/client"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/spatialmath"

	toggleswitch "go.viam.com/rdk/components/switch"
)

var defaultApproachConstraint = &StepLinearConstraint{
	LineToleranceMm:          1,
	OrientationToleranceDegs: 2,
}

// moveToPose fetches a named pose and moves to it.
func (s *beanjaminCoffee) moveToPose(ctx context.Context, step Step) error {
	pd, err := s.fetchPose(ctx, step.Component, step.PoseName)
	if err != nil {
		return err
	}
	if err := s.moveToRawPose(ctx, pd, step.LinearConstraint, step.AllowedCollisions); err != nil {
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
	fsInputs := referenceframe.NewZeroInputs(s.cachedFS)

	// Get current joint positions directly from the arm.
	armInputs, err := s.arm.CurrentInputs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get current inputs: %w", err)
	}

	// Use the config arm name as the key — this matches the frame name in the cached
	// frame system built from FrameSystemConfig.
	s.logger.Debugf("currentInputs: arm=%q, armInputsLen=%d", s.cfg.ArmName, len(armInputs))
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
	done := make(chan error, 1)
	go func() {
		done <- viz.DrawFrameSystem(s.cachedFS, fsInputs)
	}()

	select {
	case err := <-done:
		if err != nil {
			s.vizConsecutiveFailures++
			s.logger.Warnf("viz: failed to draw frame system (%d/%d): %v",
				s.vizConsecutiveFailures, vizMaxFailures, err)
		} else {
			s.vizConsecutiveFailures = 0
		}
	case <-time.After(vizTimeout):
		s.vizConsecutiveFailures++
		s.logger.Warnf("viz: draw timed out after %v (%d/%d)",
			vizTimeout, s.vizConsecutiveFailures, vizMaxFailures)
	}

	if s.vizConsecutiveFailures >= vizMaxFailures {
		s.logger.Warnf("viz: disabling visualizer after %d consecutive failures", vizMaxFailures)
		s.vizEnabled = false
	}
}

// lockFilterFrame re-parents the "filter" frame from the arm subtree to the
// world at its current pose. Call this after physically locking the portafilter.
// The cached frame system is mutated in place so all subsequent planning calls
// see the filter at its locked position.
func (s *beanjaminCoffee) lockFilterFrame(ctx context.Context) error {
	const filterFrameName = "filter"

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

	s.logger.Infof("locked filter frame at world pose %v (%d descendants preserved)", worldPose.Point(), len(descendants))
	return nil
}

// unlockFilterFrame rebuilds the cached frame system from the service,
// restoring the filter frame to its original position in the arm subtree.
func (s *beanjaminCoffee) unlockFilterFrame(ctx context.Context) error {
	fs, err := framesystem.NewFromService(ctx, s.fsSvc, nil)
	if err != nil {
		return fmt.Errorf("rebuild frame system: %w", err)
	}
	s.cachedFS = fs
	s.logger.Infof("unlocked filter frame, frame system restored from service")
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

// savePlanRequest persists a PlanRequest to the configured directory. It is a
// no-op when SaveMotionRequestsDir is empty.
func (s *beanjaminCoffee) savePlanRequest(req *armplanning.PlanRequest, label string) {
	dir := s.cfg.SaveMotionRequestsDir
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.logger.Warnf("save motion request: create dir: %v", err)
		return
	}
	filename := filepath.Join(dir, fmt.Sprintf("%s_%s.json", time.Now().Format("20060102_150405.000"), label))
	if err := req.WriteToFile(filename); err != nil {
		s.logger.Warnf("save motion request: %v", err)
		return
	}
	s.logger.Infof("saved motion request to %s", filename)
}

// moveToRawPose plans a motion using armplanning and executes it on the arm.
func (s *beanjaminCoffee) moveToRawPose(ctx context.Context, pd *poseData, lc *StepLinearConstraint, allowedCollisions []AllowedCollision) error {
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

	constraints := buildConstraints(lc, allowedCollisions)
	if lc != nil {
		s.logger.Infof("applying linear constraint (line=%.1fmm, orient=%.1f°)",
			lc.LineToleranceMm, lc.OrientationToleranceDegs)
	}
	if len(allowedCollisions) > 0 {
		s.logger.Infof("allowing %d collision pair(s)", len(allowedCollisions))
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
	plan, _, err := armplanning.PlanMotion(ctx, s.logger, req)
	if err != nil {
		return fmt.Errorf("plan motion: %w", err)
	}

	// Execute — extract joint positions for the arm frame (not the end-effector
	// component name used for the goal pose) and send to arm.
	positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
	if err != nil {
		return fmt.Errorf("get frame inputs from plan: %w", err)
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, nil, nil)
}

func (s *beanjaminCoffee) switchForComponent(componentName string) (toggleswitch.Switch, error) {
	switch componentName {
	case "filter":
		return s.filterSw, nil
	case "coffee-claws-middle":
		return s.clawsSw, nil
	default:
		return nil, fmt.Errorf("unknown reference frame %q", componentName)
	}
}

// executePivot fetches start and end poses, computes interpolated waypoints,
// plans a single multi-goal trajectory through all of them, and executes it
// in one MoveThroughJointPositions call.
func (s *beanjaminCoffee) executePivot(ctx, cancelCtx context.Context, step Step) error {
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
	s.logger.Infof("pivot %q → %q: %d waypoints (%.1f°/step)",
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
	constraints := buildConstraints(step.LinearConstraint, step.AllowedCollisions)

	// Plan all waypoints in a single call.
	req := &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       goals,
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: constraints,
	}
	s.savePlanRequest(req, "pivot")
	plan, _, err := armplanning.PlanMotion(ctx, s.logger, req)
	if err != nil {
		return fmt.Errorf("plan pivot motion: %w", err)
	}

	// Execute the full trajectory in one call — extract joint positions for the
	// arm frame, not the end-effector component name used for goal poses.
	positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
	if err != nil {
		return fmt.Errorf("get frame inputs from pivot plan: %w", err)
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, nil, nil)
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
	s.logger.Infof("circular motion around %q: radius=%.1fmm, %d pts/rev",
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

	constraints := buildConstraints(step.LinearConstraint, step.AllowedCollisions)

	req := &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       goals,
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: constraints,
	}
	s.savePlanRequest(req, "circular")
	plan, _, err := armplanning.PlanMotion(ctx, s.logger, req)
	if err != nil {
		return fmt.Errorf("plan circular motion: %w", err)
	}

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
		s.logger.Debugf("circular revolution %d", rev+1)
		if err := s.arm.MoveThroughJointPositions(ctx, positions, nil, nil); err != nil {
			return fmt.Errorf("execute circular revolution %d: %w", rev+1, err)
		}
	}
	return nil
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
