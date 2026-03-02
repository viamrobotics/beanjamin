package beanjamin

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
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

	poses := computePivotPoses(start, end, 5)

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

	poses := computePivotPoses(start, end, 5)

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

	poses := computePivotPoses(start, end, 5)

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

	poses := computePivotPoses(start, end, degreesPerStep)

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
