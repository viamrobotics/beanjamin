package coffee

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// The arm waits out the brew after the moves before it, while the planner goes
// on to what follows.
func TestBrewWaitIsLeftToTheArm(t *testing.T) {
	s, _ := runCoffee(t)
	const brew = 300 * time.Millisecond
	s.cfg.BrewTimeSec = brew.Seconds()
	var r recorder
	ctx := context.Background()

	var started atomic.Int64
	s.handOffEffect(func() { started.Store(time.Now().UnixNano()) })
	begin := time.Now()
	if err := s.handOffBrewWait(ctx, context.Background(), "espresso", &started); err != nil {
		t.Fatal(err)
	}
	if err := s.handOff(ctx, r.move("grab cup")); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(begin); waited >= brew {
		t.Errorf("the planner waited %s for the brew", waited)
	}

	if err := s.settle(); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(begin); waited < brew {
		t.Errorf("the arm went on after %s, before the %s brew was done", waited, brew)
	}
	if got := r.got(); !slices.Equal(got, []string{"grab cup"}) {
		t.Errorf("ran %v, want the move after the brew wait", got)
	}
}
