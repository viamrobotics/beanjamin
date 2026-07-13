package coffee

// Resting-surface detection for dynamic pickup.
//
// A detected cup/glass stands on a surface (a shelf or table). Rather than
// trusting the raw detected centroid Z — which depth noise pushes above or below
// the true base — pickup seats the container on that surface: with the
// container's known height it places the base surfaceRestClearanceMm above the
// top of the highest *static* box directly beneath the detection (see
// observeVantage). The small clearance keeps the base just off the surface so the
// grasp does not begin already in collision with it.
//
// "Static" means world-anchored: SharesRigidMotion(frame, World) is true, so the
// moving arm/gripper/camera/held-item chain is never mistaken for a surface. Only
// box geometries are considered (flat surfaces are modeled as boxes); each box's
// world axis-aligned bounding box gives its footprint and top Z, so a rotated box
// is bounded conservatively rather than by raw dims.

import (
	"fmt"
	"math"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// surfaceRestClearanceMm is how far above the resting surface the container's
// base is seated at pickup, so the grasp does not start in contact with the
// surface.
const surfaceRestClearanceMm = 1.0

// surfaceBox is the world axis-aligned footprint (X/Y bounds) and top-face Z of
// one static box in the frame system — a candidate resting surface.
type surfaceBox struct {
	minX, maxX float64
	minY, maxY float64
	topZ       float64
}

// contains reports whether (x, y) falls within the box's world footprint.
func (b surfaceBox) contains(x, y float64) bool {
	return x >= b.minX && x <= b.maxX && y >= b.minY && y <= b.maxY
}

// worldAnchoredSurfaceBoxes returns the footprint + top Z of every static
// (world-anchored) box in the frame system, evaluated at fsInputs. Frames that
// move with the arm — SharesRigidMotion(frame, World) is false — are skipped so a
// moving link, the gripper, the camera, or a held item is never treated as a
// surface; non-box geometries are ignored.
func worldAnchoredSurfaceBoxes(fs *referenceframe.FrameSystem, fsInputs referenceframe.FrameSystemInputs) ([]surfaceBox, error) {
	geoms, err := referenceframe.FrameSystemGeometries(fs, fsInputs)
	if err != nil {
		return nil, fmt.Errorf("enumerate frame-system geometries: %w", err)
	}
	world := fs.World()
	var boxes []surfaceBox
	for name, gif := range geoms {
		frame := fs.Frame(name)
		if frame == nil || !fs.SharesRigidMotion(frame, world) {
			continue
		}
		for _, g := range gif.Geometries() {
			if g.ToProtobuf().GetBox() == nil {
				continue
			}
			min, max := boxWorldAABB(g)
			boxes = append(boxes, surfaceBox{
				minX: min.X, maxX: max.X,
				minY: min.Y, maxY: max.Y,
				topZ: max.Z,
			})
		}
	}
	return boxes, nil
}

// surfaceTopZUnder returns the highest surface top strictly below refZ whose
// footprint contains (x, y), and true when such a surface exists. refZ is the
// detected container's centroid Z: the surface it rests on necessarily lies below
// the container's center, so tops at or above refZ are not underneath it. Returns
// (0, false) when nothing qualifies, so the caller falls back to the detected Z.
func surfaceTopZUnder(boxes []surfaceBox, x, y, refZ float64) (float64, bool) {
	best := math.Inf(-1)
	found := false
	for _, b := range boxes {
		if !b.contains(x, y) || b.topZ >= refZ {
			continue
		}
		if b.topZ > best {
			best = b.topZ
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return best, true
}

// boxWorldAABB returns the min and max corners of the world axis-aligned bounding
// box of geometry g, which must be a box. It transforms the 8 local corners by
// g's world pose and takes their extent, so a rotated box is bounded correctly
// rather than by its raw dims about the center.
func boxWorldAABB(g spatialmath.Geometry) (r3.Vector, r3.Vector) {
	dims := g.ToProtobuf().GetBox().GetDimsMm()
	hx, hy, hz := dims.GetX()/2, dims.GetY()/2, dims.GetZ()/2
	pose := g.Pose()
	min := r3.Vector{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)}
	max := r3.Vector{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)}
	for _, sx := range []float64{-1, 1} {
		for _, sy := range []float64{-1, 1} {
			for _, sz := range []float64{-1, 1} {
				local := spatialmath.NewPoseFromPoint(r3.Vector{X: hx * sx, Y: hy * sy, Z: hz * sz})
				corner := spatialmath.Compose(pose, local).Point()
				min.X, max.X = math.Min(min.X, corner.X), math.Max(max.X, corner.X)
				min.Y, max.Y = math.Min(min.Y, corner.Y), math.Max(max.Y, corner.Y)
				min.Z, max.Z = math.Min(min.Z, corner.Z), math.Max(max.Z, corner.Z)
			}
		}
	}
	return min, max
}
