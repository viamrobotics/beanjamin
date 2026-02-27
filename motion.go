package beanjamin

import (
	"context"
	"fmt"

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

// moveToPose fetches pose data from the switch and calls motion.Move directly,
// optionally applying a linear constraint for straight-line movement.
func (s *beanjaminCoffee) moveToPose(ctx context.Context, step Step) error {
	resp, err := s.sw.DoCommand(ctx, map[string]interface{}{
		"get_pose_by_name": step.PoseName,
	})
	if err != nil {
		return fmt.Errorf("get pose %q: %w", step.PoseName, err)
	}

	x, _ := resp["x"].(float64)
	y, _ := resp["y"].(float64)
	z, _ := resp["z"].(float64)
	oX, _ := resp["o_x"].(float64)
	oY, _ := resp["o_y"].(float64)
	oZ, _ := resp["o_z"].(float64)
	theta, _ := resp["theta_degrees"].(float64)
	refFrame, _ := resp["reference_frame"].(string)
	componentName, _ := resp["component_name"].(string)

	pose := spatialmath.NewPose(
		r3.Vector{X: x, Y: y, Z: z},
		&spatialmath.OrientationVectorDegrees{OX: oX, OY: oY, OZ: oZ, Theta: theta},
	)
	destination := referenceframe.NewPoseInFrame(refFrame, pose)

	moveReq := motion.MoveReq{
		ComponentName: componentName,
		Destination:   destination,
	}

	if step.LinearConstraint != nil {
		s.logger.Infof("applying linear constraint to %q (line=%.1fmm, orient=%.1f°)",
			step.PoseName, step.LinearConstraint.LineToleranceMm, step.LinearConstraint.OrientationToleranceDegs)
		moveReq.Constraints = &motionplan.Constraints{
			LinearConstraint: []motionplan.LinearConstraint{
				{
					LineToleranceMm:          step.LinearConstraint.LineToleranceMm,
					OrientationToleranceDegs: step.LinearConstraint.OrientationToleranceDegs,
				},
			},
		}
	}

	_, err = s.motion.Move(ctx, moveReq)
	if err != nil {
		return fmt.Errorf("move to %q failed: %w", step.PoseName, err)
	}
	return nil
}
