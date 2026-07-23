package coffee

// DoCommand dispatch: the command handlers reachable over the wire
// (prepare_order, execute_action, get_queue, run_cup_flow, gripper actions, …)
// and the Status reporting they build on.

import (
	"context"
	"fmt"
	"time"

	"go.viam.com/rdk/module/trace"
)

// Step labels surfaced through setStep -> get_queue, the order sensor's
// failed_step, and the web tracker. Constants so the brew sequence
// (espresso.go) and cancel recovery (cancel) reference the same strings.
const (
	stepGrinding             = "Grinding"
	stepTamping              = "Tamping"
	stepLockingPortafilter   = "Locking portafilter"
	stepReleasingFilter      = "Releasing filter"
	stepPlacingCup           = "Placing cup"
	stepBrewing              = "Brewing"
	stepServing              = "Serving"
	stepGrabbingFilter       = "Grabbing filter"
	stepUnlockingPortafilter = "Unlocking portafilter"
	stepCleaning             = "Cleaning"
	stepFinishingUp          = "Finishing up"
	stepRecoveringFilter     = "Recovering filter"
)

func (s *beanjaminCoffee) setStep(step string) {
	s.currentStep.Store(step)
	if id, ok := s.currentOrderID.Load().(string); ok && id != "" {
		s.queue.SetStep(id, step)
	}
}

func (s *beanjaminCoffee) Status(ctx context.Context) (map[string]any, error) {
	_, span := trace.StartSpan(ctx, "beanjamin::Status")
	defer span.End()
	orders := s.queue.List()
	// structpb.NewStruct (used by RDK to serialize Status over the wire) only
	// accepts []any for list values, not []map[string]any, so
	// the slice element type must be any.
	orderMaps := make([]any, len(orders))
	for i, o := range orders {
		// structpb.NewStruct rejects []map[string]any as list values,
		// so step_history must also be []any.
		history := make([]any, len(o.StepHistory))
		for j, e := range o.StepHistory {
			history[j] = map[string]any{
				"step":       e.Step,
				"started_at": e.StartedAt.Format(time.RFC3339),
			}
		}
		// Empty string when the order is still pending; the frontend uses
		// completed_at presence as the signal to render the green ready card.
		completedAt := ""
		if !o.CompletedAt.IsZero() {
			completedAt = o.CompletedAt.Format(time.RFC3339)
		}
		orderMaps[i] = map[string]any{
			"id":            o.ID,
			"drink":         o.Drink,
			"customer_name": o.CustomerName,
			"fulfillment":   o.Fulfillment,
			"enqueued_at":   o.EnqueuedAt.Format(time.RFC3339),
			"raw_step":      o.RawStep,
			"step_history":  history,
			"completed_at":  completedAt,
		}
	}
	step, _ := s.currentStep.Load().(string)
	resp := map[string]any{
		// count reports pending depth only — orders waiting to be made.
		// Recently-completed orders are visible in `orders` but don't add
		// to depth. Returned as float64 so in-process callers see the
		// same type as gRPC callers (structpb forces all numbers to
		// double on the wire).
		"count":           float64(s.queue.Len()),
		"orders":          orderMaps,
		"is_paused":       s.paused.Load(),
		"is_busy":         s.running.Load(),
		"current_step":    step,
		"can_serve_decaf": s.cfg.CanServeDecaf,
		"fault_active":    s.faultActive.Load(),
	}
	s.logger.Debugw("Status", "response", resp)
	return resp, nil
}

// parseCupFlowCount extracts the iteration count from a run_cup_flow command
// value. A JSON number is the count; bool true means a single iteration.
func parseCupFlowCount(v any) (int, error) {
	count := 1
	switch n := v.(type) {
	case bool:
		// run_cup_flow: true → one iteration.
	case float64:
		count = int(n)
	default:
		return 0, fmt.Errorf("run_cup_flow must be an iteration count (number) or true, got %T", v)
	}
	if count < 1 {
		return 0, fmt.Errorf("run_cup_flow count must be >= 1, got %d", count)
	}
	return count, nil
}

// commandDef is one entry in the DoCommand dispatch table. needsStr restricts a
// match to string values (execute_action/action dispatch on the string).
type commandDef struct {
	key      string
	needsStr bool
	// spanName overrides the trace-span suffix; nil uses key verbatim.
	spanName func(cmd map[string]any) string
	run      func(s *beanjaminCoffee, ctx context.Context, cmd map[string]any) (map[string]any, error)
}

func (d commandDef) matches(cmd map[string]any) bool {
	v, ok := cmd[d.key]
	if !ok {
		return false
	}
	if d.needsStr {
		_, isStr := v.(string)
		return isStr
	}
	return true
}

