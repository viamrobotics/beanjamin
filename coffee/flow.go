package coffee

// Plan-ahead (plan_ahead): during an order the brew code is the planning thread.
// It plans each direct move from where the previous one ends and hands it, with
// gripper actions, waits and step labels, to an execution goroutine. At a
// stopping point it settles: closes the channel and waits for execution to drain.

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/referenceframe"
)

const (
	// planAheadStartToleranceRad is how far, per joint, the arm may be from a
	// handed-off plan's start before the move is replanned.
	planAheadStartToleranceRad = 0.01
	// flowQueueSize bounds how far planning can get ahead of execution.
	flowQueueSize = 64
)

// flow is one order's plan-ahead state, owned by the planning thread.
type flow struct {
	ctx       context.Context // order ctx merged with cancelCtx
	cancelCtx context.Context

	from      []referenceframe.Input           // end of the last handed-off move; nil means read the arm
	actions   chan func(context.Context) error // nil when execution is idle
	done      chan error
	failed    atomic.Bool // set by the execution thread
	err       error       // first execution error; ends the order
	syncDepth int         // >0 inside syncRegion
}

// startFlow starts plan-ahead for an order. end settles and returns the first
// execution error.
func (s *beanjaminCoffee) startFlow(ctx, cancelCtx context.Context) (end func() error) {
	if !s.cfg.PlanAhead {
		return func() error { return nil }
	}
	merged, cancel := mergedCancelContext(ctx, cancelCtx)
	s.flow = &flow{ctx: merged, cancelCtx: cancelCtx}
	return func() error {
		defer cancel()
		err := s.settle()
		s.flow = nil
		return err
	}
}

// activeFlow returns the flow when work should be handed off, else nil.
func (s *beanjaminCoffee) activeFlow() *flow {
	if s.flow == nil || s.flow.syncDepth > 0 {
		return nil
	}
	return s.flow
}

// settle waits for all handed-off work to finish and returns the first execution
// error.
func (s *beanjaminCoffee) settle() error {
	f := s.flow
	if f == nil {
		return nil
	}
	if f.actions != nil {
		close(f.actions)
		if err := <-f.done; f.err == nil {
			f.err = err
		}
		f.actions = nil
	}
	f.from = nil
	return f.err
}

// handOff runs fn on the execution thread, or now when there's no active flow.
// A non-empty name prefixes fn's error either way.
func (s *beanjaminCoffee) handOff(ctx context.Context, name string, fn func(context.Context) error) error {
	run := fn
	if name != "" {
		run = func(ctx context.Context) error {
			if err := fn(ctx); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			return nil
		}
	}
	f := s.activeFlow()
	if f == nil {
		return run(ctx)
	}
	if f.err != nil || f.failed.Load() {
		return s.settle()
	}
	if f.actions == nil {
		f.startExecution()
	}
	f.actions <- run
	return nil
}

// handOffEffect runs a side effect in step with the arm, e.g. a step label.
func (s *beanjaminCoffee) handOffEffect(effect func()) {
	_ = s.handOff(context.Background(), "", func(context.Context) error { //nolint:errcheck // effects can't fail
		effect()
		return nil
	})
}

// syncRegion settles, then runs handoffs immediately until release. For code
// that needs each step's result right away.
func (s *beanjaminCoffee) syncRegion() (release func(), err error) {
	f := s.flow
	if f == nil {
		return func() {}, nil
	}
	if err := s.settle(); err != nil {
		return nil, err
	}
	f.syncDepth++
	return func() { f.syncDepth-- }, nil
}

// startExecution runs actions in order, skipping the rest after a failure.
func (f *flow) startExecution() {
	actions := make(chan func(context.Context) error, flowQueueSize)
	done := make(chan error, 1)
	f.actions, f.done = actions, done
	go func() {
		var first error
		for run := range actions {
			if first != nil {
				continue // drain so the planner never blocks
			}
			if err := run(f.ctx); err != nil {
				first = err
				f.failed.Store(true)
			}
		}
		done <- first
	}()
}

// handOffMove plans a direct move from where the last handed-off move ends and
// hands it off.
func (s *beanjaminCoffee) handOffMove(ctx context.Context, f *flow, step Step) error {
	from := f.from
	if from == nil {
		now, err := s.arm.CurrentInputs(ctx)
		if err != nil {
			return fmt.Errorf("get current inputs: %w", err)
		}
		from = now
	}
	plan, end, planErr := s.planMoveFrom(ctx, step, from)
	if planErr != nil {
		if err := s.settle(); err != nil {
			return err // an earlier failure caused this one
		}
		return planErr
	}
	f.from = end
	return s.handOff(ctx, "", func(ctx context.Context) error {
		return s.runHandedOffMove(ctx, f.cancelCtx, step, plan, from)
	})
}

// planMoveFrom plans a direct move from the arm joints from.
func (s *beanjaminCoffee) planMoveFrom(ctx context.Context, step Step, from []referenceframe.Input) (motionplan.Plan, []referenceframe.Input, error) {
	pd, err := s.resolvePose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return nil, nil, err
	}
	inputs := referenceframe.NewZeroInputs(s.cachedFS)
	inputs[s.cfg.ArmName] = from
	plan, err := s.planToRawPose(ctx, s.cachedFS, inputs, pd, step.LinearConstraint, step.AllowedCollisions)
	if err != nil {
		return nil, nil, fmt.Errorf("move to %q failed: %w", step.PoseName, err)
	}
	end, err := s.planEndArmInputs(plan)
	if err != nil {
		return nil, nil, fmt.Errorf("move to %q failed: %w", step.PoseName, err)
	}
	return plan, end, nil
}

// runHandedOffMove runs a move on the execution thread, replanning if the arm
// isn't where the plan starts.
func (s *beanjaminCoffee) runHandedOffMove(ctx, cancelCtx context.Context, step Step, plan motionplan.Plan, start []referenceframe.Input) error {
	now, err := s.arm.CurrentInputs(ctx)
	if err != nil {
		return fmt.Errorf("get current inputs: %w", err)
	}
	if !armNear(now, start) {
		s.activeOrderLogger().Infof("arm is off the plan's start, replanning %q", step.PoseName)
		if plan, _, err = s.planMoveFrom(ctx, step, now); err != nil {
			return err
		}
	}
	step.plan = plan
	return s.executeStep(ctx, cancelCtx, step)
}

// isDirectMove reports whether a step can be planned ahead. Pivots, circles and
// no-spill carries plan from the arm's live pose, so they can't.
func isDirectMove(step Step) bool {
	return step.PivotFromPose == "" && step.CircularRadiusMm == 0 && !step.NoSpill
}

// armNear reports whether every joint of got is within tolerance of want.
func armNear(got, want []referenceframe.Input) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if d := got[i] - want[i]; d > planAheadStartToleranceRad || d < -planAheadStartToleranceRad {
			return false
		}
	}
	return true
}
