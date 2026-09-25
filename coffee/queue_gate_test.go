package coffee

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
)

// recordingSink captures the order readings the queue pushes.
type recordingSink struct {
	mu       sync.Mutex
	readings []orderReading
}

func (r *recordingSink) pushOrderReading(o orderReading) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readings = append(r.readings, o)
}

func (r *recordingSink) all() []orderReading {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]orderReading(nil), r.readings...)
}

// runQueue starts processQueue with a fresh queueStop and stops it on cleanup,
// waiting for the goroutine to exit.
func runQueue(t *testing.T, s *beanjaminCoffee) {
	t.Helper()
	s.queueStop = make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		s.processQueue()
		close(stopped)
	}()
	t.Cleanup(func() {
		close(s.queueStop)
		<-stopped
	})
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestProcessQueueWaitsOutABusyArm is the end-to-end regression: an order
// queued behind a purge used to be started at once, lose the gate to the purge
// and be recorded as a fault. It must instead sit pending until the arm frees
// up and only then start.
func TestProcessQueueWaitsOutABusyArm(t *testing.T) {
	s, speech := newTestCoffee(t, &Config{Conversational: true})
	sink := &recordingSink{}
	s.orderSensorSink = sink
	_, releasePurge := holdArm(t, s)
	runQueue(t, s)

	s.queue.Enqueue(Order{ID: "o1", Drink: "espresso", CustomerName: "Ada", Greeting: "hello Ada"})

	time.Sleep(100 * time.Millisecond)
	if _, ok := s.queue.Current(); ok {
		t.Fatal("processQueue started the order while another sequence held the arm")
	}
	if head, ok := s.queue.Peek(); !ok || head.ID != "o1" {
		t.Fatalf("Peek = %+v, %v; the order should still be pending", head, ok)
	}
	if said := speech.calls(); len(said) != 0 {
		t.Fatalf("greeting spoken before the arm was free: %v", said)
	}
	if got := sink.all(); len(got) != 0 {
		t.Fatalf("an order reading was pushed before the order ran: %+v", got)
	}

	releasePurge()
	// The greeting is the first thing the order does once it has the arm; the
	// brew itself fails here for want of real hardware, which is beside the point.
	eventually(t, "the order to start", func() bool { return len(speech.calls()) > 0 })
	if said := speech.calls(); said[0] != "hello Ada" {
		t.Errorf("first line = %q, want the greeting", said[0])
	}
	eventually(t, "the order to retire", func() bool { return s.queue.Len() == 0 && len(sink.all()) == 1 })
	eventually(t, "the queue to release the arm", func() bool { return !s.lease.busy() })
}

// TestCancelOrderWhileWaitingForTheArm: an order waiting on a busy arm is still
// pending, so cancel_order removes it and the queue goes back to waiting
// without running anything once the arm frees up.
func TestCancelOrderWhileWaitingForTheArm(t *testing.T) {
	s, speech := newTestCoffee(t, &Config{Conversational: true})
	sink := &recordingSink{}
	s.orderSensorSink = sink
	_, releasePurge := holdArm(t, s)
	runQueue(t, s)

	s.queue.Enqueue(Order{ID: "o1", Drink: "espresso", CustomerName: "Ada", Greeting: "hello Ada"})
	time.Sleep(50 * time.Millisecond)
	if _, err := s.cancelOrder(context.Background(), "o1"); err != nil {
		t.Fatalf("cancel_order on a waiting order: %v", err)
	}
	said := len(speech.calls())
	releasePurge()

	eventually(t, "the queue to hand the arm back", func() bool { return !s.lease.busy() })
	time.Sleep(50 * time.Millisecond)
	if got := len(speech.calls()); got != said {
		t.Errorf("speech after the cancelled order's arm freed up: %v", speech.calls()[said:])
	}
	if got := sink.all(); len(got) != 0 {
		t.Errorf("a cancelled order ran: %+v", got)
	}
}

