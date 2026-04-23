package beanjamin

import (
	"math"
	"strings"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// buildTestFrameSystem returns a FrameSystem containing one SimpleModel named
// "test-arm" built from a serial chain of two revolute joints named "j0" and
// "j1", each with a wide initial limit. The inner function is kept small —
// tests only exercise limit overriding, not kinematics.
func buildTestFrameSystem(t *testing.T) *referenceframe.FrameSystem {
	t.Helper()

	wide := referenceframe.Limit{Min: -2 * math.Pi, Max: 2 * math.Pi}
	j0, err := referenceframe.NewRotationalFrame("j0", spatialmath.R4AA{Theta: 0, RX: 0, RY: 0, RZ: 1}, wide)
	if err != nil {
		t.Fatalf("new j0: %v", err)
	}
	j1, err := referenceframe.NewRotationalFrame("j1", spatialmath.R4AA{Theta: 0, RX: 0, RY: 0, RZ: 1}, wide)
	if err != nil {
		t.Fatalf("new j1: %v", err)
	}
	sm, err := referenceframe.NewSerialModel("test-arm", []referenceframe.Frame{j0, j1})
	if err != nil {
		t.Fatalf("new serial model: %v", err)
	}
	fs := referenceframe.NewEmptyFrameSystem("test")
	if err := fs.AddFrame(sm, fs.World()); err != nil {
		t.Fatalf("add frame: %v", err)
	}
	return fs
}

func TestApplyJointLimits_Nil(t *testing.T) {
	fs := buildTestFrameSystem(t)
	if err := applyJointLimits(logging.NewTestLogger(t), fs, nil); err != nil {
		t.Fatalf("nil overrides: %v", err)
	}
	dof := fs.Frame("test-arm").DoF()
	if dof[0].Min != -2*math.Pi {
		t.Errorf("j0 min changed: got %v, want %v", dof[0].Min, -2*math.Pi)
	}
}

func TestApplyJointLimits_ByIndex(t *testing.T) {
	fs := buildTestFrameSystem(t)
	err := applyJointLimits(logging.NewTestLogger(t), fs, map[string]map[string]JointLimitDegs{
		"test-arm": {"1": {MinDegs: -270, MaxDegs: 270}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	dof := fs.Frame("test-arm").DoF()
	// j0 should be untouched.
	if math.Abs(dof[0].Min-(-2*math.Pi)) > 1e-9 {
		t.Errorf("j0 min = %v, want %v", dof[0].Min, -2*math.Pi)
	}
	// j1 should be narrowed to ±270° (= ±3π/2 rad).
	want := 270 * math.Pi / 180.0
	if math.Abs(dof[1].Min-(-want)) > 1e-9 {
		t.Errorf("j1 min = %v, want %v", dof[1].Min, -want)
	}
	if math.Abs(dof[1].Max-want) > 1e-9 {
		t.Errorf("j1 max = %v, want %v", dof[1].Max, want)
	}
}

func TestApplyJointLimits_ByName(t *testing.T) {
	fs := buildTestFrameSystem(t)
	err := applyJointLimits(logging.NewTestLogger(t), fs, map[string]map[string]JointLimitDegs{
		"test-arm": {"j0": {MinDegs: -90, MaxDegs: 90}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	dof := fs.Frame("test-arm").DoF()
	want := math.Pi / 2
	if math.Abs(dof[0].Min-(-want)) > 1e-9 || math.Abs(dof[0].Max-want) > 1e-9 {
		t.Errorf("j0 limits = [%v, %v], want [%v, %v]", dof[0].Min, dof[0].Max, -want, want)
	}
}

func TestApplyJointLimits_UnknownFrame(t *testing.T) {
	fs := buildTestFrameSystem(t)
	err := applyJointLimits(logging.NewTestLogger(t), fs, map[string]map[string]JointLimitDegs{
		"nope": {"0": {MinDegs: -1, MaxDegs: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Fatalf("expected unknown-frame error, got %v", err)
	}
}

func TestApplyJointLimits_UnknownJoint(t *testing.T) {
	fs := buildTestFrameSystem(t)
	err := applyJointLimits(logging.NewTestLogger(t), fs, map[string]map[string]JointLimitDegs{
		"test-arm": {"99": {MinDegs: -1, MaxDegs: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "no joint matching") {
		t.Fatalf("expected unknown-joint error, got %v", err)
	}
}

func TestApplyJointLimits_InvalidRange(t *testing.T) {
	fs := buildTestFrameSystem(t)
	err := applyJointLimits(logging.NewTestLogger(t), fs, map[string]map[string]JointLimitDegs{
		"test-arm": {"0": {MinDegs: 10, MaxDegs: -10}},
	})
	if err == nil || !strings.Contains(err.Error(), "min_degs") {
		t.Fatalf("expected invalid-range error, got %v", err)
	}
}
