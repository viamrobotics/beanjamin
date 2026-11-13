package coffee

// Chunks: stretches of a run's arm work whose motions are planned together, in
// one armplanning.PlanMotionSequence call.
//
// Planned one motion at a time, the arm can end a move in a joint configuration
// that leaves the next one nowhere to go — wound up a joint's travel ahead of a
// pivot, say — even though another path to the same pose would have left room.
// PlanMotionSequence backtracks into the earlier motions when a later one can't
// be planned from where they end, so the motions it plans together are chosen
// to fit together.
//
// While a chunk is open (inChunk), addMotion records each motion's request and
// handOff records every other move, instead of handing them off. Outside one,
// each motion of a run is planned as a chunk of its own. Each request is built
// from the scene's inputs. Once a motion is recorded, the arm is left out of
// them: where it will stand is only known once the recorded motions are
// planned. What only needs to know where the frames on the arm will be — a
// pivot's start, a scene edit — reads it off the last recorded motion's goal
// (placement). What needs the arm's joints flushes the chunk first — plans what
// is recorded so far and hands it off: a request whose goal hangs off the arm,
// a goal that cannot place the arm, and any stopping point (settle). The chunk
// flushes whatever is left when it closes.
//
// An order's chunk hands its moves to the execution goroutine; any other run
// runs them as the chunk flushes.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

const (
	// chunkPlanTimeout bounds planning one flush of a chunk. Each request keeps
	// its own motionPlanTimeout, but backtracking can plan a request many times.
	chunkPlanTimeout = 60 * time.Second
	// chunkBacktrackBranchFactor is how many endings of a motion
	// PlanMotionSequence tries from one start before backing up further.
	chunkBacktrackBranchFactor = 5
)

// chunkOp is one recorded piece of a chunk: a motion to plan, or a move the
// execution thread runs as is.
type chunkOp struct {
	motion *motionRequest
	pause  time.Duration
	// scene is the planned scene as the op was recorded, the executed scene once
	// it has run.
	scene *sceneState

	act move
}

// chunk is a stretch of arm work being recorded.
type chunk struct {
	name string
	ops  []*chunkOp
	// start is where the frames stand as the first recorded motion begins; nil
	// until one is recorded.
	start referenceframe.FrameSystemInputs
	// begin is the planned scene before the first recorded op, which the run goes
	// back to when the ops cannot be planned.
	begin *sceneState
}

// add records op, with planned the run's planned scene as it is recorded.
func (c *chunk) add(op *chunkOp, planned *sceneState) {
	if len(c.ops) == 0 {
		c.begin = planned
	}
	if op.scene == nil {
		op.scene = planned
	}
	if op.motion == nil {
		op.act.scene = op.scene
	}
	c.ops = append(c.ops, op)
}

// hasMotions reports whether c holds a motion that is not planned yet.
func (c *chunk) hasMotions() bool {
	if c == nil {
		return false
	}
	for _, op := range c.ops {
		if op.motion != nil {
			return true
		}
	}
	return false
}

// lastGoal returns the frame the last recorded motion's final goal moves and
// where it puts it, or nil when c holds no motion or that goal moves more than
// one frame.
func (c *chunk) lastGoal() (string, *referenceframe.PoseInFrame) {
	if c == nil {
		return "", nil
	}
	for i := len(c.ops) - 1; i >= 0; i-- {
		if m := c.ops[i].motion; m != nil {
			poses := m.req.Goals[len(m.req.Goals)-1].Poses()
			if len(poses) != 1 {
				return "", nil
			}
			for frame, goal := range poses {
				return frame, goal
			}
		}
	}
	return "", nil
}

// armlessStart returns c's start without the arm named arm.
func (c *chunk) armlessStart(arm string) referenceframe.FrameSystemInputs {
	start := make(referenceframe.FrameSystemInputs, len(c.start))
	for name, in := range c.start {
		if name != arm {
			start[name] = in
		}
	}
	return start
}

