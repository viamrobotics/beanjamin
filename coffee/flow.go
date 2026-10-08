package coffee

// Plan-ahead: during an order the brew code is the planning thread.
// It turns each step into a move (trajectory, gripper action, sleep) and hands it
// to an execution goroutine, which only ever sees moves. At a stopping point it
// settles: closes the channel and waits for execution to drain.

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/referenceframe"
)

const (
	// planAheadStartToleranceRad is how far, per joint, the arm may be from a
	// move's start before the move is replanned.
	planAheadStartToleranceRad = 0.01
	// flowQueueSize bounds how far planning can get ahead of execution.
	flowQueueSize = 64
)

type gripperAction int

const (
	noGripperAction gripperAction = iota
	openGripperAction
	closeGripperAction
)

// move is one unit of work for the execution thread. Its parts run in field
// order; unset parts are skipped.
type move struct {
	name     string
	bookkeep func() // step label or state flag, applied as the move starts

	positions [][]referenceframe.Input // arm trajectory; nil for no arm motion
	start     []referenceframe.Input   // where positions starts
	speed     *arm.MoveOptions
	// replan replans the trajectory from where the arm is, if it isn't at start.
	replan func(ctx context.Context, from []referenceframe.Input) ([][]referenceframe.Input, error)

	gripper gripperAction
	sleep   time.Duration
}

// flow is one order's plan-ahead state, owned by the planning thread.
type flow struct {
	ctx context.Context // order ctx merged with cancelCtx

	from      []referenceframe.Input // end of the last handed-off move; nil means read the arm
	moves     chan move              // nil when execution is idle
	done      chan error
	failed    atomic.Bool // set by the execution thread
	err       error       // first execution error; ends the order
	syncDepth int         // >0 inside syncRegion
}

// startFlow starts plan-ahead for an order. end settles and returns the first
// execution error.
func (s *beanjaminCoffee) startFlow(ctx, cancelCtx context.Context) (end func() error) {
	merged, cancel := mergedCancelContext(ctx, cancelCtx)
	s.flow = &flow{ctx: merged}
	return func() error {
		defer cancel()
		err := s.settle()
		s.flow = nil
		return err
	}
}

// activeFlow returns the flow when moves should be handed off, else nil.
func (s *beanjaminCoffee) activeFlow() *flow {
	if s.flow == nil || s.flow.syncDepth > 0 {
		return nil
	}
	return s.flow
}

// settle waits for all handed-off moves to finish and returns the first
// execution error.
func (s *beanjaminCoffee) settle() error {
	f := s.flow
	if f == nil {
		return nil
	}
	if f.moves != nil {
		close(f.moves)
		if err := <-f.done; f.err == nil {
			f.err = err
		}
		f.moves = nil
	}
	f.from = nil
	return f.err
}

// handOff gives m to the execution thread, or runs it now when there's no
// active flow.
func (s *beanjaminCoffee) handOff(ctx context.Context, m move) error {
	f := s.activeFlow()
	if f == nil {
		return s.runMove(ctx, m)
	}
	if f.err != nil || f.failed.Load() {
		return s.settle()
	}
	if f.moves == nil {
		f.startExecution(s)
	}
	f.moves <- m
	return nil
}

// handOffEffect applies a step label or state flag in step with the arm.
func (s *beanjaminCoffee) handOffEffect(effect func()) {
	_ = s.handOff(context.Background(), move{bookkeep: effect}) //nolint:errcheck // effects can't fail
}

// syncRegion settles, then runs moves immediately until release. For code that
// needs each step's result right away.
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

// startExecution runs moves in order, skipping the rest after a failure.
func (f *flow) startExecution(s *beanjaminCoffee) {
	moves := make(chan move, flowQueueSize)
	done := make(chan error, 1)
	f.moves, f.done = moves, done
	go func() {
		var first error
		for m := range moves {
			if first != nil {
				continue // drain so the planner never blocks
			}
			if err := s.runMoveRecovered(f.ctx, m); err != nil {
				first = err
				f.failed.Store(true)
			}
		}
		done <- first
	}()
}