// TestCloseUnblocksAWaitingQueue: a consumer waiting on a held arm exits when
// the service closes, and the holder is cancelled as a shutdown.
func TestCloseUnblocksAWaitingQueue(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	holderCtx, _ := holdArm(t, s)
	s.queueStop = make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		s.processQueue()
		close(stopped)
	}()
	s.queue.Enqueue(Order{ID: "o1", Drink: "espresso", CustomerName: "Ada"})
	time.Sleep(50 * time.Millisecond)

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("processQueue did not exit after Close")
	}
	if !errors.Is(context.Cause(holderCtx), errServiceClosed) {
		t.Errorf("holder cause = %v, want errServiceClosed", context.Cause(holderCtx))
	}
	if head, ok := s.queue.Peek(); !ok || head.ID != "o1" {
		t.Errorf("Peek = %+v, %v; the order should be left pending", head, ok)
	}
}

// faultTestCoffee is coffeeWithDirtyWorld with an observed logger and an order
// sink, and one order queued, ready for runNextOrder.
func faultTestCoffee(t *testing.T) (*beanjaminCoffee, *recordingSink, func() []string) {
	t.Helper()
	s, _, _ := coffeeWithDirtyWorld(t, nil)
	base, observed := logging.NewObservedTestLogger(t)
	s.logger = base
	sink := &recordingSink{}
	s.orderSensorSink = sink
	s.queueStop = make(chan struct{})
	t.Cleanup(func() { close(s.queueStop) })
	s.queue.Enqueue(NewOrder("espresso", "Alice", "", ""))
	errorLogs := func() []string {
		var out []string
		for _, e := range observed.All() {
			if e.Level == zapcore.ErrorLevel {
				out = append(out, e.Message)
			}
		}
		return out
	}
	return s, sink, errorLogs
}

func faultLog(logs []string) string {
	for _, m := range logs {
		if strings.HasPrefix(m, "order faulted") {
			return m
		}
	}
	return ""
}

// TestFaultWithStrandedStatePausesQueue pins that a genuine fault leaving the
// machine mid-cycle holds the next order back for rewind → proceed, instead of
// letting it start from the stranded state, and that the log names what was
// left behind.
func TestFaultWithStrandedStatePausesQueue(t *testing.T) {
	s, sink, errorLogs := faultTestCoffee(t)
	s.portafilterInMachine.Store(true)
	s.gripper = faultingGripper()

	if !s.runNextOrder() {
		t.Fatal("runNextOrder reported shutdown")
	}
	if !s.lease.paused() {
		t.Error("a fault that stranded state must pause the queue")
	}
	if s.lease.busy() {
		t.Error("the arm must be released after the fault")
	}
	msg := faultLog(errorLogs())
	if !strings.Contains(msg, "portafilter in machine") || !strings.Contains(msg, "queue paused") {
		t.Errorf("fault log = %q, want the stranded state and the pause named", msg)
	}
	if r := sink.all(); len(r) != 1 || r[0].operatorCancelled || r[0].execErr == nil {
		t.Errorf("readings = %+v, want one genuine fault", r)
	}
}

// TestFaultWithCleanWorldPausesQueue covers a fault with no mid-cycle state
// recorded: the queue still pauses, because the recorded state does not show
// everything a fault can leave behind.
func TestFaultWithCleanWorldPausesQueue(t *testing.T) {
	s, _, errorLogs := faultTestCoffee(t)
	s.heldItemAttached = false
	s.filterFrameLocked = false
	s.stagedGlassPlaced = false
	s.gripper = faultingGripper()

	s.runNextOrder()
	if !s.lease.paused() {
		t.Error("a fault must pause the queue even with no mid-cycle state recorded")
	}
	if msg := faultLog(errorLogs()); !strings.Contains(msg, "no mid-cycle state recorded") {
		t.Errorf("fault log = %q, want it to say nothing was recorded", msg)
	}
}

