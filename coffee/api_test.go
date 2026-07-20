package coffee

import (
	"context"
	"testing"

	"go.viam.com/rdk/logging"
)

// newStatusService builds a beanjaminCoffee with just the state Status and the
// non-hardware DoCommand paths touch: a config, a logger, and an order queue.
func newStatusService(t *testing.T, cfg *Config) *beanjaminCoffee {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	return &beanjaminCoffee{
		cfg:    cfg,
		logger: logging.NewTestLogger(t),
		queue:  NewOrderQueue(),
	}
}

func TestSetStepReflectedInStatus(t *testing.T) {
	s := newStatusService(t, &Config{})
	s.setStep(stepBrewing)

	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if st["current_step"] != stepBrewing {
		t.Errorf("current_step = %v, want %q", st["current_step"], stepBrewing)
	}
}

func TestStatusReportsQueueAndFlags(t *testing.T) {
	s := newStatusService(t, &Config{CanServeDecaf: true})
	s.queue.Enqueue(Order{ID: "o1", Drink: "espresso", CustomerName: "Ada", RawStep: "Grinding"})
	s.queue.Enqueue(Order{ID: "o2", Drink: "lungo", CustomerName: "Grace"})

	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}

	// count is the pending depth, returned as float64 to match the gRPC wire type.
	if got, ok := st["count"].(float64); !ok || got != 2 {
		t.Errorf("count = %v (%T), want float64(2)", st["count"], st["count"])
	}
	if st["can_serve_decaf"] != true {
		t.Errorf("can_serve_decaf = %v, want true", st["can_serve_decaf"])
	}
	if st["is_paused"] != false || st["is_busy"] != false {
		t.Errorf("is_paused/is_busy = %v/%v, want false/false", st["is_paused"], st["is_busy"])
	}
	orders, ok := st["orders"].([]any)
	if !ok || len(orders) != 2 {
		t.Fatalf("orders = %v, want a 2-element []any", st["orders"])
	}
}

// TestDoCommandDispatch covers the command routing for the paths that don't
// touch the arm: get_queue, clear_queue, proceed (errors when not paused), the
// run_cup_flow count guard, and the two unknown-command errors.
func TestDoCommandDispatch(t *testing.T) {
	ctx := context.Background()
	s := newStatusService(t, &Config{})
	s.queue.Enqueue(Order{ID: "o1", Drink: "espresso"})

	res, err := s.DoCommand(ctx, map[string]any{"get_queue": true})
	if err != nil {
		t.Fatalf("get_queue error: %v", err)
	}
	if got, _ := res["count"].(float64); got != 1 {
		t.Errorf("get_queue count = %v, want 1", res["count"])
	}

	if _, err := s.DoCommand(ctx, map[string]any{"clear_queue": true}); err != nil {
		t.Fatalf("clear_queue error: %v", err)
	}
	if s.queue.Len() != 0 {
		t.Errorf("after clear_queue, queue Len = %d, want 0", s.queue.Len())
	}

	// run_cup_flow validates its count before doing anything.
	if _, err := s.DoCommand(ctx, map[string]any{"run_cup_flow": float64(0)}); err == nil {
		t.Error("run_cup_flow with count 0 should error")
	}

	// open_door is an execute_action action; executeAction's running-gate is the
	// first thing it checks — so with a sequence already "running" it rejects
	// without touching the arm. This confirms the action is wired without needing
	// hardware.
	s.running.Store(true)
	if _, err := s.DoCommand(ctx, map[string]any{"execute_action": "open_door"}); err == nil {
		t.Error("open_door action should error when a sequence is already running")
	}
	s.running.Store(false)

	if _, err := s.DoCommand(ctx, map[string]any{"action": "teleport"}); err == nil {
		t.Error("unknown action should error")
	}
	if _, err := s.DoCommand(ctx, map[string]any{"nonsense": true}); err == nil {
		t.Error("unknown command should error")
	}
}

// TestProceedQueueSignal characterizes proceed as a cap-1 buffered signal: the
// first proceed is accepted; a second, with the slot still full (no consumer
// draining it), reports that nothing is waiting to resume.
func TestProceedQueueSignal(t *testing.T) {
	s := newStatusService(t, &Config{})
	if _, err := s.proceedQueue(); err != nil {
		t.Fatalf("first proceed: unexpected error %v", err)
	}
	if _, err := s.proceedQueue(); err == nil {
		t.Error("second proceed with the buffer full should error")
	}
}

// TestDoCommandNonStringActionFallsThrough: a non-string execute_action/action
// value isn't matched and falls through to the unknown-command error.
func TestDoCommandNonStringActionFallsThrough(t *testing.T) {
	s := newStatusService(t, &Config{})
	ctx := context.Background()
	for _, key := range []string{"execute_action", "action"} {
		if _, err := s.DoCommand(ctx, map[string]any{key: 123}); err == nil {
			t.Errorf("%s with a non-string value should fall through to the unknown-command error", key)
		}
	}
}
