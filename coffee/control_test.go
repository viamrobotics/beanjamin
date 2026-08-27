package coffee

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/testutils/inject"
)

// TestCancelStopsAndNothingElse pins the contract that cancel is a stop, not a
// recovery: it halts the arm, pauses the queue and announces, while leaving the
// portafilter state flags and the cached frame system exactly as the
// interruption found them so a later rewind can act on the real world.
func TestCancelStopsAndNothingElse(t *testing.T) {
	s, speech := newTestCoffee(t, nil)

	var stops atomic.Int32
	a := inject.NewArm("arm")
	a.StopFunc = func(context.Context, map[string]any) error {
		stops.Add(1)
		return nil
	}
	s.arm = a

	// Stand in for an in-flight order: the portafilter is locked in the machine
	// with grounds in it, and a sequence is running.
	s.portafilterInMachine.Store(true)
	s.portafilterHasGrounds.Store(true)
	s.setStep(stepBrewing)
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())
	s.running.Store(true)

	// Play the sequence goroutine: unwind once the shared context is cancelled.
	// Capture it first, since signalCancel swaps in a fresh one.
	seqCtx := s.cancelCtx
	go func() {
		<-seqCtx.Done()
		s.running.Store(false)
	}()

	resp, err := s.cancel(context.Background())
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}

	if resp["cancelled"] != true || resp["queue"] != "paused" {
		t.Errorf("resp = %v, want cancelled=true queue=paused", resp)
	}
	if got := stops.Load(); got != 1 {
		t.Errorf("arm.Stop called %d times, want 1", got)
	}
	if !s.paused.Load() {
		t.Error("queue should be paused after cancel")
	}
	if !s.portafilterInMachine.Load() || !s.portafilterHasGrounds.Load() {
		t.Error("cancel must not clear the portafilter state flags — rewind reads them")
	}
	if said := speech.calls(); len(said) != 1 || said[0] != cancelAnnouncement {
		t.Errorf("speech = %v, want one cancelAnnouncement", said)
	}
	if step, _ := s.currentStep.Load().(string); step != "" {
		t.Errorf("current_step = %q, want cleared", step)
	}
}

// TestCancelIdleIsSilent covers the no-op cancel: nothing is running, so there
// is nothing to stop, nothing to say, and the arm is never touched.
func TestCancelIdleIsSilent(t *testing.T) {
	s, speech := newTestCoffee(t, nil)

	a := inject.NewArm("arm")
	a.StopFunc = func(context.Context, map[string]any) error {
		t.Error("cancel must not stop the arm when nothing is running")
		return nil
	}
	s.arm = a
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())

	resp, err := s.cancel(context.Background())
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	if resp["cancelled"] != false || resp["queue"] != "running" {
		t.Errorf("resp = %v, want cancelled=false queue=running", resp)
	}
	if s.paused.Load() {
		t.Error("an idle cancel must not pause the queue")
	}
	if said := speech.calls(); len(said) != 0 {
		t.Errorf("speech = %v, want silence", said)
	}
}

// TestRewindRefusesWhileAnotherSequenceRuns covers the ownership gate: a
// sequence that ignores its cancelled context keeps rewind out of the arm
// rather than letting two callers plan motion at once.
func TestRewindRefusesWhileAnotherSequenceRuns(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())
	s.running.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.rewind(ctx); err == nil {
		t.Fatal("rewind should fail while a sequence is still running")
	}
}

// TestRewindIsDispatched covers the DoCommand wiring for the rewind key.
func TestRewindIsDispatched(t *testing.T) {
	cmd := map[string]any{"rewind": true}
	for _, def := range coffeeCommands {
		if def.key == "rewind" {
			if !def.matches(cmd) {
				t.Fatal("rewind command should match {\"rewind\": true}")
			}
			return
		}
	}
	t.Fatal("no rewind entry in the DoCommand dispatch table")
}

