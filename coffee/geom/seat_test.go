package geom

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/spatialmath"
)

func TestSeatOnSurface(t *testing.T) {
	// A table whose top face is Z=700 under the origin.
	boxes := []SurfaceBox{{minX: -100, maxX: 100, minY: -100, maxY: 100, topZ: 700}}
	const clearance = 1.0

	tests := []struct {
		name    string
		center  r3.Vector
		wantDz  float64
		wantOK  bool
		wantPos r3.Vector
	}{
		// A 40 mm-tall box: its bottom sits 20 mm below its center.
		{name: "hanging above the table drops onto it", center: r3.Vector{X: 10, Y: -5, Z: 750}, wantDz: -29, wantOK: true, wantPos: r3.Vector{X: 10, Y: -5, Z: 721}},
		{name: "sunk into the table rises onto it", center: r3.Vector{Z: 710}, wantDz: 11, wantOK: true, wantPos: r3.Vector{Z: 721}},
		{name: "nothing beneath it stays put", center: r3.Vector{X: 300, Z: 750}, wantOK: false, wantPos: r3.Vector{X: 300, Z: 750}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			box, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(tc.center), r3.Vector{X: 30, Y: 30, Z: 40}, "glass")
			if err != nil {
				t.Fatalf("new box: %v", err)
			}
			got, dz, ok := SeatOnSurface(boxes, box, clearance)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			const tol = 1e-6
			if math.Abs(dz-tc.wantDz) > tol {
				t.Fatalf("dz = %g, want %g", dz, tc.wantDz)
			}
			if p := got.Pose().Point(); p.Sub(tc.wantPos).Norm() > tol {
				t.Fatalf("center = %v, want %v", p, tc.wantPos)
			}
		})
	}
}

func TestSeatOnSurfaceNotABox(t *testing.T) {
	boxes := []SurfaceBox{{minX: -100, maxX: 100, minY: -100, maxY: 100, topZ: 700}}
	ball, err := spatialmath.NewSphere(spatialmath.NewPoseFromPoint(r3.Vector{Z: 750}), 20, "ball")
	if err != nil {
		t.Fatalf("new sphere: %v", err)
	}
	if _, _, ok := SeatOnSurface(boxes, ball, 1); ok {
		t.Fatalf("a non-box geometry must not be seated")
	}
}
