package coffee

// Planning ahead inside a segment. A segment is a run of consecutive direct
// moves to named poses — steps whose targets are known before the arm starts
// moving and that are planned from nothing but where the previous move ends.
// While the arm executes one move of a segment, a helper goroutine plans the
// rest of the segment, so each move is ready the moment the one before it
// finishes instead of the arm pausing to plan.
//
// A segment ends at anything that cannot be planned ahead: a pivot, a circular
// motion or a no-spill carry (each planned from the arm's live pose), or the end
// of the step list. World changes (locking the filter, attaching or releasing a
// held item, moving the fridge door) never happen inside a step list, so a
// segment never spans one.
//
// A plan made ahead is only used if the arm is actually where it starts;
// otherwise that step is planned on the spot, exactly as it is without plan-ahead.

import (
	"context"
	"fmt"
	"math"

	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/referenceframe"
)

// planAheadStartToleranceRad is how far, per joint, the arm may be from where a
// plan made ahead starts and still execute it. The plan starts where the
// previous plan ends; the arm settles a servo tolerance away from that, far
// smaller than this. Anything larger means the arm is not where the plan
// assumed, and the step is planned again from where it really is.
const planAheadStartToleranceRad = 0.01

// isDirectMove reports whether a step is a plain move to a named pose: no pivot,
// no circular motion and no no-spill carry. Only these can be planned ahead.
func isDirectMove(step Step) bool {
	return step.PivotFromPose == "" && step.CircularRadiusMm == 0 && !step.NoSpill
}

// stepRun is a slice of a step list: either a segment of direct moves, or a
// single step that has to be planned when the arm reaches it.
type stepRun struct {
	steps   []Step
	segment bool
}

// splitIntoRuns cuts a step list into segments of consecutive direct moves and
// the single steps between them, preserving order.
func splitIntoRuns(steps []Step) []stepRun {
	var runs []stepRun
	for i := 0; i < len(steps); {
		if !isDirectMove(steps[i]) {
			runs = append(runs, stepRun{steps: steps[i : i+1]})
			i++
			continue
		}
		j := i
		for j < len(steps) && isDirectMove(steps[j]) {
			j++
		}
		runs = append(runs, stepRun{steps: steps[i:j], segment: true})
		i = j
	}
	return runs
}

// aheadPlan is one step's plan made ahead, with the arm joints it starts from.
// A zero aheadPlan (nil plan) means none was made: planning failed or stopped.
type aheadPlan struct {
	plan  motionplan.Plan
	start []referenceframe.Input
}

// segmentOps is what runSegmentWith needs from the service, split out so the
// plan-ahead logic can be tested without a real arm or planner.
type segmentOps struct {
	// plan plans step from the arm joints from, returning the plan and the arm
	// joints it ends at.
	plan func(ctx context.Context, step Step, from []referenceframe.Input) (motionplan.Plan, []referenceframe.Input, error)
	// armNow reads where the arm actually is.
	armNow func(ctx context.Context) ([]referenceframe.Input, error)
	// runPlanned executes a step with a plan made ahead.
	runPlanned func(ctx context.Context, step Step, plan motionplan.Plan) error
	// runLive executes a step the usual way, planning it on the spot.
	runLive func(ctx context.Context, step Step) error
}

// planInOrder plans every step on its own goroutine, in order, each from where
// the previous plan ends, and returns one channel per step. It does not wait
// for the arm: it plans as far ahead as it can, which is at most the end of the
// segment. On the first planning failure, or when ctx ends, it stops and closes
// the remaining channels, so a reader gets a zero aheadPlan for those steps.
// wait blocks until the goroutine has exited.
func planInOrder(ctx context.Context, steps []Step, start []referenceframe.Input, ops segmentOps) (plans []<-chan aheadPlan, wait func()) {
	chans := make([]chan aheadPlan, len(steps))
	plans = make([]<-chan aheadPlan, len(steps))
	for k := range steps {
		chans[k] = make(chan aheadPlan, 1) // never blocks the planner
		plans[k] = chans[k]
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Close every channel on the way out, delivered or not, so the
		// executor never waits on a step the planner gave up on.
		defer func() {
			for _, ch := range chans {
				close(ch)
			}
		}()
		from := start
		for k, step := range steps {
			if ctx.Err() != nil {
				return
			}
			plan, end, err := ops.plan(ctx, step, from)
			if err != nil {
				// The executor plans this step on the spot and reports the
				// failure there if it is real.
				return
			}
			chans[k] <- aheadPlan{plan: plan, start: from}
			from = end
		}
	}()
	return plans, func() { <-done }
}

// armNear reports whether the arm joints got are within
// planAheadStartToleranceRad of want on every joint.
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

// runSegmentWith executes a segment's steps in order while planInOrder plans the
// rest of it. Each step uses its plan made ahead when there is one and the arm is
// where that plan starts; otherwise the step runs live. It returns only after the
// planning goroutine has exited, so nothing reads the frame system once the
// caller moves on to change it.
func runSegmentWith(ctx context.Context, steps []Step, start []referenceframe.Input, ops segmentOps) error {
	planCtx, stopPlanning := context.WithCancel(ctx)
	plans, wait := planInOrder(planCtx, steps, start, ops)
	defer func() {
		stopPlanning()
		wait()
	}()

	for k, step := range steps {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("cancelled before %q: %w", step.PoseName, err)
		}
		var ahead aheadPlan
		select {
		case ahead = <-plans[k]:
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
	// Merge cancelCtx in so an operator cancel stops the planning goroutine as
	// well as the move in flight.
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

// executePlannedStep is executeStep for a direct move whose plan was made ahead:
// the same cancellation check, span, log line and pause, without planning.
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
