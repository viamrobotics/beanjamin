package coffee

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/testutils/inject"

	"beanjamin/coffee/order"
)

// TestGrindStepsCollisionScope locks down which grind moves may reach through
// a grinder's button shield. The approach is free-planned from wherever the arm is,
// and an allowance covers the whole plan, so granting it there would let the
// planner route the traverse through the button the shield protects.
func TestGrindStepsCollisionScope(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	steps := s.grindSteps(filterPoseGrinderApproach, filterPoseGrinderActivate, filterPoseGrinderCollect, grinderButtonCollisions(order.BeansRegular))

	if len(steps) != 5 {
		t.Fatalf("grindSteps returned %d steps, want 5 (approach, activate, collect, rotate, retreat)", len(steps))
	}

	tests := []struct {
		name          string
		step          Step
		wantPose      string
		wantCircular  bool
		wantCollision bool
	}{
		{"approach", steps[0], filterPoseGrinderApproach, false, false},
		{"activate", steps[1], filterPoseGrinderActivate, false, true},
		{"collect", steps[2], filterPoseGrinderCollect, false, true},
		{"rotate", steps[3], filterPoseGrinderCollect, true, true},
		{"retreat", steps[4], filterPoseGrinderApproach, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.step.PoseName != tt.wantPose {
				t.Errorf("PoseName = %q, want %q", tt.step.PoseName, tt.wantPose)
			}
			if gotCircular := tt.step.CircularRadiusMm > 0; gotCircular != tt.wantCircular {
				t.Errorf("circular = %v, want %v", gotCircular, tt.wantCircular)
			}
			if gotCollision := len(tt.step.AllowedCollisions) > 0; gotCollision != tt.wantCollision {
				t.Errorf("has AllowedCollisions = %v, want %v", gotCollision, tt.wantCollision)
			}
			if tt.name != "approach" && tt.step.LinearConstraint == nil {
				t.Error("every move after the approach should be a straight line")
			}
		})
	}
}

// Without a separate collect pose the filter is already at the approach when
// it finishes collecting, so there is nothing to retreat from: the sequence is
// the original four moves.
func TestGrindStepsWithoutCollectPose(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	steps := s.grindSteps(filterPoseGrinderApproach, filterPoseGrinderActivate, filterPoseGrinderApproach, grinderButtonCollisions(order.BeansRegular))
	var got []string
	for _, st := range steps {
		got = append(got, st.PoseName)
	}
	want := []string{filterPoseGrinderApproach, filterPoseGrinderActivate, filterPoseGrinderApproach, filterPoseGrinderApproach}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

// optionalPoseSwitch answers get_pose_by_name for the poses it has and errors
// for the rest, as a multi-poses-execution-switch does for an unknown name.
func optionalPoseSwitch(name string, poses map[string]map[string]any) *inject.Switch {
	sw := inject.NewSwitch(name)
	sw.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
		poseName, _ := cmd["get_pose_by_name"].(string)
		if p, ok := poses[poseName]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("pose %q not found", poseName)
	}
	sw.GetNumberOfPositionsFunc = func(context.Context, map[string]any) (uint32, []string, error) {
		names := make([]string, 0, len(poses))
		for n := range poses {
			names = append(names, n)
		}
		return uint32(len(names)), names, nil
	}
	return sw
}

// A grinder collects at its collect pose when the switch has one, and at its
// approach pose otherwise — the behavior of every machine configured before
// collect poses existed.
func TestGrinderCollectFallsBackToApproach(t *testing.T) {
	at := map[string]any{"x": 10.0, "y": -630.0, "z": 383.0, "o_z": 1.0}
	s := &beanjaminCoffee{
		cfg:      &Config{BeanOptions: []BeanOption{{Name: "decaf"}, {Name: "light_roast"}}},
		logger:   logging.NewTestLogger(t),
		filterSw: optionalPoseSwitch("filter", map[string]map[string]any{"light_roast_grinder_collect": at}),
	}
	if err := s.resolveGrinderCollectPoses(context.Background()); err != nil {
		t.Fatal(err)
	}
	for beans, want := range map[string]string{
		"regular":     "grinder_approach",
		"decaf":       "decaf_grinder_approach",
		"light_roast": "light_roast_grinder_collect",
	} {
		if got := s.grinderCollect(beans); got != want {
			t.Errorf("grinderCollect(%q) = %q, want %q", beans, got, want)
		}
	}

	// A collect pose that resolves to the origin is a placeholder, not a pose.
	s.filterSw = optionalPoseSwitch("filter", map[string]map[string]any{"grinder_collect": {"o_z": 1.0}})
	if err := s.resolveGrinderCollectPoses(context.Background()); err == nil || !strings.Contains(err.Error(), "grinder_collect") {
		t.Errorf("want a zero-position error naming grinder_collect, got %v", err)
	}
}