// inChunk records the arm work record does as one chunk named name and plans
// it together, flushing whatever is left when record returns. Outside a run,
// inside a sync region, or inside another chunk, record just runs.
//
// When record fails, what it recorded is dropped and the planned scene goes
// back to where the work handed off before it leaves the cell.
func (s *beanjaminCoffee) inChunk(name string, record func() error) error {
	r := s.run
	if r == nil || r.synced || r.chunk != nil {
		return record()
	}
	r.chunk = &chunk{name: name}
	defer func() { r.chunk = nil }()
	if err := record(); err != nil {
		if c := r.chunk; len(c.ops) > 0 {
			r.planned = c.begin
		}
		return err
	}
	return s.flushChunk()
}

// motionBuilder builds a motion's request from where the frames stand as it
// starts. A start without the arm means the motions before it are not planned
// yet; a builder that needs the arm returns an error, and is called again once
// they are.
type motionBuilder func(start referenceframe.FrameSystemInputs) (*motionRequest, error)

// addMotion records the motion build builds in the open chunk, or plans and
// runs it as a chunk of its own, named name. op carries how the move runs: its
// pause. Outside a run or inside a sync region, it is planned
// from the arm read now and run straight away.
func (s *beanjaminCoffee) addMotion(ctx, cancelCtx context.Context, name string, op chunkOp, build motionBuilder) error {
	r := s.run
	if r == nil || r.synced {
		return s.runMotionNow(ctx, cancelCtx, op.pause, build)
	}
	return s.inChunk(name, func() error { return s.recordMotion(r, &op, build) })
}

// recordMotion records op with the motion build builds in the open chunk.
func (s *beanjaminCoffee) recordMotion(r *armRun, op *chunkOp, build motionBuilder) error {
	m, err := s.recordedMotion(r, build)
	if err != nil && r.chunk.hasMotions() {
		// The request may need to know where the recorded motions leave the arm.
		if err := s.flushChunk(); err != nil {
			return err
		}
		m, err = s.recordedMotion(r, build)
	}
	if err != nil {
		return err
	}
	op.motion = m
	r.chunk.add(op, r.planned)
	return nil
}

// recordedMotion builds a request from where the chunk leaves the frames: the
// expected inputs for the chunk's first motion, the chunk's start without the
// arm after that.
func (s *beanjaminCoffee) recordedMotion(r *armRun, build motionBuilder) (*motionRequest, error) {
	c := r.chunk
	if c.hasMotions() {
		return build(c.armlessStart(s.cfg.ArmName))
	}
	inputs, err := s.expectedInputs(r.ctx)
	if err != nil {
		return nil, err
	}
	c.start = inputs
	return build(inputs)
}

// runMotionNow plans the motion build builds from the arm read now, runs it
// and waits out pause.
func (s *beanjaminCoffee) runMotionNow(ctx, cancelCtx context.Context, pause time.Duration, build motionBuilder) error {
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()
	_, inputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	m, err := build(inputs)
	if err != nil {
		return err
	}
	if err := s.runMotion(ctx, m); err != nil {
		return err
	}
	return sleepCtx(ctx, pause, m.name)
}

// sleepCtx waits d after the move named name, or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration, name string) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("cancelled during pause after %q: %w", name, ctx.Err())
	}
}

// flushChunk plans the open chunk's recorded motions together and hands the
// recorded ops off in order. After a failed move nothing is planned: the ops
// are dropped, and settle reports the failure. When planning fails, the moves
// handed off before the chunk run first, so a failure among them is reported
// in its place.
func (s *beanjaminCoffee) flushChunk() error {
	r := s.run
	if r == nil || r.chunk == nil || len(r.chunk.ops) == 0 {
		return nil
	}
	c := r.chunk
	ops, start, begin := c.ops, c.start, c.begin
	c.ops, c.start, c.begin = nil, nil, nil
	if r.err != nil || r.failed.Load() {
		r.planned = begin
		return nil
	}
	var motions []*motionRequest
	for _, op := range ops {
		if op.motion != nil {
			motions = append(motions, op.motion)
		}
	}
	trajectories, err := s.planChunk(c.name, motions)
	if err != nil {
		r.planned = begin
		if settleErr := s.settle(); settleErr != nil {
			return settleErr // an earlier failure caused this one
		}
		return err
	}

	from := start[s.cfg.ArmName]
	planned := 0
	for _, op := range ops {
		m := op.act
		if op.motion != nil {
			positions := trajectories[planned]
			planned++
			m = move{
				name:      op.motion.name,
				positions: positions,
				start:     from,
				speed:     buildMoveOptions(op.motion.speed),
				repeatFor: op.motion.repeatFor,
				sleep:     op.pause,
				scene:     op.scene,
			}
			from = positions[len(positions)-1]
		}
		if err := s.dispatch(r, m); err != nil {
			return err
		}
	}
	if planned > 0 {
		r.planned = r.planned.withArm(s.cfg.ArmName, from)
	}
	return nil
}

