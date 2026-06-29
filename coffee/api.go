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

func (s *beanjaminCoffee) Status(ctx context.Context) (map[string]interface{}, error) {
	_, span := trace.StartSpan(ctx, "beanjamin::Status")
	defer span.End()
	orders := s.queue.List()
	// structpb.NewStruct (used by RDK to serialize Status over the wire) only
	// accepts []interface{} for list values, not []map[string]interface{}, so
	// the slice element type must be interface{}.
	orderMaps := make([]interface{}, len(orders))
	for i, o := range orders {
		// structpb.NewStruct rejects []map[string]interface{} as list values,
		// so step_history must also be []interface{}.
		history := make([]interface{}, len(o.StepHistory))
		for j, e := range o.StepHistory {
			history[j] = map[string]interface{}{
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
		orderMaps[i] = map[string]interface{}{
			"id":            o.ID,
			"drink":         o.Drink,
			"customer_name": o.CustomerName,
			"enqueued_at":   o.EnqueuedAt.Format(time.RFC3339),
			"raw_step":      o.RawStep,
			"step_history":  history,
			"completed_at":  completedAt,
		}
	}
	step, _ := s.currentStep.Load().(string)
	resp := map[string]interface{}{
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
	}
	s.logger.Debugw("Status", "response", resp)
	return resp, nil
}

// parseCupFlowCount extracts the iteration count from a run_cup_flow command
// value. A JSON number is the count; bool true means a single iteration.
func parseCupFlowCount(v interface{}) (int, error) {
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

func (s *beanjaminCoffee) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	ctx, span := trace.StartSpan(ctx, "beanjamin::DoCommand")
	defer span.End()
	if orderRaw, ok := cmd["prepare_order"]; ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::prepare_order")
		defer cmdSpan.End()
		res, err := s.enqueueOrder(ctx, orderRaw)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if actionName, ok := cmd["execute_action"].(string); ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::execute_action["+actionName+"]")
		defer cmdSpan.End()
		res, err := s.executeAction(ctx, actionName)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if _, ok := cmd["cancel"]; ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::cancel")
		defer cmdSpan.End()
		res, err := s.cancel(ctx)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if _, ok := cmd["get_queue"]; ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::get_queue")
		defer cmdSpan.End()
		return s.Status(ctx)
	}
	if _, ok := cmd["proceed"]; ok {
		_, cmdSpan := trace.StartSpan(ctx, "beanjamin::proceed")
		defer cmdSpan.End()
		return s.proceedQueue()
	}
	if _, ok := cmd["clear_queue"]; ok {
		_, cmdSpan := trace.StartSpan(ctx, "beanjamin::clear_queue")
		defer cmdSpan.End()
		return s.clearQueue()
	}
	if _, ok := cmd["cleanup_pending_clips"]; ok {
		_, cmdSpan := trace.StartSpan(ctx, "beanjamin::cleanup_pending_clips")
		defer cmdSpan.End()
		return s.cleanupPendingClips()
	}

	if _, ok := cmd["reset_world"]; ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::reset_world")
		defer cmdSpan.End()
		res, err := s.resetWorld(ctx)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if countRaw, ok := cmd["run_cup_flow"]; ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::run_cup_flow")
		defer cmdSpan.End()
		count, err := parseCupFlowCount(countRaw)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
			return nil, err
		}
		res, err := s.runCupFlow(ctx, count)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	// Stream deck key commands
	if action, ok := cmd["action"].(string); ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::action["+action+"]")
		defer cmdSpan.End()
		switch action {
		case "open_gripper":
			return s.handleOpenGripper(ctx)
		case "close_gripper":
			return s.handleCloseGripper(ctx)
		default:
			return nil, fmt.Errorf("unknown action %q", action)
		}
	}
	err := fmt.Errorf("unknown command, supported commands: cancel, prepare_order, execute_action, get_queue, proceed, clear_queue, cleanup_pending_clips, reset_world, run_cup_flow, action")
	s.logger.Warnw("DoCommand", "error", err)
	return nil, err
}

func (s *beanjaminCoffee) handleOpenGripper(ctx context.Context) (map[string]interface{}, error) {
	if s.gripper == nil {
		return nil, fmt.Errorf("no gripper configured")
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to open gripper: %w", err)
	}
	return map[string]interface{}{"status": "opened"}, nil
}

func (s *beanjaminCoffee) handleCloseGripper(ctx context.Context) (map[string]interface{}, error) {
	if s.gripper == nil {
		return nil, fmt.Errorf("no gripper configured")
	}
	grabbed, err := s.gripper.Grab(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to close gripper: %w", err)
	}
	return map[string]interface{}{"status": "closed", "grabbed": grabbed}, nil
}