// runMoveRecovered runs m, turning a panic into an error. The queue's recover
// only covers its own goroutine, so without this a panic here kills the module.
func (s *beanjaminCoffee) runMoveRecovered(ctx context.Context, m move) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic during %q: %v", m.name, r)
		}
	}()
	return s.runMove(ctx, m)
}

// runMove executes a move: bookkeeping, trajectory, gripper, then sleep.
func (s *beanjaminCoffee) runMove(ctx context.Context, m move) error {
	if m.bookkeep != nil {
		m.bookkeep()
	}
	if m.positions != nil {
		if err := s.runTrajectory(ctx, m); err != nil {
			return fmt.Errorf("move to %q failed: %w", m.name, err)
		}
	}
	switch m.gripper {
	case openGripperAction:
		if err := s.gripper.Open(ctx, nil); err != nil {
			return fmt.Errorf("open gripper: %w", err)
		}
	case closeGripperAction:
		if _, err := s.gripper.Grab(ctx, nil); err != nil {
			return fmt.Errorf("close gripper: %w", err)
		}
	case noGripperAction:
	}
	if m.sleep > 0 {
		select {
		case <-time.After(m.sleep):
		case <-ctx.Done():
			return fmt.Errorf("cancelled during pause after %q: %w", m.name, ctx.Err())
		}
	}
	return nil
}

// runTrajectory moves the arm through m's trajectory, replanning first if the
// arm isn't where it starts.
func (s *beanjaminCoffee) runTrajectory(ctx context.Context, m move) error {
	ctx, span := trace.StartSpan(ctx, "beanjamin::executeStep::"+m.name)
	defer span.End()

	positions := m.positions
	now, err := s.arm.CurrentInputs(ctx)
	if err != nil {
		return fmt.Errorf("get current inputs: %w", err)
	}
	if !armNear(now, m.start) {
		s.activeOrderLogger().Infof("arm is off the plan's start, replanning %q", m.name)
		if positions, err = m.replan(ctx, now); err != nil {
			return err
		}
	}
	s.activeOrderLogger().Infof("moving to %q", m.name)
	return s.arm.MoveThroughJointPositions(ctx, positions, m.speed, nil)
}

// handOffStep turns a direct-move step into a move, planned from where the last
// handed-off move ends, and hands it off.
func (s *beanjaminCoffee) handOffStep(ctx context.Context, f *flow, step Step) error {
	from := f.from
	if from == nil {
		now, err := s.arm.CurrentInputs(ctx)
		if err != nil {
			return fmt.Errorf("get current inputs: %w", err)
		}
		from = now
	}
	positions, planErr := s.planStep(ctx, step, from)
	if planErr != nil {
		if err := s.settle(); err != nil {
			return err // an earlier failure caused this one
		}
		return fmt.Errorf("move to %q failed: %w", step.PoseName, planErr)
	}
	f.from = positions[len(positions)-1]
	return s.handOff(ctx, move{
		name:      step.PoseName,
		positions: positions,
		start:     from,
		speed:     s.moveOptionsFor(step.LinearConstraint, step.MoveOptions),
		replan: func(ctx context.Context, from []referenceframe.Input) ([][]referenceframe.Input, error) {
			return s.planStep(ctx, step, from)
		},
		sleep: step.Pause,
	})
}

// planStep plans a direct-move step from the arm joints from and returns its
// trajectory.
func (s *beanjaminCoffee) planStep(ctx context.Context, step Step, from []referenceframe.Input) ([][]referenceframe.Input, error) {
	pd, err := s.resolvePose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return nil, err
	}
	inputs := referenceframe.NewZeroInputs(s.cachedFS)
	inputs[s.cfg.ArmName] = from
	plan, err := s.planToRawPose(ctx, s.cachedFS, inputs, pd, step.LinearConstraint, step.AllowedCollisions)
	if err != nil {
		return nil, err
	}
	positions, err := s.armInputs(plan, "move")
	if err != nil {
		return nil, err
	}
	if len(positions) == 0 {
		return nil, fmt.Errorf("plan has an empty trajectory")
	}
	return positions, nil
}

// isDirectMove reports whether a step is handed off. Pivots, circles and no-spill
// carries plan and run inside their own executors, so for now they're stopping
// points.
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