// dispatch hands m to the execution goroutine in a run that plans ahead, and
// runs it now in any other. A move that fails now leaves the planned scene
// where the executed one is, with the arm to be read again.
func (s *beanjaminCoffee) dispatch(r *armRun, m move) error {
	if s.aheadRun() != nil {
		r.queue(s, m)
		return nil
	}
	if err := s.runMoveRecovered(r.ctx, m); err != nil {
		r.planned = r.executed.Load().withInputs(nil)
		return err
	}
	r.ran(m)
	return nil
}

// planChunk plans motions in one PlanMotionSequence call and returns each one's
// arm trajectory. A failure wraps errMotionPlanning: nothing of the chunk was
// handed off, so the arm stops where the work before it leaves it.
func (s *beanjaminCoffee) planChunk(name string, motions []*motionRequest) ([][][]referenceframe.Input, error) {
	if len(motions) == 0 {
		return nil, nil
	}
	logger := s.activeOrderLogger()
	reqs := make([]*armplanning.PlanRequest, len(motions))
	for i, m := range motions {
		m.req.PlannerOptions = withPlanTimeout(m.req.PlannerOptions)
		reqs[i] = m.req
	}
	began := time.Now()
	ctx, cancel := context.WithTimeout(s.run.ctx, chunkPlanTimeout)
	plans, metas, planErr := armplanning.PlanMotionSequence(ctx, logger, reqs,
		&armplanning.SequenceOptions{BacktrackBranchFactor: chunkBacktrackBranchFactor})
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancel()
	took := time.Since(began).Round(time.Millisecond)

	// A sequence error means no combination of plans was found; a plan short of
	// its goal, that the deadline cut a request off.
	sequenceFailed := planErr != nil
	solved := 0
	if !sequenceFailed {
		for ; solved < len(reqs); solved++ {
			if planErr = planShortOfGoal(reqs[solved], plans[solved]); planErr != nil {
				break
			}
		}
	}
	if planErr != nil {
		at, err := solved, planErr
		switch {
		case timedOut:
			// Past the deadline the planner hands back a plan that stays where it
			// starts for every motion it has left, so neither the plans nor the
			// error say what held the chunk up; the backtracking does.
			at = stuckMotion(metas, solved)
			err = fmt.Errorf("ran out of planning time (%d backtracks)", totalBacktracks(metas))
			if at > 0 && metas[at-1] != nil && metas[at-1].BacktrackCounter > 0 {
				err = fmt.Errorf("ran out of planning time: it could not be planned from where %q ended (%d backtracks)",
					motions[at-1].name, totalBacktracks(metas))
			}
		case sequenceFailed:
			at = stuckMotion(metas, solved)
		}
		s.saveChunkPlans(name, began, motions, plans, solved, err)
		return nil, fmt.Errorf("%w (chunk %q, at %q, motion %d of %d, after %s): %w",
			errMotionPlanning, name, motions[at].name, at+1, len(motions), took, err)
	}
	s.saveChunkPlans(name, began, motions, plans, solved, nil)

	trajectories := make([][][]referenceframe.Input, len(plans))
	for i, plan := range plans {
		positions, err := s.armInputs(plan, motions[i].label)
		if err != nil {
			return nil, err
		}
		if len(positions) == 0 {
			return nil, fmt.Errorf("plan for %q has an empty trajectory", motions[i].name)
		}
		trajectories[i] = positions
	}
	logger.Infof("planned %q: %d motion(s) in %s (%d backtracks)", name, len(motions), took, totalBacktracks(metas))
	return trajectories, nil
}