// pausedCoffeeWithDirtyWorld returns a service parked where a cancelled order
// leaves it: the queue paused, and a cached frame system carrying mid-cycle
// mutations (a held item, a locked filter frame, a staged glass). The frame
// system it returns is the dirty one, so a caller can tell a rebuild from a
// no-op, and the counter reports how many times the injected framesystem
// service was asked for a config. cfgErr, when set, makes every rebuild fail.
func pausedCoffeeWithDirtyWorld(t *testing.T, cfgErr error) (*beanjaminCoffee, *referenceframe.FrameSystem, *int) {
	t.Helper()
	s, _ := newTestCoffee(t, nil)

	rebuilds := 0
	fsSvc := inject.NewFrameSystemService("fs")
	fsSvc.FrameSystemConfigFunc = func(context.Context) (*framesystem.Config, error) {
		rebuilds++
		if cfgErr != nil {
			return nil, cfgErr
		}
		return &framesystem.Config{}, nil
	}
	s.fsSvc = fsSvc

	dirty := referenceframe.NewEmptyFrameSystem("dirty")
	s.cachedFS = dirty
	s.heldItemAttached = true
	s.filterFrameLocked = true
	s.stagedGlassPlaced = true
	s.paused.Store(true)

	return s, dirty, &rebuilds
}

// TestProceedRebuildsFrameSystemWhenPaused pins the reset half of proceed:
// resuming from a cancel-induced pause rebuilds the cached frame system from
// the service, so the next order plans against the configured world instead of
// the mutations the cancelled order left behind.
func TestProceedRebuildsFrameSystemWhenPaused(t *testing.T) {
	s, dirty, rebuilds := pausedCoffeeWithDirtyWorld(t, nil)

	resp, err := s.proceedQueue(context.Background())
	if err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	if resp["status"] != "resumed" || resp["frame_system_reset"] != true {
		t.Errorf("resp = %v, want status=resumed frame_system_reset=true", resp)
	}
	if *rebuilds != 1 {
		t.Errorf("frame system rebuilt %d times, want 1", *rebuilds)
	}
	if s.cachedFS == dirty {
		t.Error("cached frame system should have been replaced by the rebuild")
	}
	if s.heldItemAttached || s.filterFrameLocked || s.stagedGlassPlaced {
		t.Errorf("rebuild must clear the mutation flags: held=%v locked=%v staged=%v",
			s.heldItemAttached, s.filterFrameLocked, s.stagedGlassPlaced)
	}
	if s.running.Load() {
		t.Error("proceed must hand the arm back after rebuilding")
	}
	select {
	case <-s.queue.proceed:
	default:
		t.Error("proceed signal should have been sent to unpause the queue")
	}
}

// TestProceedRefusesWhileASequenceRuns covers the ownership gate: a cancelled
// sequence that has not unwound yet is still planning against cachedFS, so
// proceed must not swap it out from under it.
func TestProceedRefusesWhileASequenceRuns(t *testing.T) {
	s, dirty, rebuilds := pausedCoffeeWithDirtyWorld(t, nil)
	s.running.Store(true)

	if _, err := s.proceedQueue(context.Background()); err == nil {
		t.Fatal("proceed should refuse while a sequence is still running")
	}
	if *rebuilds != 0 {
		t.Errorf("frame system rebuilt %d times, want 0", *rebuilds)
	}
	if s.cachedFS != dirty {
		t.Error("a refused proceed must leave the cached frame system alone")
	}
	select {
	case <-s.queue.proceed:
		t.Error("a refused proceed must not unpause the queue")
	default:
	}
}

// TestProceedRebuildFailureKeepsQueuePaused: when the world can't be rebuilt the
// queue stays paused rather than starting the next order against a frame system
// that no longer matches the machine.
func TestProceedRebuildFailureKeepsQueuePaused(t *testing.T) {
	s, _, _ := pausedCoffeeWithDirtyWorld(t, errors.New("boom"))

	if _, err := s.proceedQueue(context.Background()); err == nil {
		t.Fatal("proceed should fail when the frame system can't be rebuilt")
	}
	if s.running.Load() {
		t.Error("a failed rebuild must still hand the arm back")
	}
	if !s.paused.Load() {
		t.Error("queue should still be paused after a failed rebuild")
	}
	select {
	case <-s.queue.proceed:
		t.Error("a failed rebuild must not unpause the queue")
	default:
	}
}
