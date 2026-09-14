package coffee

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/spatialmath"
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

// coffeeWithDirtyWorld returns a service whose cached frame system carries the
// mid-cycle mutations an interrupted order leaves behind (a held item, a locked
// filter frame, a staged glass), with the queue running. The frame system it
// returns is the dirty one, so a caller can tell a rebuild from a no-op, and
// the counter reports how many times the injected framesystem service was asked
// for a config. cfgErr, when set, makes every rebuild fail.
func coffeeWithDirtyWorld(t *testing.T, cfgErr error) (*beanjaminCoffee, *referenceframe.FrameSystem, *int) {
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

	return s, dirty, &rebuilds
}

// pausedCoffeeWithDirtyWorld is coffeeWithDirtyWorld parked where a cancelled
// order leaves it, with the queue paused as well.
func pausedCoffeeWithDirtyWorld(t *testing.T, cfgErr error) (*beanjaminCoffee, *referenceframe.FrameSystem, *int) {
	t.Helper()
	s, dirty, rebuilds := coffeeWithDirtyWorld(t, cfgErr)
	s.paused.Store(true)
	return s, dirty, rebuilds
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

// TestProceedRebuildsFrameSystemWhenNotPaused is the order-failed-on-its-own
// case: nothing cancelled, so the queue never paused, but the failed order still
// left its mutations in cachedFS and refreshFrameSystemIfClean won't clear them
// for the next order. proceed has to rebuild anyway.
func TestProceedRebuildsFrameSystemWhenNotPaused(t *testing.T) {
	s, dirty, rebuilds := coffeeWithDirtyWorld(t, nil)

	resp, err := s.proceedQueue(context.Background())
	if err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	if resp["frame_system_reset"] != true {
		t.Errorf("frame_system_reset = %v, want true even when the queue is not paused", resp["frame_system_reset"])
	}
	if resp["resumed"] != false {
		t.Errorf("resumed = %v, want false — there was no pause to release", resp["resumed"])
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
}

// TestProceedOnRunningQueueParksNoSignal pins the other half of the unpaused
// case: the resume signal must not be sent when nothing is waiting for it. A
// token left in the cap-1 buffer would be consumed by the next cancel-induced
// pause the instant it arrived, resuming the queue without an operator asking.
func TestProceedOnRunningQueueParksNoSignal(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, nil)

	if _, err := s.proceedQueue(context.Background()); err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	select {
	case <-s.queue.proceed:
		t.Error("proceed must not park a resume signal while the queue is running")
	default:
	}
}

// TestProceedTwiceResumesOnce: the first proceed claims the pause, the second
// finds none left. Only one of them may report resumed, or an operator
// double-clicking would release a pause that a cancel between the two clicks
// had just taken.
func TestProceedTwiceResumesOnce(t *testing.T) {
	s, _, _ := pausedCoffeeWithDirtyWorld(t, nil)
	ctx := context.Background()

	first, err := s.proceedQueue(ctx)
	if err != nil {
		t.Fatalf("first proceed: unexpected error %v", err)
	}
	if first["resumed"] != true {
		t.Errorf("first proceed resumed = %v, want true", first["resumed"])
	}
	second, err := s.proceedQueue(ctx)
	if err != nil {
		t.Fatalf("second proceed: unexpected error %v", err)
	}
	if second["resumed"] != false {
		t.Errorf("second proceed resumed = %v, want false — the first one took the pause", second["resumed"])
	}
}

// TestProceedClearsThePauseItself is the regression test for a queue that
// silently stopped making drinks. The paused flag used to be cleared only by
// the queue goroutine, so proceed reported success while the flag stayed set:
// Status kept claiming the queue was paused, the keepalive loop kept declining
// to purge, and the resume signal sat in the buffer waiting to release a pause
// nobody had asked to release.
func TestProceedClearsThePauseItself(t *testing.T) {
	s, _, _ := pausedCoffeeWithDirtyWorld(t, nil)

	if _, err := s.proceedQueue(context.Background()); err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	if s.paused.Load() {
		t.Error("proceed must clear the paused flag, not leave it for the queue goroutine")
	}
}

// TestCancelledManualActionPauseIsReleasable covers the pause nobody is waiting
// on: cancelling an execute_action or a keepalive purge pauses the queue while
// processQueue sits idle between orders, so no consumer is parked to take the
// resume signal. proceed still has to be able to release it.
func TestCancelledManualActionPauseIsReleasable(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, nil)
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())

	// A manual action holds the arm; the operator cancels it, then it unwinds.
	s.running.Store(true)
	if !s.signalCancel() {
		t.Fatal("signalCancel should report the running sequence")
	}
	s.running.Store(false)

	resp, err := s.proceedQueue(context.Background())
	if err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	if resp["resumed"] != true {
		t.Errorf("resumed = %v, want true — the cancel did pause the queue", resp["resumed"])
	}
	if s.paused.Load() {
		t.Error("the queue must not stay paused after proceed released it")
	}
}

