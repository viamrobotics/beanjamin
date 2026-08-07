package coffee

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/spatialmath"
)

func TestComputePivotPoses_StepCount(t *testing.T) {
	// Two poses 45° apart around the Z axis, degreesPerStep=5 → ceil(45/5)=9 steps, 10 poses total.
	start := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 0},
	)
	end := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 45},
	)

	poses := computePivotPoses(logging.NewTestLogger(t), start, end, 5)

	if len(poses) != 10 {
		t.Errorf("expected 10 poses (9 steps + start), got %d", len(poses))
	}
}

func TestComputePivotPoses_Endpoints(t *testing.T) {
	start := spatialmath.NewPose(
		r3.Vector{X: 10, Y: 20, Z: 30},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 0},
	)
	end := spatialmath.NewPose(
		r3.Vector{X: 50, Y: 60, Z: 70},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 30},
	)

	poses := computePivotPoses(logging.NewTestLogger(t), start, end, 5)

	// First pose should match start position.
	first := poses[0]
	if dist := first.Point().Sub(start.Point()).Norm(); dist > 0.01 {
		t.Errorf("first pose position differs from start by %.4f mm", dist)
	}

	// Last pose should match end position.
	last := poses[len(poses)-1]
	if dist := last.Point().Sub(end.Point()).Norm(); dist > 0.01 {
		t.Errorf("last pose position differs from end by %.4f mm", dist)
	}

	// Last pose orientation should match end orientation.
	diff := spatialmath.OrientationBetween(last.Orientation(), end.Orientation())
	angleDeg := diff.AxisAngles().Theta * 180.0 / math.Pi
	if angleDeg > 0.1 {
		t.Errorf("last pose orientation differs from end by %.4f°", angleDeg)
	}
}

func TestComputePivotPoses_MonotonicRotation(t *testing.T) {
	start := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 0},
	)
	end := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 45},
	)

	poses := computePivotPoses(logging.NewTestLogger(t), start, end, 5)

	prevAngle := 0.0
	for i := 1; i < len(poses); i++ {
		diff := spatialmath.OrientationBetween(start.Orientation(), poses[i].Orientation())
		angle := diff.AxisAngles().Theta * 180.0 / math.Pi
		if angle < prevAngle-0.01 {
			t.Errorf("rotation not monotonic at step %d: %.4f° < previous %.4f°", i, angle, prevAngle)
		}
		prevAngle = angle
	}

	// Final angle should be close to 45°.
	if math.Abs(prevAngle-45.0) > 0.1 {
		t.Errorf("final rotation angle %.4f° differs from expected 45°", prevAngle)
	}
}

func TestComputePivotPoses_OrientationVectorChange(t *testing.T) {
	// Fixed Theta, but the orientation vector (OX, OY, OZ) changes direction.
	// This tilts the local Z-axis from pointing up (0,0,1) toward the Y-axis (0,1,0)
	// while keeping the same spin around it.
	start := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 20},
	)
	end := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 1, OZ: 0, Theta: 20},
	)

	// Derive expected step count from the actual rotation angle.
	diff := spatialmath.OrientationBetween(start.Orientation(), end.Orientation())
	totalDeg := diff.AxisAngles().Theta * 180.0 / math.Pi
	degreesPerStep := 5.0
	expectedSteps := int(math.Round(totalDeg / degreesPerStep))
	t.Logf("OV change with fixed Theta=20°: total rotation = %.2f°, expected %d steps", totalDeg, expectedSteps)

	poses := computePivotPoses(logging.NewTestLogger(t), start, end, degreesPerStep)

	if len(poses) != expectedSteps+1 {
		t.Errorf("expected %d poses (%d steps + start), got %d", expectedSteps+1, expectedSteps, len(poses))
	}

	// First pose should match start.
	if dist := poses[0].Point().Sub(start.Point()).Norm(); dist > 0.01 {
		t.Errorf("first pose position differs from start by %.4f mm", dist)
	}

	// Last pose orientation should match end.
	lastDiff := spatialmath.OrientationBetween(poses[len(poses)-1].Orientation(), end.Orientation())
	lastAngle := lastDiff.AxisAngles().Theta * 180.0 / math.Pi
	if lastAngle > 0.1 {
		t.Errorf("last pose orientation differs from end by %.4f°", lastAngle)
	}

	// Rotation from start should increase monotonically.
	prevAngle := 0.0
	for i := 1; i < len(poses); i++ {
		d := spatialmath.OrientationBetween(start.Orientation(), poses[i].Orientation())
		angle := d.AxisAngles().Theta * 180.0 / math.Pi
		if angle < prevAngle-0.01 {
			t.Errorf("rotation not monotonic at step %d: %.4f° < previous %.4f°", i, angle, prevAngle)
		}
		prevAngle = angle
	}

	if math.Abs(prevAngle-totalDeg) > 0.1 {
		t.Errorf("final rotation angle %.4f° differs from expected %.4f°", prevAngle, totalDeg)
	}
}

