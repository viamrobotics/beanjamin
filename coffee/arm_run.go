package coffee

// An arm run is one holder's use of the arm. During an order the brew code is
// the planning thread: it turns each step into a move (trajectory, gripper
// action, sleep) and hands it to an execution goroutine, which only ever sees
// moves. At a stopping point it settles: closes the channel and waits for
// execution to drain. Every other run executes each move as it is handed off,
// or, inside a chunk, as the chunk is planned.

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
	// move's start before the move fails.
	planAheadStartToleranceRad = 0.01
	// runQueueSize bounds how far planning can get ahead of execution.
	runQueueSize = 64
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
	// repeatFor replays positions until this long has passed (circular motion).
	repeatFor time.Duration

	gripper gripperAction
	// run is work the execution thread does after the gripper action and before
	// the sleep, such as waiting for the jaws to read open.
	run   func(ctx context.Context) error
	sleep time.Duration

	// recover, when set, returns the scene the cell is left in when the move
	// fails partway, in place of the one it ran from.
	recover func(ctx context.Context) *sceneState

	// scene is the scene the move leaves the cell in, the run's executed scene
	// once the move has run. handOff sets it.
	scene *sceneState
}

// armRun is one holder's use of the arm: an order, a manual action, the
// keepalive purge or an operator command. It follows the scene through planning
// and execution: planned is where the moves handed off so far leave the cell,
// executed is where the moves that have run left it. Planning reads planned
// rather than the arm.
//
// An order plans ahead: its moves run on an execution goroutine while the order
// goroutine plans the next ones. Any other run executes each move as it is
// handed off, so its two scenes stay the same.
type armRun struct {
	name      string
	planAhead bool
	ctx       context.Context // plan-ahead only: the order ctx merged with cancelCtx

	planned  *sceneState // owned by the planning goroutine
	executed atomic.Pointer[sceneState]
	// chunk records moves instead of handing them off while one is open
	// (inChunk); owned by the planning goroutine.
	chunk *chunk
	// flushes counts the chunks planned and handed off.
	flushes int

	moves  chan move // nil when execution is idle
	done   chan error
	failed atomic.Bool // set by the execution thread
	err    error       // first execution error; ends the run
	synced bool        // true inside syncRegion
}

// startRun opens a run on the settled scene for whoever just claimed the arm.
// end settles, makes the executed scene the settled one and returns the first
// execution error.
func (s *beanjaminCoffee) startRun(ctx, cancelCtx context.Context, name string, planAhead bool) (end func() error) {
	if s.run != nil {
		s.activeOrderLogger().Warnf("run %q started while %q is open; it joins that run", name, s.run.name)
		return func() error { return nil }
	}
	merged, cancel := mergedCancelContext(ctx, cancelCtx)
	r := &armRun{name: name, planAhead: planAhead, ctx: merged, planned: s.settledScene.withInputs(nil)}
	r.executed.Store(r.planned)
	s.run = r
	return func() error {
		defer cancel()
		err := s.settle()
		s.settledScene = r.executed.Load().withInputs(nil)
		s.run = nil
		return err
	}
}

// startDirectRun opens a run that executes each move as it is handed off (or
// as its chunk is planned), for
// the troubleshooting commands and the keepalive purge, and returns its end.
// Its moves stop when ctx or cancelCtx is done.
func (s *beanjaminCoffee) startDirectRun(ctx, cancelCtx context.Context, name string) (end func()) {
	endRun := s.startRun(ctx, cancelCtx, name, false)
	return func() {
		if err := endRun(); err != nil {
			s.activeOrderLogger().Errorf("run %q: %v", name, err)
		}
	}
}

// aheadRun returns the run when moves should be handed to its execution
// goroutine, else nil.
func (s *beanjaminCoffee) aheadRun() *armRun {
	if s.run == nil || !s.run.planAhead || s.run.synced {
		return nil
	}
	return s.run
}

