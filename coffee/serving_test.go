package coffee

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/testutils/inject"
)

// dropTestRig is a coffee service wired with inject fakes that record every arm
// read and gripper call tryDropCupInSlot makes. The arm fake answers
// CurrentInputs — the first thing every move does — the way a real client does
// with a cancelled context, and otherwise fails the move with errArmReached so
// the test never has to plan a real motion.
type dropTestRig struct {
	s            *beanjaminCoffee
	armReads     atomic.Int32
	gripperCalls atomic.Int32
}

var errArmReached = errors.New("arm read with a live context")

func newDropTestRig(t *testing.T, onArmRead func(ctx context.Context)) *dropTestRig {
	t.Helper()
	s, _ := newTestCoffee(t, &Config{
		ArmName:                     "arm",
		ServingGrabRelativePose:     &RelativePose{OZ: -1},
		ServingApproachRelativePose: &RelativePose{Z: 100, OZ: -1},
	})
	s.cachedFS = referenceframe.NewEmptyFrameSystem("test")
	rig := &dropTestRig{s: s}

	a := inject.NewArm("arm")
	a.CurrentInputsFunc = func(ctx context.Context) ([]referenceframe.Input, error) {
		rig.armReads.Add(1)
		if onArmRead != nil {
			onArmRead(ctx)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errArmReached
	}
	s.arm = a

	g := inject.NewGripper("gripper")
	g.OpenFunc = func(context.Context, map[string]any) error {
		rig.gripperCalls.Add(1)
		return nil
	}
	g.GrabFunc = func(context.Context, map[string]any) (bool, error) {
		rig.gripperCalls.Add(1)
		return true, nil
	}
	s.gripper = g
	return rig
}

func TestTryDropCupInSlotHonorsCancelCtx(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents heldContents
	}{
		{name: "filled cup, level carry", contents: heldFilled},
		{name: "empty cup, free plan", contents: heldEmpty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("cancelled before the drop", func(t *testing.T) {
				rig := newDropTestRig(t, nil)
				cancelCtx, cancel := context.WithCancel(context.Background())
				cancel()

				err := rig.s.tryDropCupInSlot(context.Background(), cancelCtx, r3.Vector{X: 300, Y: 200}, 50, tc.contents)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
				if got := rig.armReads.Load(); got != 1 {
					t.Errorf("arm reads = %d, want 1 (the aborted approach)", got)
				}
				if got := rig.gripperCalls.Load(); got != 0 {
					t.Errorf("gripper calls = %d, want 0", got)
				}
			})

			t.Run("cancelled while the approach is in flight", func(t *testing.T) {
				cancelCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				rig := newDropTestRig(t, func(ctx context.Context) {
					// The operator cancels mid-approach; the move's context must
					// follow, as it would during planning or a gripperPause.
					cancel()
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
						t.Error("move context not cancelled after cancelCtx was")
					}
				})

				err := rig.s.tryDropCupInSlot(context.Background(), cancelCtx, r3.Vector{X: 300, Y: 200}, 50, tc.contents)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
				if got := rig.armReads.Load(); got != 1 {
					t.Errorf("arm reads = %d, want 1 (no descent or retreat after cancel)", got)
				}
				if got := rig.gripperCalls.Load(); got != 0 {
					t.Errorf("gripper calls = %d, want 0", got)
				}
			})
		})
	}
}
