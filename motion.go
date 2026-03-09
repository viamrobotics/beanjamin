package beanjamin

import (
	"context"
	"fmt"
	"math"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/spatialmath"
)

var defaultApproachConstraint = &StepLinearConstraint{
	LineToleranceMm:          1,
	OrientationToleranceDegs: 2,
}

// moveToPose fetches a named pose from the switch and moves to it.
func (s *beanjaminCoffee) moveToPose(ctx context.Context, step Step) error {
	pd, err := s.fetchPose(ctx, step.PoseName)
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

// fetchPose retrieves a named pose from the switch.
func (s *beanjaminCoffee) fetchPose(ctx context.Context, poseName string) (*poseData, error) {
	resp, err := s.sw.DoCommand(ctx, map[string]interface{}{
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
	theta, _ := resp["theta_degrees"].(float64)
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

// buildFrameSystem constructs a frame system from the framesystem service,
// optionally re-parenting the filter frame to the world when the portafilter is locked.
func (s *beanjaminCoffee) buildFrameSystem(ctx context.Context) (*referenceframe.FrameSystem, referenceframe.FrameSystemInputs, error) {
	fs, err := framesystem.NewFromService(ctx, s.fs, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build frame system: %w", err)
	}

	fsInputs, err := s.fs.CurrentInputs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get current inputs: %w", err)
	}

	if s.filterLocked {
		if err := s.reparentFilterFrame(ctx, fs, fsInputs); err != nil {
			return nil, nil, fmt.Errorf("reparent filter frame: %w", err)
		}
	}

	return fs, fsInputs, nil
}

// reparentFilterFrame detaches the "filter" frame from the arm subtree and
// re-adds it as a static frame parented to the world at its current pose.
// Any descendant frames of "filter" are preserved and re-attached under the
// new static frame, maintaining their relative transforms.
func (s *beanjaminCoffee) reparentFilterFrame(ctx context.Context, fs *referenceframe.FrameSystem, fsInputs referenceframe.FrameSystemInputs) error {
	const filterFrameName = "filter"

	filterFrame := fs.Frame(filterFrameName)
	if filterFrame == nil {
		return fmt.Errorf("frame %q not found in frame system", filterFrameName)
	}

	// 1. Compute filter's world pose using current joint inputs.
	filterPIF := referenceframe.NewPoseInFrame(filterFrameName, spatialmath.NewZeroPose())
	tf, err := fs.Transform(fsInputs.ToLinearInputs(), filterPIF, referenceframe.World)
	if err != nil {
		return fmt.Errorf("transform filter to world: %w", err)
	}
	worldPose := tf.(*referenceframe.PoseInFrame).Pose()

	// 2. Get the filter's geometry from the frame system config.
	cfg, err := s.fs.FrameSystemConfig(ctx)
	if err != nil {
		return fmt.Errorf("get frame system config: %w", err)
	}
	var geom spatialmath.Geometry
	for _, part := range cfg.Parts {
		if part.FrameConfig.Name() == filterFrameName {
			geom = part.FrameConfig.Geometry()
			break
		}
	}
	if geom == nil {
		return fmt.Errorf("no geometry found for frame %q", filterFrameName)
	}

	// 3. Collect filter's descendants in BFS order before removal.
	//    BFS guarantees parents appear before children, so re-adding in
	//    order will always find the parent frame already present.
	type descendantEntry struct {
		frame      referenceframe.Frame
		parentName string
	}
	var descendants []descendantEntry
	queue := []string{filterFrameName}
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

	// 4. Remove filter (and all descendants) from the arm subtree.
	fs.RemoveFrame(filterFrame)

	// 5. Re-add filter as a static frame parented to world at the locked position.
	newFrame, err := referenceframe.NewStaticFrameWithGeometry(filterFrameName, worldPose, geom)
	if err != nil {
		return fmt.Errorf("create static filter frame: %w", err)
	}
	if err := fs.AddFrame(newFrame, fs.World()); err != nil {
		return fmt.Errorf("add filter frame to world: %w", err)
	}

	// 6. Re-attach descendants under the new static filter, preserving subtree structure.
	for _, d := range descendants {
		parent := fs.Frame(d.parentName) // "filter" (now static) or a previously re-added descendant
		if err := fs.AddFrame(d.frame, parent); err != nil {
			return fmt.Errorf("re-add descendant %q under %q: %w", d.frame.Name(), d.parentName, err)
		}
	}

	s.logger.Infof("re-parented filter frame to world at %v (%d descendants preserved)", worldPose.Point(), len(descendants))
	return nil
}

// moveToRawPose plans a motion using armplanning and executes it on the arm.
func (s *beanjaminCoffee) moveToRawPose(ctx context.Context, pd *poseData, lc *StepLinearConstraint, allowedCollisions []AllowedCollision) error {
	fs, fsInputs, err := s.buildFrameSystem(ctx)
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

	startState := armplanning.NewPlanState(nil, fsInputs)
	goalState := armplanning.NewPlanState(
		referenceframe.FrameSystemPoses{pd.componentName: goalPose}, nil,
	)

	// Build constraints.
	var constraints *motionplan.Constraints
	if lc != nil || len(allowedCollisions) > 0 {
		constraints = &motionplan.Constraints{}
	}
	if lc != nil {
		s.logger.Infof("applying linear constraint (line=%.1fmm, orient=%.1f°)",
			lc.LineToleranceMm, lc.OrientationToleranceDegs)
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
		s.logger.Infof("allowing %d collision pair(s)", len(allows))
		constraints.CollisionSpecification = []motionplan.CollisionSpecification{
			{Allows: allows},
		}
	}

	// Plan.
	plan, _, err := armplanning.PlanMotion(ctx, s.logger, &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       []*armplanning.PlanState{goalState},
		StartState:  startState,
		Constraints: constraints,
	})
	if err != nil {
		return fmt.Errorf("plan motion: %w", err)
	}

	// Execute — extract joint positions and send to arm.
	positions, err := plan.Trajectory().GetFrameInputs(pd.componentName)
	if err != nil {
		return fmt.Errorf("get frame inputs from plan: %w", err)
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, nil, nil)
}

// executePivot fetches start and end poses, computes interpolated waypoints,
// and moves through each one (skipping the first since we're already there).
func (s *beanjaminCoffee) executePivot(ctx, cancelCtx context.Context, step Step) error {
	startPD, err := s.fetchPose(ctx, step.PivotFromPose)
	if err != nil {
		return fmt.Errorf("pivot start: %w", err)
	}
	endPD, err := s.fetchPose(ctx, step.PoseName)
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

	// Skip poses[0] — we're already at the start pose.
	for i := 1; i < len(poses); i++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("pivot cancelled at waypoint %d/%d: %w", i, len(poses)-1, ctx.Err())
		case <-cancelCtx.Done():
			return fmt.Errorf("pivot cancelled at waypoint %d/%d", i, len(poses)-1)
		default:
		}

		wp := &poseData{
			pose:          poses[i],
			refFrame:      startPD.refFrame,
			componentName: startPD.componentName,
		}
		if err := s.moveToRawPose(ctx, wp, step.LinearConstraint, step.AllowedCollisions); err != nil {
			return fmt.Errorf("pivot waypoint %d/%d failed: %w", i, len(poses)-1, err)
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
