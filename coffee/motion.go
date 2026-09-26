package coffee

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"

	"go.viam.com/rdk/components/arm"
	toggleswitch "go.viam.com/rdk/components/switch"
)

// errMotionPlanning is wrapped around armplanning.PlanMotion failures in
// moveToRawPose so callers with a recovery path (e.g. dynamic cup pickup
// falling back to another candidate cup) can use errors.Is to distinguish
// planning failures from execution errors.
var errMotionPlanning = errors.New("motion planning failed")

var defaultApproachConstraint = &StepLinearConstraint{
	LineToleranceMm:          1,
	OrientationToleranceDegs: 2,
}

// mergedCancelContext derives a context cancelled by either ctx or the shared
// cancelCtx, so an operator cancel interrupts planning and execution alike. The
// returned func must be deferred.
func mergedCancelContext(ctx, cancelCtx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	// AfterFunc runs cancel on its own goroutine even when cancelCtx is already
	// done, so without this check the first call made with the merged context
	// could still see it live after the operator has cancelled.
	if cancelCtx.Err() != nil {
		cancel()
		return ctx, cancel
	}
	stop := context.AfterFunc(cancelCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// motionPlanTimeout bounds a single planning attempt. armplanning's own default
// is 300s, long enough that an operator cannot tell a plan that will never
// succeed from one still searching, and long enough to hold the brew cycle open
// past the point where the drink is worth serving. Every plan here runs against
// a fixed cell whose reachable poses solve in well under a second, so overrunning
// this means the pose is unreachable from where the arm stands, not that the
// planner needs longer.
const motionPlanTimeout = 15 * time.Second

// withPlanTimeout bounds opts by motionPlanTimeout, substituting armplanning's
// own defaults when the caller passed none — the same options PlanMotion would
// have filled in, minus its 300s timeout.
func withPlanTimeout(opts *armplanning.PlannerOptions) *armplanning.PlannerOptions {
	if opts == nil {
		opts = armplanning.NewBasicPlannerOptions()
	}
	opts.Timeout = motionPlanTimeout.Seconds()
	return opts
}

// incompletePlanErr reports a plan covering only a prefix of the requested goals.
// armplanning returns one of those with a *nil* error when its deadline expires
// between goals, so without this check a timed-out multi-waypoint plan (pivot,
// circular) would execute as though complete and stop the arm short of the pose
// the next step assumes it reached.
func incompletePlanErr(meta *armplanning.PlanMeta, goals int) error {
	if meta == nil || meta.GoalsProcessed >= goals {
		return nil
	}
	return fmt.Errorf("planner solved %d of %d goals before the %s timeout",
		meta.GoalsProcessed, goals, motionPlanTimeout)
}

// planDuration renders how long a plan took, for logs and error messages that
// have to distinguish "gave up at the timeout" from "failed immediately".
func planDuration(meta *armplanning.PlanMeta) time.Duration {
	if meta == nil {
		return 0
	}
	return meta.Duration.Round(time.Millisecond)
}

// planMotion plans req under motionPlanTimeout, persisting the request/response
// pair under label for offline debugging.
//
// Planning failures are wrapped in errMotionPlanning: a plan that never ran
// leaves the arm where it stood, which is what lets callers with a recovery path
// (dynamic pickup falling back to another candidate, placeHeldInServingArea
// trying the next slot) tell them apart from execution errors via errors.Is.
func (s *beanjaminCoffee) planMotion(ctx context.Context, req *armplanning.PlanRequest, label string) (motionplan.Plan, error) {
	logger := s.activeOrderLogger()
	req.PlannerOptions = withPlanTimeout(req.PlannerOptions)

	plan, meta, err := armplanning.PlanMotion(ctx, logger, req)
	if err == nil {
		err = incompletePlanErr(meta, len(req.Goals))
	}
	s.savePlanRequestAndResponse(req, plan, label, err)
	if err != nil {
		return nil, fmt.Errorf("%w (%s, after %s): %w", errMotionPlanning, label, planDuration(meta), err)
	}
	logger.Infof("planned %s in %s", label, planDuration(meta))
	return plan, nil
}

// armInputs extracts a plan's joint waypoints for the arm frame — not the
// end-effector component name the goal poses are commanded against.
func (s *beanjaminCoffee) armInputs(plan motionplan.Plan, label string) ([][]referenceframe.Input, error) {
	positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
	if err != nil {
		return nil, fmt.Errorf("get frame inputs from %s plan: %w", label, err)
	}
	return positions, nil
}

// planTrajectory plans req and returns the arm joint waypoints to execute.
func (s *beanjaminCoffee) planTrajectory(ctx context.Context, req *armplanning.PlanRequest, label string) ([][]referenceframe.Input, error) {
	plan, err := s.planMotion(ctx, req, label)
	if err != nil {
		return nil, err
	}
	return s.armInputs(plan, label)
}

// worldGoals turns poses authored in refFrame into plan states commanding
// componentName, each transformed into the world frame.
func worldGoals(
	fs *referenceframe.FrameSystem,
	inputs *referenceframe.LinearInputs,
	refFrame, componentName string,
	poses []spatialmath.Pose,
) ([]*armplanning.PlanState, error) {
	goals := make([]*armplanning.PlanState, 0, len(poses))
	for _, pose := range poses {
		tf, err := fs.Transform(inputs, referenceframe.NewPoseInFrame(refFrame, pose), referenceframe.World)
		if err != nil {
			return nil, fmt.Errorf("transform waypoint to world: %w", err)
		}
		goals = append(goals, armplanning.NewPlanState(
			referenceframe.FrameSystemPoses{componentName: tf.(*referenceframe.PoseInFrame)}, nil,
		))
	}
	return goals, nil
}

// freeMoveCollisionBufferMM is the clearance the planner must keep between any
// two geometries that did not begin the motion in collision. It applies to free
// traverses — both the ordinary direct plans and the no-spill level carry — but
// not to steps carrying a LinearConstraint: those have to close the last
// millimetres onto hardware (portafilter into the group head, claws onto a cup)
// and so keep armplanning's own hair-thin default.
const freeMoveCollisionBufferMM = 2.0

// freeMovePlannerOptions returns planner options carrying the free-traverse
// collision buffer.
func freeMovePlannerOptions() *armplanning.PlannerOptions {
	opts := armplanning.NewBasicPlannerOptions()
	opts.CollisionBufferMM = freeMoveCollisionBufferMM
	return opts
}

// plannerOptionsForConstraint returns the planner options for a step's move, or
// nil to let armplanning apply its defaults. A move with no linear constraint is
// a free traverse and gets the extra collision buffer.
func plannerOptionsForConstraint(lc *StepLinearConstraint) *armplanning.PlannerOptions {
	if lc != nil {
		return nil
	}
	return freeMovePlannerOptions()
}

const (
	defaultSlowMovementVelDegsPerSec  = 25.0
	defaultSlowMovementAccDegsPerSec2 = 25.0
)

// Arm moves fall into three speeds. Most run at the arm's own DEFAULT speed
// (no override) — an ordinary free traverse with an empty gripper, or a tamped
// puck. SLOW (slowMoveOptions, below) is for the careful moves: a
// constrained/contact step, a pivot or circular motion, and the spill- or
// scatter-prone carries — the no-spill cup/glass carry, the milk bottle, and
// loose grounds on the way to the tamper. POUR (pourMoveOptions) is the fast
// tilt of a filled container over the cup/glass. An explicit Step.MoveOptions
// overrides the default.
//
// slowMoveOptions is the config-facing StepMoveOptions (degrees) for the slow
// tier, attached to a Step or converted with buildMoveOptions at an execution
// site. Velocity and acceleration fall back to their defaults when unset — the
// acceleration to a gentle value below the arm's own, since a hard acceleration
// can slosh a full cup even at a low top speed.
func (s *beanjaminCoffee) slowMoveOptions() *StepMoveOptions {
	return &StepMoveOptions{
		MaxVelDegsPerSec:  orDefault(s.cfg.SlowMovementVelDegsPerSec, defaultSlowMovementVelDegsPerSec),
		MaxAccDegsPerSec2: orDefault(s.cfg.SlowMovementAccDegsPerSec2, defaultSlowMovementAccDegsPerSec2),
	}
}

// moveToPose fetches a named pose and moves to it.
func (s *beanjaminCoffee) moveToPose(ctx, cancelCtx context.Context, step Step) error {
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	pd, err := s.resolvePose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return err
	}
	// A filled-container traverse (NoSpill) routes through the level carry so the
	// drink doesn't slosh. The carry adds an orientation constraint in place of a
	// LinearConstraint, but still honors the step's AllowedCollisions and
	// MoveOptions. For every ordinary step, plan straight to the pose.
	if step.NoSpill {
		if err := s.carryHeldLevel(ctx, pd, step.AllowedCollisions, step.MoveOptions); err != nil {
			return fmt.Errorf("no-spill carry to %q failed: %w", step.PoseName, err)
		}
		return nil
	}
	if err := s.moveToRawPose(ctx, pd, step.LinearConstraint, step.AllowedCollisions, step.MoveOptions); err != nil {
		return fmt.Errorf("move to %q failed: %w", step.PoseName, err)
	}
	return nil
}

type poseData struct {
	pose          spatialmath.Pose
	refFrame      string
	componentName string
}

// fetchPose retrieves a named pose from the given switch. The returned
// poseData.componentName is the frame the goal pose is commanded against — the
// switch's configured component_name.
func (s *beanjaminCoffee) fetchPose(ctx context.Context, sw toggleswitch.Switch, poseName string) (*poseData, error) {
	if sw == nil {
		return nil, fmt.Errorf("get pose %q: no pose switch configured", poseName)
	}
	resp, err := sw.DoCommand(ctx, map[string]any{
		"get_pose_by_name": poseName,
	})
	if err != nil {
		return nil, fmt.Errorf("get pose %q from %q: %w", poseName, sw.Name().ShortName(), err)
	}

	x, _ := resp["x"].(float64)
	y, _ := resp["y"].(float64)
	z, _ := resp["z"].(float64)
	oX, _ := resp["o_x"].(float64)
	oY, _ := resp["o_y"].(float64)
	oZ, _ := resp["o_z"].(float64)
	theta, _ := resp["theta"].(float64)
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

// fakeMissingFrames are gripper sub-geometries that only exist on the real
// ufactory gripper. When running against a fake barista (FakeMode=true),
// AllowedCollision entries referencing these frames are dropped so motion
// planning doesn't fail on unknown frames.
var fakeMissingFrames = []string{"gripper:claws", "gripper:case-gripper"}

// filterFakeModeCollisions drops AllowedCollision entries that reference a
// frame in fakeMissingFrames. Returns the input unchanged when FakeMode is off.
func (s *beanjaminCoffee) filterFakeModeCollisions(acs []AllowedCollision) []AllowedCollision {
	logger := s.activeOrderLogger()
	if !s.cfg.FakeMode {
		return acs
	}
	out := make([]AllowedCollision, 0, len(acs))
	for _, ac := range acs {
		if slices.Contains(fakeMissingFrames, ac.Frame1) || slices.Contains(fakeMissingFrames, ac.Frame2) {
			logger.Debugf("fake mode: dropping allowed collision %s <-> %s", ac.Frame1, ac.Frame2)
			continue
		}
		out = append(out, ac)
	}
	return out
}

// buildConstraints converts step-level linear constraints and allowed collisions
// into the motionplan.Constraints structure used by armplanning.
func buildConstraints(lc *StepLinearConstraint, allowedCollisions []AllowedCollision) *motionplan.Constraints {
	if lc == nil && len(allowedCollisions) == 0 {
		return nil
	}
	constraints := &motionplan.Constraints{}
	if lc != nil {
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
		constraints.CollisionSpecification = []motionplan.CollisionSpecification{
			{Allows: allows},
		}
	}
	return constraints
}

// buildMoveOptions converts step-level move options into arm.MoveOptions.
func buildMoveOptions(opts *StepMoveOptions) *arm.MoveOptions {
	if opts == nil {
		return nil
	}
	return &arm.MoveOptions{
		MaxVelRads: opts.MaxVelDegsPerSec * math.Pi / 180.0,
		MaxAccRads: opts.MaxAccDegsPerSec2 * math.Pi / 180.0,
	}
}

// moveToRawPose plans a motion using armplanning and executes it on the arm.
func (s *beanjaminCoffee) moveToRawPose(ctx context.Context, pd *poseData, lc *StepLinearConstraint, allowedCollisions []AllowedCollision, moveOpts *StepMoveOptions) error {
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	plan, err := s.planToRawPose(ctx, fs, fsInputs, pd, lc, allowedCollisions)
	if err != nil {
		return err
	}
	return s.executePlan(ctx, plan, lc, moveOpts)
}

// planToRawPose plans a motion to pd starting from startInputs, without touching
// the arm. Planning failures are wrapped in errMotionPlanning so callers with a
// recovery path (e.g. dynamic pickup falling back to another candidate) can
// distinguish them from execution errors. Both the destination transform and the
// plan begin from startInputs, so passing a prior plan's end configuration
// (planEndArmInputs) chains a second plan onto the first — letting a caller
// prepare two moves and commit to executing them only if both plan successfully
// (see tryGrab).
func (s *beanjaminCoffee) planToRawPose(
	ctx context.Context,
	fs *referenceframe.FrameSystem,
	startInputs referenceframe.FrameSystemInputs,
	pd *poseData,
	lc *StepLinearConstraint,
	allowedCollisions []AllowedCollision,
) (motionplan.Plan, error) {
	logger := s.activeOrderLogger()

	// Transform destination to world frame.
	destination := referenceframe.NewPoseInFrame(pd.refFrame, pd.pose)
	tf, err := fs.Transform(startInputs.ToLinearInputs(), destination, referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("transform destination to world: %w", err)
	}
	goalPose := tf.(*referenceframe.PoseInFrame)

	allowedCollisions = s.filterFakeModeCollisions(s.appendHeldItemCollisions(allowedCollisions))
	constraints := buildConstraints(lc, allowedCollisions)
	plannerOpts := plannerOptionsForConstraint(lc)
	if lc != nil {
		logger.Infof("applying linear constraint (line=%.1fmm, orient=%.1f°)",
			lc.LineToleranceMm, lc.OrientationToleranceDegs)
	} else {
		logger.Infof("applying %.1fmm collision buffer", plannerOpts.CollisionBufferMM)
	}
	if len(allowedCollisions) > 0 {
		logger.Infof("allowing %d collision pair(s)", len(allowedCollisions))
	}

	req := &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals: []*armplanning.PlanState{
			armplanning.NewPlanState(referenceframe.FrameSystemPoses{pd.componentName: goalPose}, nil),
		},
		StartState:     armplanning.NewPlanState(nil, startInputs),
		Constraints:    constraints,
		PlannerOptions: plannerOpts,
	}
	return s.planMotion(ctx, req, "move")
}

// executePlan sends a planned trajectory to the arm.
func (s *beanjaminCoffee) executePlan(ctx context.Context, plan motionplan.Plan, lc *StepLinearConstraint, moveOpts *StepMoveOptions) error {
	positions, err := s.armInputs(plan, "move")
	if err != nil {
		return err
	}
	// A constrained move with no explicit speed defaults to the slow tier; a free
	// traverse keeps the arm's own default speed (nil).
	opts := buildMoveOptions(moveOpts)
	if opts == nil && lc != nil {
		opts = buildMoveOptions(s.slowMoveOptions())
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, opts, nil)
}

// planEndArmInputs returns the arm's joint configuration at the end of a plan's
// trajectory. Errors on an empty trajectory.
func (s *beanjaminCoffee) planEndArmInputs(plan motionplan.Plan) ([]referenceframe.Input, error) {
	positions, err := s.armInputs(plan, "move")
	if err != nil {
		return nil, err
	}
	if len(positions) == 0 {
		return nil, fmt.Errorf("plan has an empty trajectory")
	}
	return positions[len(positions)-1], nil
}

// withArmInputs returns a copy of base with the arm frame's inputs replaced by
// armInputs, leaving base unmutated. Used to build a chained plan's start state:
// the same frame-system configuration currentInputs produces (all frames zeroed,
// arm set), but with the arm advanced to a prior plan's end.
func (s *beanjaminCoffee) withArmInputs(base referenceframe.FrameSystemInputs, armInputs []referenceframe.Input) referenceframe.FrameSystemInputs {
	out := make(referenceframe.FrameSystemInputs, len(base))
	maps.Copy(out, base)
	out[s.cfg.ArmName] = armInputs
	return out
}

// pivotPositionToleranceMm is how far apart a pivot's start and end poses may
// be: a pivot is a pure rotation about one fixed point.
const pivotPositionToleranceMm = 0.5

// executePivot fetches start and end poses, computes interpolated waypoints,
// plans a single multi-goal trajectory through all of them, and executes it
// in one MoveThroughJointPositions call.
func (s *beanjaminCoffee) executePivot(ctx, cancelCtx context.Context, step Step) error {
	logger := s.activeOrderLogger()
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	startPD, err := s.resolvePose(ctx, step.PoseSwitch, step.PivotFromPose)
	if err != nil {
		return fmt.Errorf("pivot start: %w", err)
	}
	endPD, err := s.resolvePose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return fmt.Errorf("pivot end: %w", err)
	}

	if startPD.componentName != endPD.componentName {
		return fmt.Errorf("pivot %q → %q: component mismatch (%q vs %q)",
			step.PivotFromPose, step.PoseName, startPD.componentName, endPD.componentName)
	}
	// The authored poses must describe the same point (a pivot is a pure
	// rotation), and the arm must already be standing on it.
	const pivotStartToleranceMm = 2.0
	if dist := startPD.pose.Point().Sub(endPD.pose.Point()).Norm(); dist > pivotPositionToleranceMm {
		return fmt.Errorf("pivot %q → %q: positions differ by %.2f mm (max %.1f mm) — pivot assumes a fixed point",
			step.PivotFromPose, step.PoseName, dist, pivotPositionToleranceMm)
	}

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	linearInputs := fsInputs.ToLinearInputs()

	// Seed the arc from where the arm ACTUALLY is rather than from the authored
	// start pose. Each segment's first waypoint is dropped on the assumption
	// we're already standing on it, so an arm that ended the previous step off
	// the authored pose would get one coarse planner segment to climb back onto
	// the arc — precisely the free-form rotation the fine waypoints exist to
	// prevent, and with a portafilter engaged in the bayonet while it happens.
	// The authored *position* is kept so the pivot stays a rotation about the
	// intended fixed point even if the arm has drifted a fraction of a mm.
	curTF, err := fs.Transform(linearInputs,
		referenceframe.NewPoseInFrame(startPD.componentName, spatialmath.NewZeroPose()),
		startPD.refFrame)
	if err != nil {
		return fmt.Errorf("pivot %q: current pose of %q: %w", step.PivotFromPose, startPD.componentName, err)
	}
	actual := curTF.(*referenceframe.PoseInFrame).Pose()
	if dist := actual.Point().Sub(startPD.pose.Point()).Norm(); dist > pivotStartToleranceMm {
		return fmt.Errorf("pivot %q → %q: arm is %.2f mm off the pivot start (max %.1f mm) — refusing to pivot about the wrong point",
			step.PivotFromPose, step.PoseName, dist, pivotStartToleranceMm)
	}
	fromPose := spatialmath.NewPose(startPD.pose.Point(), actual.Orientation())

	// Rotate straight onto the goal, or past it and back when the step asks for
	// an overshoot. Both segments are planned together and run as one
	// trajectory, so the arm never stops at the overshot pose.
	targets := []spatialmath.Pose{endPD.pose}
	if step.PivotExtraDegrees != 0 {
		// Axis from the authored pair, not fromPose: the arm's drift should seed
		// where the arc starts, never tilt the axis it turns about.
		over, err := pivotOvershootPose(startPD.pose, endPD.pose, step.PivotExtraDegrees)
		if err != nil {
			return fmt.Errorf("pivot %q → %q: %w", step.PivotFromPose, step.PoseName, err)
		}
		targets = []spatialmath.Pose{over, endPD.pose}
	}

	var poses []spatialmath.Pose
	segStart := fromPose
	for _, target := range targets {
		seg := computePivotPoses(logger, segStart, target, step.PivotDegreesPerStep)
		poses = append(poses, seg[1:]...) // seg[0] is where the previous segment left us
		segStart = target
	}
	logger.Infof("pivot %q → %q: %d waypoints (%.1f°/step, %.1f° overshoot)",
		step.PivotFromPose, step.PoseName, len(poses), step.PivotDegreesPerStep, step.PivotExtraDegrees)

	goals, err := worldGoals(fs, linearInputs, startPD.refFrame, startPD.componentName, poses)
	if err != nil {
		return err
	}

	// Every waypoint is planned in a single call and run as one trajectory, so
	// the arm never stops partway through the arc.
	positions, err := s.planTrajectory(ctx, &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       goals,
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: buildConstraints(step.LinearConstraint, s.filterFakeModeCollisions(s.appendHeldItemCollisions(step.AllowedCollisions))),
	}, "pivot")
	if err != nil {
		return err
	}
	opts := buildMoveOptions(step.MoveOptions)
	if opts == nil {
		opts = buildMoveOptions(s.slowMoveOptions())
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, opts, nil)
}

