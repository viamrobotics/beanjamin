package beanjamin

import (
	"context"
	"fmt"
	"math"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/services/motion"
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

// moveToRawPose moves to a computed pose with optional linear constraint and allowed collisions.
func (s *beanjaminCoffee) moveToRawPose(ctx context.Context, pd *poseData, lc *StepLinearConstraint, allowedCollisions []AllowedCollision) error {
	destination := referenceframe.NewPoseInFrame(pd.refFrame, pd.pose)

	moveReq := motion.MoveReq{
		ComponentName: pd.componentName,
		Destination:   destination,
	}

	if lc != nil || len(allowedCollisions) > 0 {
		moveReq.Constraints = &motionplan.Constraints{}
	}

	if lc != nil {
		s.logger.Infof("applying linear constraint (line=%.1fmm, orient=%.1f°)",
			lc.LineToleranceMm, lc.OrientationToleranceDegs)
		moveReq.Constraints.LinearConstraint = []motionplan.LinearConstraint{
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
		moveReq.Constraints.CollisionSpecification = []motionplan.CollisionSpecification{
			{Allows: allows},
		}
	}

	_, err := s.motion.Move(ctx, moveReq)
	return err
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
