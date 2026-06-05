package beanjamin

import (
	"context"
	"strings"
)

// collisionSensitivityKey is the move-level extra key the xArm module reads to
// override hardware collision detection for a single move, restoring the
// configured baseline once the move completes — even on failure
// (viam-modules/viam-ufactory-xarm#80). The value is an integer 0–5.
const collisionSensitivityKey = "collision_sensitivity"

// clearErrorKey is the xArm module DoCommand that clears a latched collision
// e-stop so the arm accepts motion again. Pre-existing on the arm module.
const clearErrorKey = "clear_error"

// collisionOvercurrentMarker is the stable substring the xArm driver returns
// when a move trips the firmware over-current collision e-stop. Matching the
// string couples us to the arm module's error text; if that text changes this
// detection must be updated.
const collisionOvercurrentMarker = "collision caused overcurrent"

// expectsContact reports whether a step deliberately makes contact, so collision
// protection should be relaxed for it. A step qualifies if it explicitly sets
// ExpectsContact (e.g. the tamper/grinder press) or already declares
// AllowedCollisions (lock/unlock/grab/button/cup/clean steps).
func (st Step) expectsContact() bool {
	return st.ExpectsContact || len(st.AllowedCollisions) > 0
}

// isCollisionError reports whether err is the arm's firmware collision e-stop.
func isCollisionError(err error) bool {
	return err != nil && strings.Contains(err.Error(), collisionOvercurrentMarker)
}

// collisionProtectionEnabled reports whether per-move collision protection is
// configured. When false the service never touches the arm's collision
// sensitivity and behaves exactly as before the feature existed.
func (s *beanjaminCoffee) collisionProtectionEnabled() bool {
	return s.cfg != nil && s.cfg.FreeMoveCollisionSensitivity > 0
}

// collisionExtra returns the move-level extra that sets the arm's hardware
// collision detection for this step: the configured protective level for
// free-space moves, or 0 for intentional-contact moves. It returns nil when the
// feature is disabled, so the move is issued with no override and the arm keeps
// its configured baseline. The arm restores the baseline after every move
// (#80), so there is no state to track between moves.
func (s *beanjaminCoffee) collisionExtra(step Step) map[string]interface{} {
	if !s.collisionProtectionEnabled() {
		return nil
	}
	level := s.cfg.FreeMoveCollisionSensitivity
	if step.expectsContact() {
		level = 0
	}
	return map[string]interface{}{collisionSensitivityKey: float64(level)}
}

// handleCollisionFault records a collision e-stop and auto-clears the arm so it
// isn't left latched. The order is already failing; processQueue halts the queue
// (via faultReason) until an operator clears the obstacle and sends "proceed".
// The arm restores its baseline collision sensitivity on its own after the
// failed move (#80), so there is nothing to reset here. Uses a background
// context so recovery runs even if the failed move's context is already done.
func (s *beanjaminCoffee) handleCollisionFault(step Step, cause error) {
	reason := "collision detected during " + step.PoseName
	s.faultReason.Store(reason)
	s.logger.Errorf("%s: %v — automatically clearing the arm error so it isn't left latched; the queue will halt until 'proceed'", reason, cause)

	if _, err := s.arm.DoCommand(context.Background(), map[string]interface{}{clearErrorKey: true}); err != nil {
		s.logger.Errorf("failed to auto-clear arm collision error: %v — operator must clear it via clear_error or UFACTORY Studio", err)
	}
}