// computeCircularPoses generates waypoints evenly spaced around a circle in
// the XY plane of the given center pose. Orientation is kept constant.
// It returns pointsPerRev poses forming one full revolution (the closing
// point at 360° equals the opening point at 0° and is omitted).
func computeCircularPoses(centerPose spatialmath.Pose, radiusMm float64, pointsPerRev int) []spatialmath.Pose {
	center := centerPose.Point()
	poses := make([]spatialmath.Pose, pointsPerRev)
	for i := range pointsPerRev {
		angle := 2 * math.Pi * float64(i) / float64(pointsPerRev)
		offset := r3.Vector{X: radiusMm * math.Cos(angle), Y: radiusMm * math.Sin(angle), Z: 0}
		poses[i] = spatialmath.NewPose(center.Add(offset), centerPose.Orientation())
	}
	return poses
}

// executeCircularMotion fetches the center pose, computes one revolution of
// circular waypoints, plans the trajectory once, then executes it in a loop
// until the configured duration is exceeded.
func (s *beanjaminCoffee) executeCircularMotion(ctx, cancelCtx context.Context, step Step) error {
	logger := s.activeOrderLogger()
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	centerPD, err := s.resolvePose(ctx, step.PoseSwitch, step.PoseName)
	if err != nil {
		return fmt.Errorf("circular center: %w", err)
	}

	pointsPerRev := step.CircularPointsPerRev
	if pointsPerRev < 4 {
		pointsPerRev = 8
	}

	poses := computeCircularPoses(centerPD.pose, step.CircularRadiusMm, pointsPerRev)
	logger.Infof("circular motion around %q: radius=%.1fmm, %d pts/rev",
		step.PoseName, step.CircularRadiusMm, pointsPerRev)

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	linearInputs := fsInputs.ToLinearInputs()

	// One revolution is planned once and replayed until the duration is up.
	goals, err := worldGoals(fs, linearInputs, centerPD.refFrame, centerPD.componentName, poses)
	if err != nil {
		return err
	}
	positions, err := s.planTrajectory(ctx, &armplanning.PlanRequest{
		FrameSystem: fs,
		Goals:       goals,
		StartState:  armplanning.NewPlanState(nil, fsInputs),
		Constraints: buildConstraints(step.LinearConstraint, s.filterFakeModeCollisions(s.appendHeldItemCollisions(step.AllowedCollisions))),
	}, "circular")
	if err != nil {
		return err
	}

	// Execute revolutions until the duration is exceeded.
	deadline := time.Now().Add(time.Duration(step.CircularDurationSec * float64(time.Second)))
	for rev := 0; time.Now().Before(deadline); rev++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled during circular motion: %w", ctx.Err())
		default:
		}
		logger.Debugf("circular revolution %d", rev+1)
		circOpts := buildMoveOptions(step.MoveOptions)
		if circOpts == nil {
			circOpts = buildMoveOptions(s.slowMoveOptions())
		}
		if err := s.arm.MoveThroughJointPositions(ctx, positions, circOpts, nil); err != nil {
			return fmt.Errorf("execute circular revolution %d: %w", rev+1, err)
		}
	}
	return nil
}