// TestGrinderNames pins how each beans' grinder is found: the regular grinder
// keeps its original pose and shield names, and every other grinder's are
// derived from its beans, so a new grinder is config plus poses, no code.
func TestGrinderNames(t *testing.T) {
	for _, tt := range []struct {
		beans, approach, activate, shield, action string
	}{
		{order.BeansRegular, "grinder_approach", "grinder_activate", "grinder-button-shield", "grind_coffee"},
		{"decaf", "decaf_grinder_approach", "decaf_grinder_activate", "decaf-grinder-button-shield", "grind_decaf"},
		{"light_roast", "light_roast_grinder_approach", "light_roast_grinder_activate", "light-roast-grinder-button-shield", "grind_light_roast"},
	} {
		if got, want := grinderCollectPose(tt.beans), strings.Replace(tt.approach, "approach", "collect", 1); got != want {
			t.Errorf("grinderCollectPose(%q) = %q, want %q", tt.beans, got, want)
		}
		approach, activate := grinderPoses(tt.beans)
		if approach != tt.approach || activate != tt.activate {
			t.Errorf("grinderPoses(%q) = %q, %q; want %q, %q", tt.beans, approach, activate, tt.approach, tt.activate)
		}
		if got := grinderShieldFrame(tt.beans); got != tt.shield {
			t.Errorf("grinderShieldFrame(%q) = %q, want %q", tt.beans, got, tt.shield)
		}
		if got := grinderButtonCollisions(tt.beans)[0].Frame2; got != tt.shield {
			t.Errorf("grinderButtonCollisions(%q) shields %q, want %q", tt.beans, got, tt.shield)
		}
		if got := grindActionName(tt.beans); got != tt.action {
			t.Errorf("grindActionName(%q) = %q, want %q", tt.beans, got, tt.action)
		}
	}
}

// Each configured grinder gets its own execute_action, and only configured
// ones: the unknown-action error lists what the operator can really run.
func TestGrindActionsFollowBeanOptions(t *testing.T) {
	plain := (&beanjaminCoffee{cfg: &Config{}}).actionFuncs()
	if _, ok := plain["grind_decaf"]; ok {
		t.Error("grind_decaf offered without a decaf grinder")
	}
	if _, ok := plain["grind_coffee"]; !ok {
		t.Error("grind_coffee should always be offered")
	}
	beans := (&beanjaminCoffee{cfg: &Config{BeanOptions: []BeanOption{{Name: "decaf"}, {Name: "light_roast"}}}}).actionFuncs()
	for _, name := range []string{"grind_coffee", "grind_decaf", "grind_light_roast"} {
		if _, ok := beans[name]; !ok {
			t.Errorf("%s missing with bean_options set", name)
		}
	}
}

// An extra grinder's shield must be in the frame system under the name the
// grind allows a collision with, or the service refuses to start.
func TestCheckGrinderShields(t *testing.T) {
	fs := referenceframe.NewEmptyFrameSystem("test")
	for _, name := range []string{"grinder-button-shield", "decaf-grinder-button-shield", "lightroast-grinder-button-shield"} {
		f, err := referenceframe.NewStaticFrame(name, spatialmath.NewZeroPose())
		if err != nil {
			t.Fatal(err)
		}
		if err := fs.AddFrame(f, fs.World()); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkGrinderShields(fs, []string{"decaf"}); err != nil {
		t.Errorf("decaf shield present, got %v", err)
	}
	err := checkGrinderShields(fs, []string{"decaf", "light_roast"})
	if err == nil || !strings.Contains(err.Error(), "light-roast-grinder-button-shield") ||
		!strings.Contains(err.Error(), "disabled") || !strings.Contains(err.Error(), `prefix "light-roast"`) {
		t.Errorf("want an error naming the missing light-roast shield and its prefix, got %v", err)
	}
	if err := checkGrinderShields(fs, nil); err != nil {
		t.Errorf("no extra grinders, got %v", err)
	}
}