// TestWaitForProceedHoldsTheQueueUntilProceed pins the consumer side: the flag
// stays set for the whole wait, so a proceed arriving at any moment sees a
// paused queue and grants the resume. Clearing it on the way in (the old
// Swap(false) + Store(true)) opened a window where a concurrent proceed read an
// unpaused queue, declined to signal, and parked this goroutine for good.
func TestWaitForProceedHoldsTheQueueUntilProceed(t *testing.T) {
	s, _, _ := pausedCoffeeWithDirtyWorld(t, nil)
	s.queueStop = make(chan struct{})
	t.Cleanup(func() { close(s.queueStop) })

	resumed := make(chan bool, 1)
	go func() { resumed <- s.waitForProceed() }()

	// The consumer is parked; from a concurrent proceed's point of view the
	// queue must still read paused.
	select {
	case <-resumed:
		t.Fatal("waitForProceed returned while the queue was still paused")
	case <-time.After(50 * time.Millisecond):
	}
	if !s.paused.Load() {
		t.Fatal("paused must stay set while a consumer waits — a proceed reading false would never signal")
	}

	if _, err := s.proceedQueue(context.Background()); err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	select {
	case ok := <-resumed:
		if !ok {
			t.Error("waitForProceed reported shutdown, want a resume")
		}
	case <-time.After(time.Second):
		t.Fatal("waitForProceed never woke up after proceed")
	}
}

// TestWaitForProceedIgnoresAStaleSignal: a resume signal can be parked with no
// consumer waiting (proceed after a cancelled manual action). It must not
// release the next pause on arrival — that pause is a fresh cancel, and an
// operator has to ask for that one too.
func TestWaitForProceedIgnoresAStaleSignal(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, nil)
	s.queueStop = make(chan struct{})
	s.queue.proceed <- struct{}{} // parked by an earlier proceed

	s.paused.Store(true)
	resumed := make(chan bool, 1)
	go func() { resumed <- s.waitForProceed() }()

	select {
	case <-resumed:
		t.Fatal("a stale resume signal must not release a later pause")
	case <-time.After(50 * time.Millisecond):
	}

	close(s.queueStop)
	if ok := <-resumed; ok {
		t.Error("waitForProceed should report shutdown once queueStop closes")
	}
}

// coffeeWithFridge is coffeeWithDirtyWorld on a machine that has a fridge, so a
// rebuild has a real door frame to land at its authored shut transform — the
// only way to tell a cleared angle from one the rebuild put straight back.
// Returns the authored shut pose of the door origin frame to compare against.
func coffeeWithFridge(t *testing.T) (*beanjaminCoffee, spatialmath.Pose) {
	t.Helper()
	s, _, _ := coffeeWithDirtyWorld(t, nil)

	doorLink := referenceframe.NewLinkInFrame(referenceframe.World,
		spatialmath.NewPoseFromPoint(r3.Vector{X: 500}), frameFridgeDoor, nil)
	fsSvc := inject.NewFrameSystemService("fs")
	fsSvc.FrameSystemConfigFunc = func(context.Context) (*framesystem.Config, error) {
		return &framesystem.Config{Parts: []*referenceframe.FrameSystemPart{{FrameConfig: doorLink}}}, nil
	}
	s.fsSvc = fsSvc

	shut, err := framesystem.NewFromService(context.Background(), fsSvc, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := doorBasePose(shut)
	if err != nil {
		t.Fatal(err)
	}
	return s, base
}

// doorOriginPose reads the door origin frame's current transform out of the
// service's cached frame system.
func doorOriginPose(t *testing.T, s *beanjaminCoffee) spatialmath.Pose {
	t.Helper()
	pose, err := doorBasePose(s.cachedFS)
	if err != nil {
		t.Fatal(err)
	}
	return pose
}

// TestProceedClearsTheFridgeDoorAngle: proceed is the operator saying the
// machine has been put right, fridge included, so the recorded swing goes with
// every other mid-cycle mutation and the rebuilt world has a shut door. The
// angle must be cleared BEFORE the rebuild — cleared after, resetFrameSystem
// would have already re-applied the swing to the frame system it just built.
func TestProceedClearsTheFridgeDoorAngle(t *testing.T) {
	s, shut := coffeeWithFridge(t)
	s.doorOpenDegs = 90

	resp, err := s.proceedQueue(context.Background())
	if err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	if got, _ := resp["fridge_door_cleared_degs"].(float64); got != 90 {
		t.Errorf("fridge_door_cleared_degs = %v, want 90", resp["fridge_door_cleared_degs"])
	}
	if s.doorOpenDegs != 0 {
		t.Errorf("doorOpenDegs = %v, want 0 — proceed asserts the door is shut", s.doorOpenDegs)
	}
	if got := doorOriginPose(t, s); !spatialmath.PoseAlmostEqual(got, shut) {
		t.Errorf("rebuilt door origin at %v, want the authored shut transform %v", got, shut)
	}
}

// TestProceedRebuildFailureKeepsTheFridgeDoorAngle: a proceed that cannot
// rebuild asserts nothing. cachedFS still holds the swung door, so the recorded
// angle has to stay with it — dropped here, the next rebuild would quietly shut
// a door this proceed never got to vouch for.
func TestProceedRebuildFailureKeepsTheFridgeDoorAngle(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, errors.New("boom"))
	s.doorOpenDegs = 90

	if _, err := s.proceedQueue(context.Background()); err == nil {
		t.Fatal("proceed should fail when the frame system can't be rebuilt")
	}
	if s.doorOpenDegs != 90 {
		t.Errorf("doorOpenDegs = %v, want 90 — a failed proceed must not forget the door", s.doorOpenDegs)
	}
}

// TestProceedOmitsTheFridgeFieldWhenShut keeps the field's presence meaningful:
// it flags the assertion proceed just made about a door that was standing open,
// so a door already shut must not report one.
func TestProceedOmitsTheFridgeFieldWhenShut(t *testing.T) {
	s, _ := coffeeWithFridge(t)

	resp, err := s.proceedQueue(context.Background())
	if err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	if _, ok := resp["fridge_door_cleared_degs"]; ok {
		t.Errorf("fridge_door_cleared_degs = %v, want the field omitted for a door already shut", resp["fridge_door_cleared_degs"])
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