// noSpillOrientationToleranceDegs caps how far the carried container's
// orientation may stray from the slerp between the carry's start and its goal.
// RDK checks this along the whole path as a tube of this width around that
// direct reorientation, so a single start-to-goal segment is enough to hold the
// drink near level for the entire traverse.
//
// The bound is on the excursion off the slerp, not on the commanded rotation:
// both endpoints are upright container poses, so the slerp itself stays level
// and 15° leaves the drink well inside a full cup's static spill angle.
//
// The constraint ignores theta — spin about the moving frame's own +Z, which is
// the container's vertical axis (heldItemFramePose). A cup or bottle spills when
// tipped, not when spun about its axis, so bounding spin would only shrink the
// planner's reachable space without keeping the drink any more level.
const noSpillOrientationToleranceDegs = 15.0

// withNoSpillOrientationConstraint adds the carry's path orientation bound,
// allocating the Constraints when the caller has none (no linear constraint and
// no allowed collisions, so buildConstraints returned nil).
func withNoSpillOrientationConstraint(constraints *motionplan.Constraints) *motionplan.Constraints {
	if constraints == nil {
		constraints = &motionplan.Constraints{}
	}
	constraints.OrientationConstraint = append(constraints.OrientationConstraint,
		motionplan.OrientationConstraint{OrientationToleranceDegs: noSpillOrientationToleranceDegs, IgnoreTheta: true})
	return constraints
}