// TestComputePivotPoses_NegativeAxisAngle guards a sign bug: for some rotations
// OrientationBetween(...).AxisAngles() returns the (-axis, -θ) form, so the raw
// Theta is negative. computePivotPoses must use the angle's magnitude — otherwise
// max(1, round(negativeDegrees/step)) collapses to a single step and the pivot
// degenerates into one straight-to-goal waypoint. These are the espresso-pour
// claw orientations, whose relative rotation reports a negative Theta (~ -106.7°).
func TestComputePivotPoses_NegativeAxisAngle(t *testing.T) {
	start := spatialmath.NewPoseFromOrientation(
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 1, OZ: 0, Theta: -180},
	)
	end := spatialmath.NewPoseFromOrientation(
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: -0.3, OZ: -1, Theta: 0},
	)

	rawTheta := spatialmath.OrientationBetween(start.Orientation(), end.Orientation()).AxisAngles().Theta
	if rawTheta >= 0 {
		t.Fatalf("test premise broken: expected a negative raw AxisAngles().Theta, got %.4f rad", rawTheta)
	}

	const degreesPerStep = 5.0
	expectedSteps := int(math.Round(math.Abs(rawTheta) * 180.0 / math.Pi / degreesPerStep))

	// Both directions must produce intermediate waypoints, not a single goal.
	for _, tc := range []struct {
		name       string
		start, end spatialmath.Pose
	}{
		{"pour", start, end},
		{"upright-return", end, start},
	} {
		poses := computePivotPoses(logging.NewTestLogger(t), tc.start, tc.end, degreesPerStep)
		if len(poses) < 3 {
			t.Errorf("%s: pivot degenerated to %d poses (%d waypoints); expected ~%d steps",
				tc.name, len(poses), len(poses)-1, expectedSteps)
		}
		if len(poses) != expectedSteps+1 {
			t.Errorf("%s: expected %d poses (%d steps + start), got %d", tc.name, expectedSteps+1, expectedSteps, len(poses))
		}
	}
}

// angleBetweenDegs is the magnitude of the rotation carrying a to b.
func angleBetweenDegs(a, b spatialmath.Orientation) float64 {
	return math.Abs(spatialmath.OrientationBetween(a, b).AxisAngles().Theta) * 180 / math.Pi
}

func TestPivotOvershootPose_ContinuesPastEnd(t *testing.T) {
	pt := r3.Vector{X: 100, Y: 200, Z: 300}
	start := spatialmath.NewPose(pt, &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 0})
	end := spatialmath.NewPose(pt, &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 30})

	over, err := pivotOvershootPose(start, end, 8)
	if err != nil {
		t.Fatalf("pivotOvershootPose: %v", err)
	}

	// The overshoot must extend the arc, not fold back onto it: 38° from the
	// start and 8° past the end.
	if got := angleBetweenDegs(start.Orientation(), over.Orientation()); math.Abs(got-38) > 0.01 {
		t.Errorf("start→overshoot = %.3f°, want 38", got)
	}
	if got := angleBetweenDegs(end.Orientation(), over.Orientation()); math.Abs(got-8) > 0.01 {
		t.Errorf("end→overshoot = %.3f°, want 8", got)
	}
	if d := over.Point().Sub(pt).Norm(); d > 1e-9 {
		t.Errorf("overshoot moved the pivot point by %.6f mm, want 0", d)
	}
}

