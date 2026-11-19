package main

import (
	"context"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
)

// TestStepsPlanOffline plans every dry-run move against a saved motion plan's
// frame system, chaining each move from the previous one's end. Point
// DIALSIM_PLAN at a plan file saved while the portafilter was in the claws
// (e.g. a keepalive purge move), so the frame system matches the machine's.
func TestStepsPlanOffline(t *testing.T) {
	path := os.Getenv("DIALSIM_PLAN")
	if path == "" {
		t.Skip("DIALSIM_PLAN not set")
	}
	req, _, err := armplanning.ReadRequestAndResponseFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := logging.NewTestLogger(t)
	logger.SetLevel(logging.WARN)
	l := defaultLever()
	// DIALSIM_OFFSET overrides how far out from the real lever the moves run;
	// 0 plans the real grab and turn, with the claws allowed to touch the lever.
	if v, err := strconv.ParseFloat(os.Getenv("DIALSIM_OFFSET"), 64); err == nil {
		l.Offset = v
	}
	if v, err := strconv.ParseFloat(os.Getenv("DIALSIM_ROLL"), 64); err == nil {
		l.Roll = v
	}
	var start referenceframe.FrameSystemInputs
	for _, s := range l.steps(5) {
		r := *req
		if start != nil {
			r.StartState = armplanning.NewPlanState(nil, start)
		}
		r.Goals = []*armplanning.PlanState{armplanning.NewPlanState(referenceframe.FrameSystemPoses{
			"grip-point": referenceframe.NewPoseInFrame(referenceframe.World, s.Pose),
		}, nil)}
		r.Constraints = constraints(s.Linear)
		if l.Offset == 0 {
			// On the real lever: the claws close on it, so let them touch it.
			for _, claws := range []string{"coffee-claws-middle", "gripper:claws"} {
				r.Constraints.CollisionSpecification[0].Allows = append(r.Constraints.CollisionSpecification[0].Allows,
					motionplan.CollisionSpecificationAllowedFrameCollisions{Frame1: claws, Frame2: "hot-water-lever"})
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		plan, _, err := armplanning.PlanMotion(ctx, logger, &r)
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", s.Label, err)
		}
		traj := plan.Trajectory()
		start = traj[len(traj)-1]
		j := start["arm"]
		deg := func(i int) float64 { return j[i] * 180 / math.Pi }
		t.Logf("%-28s flange z %4.0f  joints %5.0f %5.0f %5.0f %5.0f %5.0f %5.0f", s.Label, flangeZ(s.Pose), deg(0), deg(1), deg(2), deg(3), deg(4), deg(5))
	}
}