// carryGoalForMoveFrame converts dest — a pose authored for dest.componentName —
// into the world pose moveFrame must reach for that component to land on dest.
// Returns dest's world pose unchanged when moveFrame is the authored component.
//
// The two frames are rigidly linked but not co-oriented: held-item sits on the
// grip point but is rotated onto the container's axes (heldItemFramePose).
// Commanding the container straight at a grip-point goal leaves the gripper
// mis-rotated. The conversion goes through the frame system rather than assuming
// the frames share an origin, so it holds for any rigid offset between them.
func carryGoalForMoveFrame(
	fs *referenceframe.FrameSystem,
	inputs *referenceframe.LinearInputs,
	dest *poseData,
	moveFrame string,
) (spatialmath.Pose, error) {
	destTF, err := fs.Transform(inputs,
		referenceframe.NewPoseInFrame(dest.refFrame, dest.pose), referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("transform carry destination to world: %w", err)
	}
	destPose := destTF.(*referenceframe.PoseInFrame).Pose()
	if moveFrame == dest.componentName {
		return destPose, nil
	}
	// moveFrame expressed in the authored component's frame — constant, since both
	// hang rigidly off the gripper.
	offTF, err := fs.Transform(inputs,
		referenceframe.NewPoseInFrame(moveFrame, spatialmath.NewZeroPose()), dest.componentName)
	if err != nil {
		return nil, fmt.Errorf("transform %q into %q: %w", moveFrame, dest.componentName, err)
	}
	return spatialmath.Compose(destPose, offTF.(*referenceframe.PoseInFrame).Pose()), nil
}

