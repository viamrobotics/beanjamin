package beanjamin

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/spatialmath"
)

func TestComputeCircularPoses_Count(t *testing.T) {
	center := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 0},
	)

	poses := computeCircularPoses(center, 5.0, 8)
	if len(poses) != 8 {
		t.Errorf("expected 8 poses, got %d", len(poses))
	}
}

func TestComputeCircularPoses_Radius(t *testing.T) {
	center := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 0},
	)
	radius := 5.0

	poses := computeCircularPoses(center, radius, 8)

	for i, pose := range poses {
		dist := pose.Point().Sub(center.Point()).Norm()
		if math.Abs(dist-radius) > 0.001 {
			t.Errorf("pose %d: distance from center = %.4f mm, expected %.1f mm", i, dist, radius)
		}
	}
}

func TestComputeCircularPoses_ConstantOrientation(t *testing.T) {
	center := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 45},
	)

	poses := computeCircularPoses(center, 5.0, 8)

	for i, pose := range poses {
		diff := spatialmath.OrientationBetween(center.Orientation(), pose.Orientation())
		angleDeg := diff.AxisAngles().Theta * 180.0 / math.Pi
		if angleDeg > 0.01 {
			t.Errorf("pose %d: orientation differs from center by %.4f°", i, angleDeg)
		}
	}
}

func TestComputeCircularPoses_ConstantZ(t *testing.T) {
	center := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 0},
	)

	poses := computeCircularPoses(center, 5.0, 8)

	for i, pose := range poses {
		if math.Abs(pose.Point().Z-center.Point().Z) > 0.001 {
			t.Errorf("pose %d: Z = %.4f, expected %.4f", i, pose.Point().Z, center.Point().Z)
		}
	}
}

func TestComputeCircularPoses_EvenlySpaced(t *testing.T) {
	center := spatialmath.NewPose(
		r3.Vector{X: 0, Y: 0, Z: 0},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 0},
	)
	pointsPerRev := 8

	poses := computeCircularPoses(center, 5.0, pointsPerRev)

	// All consecutive pairs should be the same distance apart.
	expectedArcDist := 2 * 5.0 * math.Sin(math.Pi/float64(pointsPerRev))
	for i := range len(poses) {
		next := (i + 1) % len(poses)
		dist := poses[i].Point().Sub(poses[next].Point()).Norm()
		if math.Abs(dist-expectedArcDist) > 0.001 {
			t.Errorf("distance between pose %d and %d = %.4f mm, expected %.4f mm", i, next, dist, expectedArcDist)
		}
	}
}

func TestComputeCircularPoses_FirstPointOnXAxis(t *testing.T) {
	center := spatialmath.NewPose(
		r3.Vector{X: 100, Y: 200, Z: 300},
		&spatialmath.OrientationVectorDegrees{OX: 0, OY: 0, OZ: 1, Theta: 0},
	)
	radius := 4.0

	poses := computeCircularPoses(center, radius, 8)

	// First point should be at (center.X + radius, center.Y, center.Z).
	first := poses[0].Point()
	if math.Abs(first.X-(100+radius)) > 0.001 || math.Abs(first.Y-200) > 0.001 {
		t.Errorf("first pose at (%.4f, %.4f), expected (%.1f, 200.0)", first.X, first.Y, 100+radius)
	}
}
