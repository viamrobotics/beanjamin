package coffee

import (
	"context"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/testutils/inject"
)

// poseSwitch returns a switch answering get_pose_by_name with poses, keyed by
// name in the switch's wire format.
func poseSwitch(t *testing.T, name string, poses map[string]map[string]any) *inject.Switch {
	t.Helper()
	sw := inject.NewSwitch(name)
	sw.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
		poseName, _ := cmd["get_pose_by_name"].(string)
		p, ok := poses[poseName]
		if !ok {
			t.Fatalf("switch %q asked for unexpected pose %q", name, poseName)
		}
		return p, nil
	}
	return sw
}

func TestResolvePoseHandleRelativeGrip(t *testing.T) {
	// The filter locked at (500, 0, 300), turned 90° about Z: the handle, 80 mm
	// out along the filter's +X, sits at (500, 80, 300) facing world +Y.
	filterSw := poseSwitch(t, "filter", map[string]map[string]any{
		filterPoseCoffeeLockedFinal: {
			"x": 500.0, "y": 0.0, "z": 300.0, "o_z": 1.0, "theta": 90.0,
			"reference_frame": referenceframe.World, "component_name": componentFilter,
		},
	})
	s := &beanjaminCoffee{
		cfg: &Config{
			FilterReleasedGripRelativePose:    &RelativePose{X: 10, Z: -40, OZ: 1},
			CoffeeLockedFinalGripRelativePose: &RelativePose{X: 10, Z: -5, OZ: 1},
		},
		filterSw: filterSw,
		// The claws switch has no DoFunc: the handoff must never read it.
		clawsSw:  inject.NewSwitch("claws"),
		cachedFS: filterOnArmFS(t),
		logger:   logging.NewTestLogger(t),
	}

	// The offsets are in the handle's axes, so their +X runs along world +Y.
	for _, tc := range []struct {
		poseName string
		want     r3.Vector
	}{
		{clawPoseFilterReleased, r3.Vector{X: 500, Y: 90, Z: 260}},
		{clawPoseCoffeeLockedFinal, r3.Vector{X: 500, Y: 90, Z: 295}},
	} {
		t.Run(tc.poseName, func(t *testing.T) {
			pd, err := s.resolvePose(context.Background(), s.clawsSw, tc.poseName)
			if err != nil {
				t.Fatalf("resolvePose: %v", err)
			}
			if pd.componentName != gripPoint || pd.refFrame != referenceframe.World {
				t.Errorf("goal frames = (%q in %q), want (%q in %q)", pd.componentName, pd.refFrame, gripPoint, referenceframe.World)
			}
			if pd.pose.Point().Sub(tc.want).Norm() > vecTolMm {
				t.Errorf("grip point = %v, want %v", pd.pose.Point(), tc.want)
			}
			wantOrient := spatialmath.NewPoseFromOrientation(&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 90}).Orientation()
			if !spatialmath.OrientationAlmostEqual(pd.pose.Orientation(), wantOrient) {
				t.Errorf("grip orientation = %v, want %v", pd.pose.Orientation().OrientationVectorDegrees(), wantOrient.OrientationVectorDegrees())
			}
		})
	}
}

func TestResolvePoseHandleRelativeGripMissingHandle(t *testing.T) {
	filterSw := poseSwitch(t, "filter", map[string]map[string]any{
		filterPoseCoffeeLockedFinal: {"x": 500.0, "o_z": 1.0, "component_name": componentFilter},
	})
	s := &beanjaminCoffee{
		cfg:      &Config{CoffeeLockedFinalGripRelativePose: &RelativePose{OZ: 1}},
		filterSw: filterSw,
		clawsSw:  inject.NewSwitch("claws"),
		cachedFS: clawsStaticFS(t, spatialmath.NewZeroPose()),
		logger:   logging.NewTestLogger(t),
	}
	if _, err := s.resolvePose(context.Background(), s.clawsSw, clawPoseCoffeeLockedFinal); err == nil {
		t.Fatal("resolvePose succeeded without a portafilter-handle frame")
	}
}