// coffeeCommands is the ordered DoCommand dispatch table (first match wins).
var coffeeCommands = []commandDef{
	{key: "prepare_order", run: func(s *beanjaminCoffee, ctx context.Context, cmd map[string]any) (map[string]any, error) {
		return s.enqueueOrder(ctx, cmd["prepare_order"])
	}},
	{key: "execute_action", needsStr: true,
		spanName: func(cmd map[string]any) string {
			return "execute_action[" + cmd["execute_action"].(string) + "]"
		},
		run: func(s *beanjaminCoffee, ctx context.Context, cmd map[string]any) (map[string]any, error) {
			return s.executeAction(ctx, cmd["execute_action"].(string))
		}},
	{key: "cancel", run: func(s *beanjaminCoffee, ctx context.Context, _ map[string]any) (map[string]any, error) {
		return s.cancel(ctx)
	}},
	{key: "get_queue", run: func(s *beanjaminCoffee, ctx context.Context, _ map[string]any) (map[string]any, error) {
		return s.Status(ctx)
	}},
	{key: "proceed", run: func(s *beanjaminCoffee, _ context.Context, _ map[string]any) (map[string]any, error) {
		return s.proceedQueue()
	}},
	{key: "clear_queue", run: func(s *beanjaminCoffee, _ context.Context, _ map[string]any) (map[string]any, error) {
		return s.clearQueue()
	}},
	{key: "cleanup_pending_clips", run: func(s *beanjaminCoffee, _ context.Context, _ map[string]any) (map[string]any, error) {
		return s.cleanupPendingClips()
	}},
	{key: "reset_world", run: func(s *beanjaminCoffee, ctx context.Context, _ map[string]any) (map[string]any, error) {
		return s.resetWorld(ctx)
	}},
	{key: "send_delivery_message", run: func(s *beanjaminCoffee, ctx context.Context, cmd map[string]any) (map[string]any, error) {
		return s.sendDeliveryMessage(ctx, cmd["send_delivery_message"])
	}},
	{key: "run_cup_flow", run: func(s *beanjaminCoffee, ctx context.Context, cmd map[string]any) (map[string]any, error) {
		count, err := parseCupFlowCount(cmd["run_cup_flow"])
		if err != nil {
			return nil, err
		}
		return s.runCupFlow(ctx, count)
	}},
	// Stream deck key commands.
	{key: "action", needsStr: true,
		spanName: func(cmd map[string]any) string { return "action[" + cmd["action"].(string) + "]" },
		run: func(s *beanjaminCoffee, ctx context.Context, cmd map[string]any) (map[string]any, error) {
			switch action := cmd["action"].(string); action {
			case "open_gripper":
				return s.handleOpenGripper(ctx)
			case "close_gripper":
				return s.handleCloseGripper(ctx)
			default:
				return nil, fmt.Errorf("unknown action %q", action)
			}
		}},
}

func (s *beanjaminCoffee) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	ctx, span := trace.StartSpan(ctx, "beanjamin::DoCommand")
	defer span.End()

	for _, def := range coffeeCommands {
		if def.matches(cmd) {
			return s.runCommand(ctx, def, cmd)
		}
	}

	err := fmt.Errorf("unknown command, supported commands: cancel, prepare_order, execute_action, get_queue, proceed, clear_queue, cleanup_pending_clips, reset_world, run_cup_flow, action, send_delivery_message")
	s.logger.Warnw("DoCommand", "error", err)
	return nil, err
}

// runCommand runs a matched command inside its trace span, logging any
// returned error.
func (s *beanjaminCoffee) runCommand(ctx context.Context, def commandDef, cmd map[string]any) (map[string]any, error) {
	suffix := def.key
	if def.spanName != nil {
		suffix = def.spanName(cmd)
	}
	ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::"+suffix)
	defer cmdSpan.End()

	res, err := def.run(s, ctx, cmd)
	if err != nil {
		s.logger.Errorw("DoCommand", "error", err)
	}
	return res, err
}

func (s *beanjaminCoffee) handleOpenGripper(ctx context.Context) (map[string]any, error) {
	if s.gripper == nil {
		return nil, fmt.Errorf("no gripper configured")
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to open gripper: %w", err)
	}
	return map[string]any{"status": "opened"}, nil
}

func (s *beanjaminCoffee) handleCloseGripper(ctx context.Context) (map[string]any, error) {
	if s.gripper == nil {
		return nil, fmt.Errorf("no gripper configured")
	}
	grabbed, err := s.gripper.Grab(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to close gripper: %w", err)
	}
	return map[string]any{"status": "closed", "grabbed": grabbed}, nil
}
