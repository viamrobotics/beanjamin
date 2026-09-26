// Package geom holds the coffee service's pure spatial math: merging and
// ranking detected pickup centroids, composing configured offsets onto them,
// lifting camera-frame detections into world frame, modeling held containers
// as boxes, laying out served-shelf slots, and finding the static surface a
// detected container rests on. Nothing here holds service state.
package geom

import (
	"fmt"
	"math"
	"sort"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// Candidate is one detected item: its world-frame grasp centroid plus the
// world-frame detected geometry (nil when geometry is unavailable). The geometry
// rides alongside the centroid so the held-item tracker can attach the detected
// shape to the gripper after a successful grab.
type Candidate struct {
	Centroid r3.Vector
	Geom     spatialmath.Geometry
}

// MergeNearbyCentroids clusters centroids that fall within mm of an existing
// cluster's running mean and returns one centroid per cluster: the mean of its
// members. First-seen order determines cluster assignment. Input is not mutated.
// mm <= 0 disables merging and returns a copy.
func MergeNearbyCentroids(centroids []r3.Vector, mm float64) []r3.Vector {
	if mm <= 0 || len(centroids) <= 1 {
		return append([]r3.Vector(nil), centroids...)
	}
	type cluster struct {
		sum   r3.Vector
		count float64
	}
	var clusters []cluster
	for _, c := range centroids {
		merged := false
		for i := range clusters {
			mean := clusters[i].sum.Mul(1 / clusters[i].count)
			if c.Sub(mean).Norm() < mm {
				clusters[i].sum = clusters[i].sum.Add(c)
				clusters[i].count++
				merged = true
				break
			}
		}
		if !merged {
			clusters = append(clusters, cluster{sum: c, count: 1})
		}
	}
	out := make([]r3.Vector, len(clusters))
	for i, cl := range clusters {
		out[i] = cl.sum.Mul(1 / cl.count)
	}
	return out
}

// RankCentroidsByProximity returns centroids sorted by distance to reference
// ascending (closest first). reference is the gripper's world-frame position, so
// the item nearest the gripper is grabbed first. The returned slice is a new
// allocation; the input is not mutated. Ties keep their original relative order
// (stable sort).
func RankCentroidsByProximity(centroids []r3.Vector, reference r3.Vector) []r3.Vector {
	ranked := append([]r3.Vector(nil), centroids...)
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].Sub(reference).Norm() < ranked[j].Sub(reference).Norm()
	})
	return ranked
}

// ComposeCupPose builds a world-frame target pose by composing a relative
// pose (translation + orientation) onto a centroid point with identity
// orientation. The relative pose comes from the coffee Config
// (cup_approach_relative_pose / cup_grab_relative_pose) and is interpreted as an
// offset onto the runtime centroid — these are offsets, not absolute world-frame
// poses.
func ComposeCupPose(centroidWorld r3.Vector, relative spatialmath.Pose) spatialmath.Pose {
	centroid := spatialmath.NewPoseFromPoint(centroidWorld)
	return spatialmath.Compose(centroid, relative)
}

// CameraToWorldPose resolves the camera frame's pose in the world frame at the
// given inputs. Composing it onto a camera-frame pose lifts that pose into world
// coordinates — the vision service reports object geometry and point clouds in
// the camera frame, so everything an observation produces goes through it.
func CameraToWorldPose(
	fs *referenceframe.FrameSystem,
	fsInputs referenceframe.FrameSystemInputs,
	cameraFrame string,
) (spatialmath.Pose, error) {
	pif := referenceframe.NewPoseInFrame(cameraFrame, spatialmath.NewZeroPose())
	tf, err := fs.Transform(fsInputs.ToLinearInputs(), pif, referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("transform %q to world: %w", cameraFrame, err)
	}
	return tf.(*referenceframe.PoseInFrame).Pose(), nil
}

// ContainerBox builds the held-item geometry from the configured container
// size (cup_dimensions / glass_dimensions, as box extents): an axis-aligned box
// (orientation OZ=1) of that size, centered on the grasp centroid — the point
// the gripper is sent to. Modeling a known-size container around its grasp point
// avoids a center skewed by a point cloud that only captured part of the
// container. label is set on the box.
func ContainerBox(centroid, dims r3.Vector, label string) (spatialmath.Geometry, error) {
	box, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(centroid), dims, label)
	if err != nil {
		return nil, fmt.Errorf("new %s bounding box: %w", label, err)
	}
	return box, nil
}

// Centroids extracts the world-frame centroids from a slice of candidates,
// preserving order. Used to feed the centroid-only merge/rank helpers.
func Centroids(candidates []Candidate) []r3.Vector {
	out := make([]r3.Vector, len(candidates))
	for i, c := range candidates {
		out[i] = c.Centroid
	}
	return out
}

// CandidatesForCentroids pairs each ranked centroid with the geometry of the
// nearest original detection, so the held-item tracker can attach the detected
// shape after the grab. Geometry is matched back by nearest original rather than
// threaded through the merge (which averages centroids).
func CandidatesForCentroids(ranked []r3.Vector, originals []Candidate) []Candidate {
	out := make([]Candidate, len(ranked))
	for i, c := range ranked {
		out[i] = Candidate{Centroid: c, Geom: NearestGeometry(c, originals)}
	}
	return out
}

// NearestGeometry returns the geometry of the original detection whose centroid
// is closest to c, skipping detections with no geometry. Returns nil when no
// original carries a geometry.
func NearestGeometry(c r3.Vector, originals []Candidate) spatialmath.Geometry {
	var best spatialmath.Geometry
	bestDist := math.MaxFloat64
	for _, o := range originals {
		if o.Geom == nil {
			continue
		}
		if d := o.Centroid.Sub(c).Norm(); d < bestDist {
			bestDist = d
			best = o.Geom
		}
	}
	return best
}
