package coffee

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	defaultGripperHoldMinPos = 430.0
	defaultGripperHoldMaxPos = 685.0
)

// errGripMissed means a Grab closed the jaws but they did not land in the
// holding band — no object is between the claws.
var errGripMissed = errors.New("gripper did not close on an object")

// errGripperNotOpen means the jaws did not reach the open band within the
// timeout after an Open command.
var errGripperNotOpen = errors.New("gripper did not open")

// defaultGripperOpenTimeoutSec bounds the wait for the jaws to read open. Jaw
// travel time scales with how hard the jaws were clamped and with the build's
// jaw range, so this is a per-machine calibration knob
// (gripper_open_timeout_sec), not a constant of the code.
const defaultGripperOpenTimeoutSec = 3.0

func (s *beanjaminCoffee) gripperOpenTimeout() time.Duration {
	return time.Duration(orDefault(s.cfg.GripperOpenTimeoutSec, defaultGripperOpenTimeoutSec) * float64(time.Second))
}

func (s *beanjaminCoffee) gripperHoldMinPos() float64 {
	return orDefault(s.cfg.GripperHoldMinPos, defaultGripperHoldMinPos)
}

func (s *beanjaminCoffee) gripperHoldMaxPos() float64 {
	return orDefault(s.cfg.GripperHoldMaxPos, defaultGripperHoldMaxPos)
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
	if s.gripper == nil {
		return 0, errors.New("gripper_pos: no gripper")
	}
	resp, err := s.gripper.DoCommand(ctx, map[string]any{"get": true})
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
// holding band, after reopening the jaws: a close that reads as missed may be a
// jaw that snagged on the way in and is still around the item, and leaving it
// closing lets it catch the item after the caller has written the grab off.
// The open is verified, so on errGripMissed the jaws are known open. A failed
// position read or a failed reopen returns a plain error (NOT errGripMissed) so
// callers never treat an unreadable or stuck gripper as a missed grab. Waits
// for the jaws to settle so the read happens at rest.
//
// During an order the grab and its check are handed off like a move, so the
// planner goes on planning while the jaws close. A miss then fails the order at
// its next hand-off or stopping point, before any later move runs; only inside
// a syncRegion (the vision pickup) does the error come back from this call.
func (s *beanjaminCoffee) grabAndVerifyHolding(ctx context.Context) error {
	return s.handOff(ctx, move{name: "grab and verify", gripper: grabAndVerifyGripperAction})
}

// runGrabAndVerify is grabAndVerifyHolding as the execution thread runs it.
func (s *beanjaminCoffee) runGrabAndVerify(ctx context.Context) error {
	if err := s.actuateGripper(ctx, closeGripperAction); err != nil {
		return fmt.Errorf("grab: %w", err)
	}
	pos, err := s.gripperPos(ctx)
	if err != nil {
		return err
	}
	if state := s.classifyGripper(pos); state != gripperHolding {
		missErr := fmt.Errorf("%w (pos=%.0f, state=%s)", errGripMissed, pos, state)
		if err := s.runOpenAndVerify(ctx); err != nil {
			return fmt.Errorf("%v; reopen: %w", missErr, err)
		}
		return missErr
	}
	return nil
}

// openAndVerifyOpen opens the jaws and blocks until they actually read open,
// the mirror of grabAndVerifyHolding on the closing side. Returns
// errGripperNotOpen (wrapped) when they have not reached the open band within
// gripperOpenTimeout; a failed position read returns a plain error so an
// unreadable gripper is never mistaken for a stuck one.
//
// A fixed sleep is not enough here. Jaw travel scales with clamp force and jaw
// range, so a constant tuned on one build silently becomes too short on a
// stronger one — and the failure mode is the arm moving while the jaws are still
// closed on the portafilter, which wrenches the filter against the bayonet
// rather than releasing it. Polling costs nothing when the jaws are quick and
// fails loudly instead of silently when they are not.
//
// Handed off like grabAndVerifyHolding: the next move waits for the jaws to
// read open, but the planner doesn't.
func (s *beanjaminCoffee) openAndVerifyOpen(ctx context.Context) error {
	return s.handOff(ctx, move{name: "open and verify", gripper: openAndVerifyGripperAction})
}

// runOpenAndVerify is openAndVerifyOpen as the execution thread runs it.
func (s *beanjaminCoffee) runOpenAndVerify(ctx context.Context) error {
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("open: %w", err)
	}
	// An open settles on the band alone, so the pre-command position is unused.
	pos, settled, err := s.waitGripperSettled(ctx, openGripperAction, 0, s.gripperOpenTimeout())
	if err != nil {
		return err
	}
	if !settled {
		return fmt.Errorf("%w within %s (pos=%.0f, state=%s)",
			errGripperNotOpen, s.gripperOpenTimeout(), pos, s.classifyGripper(pos))
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
	if err := s.actuateGripper(ctx, closeGripperAction); err != nil {
		return fmt.Errorf("normalize gripper: close: %w", err)
	}
	if pos, err = s.gripperPos(ctx); err != nil {
		return err
	}
	if s.classifyGripper(pos) != gripperClosed {
		return fmt.Errorf("gripper won't close at cycle start (pos=%.0f); item jammed between claws?", pos)
	}
	return nil
}

// openGripper hands off opening the jaws and waiting for them to settle.
func (s *beanjaminCoffee) openGripper(ctx context.Context) error {
	return s.handOff(ctx, move{name: "open gripper", gripper: openGripperAction, settleGripper: true})
}

// closeGripper hands off closing the jaws and waiting for them to settle.
func (s *beanjaminCoffee) closeGripper(ctx context.Context) error {
	return s.handOff(ctx, move{name: "close gripper", gripper: closeGripperAction, settleGripper: true})
}

const (
	// gripperPollInterval is how often a settle wait reads the jaw position. The
	// jaws close in about 0.35 s and open in about 0.75 s, so a 50 ms poll
	// overshoots the moment they stop by a small fraction of the travel, while a
	// "get" round trip costs only a few milliseconds.
	gripperPollInterval = 50 * time.Millisecond
	// gripperSettleTolerance is the jaw-position change (0–850 scale) between two
	// consecutive reads that still counts as stopped. Moving jaws cover tens of
	// units per poll; jaws clamped on an item can still creep a few units.
	gripperSettleTolerance = 5.0
)

// sendGripper commands the jaws without waiting for them to move.
func (s *beanjaminCoffee) sendGripper(ctx context.Context, action gripperAction) error {
	switch action {
	case openGripperAction:
		return s.gripper.Open(ctx, nil)
	case closeGripperAction:
		_, err := s.gripper.Grab(ctx, nil)
		return err
	case noGripperAction:
	}
	return nil
}

// actuateGripper commands the jaws and returns once they have settled, for
// callers that do not check where the jaws ended up. The wait is capped at
// gripperPause; one that runs out logs a warning and returns nil, since these
// callers have no failure path for slow jaws. When the position cannot be read
// (no readable gripper, as in fake mode) it waits out gripperPause instead.
// Only a failed command or a cancelled ctx returns an error.
func (s *beanjaminCoffee) actuateGripper(ctx context.Context, action gripperAction) error {
	return s.actuateGripperWithin(ctx, action, gripperPause)
}

// actuateGripperWithin is actuateGripper with the settle wait capped at limit
// instead of gripperPause, for a caller that must not move on before the jaws
// get there.
func (s *beanjaminCoffee) actuateGripperWithin(ctx context.Context, action gripperAction, limit time.Duration) error {
	logger := s.activeOrderLogger()
	before, beforeErr := s.gripperPos(ctx)
	if err := s.sendGripper(ctx, action); err != nil {
		return err
	}
	sent := time.Now()
	fixedPause := func() error {
		select {
		case <-time.After(time.Until(sent.Add(gripperPause))):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if beforeErr != nil {
		logger.Debugf("gripper settle: cannot read the jaws, waiting %s: %v", gripperPause, beforeErr)
		return fixedPause()
	}
	pos, settled, err := s.waitGripperSettled(ctx, action, before, limit)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		logger.Debugf("gripper settle: lost the jaw position, waiting out %s: %v", gripperPause, err)
		return fixedPause()
	case !settled:
		logger.Warnf("gripper settle: jaws not settled %s after %s (pos=%.0f, from %.0f); continuing",
			limit, gripperActionName(action), pos, before)
	}
	return nil
}

// waitGripperSettled polls the jaw position every gripperPollInterval until
// the jaws have settled after action or timeout has passed, and returns the
// last position read and whether they settled. before is the position read
// just before the command was sent. Settled means:
//   - after an open, the jaws read in the open band;
//   - after a close, two consecutive reads agree within gripperSettleTolerance,
//     and the jaws have either moved away from before or are out of the open
//     band.
//
// The second close condition rejects reads taken before the jaws start moving:
// those agree with each other too, at the open position the close started
// from. A close that starts in the closed or holding band has nowhere to go, so
// a stable read there is accepted as is. The open band needs no such guard,
// since a stale read is outside it whenever there is travel left to do. A
// failed read returns its error.
func (s *beanjaminCoffee) waitGripperSettled(ctx context.Context, action gripperAction, before float64,
	timeout time.Duration,
) (pos float64, settled bool, err error) {
	deadline := time.Now().Add(timeout)
	prev, havePrev := 0.0, false
	for {
		select {
		case <-time.After(gripperPollInterval):
		case <-ctx.Done():
			return pos, false, ctx.Err()
		}
		pos, err = s.gripperPos(ctx)
		if err != nil {
			return pos, false, err
		}
		if s.jawsSettled(action, before, prev, havePrev, pos) {
			return pos, true, nil
		}
		prev, havePrev = pos, true
		if !time.Now().Before(deadline) {
			return pos, false, nil
		}
	}
}

// jawsSettled is waitGripperSettled's per-read test; see its doc for the rule.
func (s *beanjaminCoffee) jawsSettled(action gripperAction, before, prev float64, havePrev bool, pos float64) bool {
	switch action {
	case openGripperAction:
		return s.classifyGripper(pos) == gripperOpen
	case closeGripperAction:
		if !havePrev || math.Abs(pos-prev) > gripperSettleTolerance {
			return false
		}
		return math.Abs(pos-before) > gripperSettleTolerance || s.classifyGripper(pos) != gripperOpen
	case noGripperAction:
	}
	return true
}

func gripperActionName(action gripperAction) string {
	switch action {
	case openGripperAction:
		return "open"
	case closeGripperAction:
		return "close"
	case noGripperAction:
	}
	return "no"
}
