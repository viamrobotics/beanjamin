package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// gripperState is the coarse physical state of the gripper jaws, derived from
// the raw uFactory jaw position (0–850) via two configurable thresholds.
type gripperState int

const (
	gripperClosed  gripperState = iota // jaws shut (empty, or on the thin filter handle)
	gripperHolding                     // jaws stopped on a cup or glass
	gripperOpen                        // jaws open
)

func (g gripperState) String() string {
	switch g {
	case gripperClosed:
		return "closed"
	case gripperHolding:
		return "holding"
	case gripperOpen:
		return "open"
	default:
		return "unknown"
	}
}

// Default holding-band thresholds — midpoint cuts between the measured anchors
// (empty 357, glass 494, cup 520, open 850). Tunable per build via Config, since
// 357 is this arm's claws-extension rest position.
const (
	gripperHoldMinPosDefault = 430.0
	gripperHoldMaxPosDefault = 685.0
)

// errGripMissed means a Grab closed the jaws but they did not land in the
// holding band — no object is between the claws.
var errGripMissed = errors.New("gripper did not close on an object")

func (s *beanjaminCoffee) gripperHoldMinPos() float64 {
	if s.cfg.GripperHoldMinPos > 0 {
		return s.cfg.GripperHoldMinPos
	}
	return gripperHoldMinPosDefault
}

func (s *beanjaminCoffee) gripperHoldMaxPos() float64 {
	if s.cfg.GripperHoldMaxPos > 0 {
		return s.cfg.GripperHoldMaxPos
	}
	return gripperHoldMaxPosDefault
}

// classifyGripper maps a raw jaw position to a coarse state. Pure: the only
// branch logic, and the only part testable without hardware. closed and open
// are derived half-lines on either side of the holding band.
func (s *beanjaminCoffee) classifyGripper(pos float64) gripperState {
	switch {
	case pos < s.gripperHoldMinPos():
		return gripperClosed
	case pos > s.gripperHoldMaxPos():
		return gripperOpen
	default:
		return gripperHolding
	}
}

// gripperPos reads the current jaw position (0–850) from the uFactory gripper.
// Position is only exposed via DoCommand{"get": true}; the standard Gripper API
// (Grab/Open) does not surface it.
func (s *beanjaminCoffee) gripperPos(ctx context.Context) (float64, error) {
	resp, err := s.gripper.DoCommand(ctx, map[string]interface{}{"get": true})
	if err != nil {
		return 0, fmt.Errorf("gripper_pos: %w", err)
	}
	pos, ok := resp["pos"].(float64)
	if !ok {
		return 0, fmt.Errorf("gripper_pos: unexpected DoCommand response %v", resp)
	}
	return pos, nil
}

// grabAndVerifyHolding closes the jaws and confirms they landed on an object.
// Returns errGripMissed (wrapped) when the settled position is not in the
// holding band. A failed position read returns a plain error (NOT errGripMissed)
// so callers never treat an unreadable gripper as a missed grab. Absorbs the
// post-grab settle pause so the read happens at rest.
func (s *beanjaminCoffee) grabAndVerifyHolding(ctx context.Context) error {
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("grab: %w", err)
	}
	time.Sleep(gripperPause)
	pos, err := s.gripperPos(ctx)
	if err != nil {
		return err
	}
	if state := s.classifyGripper(pos); state != gripperHolding {
		return fmt.Errorf("%w (pos=%.0f, state=%s)", errGripMissed, pos, state)
	}
	return nil
}

// normalizeGripperAtStart ensures the gripper is closed before a brew cycle
// begins. Nothing closes the gripper on the prior cycle's exit, so the first
// motion can inherit an open gripper — which has a larger collision silhouette,
// and the allowed-collision tuning assumes a closed gripper. Self-heals the
// common open case; aborts only if the jaws cannot reach closed (an object
// physically jammed between the claws), since traversing jammed risks collision.
func (s *beanjaminCoffee) normalizeGripperAtStart(ctx context.Context) error {
	if s.gripper == nil {
		return nil
	}
	pos, err := s.gripperPos(ctx)
	if err != nil {
		return err
	}
	if s.classifyGripper(pos) == gripperClosed {
		return nil
	}
	s.activeOrderLogger().Infof("gripper not closed at cycle start (pos=%.0f); closing", pos)
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("normalize gripper: close: %w", err)
	}
	time.Sleep(gripperPause)
	if pos, err = s.gripperPos(ctx); err != nil {
		return err
	}
	if s.classifyGripper(pos) != gripperClosed {
		return fmt.Errorf("gripper won't close at cycle start (pos=%.0f); item jammed between claws?", pos)
	}
	return nil
}
