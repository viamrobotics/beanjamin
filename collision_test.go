package beanjamin

import (
	"context"
	"errors"
	"sync"
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

// recordingArm wraps inject.Arm and records every set_collision_sensitivity
// value it receives via DoCommand.
func recordingArm() (*inject.Arm, *[]float64) {
	var mu sync.Mutex
	var got []float64
	a := &inject.Arm{}
	a.DoFunc = func(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
		if v, ok := cmd[setCollisionSensitivityKey].(float64); ok {
			mu.Lock()
			got = append(got, v)
			mu.Unlock()
		}
		return nil, nil
	}
	return a, &got
}

func newCollisionTestService(t *testing.T, sensitivity int) (*beanjaminCoffee, *[]float64) {
	t.Helper()
	a, got := recordingArm()
	s := &beanjaminCoffee{
		logger: logging.NewTestLogger(t),
		cfg:    &Config{FreeMoveCollisionSensitivity: sensitivity},
		arm:    a,
	}
	s.currentSensitivity.Store(-1)
	s.faultReason.Store("")
	return s, got
}

func TestApplyCollisionSensitivity_DisabledIsNoOp(t *testing.T) {
	s, got := newCollisionTestService(t, 0)
	if err := s.applyCollisionSensitivity(context.Background(), Step{PoseName: "approach"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("feature disabled must not issue set_collision_sensitivity, got %v", *got)
	}
}

func TestApplyCollisionSensitivity_FreeSpaceThenContactThenSkip(t *testing.T) {
	s, got := newCollisionTestService(t, 3)
	ctx := context.Background()

	// Free-space move applies the protective level.
	if err := s.applyCollisionSensitivity(ctx, Step{PoseName: "approach"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A second free-space move is a no-op (already at the protective level).
	if err := s.applyCollisionSensitivity(ctx, Step{PoseName: "approach2"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A contact move drops to 0.
	if err := s.applyCollisionSensitivity(ctx, Step{PoseName: "tamper_activate", ExpectsContact: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Another contact move is a no-op (already at 0).
	if err := s.applyCollisionSensitivity(ctx, Step{PoseName: "grinder_activate", AllowedCollisions: []AllowedCollision{{Frame1: "a", Frame2: "b"}}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []float64{3, 0}
	if len(*got) != len(want) {
		t.Fatalf("expected DoCommand values %v, got %v", want, *got)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("DoCommand[%d] = %v, want %v", i, (*got)[i], want[i])
		}
	}
}

func TestHandleCollisionFault_SetsReasonClearsAndZeroes(t *testing.T) {
	a := &inject.Arm{}
	var cleared bool
	var sens []float64
	a.DoFunc = func(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
		if v, ok := cmd[clearErrorKey].(bool); ok && v {
			cleared = true
		}
		if v, ok := cmd[setCollisionSensitivityKey].(float64); ok {
			sens = append(sens, v)
		}
		return nil, nil
	}
	s := &beanjaminCoffee{
		logger: logging.NewTestLogger(t),
		cfg:    &Config{FreeMoveCollisionSensitivity: 3},
		arm:    a,
	}
	s.currentSensitivity.Store(3)
	s.faultReason.Store("")

	s.handleCollisionFault(Step{PoseName: "coffee_approach"}, errors.New("collision caused overcurrent"))

	if reason, _ := s.faultReason.Load().(string); reason == "" {
		t.Error("expected faultReason to be set after a collision fault")
	}
	if !cleared {
		t.Error("expected clear_error to be issued")
	}
	if len(sens) != 1 || sens[0] != 0 {
		t.Errorf("expected sensitivity reset to 0 after clear, got %v", sens)
	}
	if s.currentSensitivity.Load() != 0 {
		t.Errorf("expected currentSensitivity cached as 0, got %d", s.currentSensitivity.Load())
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
