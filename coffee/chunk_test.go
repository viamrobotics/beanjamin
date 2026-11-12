package coffee

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"beanjamin/coffee/order"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/arm/kinematics"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/testutils/inject"
)

// Moves handed off inside a chunk run only once it closes, in order.
func TestChunkDefersMovesUntilItCloses(t *testing.T) {
	s := sceneCoffee(t)
	end := s.startRun(context.Background(), context.Background(), "order", true)
	var r recorder
	if err := s.inChunk("test", func() error {
		_ = s.handOff(context.Background(), r.move("a"))
		_ = s.handOff(context.Background(), r.move("b"))
		if got := r.got(); len(got) != 0 {
			t.Errorf("ran %v while the chunk was open, want nothing", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("ran %v, want a then b", got)
	}
}

// A chunk whose recording fails hands nothing off, and the planned scene goes
// back to what the work before it leaves.
func TestFailedChunkDropsWhatItRecorded(t *testing.T) {
	s := sceneCoffee(t)
	end := s.startRun(context.Background(), context.Background(), "order", true)
	var r recorder
	boom := errors.New("boom")
	if err := s.inChunk("test", func() error {
		_ = s.handOff(context.Background(), r.move("a"))
		setMilk(t, s, "oat")
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("inChunk = %v, want boom", err)
	}
	if got := s.scene().heldMilk; got != "" {
		t.Errorf("planned heldMilk = %q, want the recorded edit gone", got)
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); len(got) != 0 {
		t.Errorf("ran %v, want nothing", got)
	}
}

// A scene edit recorded after a move reaches the executed scene once that move
// has run, not before.
func TestChunkSceneEditFollowsTheMovesBeforeIt(t *testing.T) {
	s := sceneCoffee(t)
	end := s.startRun(context.Background(), context.Background(), "order", true)
	release := make(chan struct{})
	if err := s.inChunk("test", func() error {
		_ = s.handOff(context.Background(), blocking(release))
		setMilk(t, s, "oat")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.run.executed.Load().heldMilk; got != "" {
		t.Errorf("executed heldMilk = %q while the move before it runs, want empty", got)
	}
	close(release)
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if got := s.settledScene.heldMilk; got != "oat" {
		t.Errorf("settled heldMilk = %q, want oat", got)
	}
}

// fakeArm is an arm that goes wherever it is told and records each trajectory.
type fakeArm struct {
	mu    sync.Mutex
	at    []referenceframe.Input
	moves [][][]referenceframe.Input
}

func (f *fakeArm) inject() *inject.Arm {
	return &inject.Arm{
		CurrentInputsFunc: func(context.Context) ([]referenceframe.Input, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return slices.Clone(f.at), nil
		},
		MoveThroughJointPositionsFunc: func(
			_ context.Context, positions [][]referenceframe.Input, _ *arm.MoveOptions, _ map[string]any,
		) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.moves = append(f.moves, positions)
			f.at = positions[len(positions)-1]
			return nil
		},
	}
}

// Joint configurations of the xArm6 the chunk tests start at and move between.
var (
	chunkArmStart = []referenceframe.Input{0, -0.4, -0.6, 0, 1.0, 0}
	chunkArmA     = []referenceframe.Input{0.3, -0.4, -0.6, 0, 1.0, 0}
	chunkArmB     = []referenceframe.Input{0.6, -0.3, -0.7, 0, 1.0, 0}
)

// chunkCoffee returns a service whose arm is an xArm6 with the claws on its end,
// inside an order run. Its claws switch serves "a" and "b", the claws with the
// arm at chunkArmA and chunkArmB, and "in_place", the claws where they stand.
func chunkCoffee(t *testing.T) (*beanjaminCoffee, *fakeArm, func() error) {
	t.Helper()
	model, err := kinematics.ModelFromName(kinematics.XArm6, "arm")
	if err != nil {
		t.Fatal(err)
	}
	fs := referenceframe.NewEmptyFrameSystem("test")
	if err := fs.AddFrame(model, fs.World()); err != nil {
		t.Fatal(err)
	}
	claws, err := referenceframe.NewStaticFrame(componentClaws, spatialmath.NewZeroPose())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.AddFrame(claws, model); err != nil {
		t.Fatal(err)
	}
	addGripPoint(t, fs)

	s := heldGeomService(t, fs)
	s.cfg.ArmName = "arm"
	f := &fakeArm{at: chunkArmStart}
	s.arm = f.inject()
	poses := map[string]spatialmath.Pose{
		"a": clawsPose(t, fs, chunkArmA),
		"b": clawsPose(t, fs, chunkArmB),
	}
	sw := inject.NewSwitch("claws")
	sw.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
		name, _ := cmd["get_pose_by_name"].(string)
		if name == "in_place" {
			return map[string]any{"o_z": 1.0, "reference_frame": componentClaws, "component_name": componentClaws}, nil
		}
		pose, ok := poses[name]
		if !ok {
			return nil, fmt.Errorf("no pose %q", name)
		}
		pt, ov := pose.Point(), pose.Orientation().OrientationVectorDegrees()
		return map[string]any{
			"x": pt.X, "y": pt.Y, "z": pt.Z, "o_x": ov.OX, "o_y": ov.OY, "o_z": ov.OZ, "theta": ov.Theta,
			"component_name": componentClaws,
		}, nil
	}
	s.clawsSw = sw
	end := s.startRun(context.Background(), context.Background(), "order", true)
	return s, f, end
}

// clawsPose is the claws' world pose with the arm at joints.
func clawsPose(t *testing.T, fs *referenceframe.FrameSystem, joints []referenceframe.Input) spatialmath.Pose {
	t.Helper()
	pose, err := toWorld(fs, referenceframe.FrameSystemInputs{"arm": joints}, componentClaws, spatialmath.NewZeroPose())
	if err != nil {
		t.Fatal(err)
	}
	return pose
}

// requireClawsAt fails unless the arm at joints puts the claws within 1 mm of
// where want does.
func requireClawsAt(t *testing.T, s *beanjaminCoffee, joints, want []referenceframe.Input) {
	t.Helper()
	fs := s.scene().fs
	requireVecEqual(t, clawsPose(t, fs, joints).Point(), clawsPose(t, fs, want).Point(), 1)
}

// recordStepNamed runs the step to the claws pose name.
func recordStepNamed(t *testing.T, s *beanjaminCoffee, name string) {
	t.Helper()
	if err := s.executeStep(context.Background(), context.Background(), Step{PoseName: name, PoseSwitch: s.clawsSw}); err != nil {
		t.Fatal(err)
	}
}

// A chunk's motions are planned together and run back to back, each starting
// where the one before it ends.
func TestChunkPlansItsMotionsAsOneSequence(t *testing.T) {
	s, f, end := chunkCoffee(t)
	if err := s.inChunk("test", func() error {
		recordStepNamed(t, s, "a")
		recordStepNamed(t, s, "b")
		if len(f.moves) != 0 {
			t.Errorf("ran %d trajectories while the chunk was open, want none", len(f.moves))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if len(f.moves) != 2 {
		t.Fatalf("ran %d trajectories, want 2", len(f.moves))
	}
	first, second := f.moves[0], f.moves[1]
	requireClawsAt(t, s, first[len(first)-1], chunkArmA)
	requireClawsAt(t, s, second[len(second)-1], chunkArmB)
}

// Asking where the arm will be plans and hands off what the chunk recorded, and
// answers with where that leaves the arm.
func TestExpectedInputsFlushesTheChunk(t *testing.T) {
	s, _, end := chunkCoffee(t)
	defer func() { _ = end() }()
	if err := s.inChunk("test", func() error {
		recordStepNamed(t, s, "a")
		inputs, err := s.expectedInputs(context.Background())
		if err != nil {
			return err
		}
		requireClawsAt(t, s, inputs["arm"], chunkArmA)
		if s.run.chunk.hasMotions() {
			t.Error("the chunk still holds the motion after expectedInputs")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A pivot recorded after a motion starts where that motion's goal puts the
// pivoting frame, since the arm is left out of the start until it is planned.
func TestPivotStartsAtTheRecordedGoal(t *testing.T) {
	s, _, end := chunkCoffee(t)
	defer func() { _ = end() }()
	if err := s.inChunk("test", func() error {
		recordStepNamed(t, s, "a")
		pd := &poseData{refFrame: referenceframe.World, componentName: componentClaws}
		got, err := s.pivotStartPose(s.scene().fs, referenceframe.FrameSystemInputs{}, pd)
		if err != nil {
			return err
		}
		requireVecEqual(t, got.Point(), clawsPose(t, s.scene().fs, chunkArmA).Point(), 1e-6)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A goal no motion can reach fails the chunk as a planning error, with nothing
// handed off, and saves every request of the chunk as one sequence.
func TestUnplannableChunkRunsNothing(t *testing.T) {
	s, f, end := chunkCoffee(t)
	saved := t.TempDir()
	s.cfg.SaveMotionRequestsDir = saved
	s.queue = order.NewQueue()
	unreachable := &poseData{
		pose: spatialmath.NewPoseFromPoint(r3.Vector{X: 5000}), refFrame: referenceframe.World, componentName: componentClaws,
	}
	err := s.inChunk("test", func() error {
		recordStepNamed(t, s, "a")
		m, err := s.rawMotion("away", unreachable, nil, 0, nil, nil, referenceframe.FrameSystemInputs{})
		if err != nil {
			return err
		}
		s.run.chunk.add(&chunkOp{motion: m}, s.run.planned)
		return nil
	})
	if !errors.Is(err, errMotionPlanning) {
		t.Fatalf("inChunk = %v, want a planning error", err)
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if len(f.moves) != 0 {
		t.Errorf("ran %d trajectories, want none", len(f.moves))
	}

	var names []string
	if err := filepath.WalkDir(saved, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if !strings.Contains(path, "tag="+tagPlanningFailure) {
				t.Errorf("%s is not tagged as a planning failure", path)
			}
			names = append(names, d.Name())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	if len(names) != 2 {
		t.Fatalf("saved %v, want both requests of the chunk", names)
	}
	stamp := names[0][:len(planStampLayout)]
	for i, name := range names {
		if want := fmt.Sprintf("%s_chunk_test_%dof2_", stamp, i+1); !strings.HasPrefix(name, want) {
			t.Errorf("saved %q, want it named %q…", name, want)
		}
	}
}

// A chunk's files share the time it started planning and sort in the order of
// its motions.
func TestChunkPlanFileName(t *testing.T) {
	began := time.Date(2026, 11, 12, 18, 31, 52, 82_000_000, time.UTC)
	if got, want := chunkPlanFileName(began, "Fetch, pour and return milk", 4, 9, "move"),
		"20261112_183152.082_chunk_fetch_pour_and_return_milk_5of9_move"; got != want {
		t.Errorf("chunkPlanFileName = %q, want %q", got, want)
	}
	if got, want := chunkPlanFileName(began, "prepare portafilter", 2, 12, "pivot"),
		"20261112_183152.082_chunk_prepare_portafilter_03of12_pivot"; got != want {
		t.Errorf("chunkPlanFileName = %q, want %q", got, want)
	}
}

// A step whose goal hangs off the arm cannot be built until the motions before
// it are planned, so recording it flushes them first.
func TestStepNeedingTheArmFlushesTheChunk(t *testing.T) {
	s, f, end := chunkCoffee(t)
	if err := s.inChunk("test", func() error {
		recordStepNamed(t, s, "a")
		recordStepNamed(t, s, "in_place")
		if got := s.run.chunk.ops; len(got) != 1 {
			t.Errorf("chunk holds %d ops, want only the step recorded after the flush", len(got))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if len(f.moves) != 2 {
		t.Fatalf("ran %d trajectories, want 2", len(f.moves))
	}
	requireClawsAt(t, s, f.at, chunkArmA)
}

// Outside a chunk, each step of an order — a circle too — is planned from where
// the step before it leaves the arm and handed off without waiting for the arm.
func TestOrderStepsOutsideAChunkPlanAhead(t *testing.T) {
	s, f, end := chunkCoffee(t)
	recordStepNamed(t, s, "a")
	circle := Step{PoseName: "b", PoseSwitch: s.clawsSw, CircularRadiusMm: 5, CircularPointsPerRev: 8}
	if err := s.executeStep(context.Background(), context.Background(), circle); err != nil {
		t.Fatal(err)
	}
	if s.run.moves == nil {
		t.Error("the circle settled instead of handing its move off")
	}
	if s.scene().inputs == nil {
		t.Error("the planned scene doesn't know where the circle leaves the arm")
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if len(f.moves) != 2 {
		t.Fatalf("ran %d trajectories, want 2", len(f.moves))
	}
}

// Inside a chunk, where the frames on the arm will be is read off the last
// recorded goal, without planning what the chunk holds.
func TestPlacementReadsTheRecordedGoal(t *testing.T) {
	s, _, end := chunkCoffee(t)
	defer func() { _ = end() }()
	if err := s.inChunk("test", func() error {
		recordStepNamed(t, s, "a")
		at, err := s.expectedPlacement(context.Background())
		if err != nil {
			return err
		}
		if !s.run.chunk.hasMotions() {
			t.Error("placing the arm planned the chunk")
		}
		fs := s.scene().fs
		got, err := at.worldPose(fs, gripPoint)
		if err != nil {
			return err
		}
		want, err := toWorld(fs, referenceframe.FrameSystemInputs{"arm": chunkArmA}, gripPoint, spatialmath.NewZeroPose())
		if err != nil {
			return err
		}
		requireVecEqual(t, got.Point(), want.Point(), 1e-6)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A held item attached inside a chunk is placed where the recorded grasp leaves
// the gripper, without planning the chunk.
func TestAttachingInAChunkKeepsItWhole(t *testing.T) {
	s, f, end := chunkCoffee(t)
	fs := s.scene().fs
	gripAtA, err := toWorld(fs, referenceframe.FrameSystemInputs{"arm": chunkArmA}, gripPoint, spatialmath.NewZeroPose())
	if err != nil {
		t.Fatal(err)
	}
	// Out along the tool axis, clear of the arm's last link.
	cupAt := spatialmath.Compose(gripAtA, spatialmath.NewPoseFromPoint(r3.Vector{Z: 100}))
	cup, err := spatialmath.NewBox(cupAt, r3.Vector{X: 10, Y: 10, Z: 10}, "cup")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.inChunk("test", func() error {
		recordStepNamed(t, s, "a")
		if err := s.attachDetectedGeometry(context.Background(), pickupLabelCup, cup); err != nil {
			return err
		}
		if len(f.moves) != 0 || !s.run.chunk.hasMotions() {
			t.Error("attaching the cup planned the chunk")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if len(f.moves) != 1 {
		t.Fatalf("ran %d trajectories, want 1", len(f.moves))
	}
	local := s.settledScene.cachedHeldGeometry(pickupLabelCup)
	if local == nil {
		t.Fatal("no cup geometry cached")
	}
	requireVecEqual(t, local.Pose().Point(), r3.Vector{Z: 100}, 1e-6)
}

// Joined motions run as one arm move.
func TestJoinedMotionsRunAsOneMove(t *testing.T) {
	s, f, end := chunkCoffee(t)
	fs := s.scene().fs
	ctx := context.Background()
	if err := s.inChunk("test", func() error {
		for i, joints := range [][]referenceframe.Input{chunkArmA, chunkArmB} {
			pd := &poseData{pose: clawsPose(t, fs, joints), refFrame: referenceframe.World, componentName: componentClaws}
			op := chunkOp{join: i == 0}
			if err := s.addMotion(ctx, ctx, "hop", op, func(start referenceframe.FrameSystemInputs) (*motionRequest, error) {
				return s.rawMotion("hop", pd, nil, 0, nil, nil, start)
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	if len(f.moves) != 1 {
		t.Fatalf("ran %d trajectories, want 1", len(f.moves))
	}
	requireClawsAt(t, s, f.at, chunkArmB)
}

// A run that doesn't plan ahead runs its chunk as the chunk flushes.
func TestDirectRunRunsItsChunkWhenItFlushes(t *testing.T) {
	s, f, endOrder := chunkCoffee(t)
	if err := endOrder(); err != nil {
		t.Fatal(err)
	}
	end := s.startDirectRun(context.Background(), context.Background(), "action")
	defer end()
	if err := s.inChunk("test", func() error {
		recordStepNamed(t, s, "a")
		recordStepNamed(t, s, "b")
		if len(f.moves) != 0 {
			t.Errorf("ran %d trajectories while the chunk was open, want none", len(f.moves))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.moves) != 2 {
		t.Fatalf("ran %d trajectories once the chunk closed, want 2", len(f.moves))
	}
	requireClawsAt(t, s, f.at, chunkArmB)
}

// A failed chunk is reported at the motion after the one planned again most
// often, or at the first unsolved motion when nothing was planned again.
func TestStuckMotion(t *testing.T) {
	meta := func(backtracks int) *armplanning.PlanMeta { return &armplanning.PlanMeta{BacktrackCounter: backtracks} }
	for _, tc := range []struct {
		name   string
		metas  []*armplanning.PlanMeta
		solved int
		want   int
	}{
		{name: "backtracked to the motion before", metas: []*armplanning.PlanMeta{meta(2), meta(1), meta(3), meta(9), meta(0), meta(0)}, solved: 2, want: 4},
		{name: "no backtracking", metas: []*armplanning.PlanMeta{meta(0), meta(0), nil}, solved: 1, want: 1},
		{name: "first motion failed", metas: []*armplanning.PlanMeta{meta(0), nil}, solved: 0, want: 0},
		{name: "every motion solved", metas: []*armplanning.PlanMeta{meta(0), meta(0)}, solved: 2, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stuckMotion(tc.metas, tc.solved); got != tc.want {
				t.Errorf("stuckMotion = %d, want %d", got, tc.want)
			}
		})
	}
}
