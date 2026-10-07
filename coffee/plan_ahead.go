package coffee

// Plan ahead: while the arm runs one move of a segment (consecutive direct
// moves), a goroutine plans the rest, so the arm doesn't pause between moves.

import (
	"context"
	"fmt"
	"math"

	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/referenceframe"
)

// planAheadStartToleranceRad is the per-joint slack between a plan's start and
// the arm before the plan is discarded and the move replanned.
const planAheadStartToleranceRad = 0.01

// isDirectMove reports whether a step can be planned ahead. Pivots, circles and
// no-spill carries plan from the arm's live pose, so they can't.
func isDirectMove(step Step) bool {
	return step.PivotFromPose == "" && step.CircularRadiusMm == 0 && !step.NoSpill
}

// segmentEnd returns the index just past the direct moves starting at i.
func segmentEnd(steps []Step, i int) int {
	j := i
	for j < len(steps) && isDirectMove(steps[j]) {
		j++
	}
	return j
}

// aheadPlan is a plan made ahead and the arm joints it starts from. A nil plan
// means planning failed or stopped.
type aheadPlan struct {
	plan  motionplan.Plan
	start []referenceframe.Input
}

// segmentOps abstracts the arm and planner so the plan-ahead logic is testable.
type segmentOps struct {
	plan       func(ctx context.Context, step Step, from []referenceframe.Input) (plan motionplan.Plan, end []referenceframe.Input, err error)
	armNow     func(ctx context.Context) ([]referenceframe.Input, error)
	runPlanned func(ctx context.Context, step Step, plan motionplan.Plan) error
	runLive    func(ctx context.Context, step Step) error // plans on the spot
}

// planInOrder plans the steps on a goroutine, each from the previous plan's end,
// and sends them on the returned channel in order. It stops at the first
// failure or cancel; the closed channel then makes later steps plan live. wait
// blocks until the goroutine exits.
func planInOrder(ctx context.Context, steps []Step, start []referenceframe.Input, ops segmentOps) (plans <-chan aheadPlan, wait func()) {
	ch := make(chan aheadPlan, len(steps)) // never blocks the planner
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(ch)
		from := start
		for _, step := range steps {
			if ctx.Err() != nil {
				return
			}
			plan, end, err := ops.plan(ctx, step, from)
			if err != nil {
				return // runLive replans and reports any real failure
			}
			ch <- aheadPlan{plan: plan, start: from}
			from = end
		}
	}()
	return ch, func() { <-done }
}

// armNear reports whether every joint of got is within tolerance of want.
func armNear(got, want []referenceframe.Input) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > planAheadStartToleranceRad {
			return false
		}
	}
	return true
}

// runSegmentWith runs the steps while planInOrder plans ahead, using each plan
// only if the arm is at its start. It waits for the planner to exit before
// returning, so the caller can safely change the frame system after.
func runSegmentWith(ctx context.Context, steps []Step, start []referenceframe.Input, ops segmentOps) error {
	planCtx, stopPlanning := context.WithCancel(ctx)
	plans, wait := planInOrder(planCtx, steps, start, ops)
	defer func() {
		stopPlanning()
		wait()
	}()

	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("cancelled before %q: %w", step.PoseName, err)
		}
		var ahead aheadPlan
		select {
		case ahead = <-plans: // zero value once closed
		case <-ctx.Done():
			return fmt.Errorf("cancelled before %q: %w", step.PoseName, ctx.Err())
		}
		if ahead.plan != nil {
			if now, err := ops.armNow(ctx); err == nil && armNear(now, ahead.start) {
				if err := ops.runPlanned(ctx, step, ahead.plan); err != nil {
					return err
				}
				continue
			}
		}
		if err := ops.runLive(ctx, step); err != nil {
			return err
		}
	}
	return nil
}

// runSegment runs a segment of direct moves with plan-ahead on the real arm.
func (s *beanjaminCoffee) runSegment(ctx, cancelCtx context.Context, steps []Step) error {
	// So an operator cancel also stops the planner.
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	fs, baseInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	ops := segmentOps{
		plan: func(ctx context.Context, step Step, from []referenceframe.Input) (motionplan.Plan, []referenceframe.Input, error) {
			pd, err := s.resolvePose(ctx, step.PoseSwitch, step.PoseName)
			if err != nil {
				return nil, nil, err
			}
			plan, err := s.planToRawPose(ctx, fs, s.withArmInputs(baseInputs, from), pd, step.LinearConstraint, step.AllowedCollisions)
			if err != nil {
				return nil, nil, err
			}
			end, err := s.planEndArmInputs(plan)
			if err != nil {
				return nil, nil, err
			}
			return plan, end, nil
		},
		armNow: func(ctx context.Context) ([]referenceframe.Input, error) {
			return s.arm.CurrentInputs(ctx)
		},
		runPlanned: func(ctx context.Context, step Step, plan motionplan.Plan) error {
			return s.executePlannedStep(ctx, cancelCtx, step, plan)
		},
		runLive: func(ctx context.Context, step Step) error {
			return s.executeStep(ctx, cancelCtx, step)
		},
	}
	return runSegmentWith(ctx, steps, baseInputs[s.cfg.ArmName], ops)
}

// executePlannedStep is executeStep for an already-planned direct move.
func (s *beanjaminCoffee) executePlannedStep(ctx, cancelCtx context.Context, step Step, plan motionplan.Plan) error {
	ctx, span := trace.StartSpan(ctx, "beanjamin::executeStep::"+step.PoseName)
	defer span.End()

	if err := stepCancelled(ctx, cancelCtx, step); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("moving to %q (planned ahead)", step.PoseName)
	if err := s.executePlan(ctx, plan, step.LinearConstraint, step.MoveOptions); err != nil {
		return fmt.Errorf("move to %q failed: %w", step.PoseName, err)
	}
	return pauseAfterStep(ctx, cancelCtx, step, s.activeOrderLogger())
}
