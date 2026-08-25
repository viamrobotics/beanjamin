package coffee

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

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
