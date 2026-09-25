package coffee

import (
	"context"
	"testing"
	"time"
)

// gateTestCoffee is newTestCoffee wired for the queue consumer: a cancel
// context to snapshot and a queueStop the test closes on cleanup.
func gateTestCoffee(t *testing.T, cfg *Config) (*beanjaminCoffee, *fakeSpeech) {
	t.Helper()
	s, speech := newTestCoffee(t, cfg)
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())
	s.queueStop = make(chan struct{})
	return s, speech
}

type claimResult struct {
	cancelCtx context.Context
	ok        bool
}

func startClaim(s *beanjaminCoffee) <-chan claimResult {
	out := make(chan claimResult, 1)
	go func() {
		ctx, ok := s.claimArm()
		out <- claimResult{ctx, ok}
	}()
	return out
}

// assertStillWaiting fails if claimArm returns within a few poll intervals.
func assertStillWaiting(t *testing.T, claimed <-chan claimResult, why string) {
	t.Helper()
	select {
	case <-claimed:
		t.Fatalf("claimArm returned %s", why)
	case <-time.After(3 * armPollInterval):
	}
}

// TestClaimArmWaitsForAnotherSequence: an order that arrives while a purge or a
// manual action holds the arm must wait for it, not start and fail at the gate.
// The order stays pending throughout, and the gate passes to the queue as soon
// as the other holder lets go.
func TestClaimArmWaitsForAnotherSequence(t *testing.T) {
	s, _ := gateTestCoffee(t, nil)
	t.Cleanup(func() { close(s.queueStop) })
	s.queue.Enqueue(Order{ID: "o1", Drink: "espresso", CustomerName: "Ada"})

	s.running.Store(true) // a keepalive purge, say
	claimed := startClaim(s)
	assertStillWaiting(t, claimed, "while another sequence held the running gate")

	if _, ok := s.queue.Current(); ok {
		t.Error("the waiting order must not be started while the arm is busy")
	}
	if head, ok := s.queue.Peek(); !ok || head.ID != "o1" {
		t.Errorf("Peek = %+v, %v; the order should still be pending", head, ok)
	}

	s.running.Store(false)
	select {
	case r := <-claimed:
		if !r.ok {
			t.Fatal("claimArm reported shutdown, want the gate")
		}
		if r.cancelCtx != s.cancelCtx {
			t.Error("claimArm must hand back the cancel context current at the claim")
		}
	case <-time.After(time.Second):
		t.Fatal("claimArm never took the gate after it was released")
	}
	if !s.running.Load() {
		t.Error("claimArm must leave the queue holding the running gate")
	}
}

// TestClaimArmStopsOnShutdownWhileWaiting: a consumer waiting on a busy arm has
// to exit when the service closes, without touching a gate it never got.
func TestClaimArmStopsOnShutdownWhileWaiting(t *testing.T) {
	s, _ := gateTestCoffee(t, nil)
	s.running.Store(true)

	claimed := startClaim(s)
	assertStillWaiting(t, claimed, "while another sequence held the running gate")

	close(s.queueStop)
	select {
	case r := <-claimed:
		if r.ok {
			t.Error("claimArm reported the gate, want shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("claimArm did not exit after queueStop closed")
	}
	if !s.running.Load() {
		t.Error("claimArm must not release a gate held by someone else")
	}
}

// TestClaimArmRespectsThePause: a free arm is not enough while a cancel has the
// queue paused. The gate must stay untouched until the operator proceeds, so
// proceed and manual actions can still take it in the meantime.
func TestClaimArmRespectsThePause(t *testing.T) {
	s, _ := gateTestCoffee(t, nil)
	t.Cleanup(func() { close(s.queueStop) })
	s.paused.Store(true)

	claimed := startClaim(s)
	assertStillWaiting(t, claimed, "while the queue was paused")
	if s.running.Load() {
		t.Fatal("claimArm must not hold the gate while the queue is paused")
	}

	// What proceedQueue does once it has rebuilt the world.
	s.paused.Store(false)
	s.queue.proceed <- struct{}{}
	select {
	case r := <-claimed:
		if !r.ok {
			t.Error("claimArm reported shutdown, want the gate")
		}
	case <-time.After(time.Second):
		t.Fatal("claimArm never resumed after proceed")
	}
}

// TestClaimArmHoldsBackAfterACancel: cancelling the sequence an order is
// waiting behind pauses the queue, so the order must not start when it unwinds.
func TestClaimArmHoldsBackAfterACancel(t *testing.T) {
	s, _ := gateTestCoffee(t, nil)
	t.Cleanup(func() { close(s.queueStop) })
	// A manual action holds the arm while the order waits; the operator
	// cancels it and it unwinds.
	s.running.Store(true)
	claimed := startClaim(s)
	if !s.signalCancel() {
		t.Fatal("signalCancel should see the held gate")
	}
	s.running.Store(false)

	assertStillWaiting(t, claimed, "while the cancel's pause was in force")
	if s.running.Load() {
		t.Error("claimArm must hand the gate back while the queue is paused")
	}
}

// TestRunPurgeYieldsToAQueuedOrder: an order that lands between the keepalive
// tick's check and the purge's claim goes first — the purge says nothing, moves
// nothing, and hands the gate straight back.
func TestRunPurgeYieldsToAQueuedOrder(t *testing.T) {
	s, speech := gateTestCoffee(t, nil)
	t.Cleanup(func() { close(s.queueStop) })
	s.queue.Enqueue(Order{ID: "o1", Drink: "espresso", CustomerName: "Ada"})

	if err := s.runPurge(context.Background()); err != nil {
		t.Fatalf("runPurge error: %v, want a quiet skip", err)
	}
	if s.running.Load() {
		t.Error("runPurge must release the gate when it yields")
	}
	if said := speech.calls(); len(said) != 0 {
		t.Errorf("runPurge announced a purge it skipped: %v", said)
	}
}

// TestProcessQueueWaitsOutABusyArm is the end-to-end regression: an order
// queued behind a purge used to be started at once, lose the gate to the purge
// and be recorded as a fault. It must instead sit pending until the arm frees
// up and only then start.
func TestProcessQueueWaitsOutABusyArm(t *testing.T) {
	s, speech := gateTestCoffee(t, &Config{Conversational: true})
	stopped := make(chan struct{})
	go func() {
		s.processQueue()
		close(stopped)
	}()
	t.Cleanup(func() {
		close(s.queueStop)
		<-stopped
	})

	s.running.Store(true)
	s.queue.Enqueue(Order{ID: "o1", Drink: "espresso", CustomerName: "Ada", Greeting: "hello Ada"})

	time.Sleep(3 * armPollInterval)
	if _, ok := s.queue.Current(); ok {
		t.Fatal("processQueue started the order while another sequence held the arm")
	}
	if n := s.queue.Len(); n != 1 {
		t.Fatalf("Len = %d, want the order still queued", n)
	}
	if said := speech.calls(); len(said) != 0 {
		t.Fatalf("greeting spoken before the arm was free: %v", said)
	}

	s.running.Store(false)
	// The greeting is the first thing the order does once it has the arm; the
	// brew itself fails here for want of real hardware, which is beside the point.
	deadline := time.Now().Add(2 * time.Second)
	for len(speech.calls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the order never started after the arm was released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for s.queue.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the order never retired after starting")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for s.running.Load() {
		if time.Now().After(deadline) {
			t.Fatal("processQueue kept the running gate after the order retired")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