// TestPivotOvershootPose_NegativeAxisAngle guards the same sign trap
// TestComputePivotPoses_NegativeAxisAngle covers, on the overshoot path: these
// orientations report the (-axis, -θ) form, so an un-normalized axis turns a
// positive overshoot into a rotation back toward the start — unwinding a
// bayonet the arm is trying to seat, which is the exact opposite of the intent.
func TestPivotOvershootPose_NegativeAxisAngle(t *testing.T) {
	start := spatialmath.NewPoseFromOrientation(
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 1, OZ: 0, Theta: -180},
	)
	end := spatialmath.NewPoseFromOrientation(
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: -0.3, OZ: -1, Theta: 0},
	)

	rawTheta := spatialmath.OrientationBetween(start.Orientation(), end.Orientation()).AxisAngles().Theta
	if rawTheta >= 0 {
		t.Fatalf("test premise broken: expected a negative raw AxisAngles().Theta, got %.4f rad", rawTheta)
	}
	totalDegs := math.Abs(rawTheta) * 180 / math.Pi

	over, err := pivotOvershootPose(start, end, 8)
	if err != nil {
		t.Fatalf("pivotOvershootPose: %v", err)
	}

	if got := angleBetweenDegs(start.Orientation(), over.Orientation()); math.Abs(got-(totalDegs+8)) > 0.01 {
		t.Errorf("start→overshoot = %.3f°, want %.3f (overshoot rotated back toward the start)", got, totalDegs+8)
	}
	if got := angleBetweenDegs(end.Orientation(), over.Orientation()); math.Abs(got-8) > 0.01 {
		t.Errorf("end→overshoot = %.3f°, want 8", got)
	}
}

// TestPivotOvershootPose_DegenerateRotation: with no authored rotation there is
// no axis to continue along, so the caller must get an error rather than a
// silent rotation about QuatToR4AA's hardcoded +Z fallback.
func TestPivotOvershootPose_DegenerateRotation(t *testing.T) {
	pt := r3.Vector{X: 100, Y: 200, Z: 300}
	pose := spatialmath.NewPose(pt, &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 0})

	if _, err := pivotOvershootPose(pose, pose, 8); err == nil {
		t.Error("expected an error for a zero-rotation pivot, got nil")
	}
}

// TestPivotOvershootPose_ArcGoesPastAndBack checks the waypoint list executePivot
// builds: it must climb past the goal and unwind onto it, ending exactly there.
func TestPivotOvershootPose_ArcGoesPastAndBack(t *testing.T) {
	pt := r3.Vector{X: 100, Y: 200, Z: 300}
	start := spatialmath.NewPose(pt, &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 0})
	end := spatialmath.NewPose(pt, &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 30})

	over, err := pivotOvershootPose(start, end, 8)
	if err != nil {
		t.Fatalf("pivotOvershootPose: %v", err)
	}

	logger := logging.NewTestLogger(t)
	var poses []spatialmath.Pose
	segStart := start
	for _, target := range []spatialmath.Pose{over, end} {
		seg := computePivotPoses(logger, segStart, target, 5)
		poses = append(poses, seg[1:]...)
		segStart = target
	}

	var maxDegs float64
	for _, p := range poses {
		if d := angleBetweenDegs(start.Orientation(), p.Orientation()); d > maxDegs {
			maxDegs = d
		}
	}
	if math.Abs(maxDegs-38) > 0.01 {
		t.Errorf("arc peaked at %.3f° from start, want 38", maxDegs)
	}
	last := poses[len(poses)-1]
	if d := angleBetweenDegs(end.Orientation(), last.Orientation()); d > 0.01 {
		t.Errorf("arc ended %.3f° off the goal, want 0", d)
	}
}