// TestOperatorCancelIsNotAFault pins that an interrupted order is classified as
// a cancel from the lease's cancel cause, whatever error the step returned, and
// that the fault path adds no pause of its own: the pause belongs to whoever
// interrupted the order, so one they have already lifted must stay lifted.
func TestOperatorCancelIsNotAFault(t *testing.T) {
	s, sink, errorLogs := faultTestCoffee(t)
	g := inject.NewGripper("g")
	g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
		if _, ok := s.lease.cancelHolder(); !ok {
			t.Error("cancelHolder found no holder mid-order")
		}
		s.lease.unpause()
		// A plain error, deliberately not wrapping context.Canceled.
		return nil, errors.New("gripper interrupted")
	}
	s.gripper = g

	s.runNextOrder()
	if s.lease.paused() {
		t.Error("the fault path must not pause an operator-interrupted order")
	}
	if msg := faultLog(errorLogs()); msg != "" {
		t.Errorf("an interrupted order was logged as a fault: %q", msg)
	}
	if r := sink.all(); len(r) != 1 || !r[0].operatorCancelled {
		t.Errorf("readings = %+v, want one operator-cancelled reading", r)
	}
}

// TestFailedManualActionDoesNotPause: execute_action releases its lease without
// a pause however it ends, so a failed manual step leaves the queue running.
func TestFailedManualActionDoesNotPause(t *testing.T) {
	// A clean world makes the action refresh the frame system first, and the
	// failing framesystem service fails it there.
	s, _, _ := coffeeWithDirtyWorld(t, errors.New("boom"))
	s.heldItemAttached = false
	s.filterFrameLocked = false
	s.stagedGlassPlaced = false
	if _, err := s.executeAction(context.Background(), "grind_coffee", false); err == nil {
		t.Fatal("execute_action should fail when the frame system can't be refreshed")
	}
	if s.lease.paused() || s.lease.busy() {
		t.Errorf("paused=%v busy=%v after a failed execute_action, want both false", s.lease.paused(), s.lease.busy())
	}
}

// TestRunPurgeYieldsToAQueuedOrder: an order that lands between the keepalive
// tick's check and the purge's claim goes first — the purge says nothing, moves
// nothing, and hands the arm straight back.
func TestRunPurgeYieldsToAQueuedOrder(t *testing.T) {
	s, speech := newTestCoffee(t, nil)
	s.queue.Enqueue(Order{ID: "o1", Drink: "espresso", CustomerName: "Ada"})

	if err := s.runPurge(context.Background()); err != nil {
		t.Fatalf("runPurge error: %v, want a quiet skip", err)
	}
	if s.lease.busy() {
		t.Error("runPurge must release the arm when it yields")
	}
	if said := speech.calls(); len(said) != 0 {
		t.Errorf("runPurge announced a purge it skipped: %v", said)
	}
}

// TestRunPurgeRefusesWhileHeld: the keepalive tick never waits for the arm.
func TestRunPurgeRefusesWhileHeld(t *testing.T) {
	s, speech := newTestCoffee(t, nil)
	holdArm(t, s)
	if err := s.runPurge(context.Background()); !errors.Is(err, errArmBusy) {
		t.Fatalf("runPurge err = %v, want errArmBusy", err)
	}
	if said := speech.calls(); len(said) != 0 {
		t.Errorf("a refused purge spoke: %v", said)
	}
}

// TestStatusReportsTheLease: is_busy and is_paused are read off the lease.
func TestStatusReportsTheLease(t *testing.T) {
	s := newStatusService(t, nil)
	_, release := holdArm(t, s)
	s.lease.cancelHolder()
	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st["is_busy"] != true || st["is_paused"] != true {
		t.Errorf("is_busy/is_paused = %v/%v, want true/true", st["is_busy"], st["is_paused"])
	}
	release()
	s.lease.unpause()
	st, _ = s.Status(context.Background())
	if st["is_busy"] != false || st["is_paused"] != false {
		t.Errorf("is_busy/is_paused = %v/%v, want false/false", st["is_busy"], st["is_paused"])
	}
}