// planGoalTolMm and planGoalTolDegs are how close a plan's last configuration
// must bring each goal frame to the request's final goal to count as complete.
const (
	planGoalTolMm   = 1.0
	planGoalTolDegs = 1.0
)

// planShortOfGoal reports a plan that stops short of req's final goal, as one
// whose deadline expired between goals does. It looks at where the plan ends
// rather than at its meta, whose counts PlanMotionSequence sums over every
// attempt at the request.
func planShortOfGoal(req *armplanning.PlanRequest, plan motionplan.Plan) error {
	if plan == nil {
		return errors.New("no plan")
	}
	traj := plan.Trajectory()
	if len(traj) == 0 {
		return errors.New("empty trajectory")
	}
	last := traj[len(traj)-1].ToLinearInputs()
	for frame, goal := range req.Goals[len(req.Goals)-1].Poses() {
		tf, err := req.FrameSystem.Transform(last, referenceframe.NewPoseInFrame(frame, spatialmath.NewZeroPose()), goal.Parent())
		if err != nil {
			return fmt.Errorf("locate %q at the end of the plan: %w", frame, err)
		}
		reached := tf.(*referenceframe.PoseInFrame).Pose()
		if d := reached.Point().Sub(goal.Pose().Point()).Norm(); d > planGoalTolMm {
			return fmt.Errorf("plan ends %.1fmm from the goal for %q", d, frame)
		}
		between := spatialmath.OrientationBetween(reached.Orientation(), goal.Pose().Orientation())
		if degs := math.Abs(spatialmath.QuatToR4AA(between.Quaternion()).Theta) * 180 / math.Pi; degs > planGoalTolDegs {
			return fmt.Errorf("plan ends %.1f° from the goal for %q", degs, frame)
		}
	}
	return nil
}

// saveChunkPlans persists every request of the chunk named name, planned
// together from began: the first solved with their plans, and the rest, which
// a failure left unplanned, with planErr and no plan. Replaying the whole
// sequence from the first request's start reproduces the planning, whichever
// motion it got stuck on.
func (s *beanjaminCoffee) saveChunkPlans(
	name string, began time.Time, motions []*motionRequest, plans []motionplan.Plan, solved int, planErr error,
) {
	for i, m := range motions {
		plan, err := motionplan.Plan(nil), planErr
		if i < solved {
			plan, err = plans[i], nil
		}
		s.savePlanFile(m.req, plan, m.label, err, chunkPlanFileName(began, name, i, len(motions), m.label))
	}
}

// chunkPlanFileName names the file of motion i of the n in the chunk named
// chunk: the time the chunk started planning, so its files sort together and
// in order (the motion's place is zero-padded), then the chunk, the motion's
// place in it, and its label. For example
// 20261112_183152.082_chunk_fetch_pour_and_return_milk_5of9_move.
func chunkPlanFileName(began time.Time, chunk string, i, n int, label string) string {
	width := len(strconv.Itoa(n))
	return fmt.Sprintf("%s_chunk_%s_%0*dof%d_%s", began.Format(planStampLayout), slugify(chunk), width, i+1, n, label)
}

// stuckMotion returns the index of the motion a failed chunk was stuck on. A
// motion is planned again when the one after it cannot start from where it
// ends, so that is the motion after the one planned again most often. Without
// any backtracking, it is the first motion that did not plan.
func stuckMotion(metas []*armplanning.PlanMeta, solved int) int {
	most, at := 0, -1
	for i, m := range metas {
		if m != nil && m.BacktrackCounter > most {
			most, at = m.BacktrackCounter, i
		}
	}
	if at >= 0 && at+1 < len(metas) {
		return at + 1
	}
	return max(min(solved, len(metas)-1), 0)
}

// totalBacktracks is how many times the planner went back to an earlier
// motion, over the whole chunk.
func totalBacktracks(metas []*armplanning.PlanMeta) int {
	n := 0
	for _, m := range metas {
		if m != nil {
			n += m.BacktrackCounter
		}
	}
	return n
}