// carryHeldLevel carries the held container from its current pose to dest,
// free-planning the path but holding the container's orientation within
// noSpillOrientationToleranceDegs of the direct start-to-goal reorientation for
// the whole traverse — so the drink doesn't slosh.
//
// The goal commands the held-item frame rather than the gripper, because the
// orientation bound only bounds the drink's tilt when expressed about the
// container's own axis. That frame is neither coincident nor co-oriented with
// the one dest is authored for, so dest is converted into it
// (carryGoalForMoveFrame); with nothing attached it falls back to the gripper
// frame and the conversion is a no-op.
func (s *beanjaminCoffee) carryHeldLevel(ctx context.Context, dest *poseData, allowedCollisions []AllowedCollision, moveOpts *StepMoveOptions) error {
	logger := s.activeOrderLogger()
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	linearInputs := fsInputs.ToLinearInputs()

	// dest targets its own component (grip-point for a switch pose, held-item
	// for a pour pose). When an item is held, move the held-item frame: it is
	// the container that must stay level.
	moveFrame := gripPoint
	if s.heldItemAttached {
		moveFrame = heldItemFrameName
	}

	// Start: current world pose of the moving frame (the container is upright).
	startPIF := referenceframe.NewPoseInFrame(moveFrame, spatialmath.NewZeroPose())
	startTF, err := fs.Transform(linearInputs, startPIF, referenceframe.World)
	if err != nil {
		return fmt.Errorf("transform held-container start pose to world: %w", err)
	}
	startPose := startTF.(*referenceframe.PoseInFrame).Pose()

	// End: the goal moveFrame must reach for dest's component to land on dest.
	destPose, err := carryGoalForMoveFrame(fs, linearInputs, dest, moveFrame)
	if err != nil {
		return err
	}

	logger.Infof("no-spill carry: moving %q over %.0fmm (path±%.0f°, buffer: %.1fmm)",
		moveFrame, destPose.Point().Sub(startPose.Point()).Norm(),
		noSpillOrientationToleranceDegs, freeMoveCollisionBufferMM)

	goal := armplanning.NewPlanState(referenceframe.FrameSystemPoses{
		moveFrame: referenceframe.NewPoseInFrame(referenceframe.World, destPose),
	}, nil)

	constraints := withNoSpillOrientationConstraint(
		buildConstraints(nil, s.filterFakeModeCollisions(s.appendHeldItemCollisions(allowedCollisions))))

	positions, err := s.planTrajectory(ctx, &armplanning.PlanRequest{
		FrameSystem:    fs,
		Goals:          []*armplanning.PlanState{goal},
		StartState:     armplanning.NewPlanState(nil, fsInputs),
		Constraints:    constraints,
		PlannerOptions: freeMovePlannerOptions(),
	}, "carry")
	if err != nil {
		return err
	}
	// The level carry moves a filled container, so default to the slow tier.
	opts := buildMoveOptions(moveOpts)
	if opts == nil {
		opts = buildMoveOptions(s.slowMoveOptions())
	}
	return s.arm.MoveThroughJointPositions(ctx, positions, opts, nil)
}

