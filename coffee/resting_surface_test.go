package coffee

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

func TestSurfaceTopZUnder(t *testing.T) {
	// A low table (top 700) and a taller ledge (top 900) both under the origin,
	// plus a shelf off to the side that does not cover the origin.
	boxes := []surfaceBox{
		{minX: -100, maxX: 100, minY: -100, maxY: 100, topZ: 700}, // table under origin
		{minX: -50, maxX: 50, minY: -50, maxY: 50, topZ: 900},     // ledge under origin
		{minX: 500, maxX: 700, minY: 500, maxY: 700, topZ: 800},   // shelf elsewhere
	}

	tests := []struct {
		name      string
		x, y      float64
		refZ      float64
		wantZ     float64
		wantFound bool
	}{
		{name: "highest surface below the object wins", x: 0, y: 0, refZ: 1000, wantZ: 900, wantFound: true},
		{name: "skip surfaces at/above the object center", x: 0, y: 0, refZ: 850, wantZ: 700, wantFound: true},
		{name: "no surface below the object", x: 0, y: 0, refZ: 650, wantFound: false},
		{name: "point outside every footprint", x: 300, y: 300, refZ: 1000, wantFound: false},
		{name: "point over the off-to-the-side shelf only", x: 600, y: 600, refZ: 1000, wantZ: 800, wantFound: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotZ, gotFound := surfaceTopZUnder(boxes, tc.x, tc.y, tc.refZ)
			if gotFound != tc.wantFound {
				t.Fatalf("found = %v, want %v", gotFound, tc.wantFound)
			}
			if gotFound && gotZ != tc.wantZ {
				t.Fatalf("topZ = %g, want %g", gotZ, tc.wantZ)
			}
		})
	}
}

// staticBoxFrame adds a world-anchored box (zero frame pose, geometry carrying
// the world pose — the codebase convention that sidesteps the GeometriesInFrame
// transform quirk) so FrameSystemGeometries reports it at exactly boxPose.
func staticBoxFrame(t *testing.T, fs *referenceframe.FrameSystem, parent referenceframe.Frame, name string, boxPose spatialmath.Pose, dims r3.Vector) {
	t.Helper()
	box, err := spatialmath.NewBox(boxPose, dims, name)
	if err != nil {
		t.Fatalf("new box %q: %v", name, err)
	}
	frame, err := referenceframe.NewStaticFrameWithGeometry(name, spatialmath.NewZeroPose(), box)
	if err != nil {
		t.Fatalf("new frame %q: %v", name, err)
	}
	if err := fs.AddFrame(frame, parent); err != nil {
		t.Fatalf("add frame %q: %v", name, err)
	}
}

func TestWorldAnchoredSurfaceBoxes(t *testing.T) {
	fs := referenceframe.NewEmptyFrameSystem("test")

	// A static shelf parented to world: center (200,100,700), dims (400,300,20),
	// so world footprint x[0,400] y[-50,250], top face Z=710.
	staticBoxFrame(t, fs, fs.World(), "shelf",
		spatialmath.NewPoseFromPoint(r3.Vector{X: 200, Y: 100, Z: 700}),
		r3.Vector{X: 400, Y: 300, Z: 20})

	// A box hanging off a revolute joint: it moves with the arm, so it must be
	// excluded even though at zero inputs it sits over the same footprint.
	wide := referenceframe.Limit{Min: -2 * math.Pi, Max: 2 * math.Pi}
	j0, err := referenceframe.NewRotationalFrame("j0", spatialmath.R4AA{RZ: 1}, wide)
	if err != nil {
		t.Fatalf("new j0: %v", err)
	}
	if err := fs.AddFrame(j0, fs.World()); err != nil {
		t.Fatalf("add j0: %v", err)
	}
	staticBoxFrame(t, fs, j0, "arm-link",
		spatialmath.NewPoseFromPoint(r3.Vector{X: 200, Y: 100, Z: 600}),
		r3.Vector{X: 400, Y: 300, Z: 20})

	// A non-box world-anchored geometry: skipped (only boxes model flat surfaces).
	ball, err := spatialmath.NewSphere(spatialmath.NewPoseFromPoint(r3.Vector{X: 200, Y: 100, Z: 500}), 30, "ball")
	if err != nil {
		t.Fatalf("new sphere: %v", err)
	}
	ballFrame, err := referenceframe.NewStaticFrameWithGeometry("ball", spatialmath.NewZeroPose(), ball)
	if err != nil {
		t.Fatalf("new ball frame: %v", err)
	}
	if err := fs.AddFrame(ballFrame, fs.World()); err != nil {
		t.Fatalf("add ball frame: %v", err)
	}

	boxes, err := worldAnchoredSurfaceBoxes(fs, referenceframe.NewZeroInputs(fs))
	if err != nil {
		t.Fatalf("worldAnchoredSurfaceBoxes: %v", err)
	}
	if len(boxes) != 1 {
		t.Fatalf("got %d surface box(es), want 1 (the shelf); got %+v", len(boxes), boxes)
	}
	got := boxes[0]
	want := surfaceBox{minX: 0, maxX: 400, minY: -50, maxY: 250, topZ: 710}
	const tol = 1e-6
	if math.Abs(got.minX-want.minX) > tol || math.Abs(got.maxX-want.maxX) > tol ||
		math.Abs(got.minY-want.minY) > tol || math.Abs(got.maxY-want.maxY) > tol ||
		math.Abs(got.topZ-want.topZ) > tol {
		t.Fatalf("shelf footprint = %+v, want %+v", got, want)
	}

	// The moving arm-link box, at zero inputs, sits over (200,100). It must not
	// be treated as the surface there.
	if _, ok := surfaceTopZUnder(boxes, 200, 100, 1000); !ok {
		t.Fatalf("expected the shelf to be found under (200,100)")
	}
	if z, _ := surfaceTopZUnder(boxes, 200, 100, 1000); z != 710 {
		t.Fatalf("surface under (200,100) = %g, want 710 (shelf, not the moving arm-link)", z)
	}
}

func TestBoxWorldAABB_Rotated(t *testing.T) {
	// A 100x100 (footprint) box rotated 45° about Z has a world footprint whose
	// half-extent grows to 100/sqrt(2) ≈ 70.71 on each axis.
	rot := &spatialmath.OrientationVectorDegrees{OZ: 1, Theta: 45}
	pose := spatialmath.NewPose(r3.Vector{X: 10, Y: 20, Z: 700}, rot)
	box, err := spatialmath.NewBox(pose, r3.Vector{X: 100, Y: 100, Z: 40}, "r")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	min, max := boxWorldAABB(box)
	half := 100 / math.Sqrt2
	const tol = 1e-6
	if math.Abs((max.X-min.X)/2-half) > tol || math.Abs((max.Y-min.Y)/2-half) > tol {
		t.Fatalf("rotated footprint half-extent = (%g, %g), want %g", (max.X-min.X)/2, (max.Y-min.Y)/2, half)
	}
	if math.Abs(max.Z-720) > tol {
		t.Fatalf("top Z = %g, want 720", max.Z)
	}
}
