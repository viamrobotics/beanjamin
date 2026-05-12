package beanjamin

import (
	"fmt"
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/spatialmath"
)

func vecAlmostEqual(a, b r3.Vector, eps float64) bool {
	return math.Abs(a.X-b.X) < eps && math.Abs(a.Y-b.Y) < eps && math.Abs(a.Z-b.Z) < eps
}

func TestComputeShelfTileCenters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pose    spatialmath.Pose
		dims    r3.Vector
		spacing float64
		margin  float64
		want    []r3.Vector
	}{
		{
			name:    "x-long axis identity pose",
			pose:    spatialmath.NewZeroPose(),
			dims:    r3.Vector{X: 800, Y: 200, Z: 20},
			spacing: 120, margin: 60,
			// usable = 800 - 120 = 680. N = floor(680/120)+1 = 6. span=5*120=600. centers from -300 to +300.
			want: []r3.Vector{
				{X: -300, Y: 0, Z: 10},
				{X: -180, Y: 0, Z: 10},
				{X: -60, Y: 0, Z: 10},
				{X: 60, Y: 0, Z: 10},
				{X: 180, Y: 0, Z: 10},
				{X: 300, Y: 0, Z: 10},
			},
		},
		{
			name:    "y-long axis identity pose",
			pose:    spatialmath.NewZeroPose(),
			dims:    r3.Vector{X: 200, Y: 800, Z: 20},
			spacing: 120, margin: 60,
			want: []r3.Vector{
				{X: 0, Y: -300, Z: 10},
				{X: 0, Y: -180, Z: 10},
				{X: 0, Y: -60, Z: 10},
				{X: 0, Y: 60, Z: 10},
				{X: 0, Y: 180, Z: 10},
				{X: 0, Y: 300, Z: 10},
			},
		},
		{
			name: "x-long shelf translated and rotated 90deg around Z",
			pose: spatialmath.NewPose(
				r3.Vector{X: 1000, Y: 500, Z: 100},
				&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 90},
			),
			dims:    r3.Vector{X: 800, Y: 200, Z: 20},
			spacing: 120, margin: 60,
			// 90deg around Z maps local +X -> world +Y. Local Z stays world Z.
			// shelf center is at (1000, 500, 100); top face Z = 110.
			// Local x in [-300..300] -> world y in [500-300..500+300] = [200..800].
			want: []r3.Vector{
				{X: 1000, Y: 200, Z: 110},
				{X: 1000, Y: 320, Z: 110},
				{X: 1000, Y: 440, Z: 110},
				{X: 1000, Y: 560, Z: 110},
				{X: 1000, Y: 680, Z: 110},
				{X: 1000, Y: 800, Z: 110},
			},
		},
		{
			name:    "single tile fits when usable < spacing",
			pose:    spatialmath.NewZeroPose(),
			dims:    r3.Vector{X: 150, Y: 100, Z: 10},
			spacing: 120, margin: 60,
			// usable = 150 - 120 = 30. N = floor(30/120)+1 = 1. span=0. tile at center.
			want: []r3.Vector{{X: 0, Y: 0, Z: 5}},
		},
		{
			name:    "no tiles when shelf shorter than 2x margin",
			pose:    spatialmath.NewZeroPose(),
			dims:    r3.Vector{X: 100, Y: 100, Z: 10},
			spacing: 120, margin: 60,
			// usable = 100 - 120 = -20. nil.
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeShelfTileCenters(tc.pose, tc.dims, tc.spacing, tc.margin)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %d %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if !vecAlmostEqual(got[i], tc.want[i], 1e-6) {
					t.Errorf("tile[%d]: got %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// makeCupSpheres builds zero-orientation cup sphere geometries at the given
// centers for collision testing. Radius mirrors a typical 40mm cup.
func makeCupSpheres(t *testing.T, centers []r3.Vector) []spatialmath.Geometry {
	t.Helper()
	const cupRadius = 40.0
	geos := make([]spatialmath.Geometry, len(centers))
	for i, c := range centers {
		g, err := spatialmath.NewSphere(spatialmath.NewPoseFromPoint(c), cupRadius, fmt.Sprintf("cup_%d", i))
		if err != nil {
			t.Fatalf("build cup sphere %d: %v", i, err)
		}
		geos[i] = g
	}
	return geos
}

func TestFirstFreeTile(t *testing.T) {
	t.Parallel()

	tiles := []r3.Vector{
		{X: 0, Y: 0, Z: 0},
		{X: 120, Y: 0, Z: 0},
		{X: 240, Y: 0, Z: 0},
	}

	// With cupRadius=40 and bufferMm=80, the effective exclusion distance
	// from tile center to cup center is 40+80 = 120mm. So a cup at tile N
	// also collides with tile N±1 at 120mm spacing.
	tests := []struct {
		name      string
		cups      []r3.Vector
		buffer    float64
		wantIdx   int
		wantPoint r3.Vector
		wantOK    bool
	}{
		{
			name:      "all free returns idx 0",
			cups:      nil,
			buffer:    80,
			wantIdx:   0,
			wantPoint: tiles[0],
			wantOK:    true,
		},
		{
			name:      "cup at tile 0 occupies tiles 0 and 1; idx 2 is first free",
			cups:      []r3.Vector{tiles[0]},
			buffer:    80,
			wantIdx:   2,
			wantPoint: tiles[2],
			wantOK:    true,
		},
		{
			name:      "cups at every tile leave no free tile",
			cups:      tiles,
			buffer:    80,
			wantIdx:   -1,
			wantPoint: r3.Vector{},
			wantOK:    false,
		},
		{
			name:      "cup well off-shelf does not occupy any tile",
			cups:      []r3.Vector{{X: 10000, Y: 10000, Z: 10000}},
			buffer:    80,
			wantIdx:   0,
			wantPoint: tiles[0],
			wantOK:    true,
		},
		{
			// With a 0 buffer the tile-point only collides if the cup
			// geometry actually overlaps the point. A cup at the exact tile
			// position overlaps tile 0; tile 1 (120mm away) is free.
			name:      "zero buffer only excludes overlapping cup",
			cups:      []r3.Vector{tiles[0]},
			buffer:    0,
			wantIdx:   1,
			wantPoint: tiles[1],
			wantOK:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cupGeoms := makeCupSpheres(t, tc.cups)
			idx, pt, ok, err := firstFreeTile(tiles, cupGeoms, tc.buffer)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if idx != tc.wantIdx || ok != tc.wantOK {
				t.Fatalf("idx/ok mismatch: got (%d, %v), want (%d, %v)", idx, ok, tc.wantIdx, tc.wantOK)
			}
			if ok && !vecAlmostEqual(pt, tc.wantPoint, 1e-6) {
				t.Errorf("point: got %v, want %v", pt, tc.wantPoint)
			}
		})
	}
}