// settle waits for all handed-off moves to finish and returns the first
// execution error. After a failure the planned scene goes back to the executed
// one; either way the arm has to be read again to know where it stands.
func (s *beanjaminCoffee) settle() error {
	r := s.run
	if r == nil {
		s.activeOrderLogger().Warn("settle: no arm run, nothing to wait for")
		return nil
	}
	// What a chunk has recorded runs before the arm can stop.
	flushErr := s.flushChunk()
	if r.moves != nil {
		close(r.moves)
		if err := <-r.done; r.err == nil {
			r.err = err
		}
		r.moves = nil
	}
	if r.err != nil {
		r.planned = r.executed.Load()
	}
	r.planned = r.planned.withInputs(nil)
	if r.err != nil {
		return r.err
	}
	return flushErr
}

// handOff gives m to the execution thread, records it in the open chunk, or runs
// it now when the run doesn't plan ahead.
func (s *beanjaminCoffee) handOff(ctx context.Context, m move) error {
	r := s.run
	if r == nil {
		s.activeOrderLogger().Warnf("handOff: no arm run, running %q directly", m.name)
		return s.runMove(ctx, m)
	}
	m.scene = r.planned
	if s.aheadRun() != nil && (r.err != nil || r.failed.Load()) {
		return s.settle()
	}
	if r.chunk != nil && !r.synced {
		r.chunk.add(&chunkOp{act: m}, r.planned)
		return nil
	}
	if s.aheadRun() == nil {
		if err := s.runMove(ctx, m); err != nil {
			r.recoverFrom(ctx, m)
			return err
		}
		r.ran(m)
		return nil
	}
	r.queue(s, m)
	return nil
}

// queue gives m to the execution thread, starting it if it is idle.
func (r *armRun) queue(s *beanjaminCoffee, m move) {
	if r.moves == nil {
		r.startExecution(s)
	}
	r.moves <- m
}

// handOffEffect applies a step label or state flag in step with the arm.
func (s *beanjaminCoffee) handOffEffect(effect func()) {
	_ = s.handOff(context.Background(), move{bookkeep: effect}) //nolint:errcheck // effects can't fail
}

// syncRegion settles, then runs moves immediately until release. For code that
// needs each step's result right away.
func (s *beanjaminCoffee) syncRegion() (release func(), err error) {
	r := s.run
	if r == nil {
		s.activeOrderLogger().Warn("syncRegion: no arm run, moves already run directly")
		return func() {}, nil
	}
	if err := s.settle(); err != nil {
		return nil, err
	}
	r.synced = true
	return func() { r.synced = false }, nil
}

// ran makes the scene m leaves the cell in the executed one, once m has run.
func (r *armRun) ran(m move) {
	if m.scene != nil {
		r.executed.Store(m.scene)
	}
}

// recoverFrom makes the scene a failed m left the cell in the executed one,
// when m knows it.
func (r *armRun) recoverFrom(ctx context.Context, m move) {
	if m.recover == nil {
		return
	}
	if sc := m.recover(context.WithoutCancel(ctx)); sc != nil {
		r.executed.Store(sc)
	}
}

// startExecution runs moves in order, skipping the rest after a failure.
func (r *armRun) startExecution(s *beanjaminCoffee) {
	moves := make(chan move, runQueueSize)
	done := make(chan error, 1)
	r.moves, r.done = moves, done
	go func() {
		var first error
		for m := range moves {
			if first != nil {
				continue // drain so the planner never blocks
			}
			if err := s.runMoveRecovered(r.ctx, m); err != nil {
				first = err
				r.recoverFrom(r.ctx, m)
				r.failed.Store(true)
				continue
			}
			r.ran(m)
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

// runMove executes a move: bookkeeping, trajectory, gripper, run, then sleep.
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
	if m.run != nil {
		if err := m.run(ctx); err != nil {
			return err
		}
	}
	return sleepCtx(ctx, m.sleep, m.name)
}

// runTrajectory moves the arm through m's trajectory, failing if the arm isn't
// where it starts.
func (s *beanjaminCoffee) runTrajectory(ctx context.Context, m move) error {
	ctx, span := trace.StartSpan(ctx, "beanjamin::executeStep::"+m.name)
	defer span.End()

	now, err := s.arm.CurrentInputs(ctx)
	if err != nil {
		return fmt.Errorf("get current inputs: %w", err)
	}
	if !armNear(now, m.start) {
		return fmt.Errorf("arm is at %v, not where the plan starts (%v)", now, m.start)
	}
	s.activeOrderLogger().Infof("moving to %q", m.name)
	return s.runPositions(ctx, m.name, m.positions, m.speed, m.repeatFor)
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