// minPivotThetaRads is the smallest authored rotation an overshoot direction
// can be derived from. Below it QuatToR4AA's axis is numerical noise (it falls
// back to a hardcoded +Z), so continuing "along the same axis" is meaningless.
const minPivotThetaRads = 1e-3

// pivotOvershootPose returns endPose rotated a further degrees about the same
// axis that carries startPose's orientation to endPose's, with the position
// left untouched. Positive degrees continues past endPose in the direction of
// travel, so callers do not have to know the pivot's handedness.
//
// The extra rotation pre-multiplies because QuatBetween is a left difference
// (q2 * conj(q1)) — the axis is expressed in the poses' shared reference frame,
// not the tool's body frame.
func pivotOvershootPose(startPose, endPose spatialmath.Pose, degrees float64) (spatialmath.Pose, error) {
	aa := spatialmath.OrientationBetween(startPose.Orientation(), endPose.Orientation()).AxisAngles()
	// AxisAngles reports the same rotation as either (axis, +θ) or (-axis, -θ).
	// Normalize to a positive angle so `degrees` continues the travel direction
	// instead of silently reversing into it. Same signed-Theta trap that
	// computePivotPoses guards against with math.Abs.
	if aa.Theta < 0 {
		aa = &spatialmath.R4AA{Theta: -aa.Theta, RX: -aa.RX, RY: -aa.RY, RZ: -aa.RZ}
	}
	if aa.Theta < minPivotThetaRads {
		return nil, fmt.Errorf("rotation is %.5f rad, too small to derive an overshoot axis", aa.Theta)
	}
	extra := spatialmath.NewPoseFromOrientation(&spatialmath.R4AA{
		Theta: degrees * math.Pi / 180.0, RX: aa.RX, RY: aa.RY, RZ: aa.RZ,
	})
	rotated := spatialmath.Compose(extra, spatialmath.NewPoseFromOrientation(endPose.Orientation()))
	return spatialmath.NewPose(endPose.Point(), rotated.Orientation()), nil
}

