package beanjamin

import (
	"context"
	"errors"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
)

func TestStepExpectsContact(t *testing.T) {
	cases := []struct {
		name string
		step Step
		want bool
	}{
		{"free-space move", Step{PoseName: "approach"}, false},
		{"explicit flag", Step{PoseName: "tamper_activate", ExpectsContact: true}, true},
		{"declares allowed collisions", Step{PoseName: "coffee_in", AllowedCollisions: []AllowedCollision{{Frame1: "a", Frame2: "b"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.step.expectsContact(); got != tc.want {
				t.Errorf("expectsContact() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsCollisionError(t *testing.T) {
	if isCollisionError(nil) {
		t.Error("nil error must not be a collision error")
	}
	if isCollisionError(errors.New("some unrelated failure")) {
		t.Error("unrelated error must not be a collision error")
	}
	wrapped := errors.New("execute circular revolution 2: collision caused overcurrent: ensure robot is clear")
	if !isCollisionError(wrapped) {
		t.Error("expected collision-overcurrent error to be detected")
	}
}

func newCollisionTestService(t *testing.T, sensitivity int) *beanjaminCoffee {
	t.Helper()
	s := &beanjaminCoffee{
		logger: logging.NewTestLogger(t),
		cfg:    &Config{FreeMoveCollisionSensitivity: sensitivity},
	}
	s.faultReason.Store("")
	return s
}

func TestCollisionExtra_DisabledIsNil(t *testing.T) {
	s := newCollisionTestService(t, 0)
	if extra := s.collisionExtra(Step{PoseName: "approach"}); extra != nil {
		t.Errorf("feature disabled must produce nil extra, got %v", extra)
	}
}

func TestCollisionExtra_FreeSpaceAndContact(t *testing.T) {
	s := newCollisionTestService(t, 3)

	// Free-space move carries the protective level.
	if extra := s.collisionExtra(Step{PoseName: "approach"}); extra[collisionSensitivityKey] != float64(3) {
		t.Errorf("free-space extra = %v, want %s=3", extra, collisionSensitivityKey)
	}
	// Explicit-contact move drops to 0.
	if extra := s.collisionExtra(Step{PoseName: "tamper_activate", ExpectsContact: true}); extra[collisionSensitivityKey] != float64(0) {
		t.Errorf("contact extra = %v, want %s=0", extra, collisionSensitivityKey)
	}
	// AllowedCollisions also classifies as contact → 0.
	contactStep := Step{PoseName: "coffee_in", AllowedCollisions: []AllowedCollision{{Frame1: "a", Frame2: "b"}}}
	if extra := s.collisionExtra(contactStep); extra[collisionSensitivityKey] != float64(0) {
		t.Errorf("allowed-collisions extra = %v, want %s=0", extra, collisionSensitivityKey)
	}
}

func TestHandleCollisionFault_SetsReasonAndClears(t *testing.T) {
	a := &inject.Arm{}
	var cleared bool
	a.DoFunc = func(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
		if v, ok := cmd[clearErrorKey].(bool); ok && v {
			cleared = true
		}
		return nil, nil
	}
	s := &beanjaminCoffee{
		logger: logging.NewTestLogger(t),
		cfg:    &Config{FreeMoveCollisionSensitivity: 3},
		arm:    a,
	}
	s.faultReason.Store("")

	s.handleCollisionFault(Step{PoseName: "coffee_approach"}, errors.New("collision caused overcurrent"))

	if reason, _ := s.faultReason.Load().(string); reason == "" {
		t.Error("expected faultReason to be set after a collision fault")
	}
	if !cleared {
		t.Error("expected clear_error to be issued")
	}
}

func TestValidate_FreeMoveCollisionSensitivity_Range(t *testing.T) {
	for _, v := range []int{-1, 6} {
		cfg := validBaseConfig()
		cfg.FreeMoveCollisionSensitivity = v
		if _, _, err := cfg.Validate(""); err == nil {
			t.Errorf("expected error for free_move_collision_sensitivity=%d", v)
		}
	}
	for _, v := range []int{0, 1, 5} {
		cfg := validBaseConfig()
		cfg.FreeMoveCollisionSensitivity = v
		if _, _, err := cfg.Validate(""); err != nil {
			t.Errorf("expected no error for free_move_collision_sensitivity=%d, got %v", v, err)
		}
	}
}
