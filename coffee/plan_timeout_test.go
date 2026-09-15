package coffee

import (
	"strings"
	"testing"
	"time"

	"go.viam.com/rdk/motionplan/armplanning"
)

func TestWithPlanTimeout(t *testing.T) {
	t.Run("nil options get armplanning defaults under our timeout", func(t *testing.T) {
		opts := withPlanTimeout(nil)
		if opts == nil {
			t.Fatal("withPlanTimeout(nil) = nil, want defaults")
		}
		if opts.Timeout != motionPlanTimeout.Seconds() {
			t.Errorf("Timeout = %v, want %v", opts.Timeout, motionPlanTimeout.Seconds())
		}
		if want := armplanning.NewBasicPlannerOptions().CollisionBufferMM; opts.CollisionBufferMM != want {
			t.Errorf("CollisionBufferMM = %v, want armplanning's default %v", opts.CollisionBufferMM, want)
		}
	})

	t.Run("caller options keep their settings but lose the 300s default", func(t *testing.T) {
		opts := withPlanTimeout(freeMovePlannerOptions())
		if opts.CollisionBufferMM != freeMoveCollisionBufferMM {
			t.Errorf("CollisionBufferMM = %v, want %v", opts.CollisionBufferMM, freeMoveCollisionBufferMM)
		}
		if opts.Timeout != motionPlanTimeout.Seconds() {
			t.Errorf("Timeout = %v, want %v", opts.Timeout, motionPlanTimeout.Seconds())
		}
	})

	t.Run("timeout is at most fifteen seconds", func(t *testing.T) {
		if motionPlanTimeout > 15*time.Second {
			t.Errorf("motionPlanTimeout = %v, want no more than 15s", motionPlanTimeout)
		}
	})
}

func TestIncompletePlanErr(t *testing.T) {
	tests := []struct {
		name    string
		meta    *armplanning.PlanMeta
		goals   int
		wantErr bool
	}{
		{name: "all goals solved", meta: &armplanning.PlanMeta{GoalsProcessed: 3}, goals: 3},
		{name: "no goals requested", meta: &armplanning.PlanMeta{}, goals: 0},
		{name: "nil meta", meta: nil, goals: 3},
		{name: "prefix of a multi-goal plan", meta: &armplanning.PlanMeta{GoalsProcessed: 2}, goals: 5, wantErr: true},
		{name: "nothing solved", meta: &armplanning.PlanMeta{}, goals: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := incompletePlanErr(tt.meta, tt.goals)
			if (err != nil) != tt.wantErr {
				t.Fatalf("incompletePlanErr = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "goals") {
				t.Errorf("error %q should name how many goals were solved", err)
			}
		})
	}
}

func TestPlanDuration(t *testing.T) {
	if got := planDuration(nil); got != 0 {
		t.Errorf("planDuration(nil) = %v, want 0", got)
	}
	if got, want := planDuration(&armplanning.PlanMeta{Duration: 1234567 * time.Nanosecond}), time.Millisecond; got != want {
		t.Errorf("planDuration = %v, want %v", got, want)
	}
}