// computePivotPoses returns interpolated poses between startPose and endPose.
// The step count is derived from the total rotation angle divided by degreesPerStep.
func computePivotPoses(logger logging.Logger, startPose, endPose spatialmath.Pose, degreesPerStep float64) []spatialmath.Pose {
	diff := spatialmath.OrientationBetween(startPose.Orientation(), endPose.Orientation())
	// AxisAngles().Theta is signed: the axis/angle pair can come back as
	// (axis, +θ) or (-axis, -θ) depending on the rotation. Use the magnitude so a
	// negative angle doesn't collapse numSteps to 1 (max(1, round(negative)) == 1),
	// which would degenerate the pivot into a single straight-to-goal waypoint.
	totalRadians := math.Abs(diff.AxisAngles().Theta)
	totalDegrees := totalRadians * 180.0 / math.Pi

	numSteps := max(1, int(math.Round(totalDegrees/degreesPerStep)))

	logger.Infof("pivot rotation: %.1f° total (%d steps at %.1f°/step)", totalDegrees, numSteps, degreesPerStep)

	poses := make([]spatialmath.Pose, numSteps+1)
	for i := 0; i <= numSteps; i++ {
		t := float64(i) / float64(numSteps)
		poses[i] = spatialmath.Interpolate(startPose, endPose, t)
	}
	return poses
}
