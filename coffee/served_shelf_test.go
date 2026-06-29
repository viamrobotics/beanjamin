package coffee

import (
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

func TestServingAreaDropZOffset(t *testing.T) {
	fs := clawsStaticFS(t, spatialmath.NewZeroPose())
	s := heldGeomService(t, fs)

	// Nothing held: the fixed fallback offset (tuned for the cup).
	if got := s.servingAreaDropZOffset(); got != shelfDropZOffsetMm {
		t.Fatalf("fallback offset = %g, want %g", got, shelfDropZOffsetMm)
	}

	// Container held: half its height (the 80mm test box -> 40), so its bottom
	// lands on the shelf rather than being driven in like a taller glass was.
	if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}
	if got := s.servingAreaDropZOffset(); math.Abs(got-40) > 1e-6 {
		t.Fatalf("height-derived offset = %g, want 40", got)
	}
}

func TestSlotIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		counter uint64
		n       int
		want    int
	}{
		{name: "first placement uses slot 0", counter: 0, n: 4, want: 0},
		{name: "second placement uses slot 1", counter: 1, n: 4, want: 1},
		{name: "last slot before wrap", counter: 3, n: 4, want: 3},
		{name: "wraps back to slot 0", counter: 4, n: 4, want: 0},
		{name: "wraps to slot 1", counter: 5, n: 4, want: 1},
		{name: "single slot always 0", counter: 7, n: 1, want: 0},
		{name: "n<=0 returns 0", counter: 9, n: 0, want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := slotIndex(tc.counter, tc.n); got != tc.want {
				t.Fatalf("slotIndex(%d, %d) = %d, want %d", tc.counter, tc.n, got, tc.want)
			}
		})
	}
}
