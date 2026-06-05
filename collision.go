package beanjamin

import (
	"context"
	"fmt"
	"strings"
)

// setCollisionSensitivityKey and clearErrorKey are the xArm module DoCommand
// keys (viam-modules/viam-ufactory-xarm#79). set_collision_sensitivity takes an
// integer 0–5 and errors if the arm is moving; clear_error clears the latched
// collision e-stop.
const (
	setCollisionSensitivityKey = "set_collision_sensitivity"
	clearErrorKey              = "clear_error"
)

// collisionOvercurrentMarker is the stable substring the xArm driver returns
// when a move trips the firmware over-current collision e-stop. Matching the
// string couples us to the arm module's error text; if that text changes this
// detection must be updated.
const collisionOvercurrentMarker = "collision caused overcurrent"

// expectsContact reports whether a step deliberately makes contact, so collision
// protection should be disabled for it. A step qualifies if it explicitly sets
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

// applyCollisionSensitivity sets the arm's hardware collision detection for the
// upcoming step: the configured protective level for free-space moves, or 0 for
// intentional-contact moves. It is a no-op when the feature is disabled or the
// level is already what we want. Called from executeStep between moves (the arm
// is idle under the running gate), since set_collision_sensitivity errors mid-motion.
func (s *beanjaminCoffee) applyCollisionSensitivity(ctx context.Context, step Step) error {
	if !s.collisionProtectionEnabled() {
		return nil
	}
	target := int64(s.cfg.FreeMoveCollisionSensitivity)
	if step.expectsContact() {
		target = 0
	}
	if s.currentSensitivity.Load() == target {
		return nil
	}
	if _, err := s.arm.DoCommand(ctx, map[string]interface{}{setCollisionSensitivityKey: float64(target)}); err != nil {
		return fmt.Errorf("set collision sensitivity to %d before %q: %w", target, step.PoseName, err)
	}
	s.logger.Infof("collision sensitivity → %d for %q (contact=%v)", target, step.PoseName, step.expectsContact())
	s.currentSensitivity.Store(target)
	return nil
}

// handleCollisionFault records a collision e-stop, auto-clears the arm so it
// isn't left latched, and drops sensitivity back to 0. The order is already
// failing; processQueue halts the queue (via faultReason) until an operator
// clears the obstacle and sends "proceed". Uses a background context so recovery
// runs even if the failed move's context is already done.
func (s *beanjaminCoffee) handleCollisionFault(step Step, cause error) {
	reason := fmt.Sprintf("collision detected during %q", step.PoseName)
	s.faultReason.Store(reason)
	s.logger.Errorf("%s: %v — automatically clearing the arm error so it isn't left latched; the queue will halt until 'proceed'", reason, cause)

	ctx := context.Background()
	if _, err := s.arm.DoCommand(ctx, map[string]interface{}{clearErrorKey: true}); err != nil {
		s.logger.Errorf("failed to auto-clear arm collision error: %v — operator must clear it via clear_error or UFACTORY Studio", err)
	}
	// After clearing, leave the arm unprotected so the resume move can drive
	// clear of the obstacle without immediately re-tripping; the next free-space
	// move re-applies the protective level.
	if _, err := s.arm.DoCommand(ctx, map[string]interface{}{setCollisionSensitivityKey: float64(0)}); err != nil {
		s.logger.Errorf("failed to reset collision sensitivity to 0 after clear: %v", err)
	}
	s.currentSensitivity.Store(0)
}
