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

const vecTolMm = 1e-6

// withStagedGlass adds an 80×140 mm staged-glass obstacle centered at centroid
// to s's frame system, as stageGlassAsObstacle would.
func withStagedGlass(t *testing.T, s *beanjaminCoffee, centroid spatialmath.Pose) {
	t.Helper()
	box, err := spatialmath.NewBox(centroid, r3.Vector{X: 80, Y: 80, Z: 140}, pickupLabelGlass)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := referenceframe.NewStaticFrameWithGeometry(stagedGlassFrameName, spatialmath.NewZeroPose(), box)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cachedFS.AddFrame(frame, s.cachedFS.World()); err != nil {
		t.Fatal(err)
	}
	s.stagedGlassPlaced = true
}

// newPourTestCoffee returns a service holding a cup: upright above the glass on
// the approach, tilted 45° toward +X to pour.
func newPourTestCoffee(t *testing.T) *beanjaminCoffee {
	s := &beanjaminCoffee{
		cfg: &Config{
			PourApproachRelativePose: &RelativePose{X: 20, Z: 50, OZ: 1},
			PourRelativePose:         &RelativePose{X: 20, Z: 50, OX: 1, OZ: 1},
		},
		// The switch has no DoFunc: resolving a pour pose must never read it.
		clawsSw:  inject.NewSwitch("claws"),
		cachedFS: clawsStaticFS(t, spatialmath.NewZeroPose()),
		logger:   logging.NewTestLogger(t),
	}
	if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}
	return s
}

func TestGlassRimCenter(t *testing.T) {
	got := glassRimCenter(spatialmath.NewPoseFromPoint(r3.Vector{X: 560, Y: -200, Z: 290}), 140)
	if want := (r3.Vector{X: 560, Y: -200, Z: 360}); got.Sub(want).Norm() > vecTolMm {
		t.Errorf("rim center = %v, want %v", got, want)
	}
}

func TestResolvePoseGlassRelative(t *testing.T) {
	s := newPourTestCoffee(t)
	withStagedGlass(t, s, spatialmath.NewPoseFromPoint(r3.Vector{X: 560, Y: -200, Z: 290}))
	rim := r3.Vector{X: 560, Y: -200, Z: 360}

	approach, err := s.resolvePose(context.Background(), s.clawsSw, clawPosePourApproach)
	if err != nil {
		t.Fatalf("resolve pour_approach: %v", err)
	}
	pour, err := s.resolvePose(context.Background(), s.clawsSw, clawPosePour)
	if err != nil {
		t.Fatalf("resolve pour: %v", err)
	}
	if want := rim.Add(r3.Vector{X: 20, Z: 50}); approach.pose.Point().Sub(want).Norm() > vecTolMm {
		t.Errorf("pour_approach at %v, want %v", approach.pose.Point(), want)
	}
	// The pivot needs both ends on the same point.
	if d := approach.pose.Point().Sub(pour.pose.Point()).Norm(); d > vecTolMm {
		t.Errorf("pour_approach and pour differ by %.3f mm", d)
	}
	wantOrient := relativePoseToSpatial(s.cfg.PourRelativePose).Orientation()
	if !spatialmath.OrientationAlmostEqual(pour.pose.Orientation(), wantOrient) {
		t.Errorf("pour orientation = %v, want the configured %v", pour.pose.Orientation(), wantOrient)
	}
	// The pose describes the held container, not the gripper.
	if pour.componentName != heldItemFrameName || pour.refFrame != referenceframe.World {
		t.Errorf("pour moves %q in %q, want %q in world", pour.componentName, pour.refFrame, heldItemFrameName)
	}
}

func TestResolvePoseGlassRelativeFailures(t *testing.T) {
	t.Run("no glass staged", func(t *testing.T) {
		s := newPourTestCoffee(t)
		if _, err := s.resolvePose(context.Background(), s.clawsSw, clawPosePour); err == nil {
			t.Error("resolved pour with no staged glass, want an error")
		}
	})
	t.Run("nothing held", func(t *testing.T) {
		s := newPourTestCoffee(t)
		withStagedGlass(t, s, spatialmath.NewPoseFromPoint(r3.Vector{X: 560, Y: -200, Z: 290}))
		s.detachHeldGeometry()
		if _, err := s.resolvePose(context.Background(), s.clawsSw, clawPosePour); err == nil {
			t.Error("resolved pour with no container modeled in the gripper, want an error")
		}
	})
	t.Run("pair not configured", func(t *testing.T) {
		s := newPourTestCoffee(t)
		withStagedGlass(t, s, spatialmath.NewPoseFromPoint(r3.Vector{X: 560, Y: -200, Z: 290}))
		if _, err := s.resolvePose(context.Background(), s.clawsSw, clawPoseMilkPour); err == nil {
			t.Error("resolved milk_pour without milk_pour_relative_pose, want an error")
		}
	})
}

func TestValidatePourPair(t *testing.T) {
	p := func(x, y, z float64) *RelativePose { return &RelativePose{X: x, Y: y, Z: z, OX: 1, Theta: 90} }
	tests := []struct {
		name     string
		approach *RelativePose
		pour     *RelativePose
		wantErr  bool
	}{
		{"both at the same point", p(10, 0, 50), p(10, 0, 50), false},
		{"neither", nil, nil, true},
		{"only approach", p(10, 0, 50), nil, true},
		{"only pour", nil, p(10, 0, 50), true},
		{"different points", p(10, 0, 50), p(10, 0, 55), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePourPair("coffee", "pour", tt.approach, tt.pour)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
