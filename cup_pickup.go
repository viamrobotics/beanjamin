// Package beanjamin: dynamic cup pickup.
//
// pickCupDynamic replaces the static empty_cup grab in setCupForCoffee
// when dynamic_cup_pickup is enabled. It moves the arm to a configured
// observe pose, calls a vision service for cup detections, lifts each
// centroid into world frame, picks the closest detection within range,
// composes the configured approach/grab relative poses (read from the
// claws pose switch by name), and feeds the resulting world poses to
// moveToRawPose.
package beanjamin

import (
	"fmt"

	"github.com/golang/geo/r3"
)

// selectCupCentroid returns the centroid in `centroids` closest to `target`
// (Euclidean distance), provided it is within maxDistMm. maxDistMm == 0
// disables the cutoff. Returns the chosen centroid and its original index
// (for log correlation), or an error if the input is empty or no centroid
// is within range.
func selectCupCentroid(centroids []r3.Vector, target r3.Vector, maxDistMm float64) (r3.Vector, int, error) {
	if len(centroids) == 0 {
		return r3.Vector{}, -1, fmt.Errorf("no centroids")
	}
	bestIdx := -1
	bestDist := 0.0
	for i, c := range centroids {
		d := c.Sub(target).Norm()
		if maxDistMm > 0 && d > maxDistMm {
			continue
		}
		if bestIdx == -1 || d < bestDist {
			bestIdx = i
			bestDist = d
		}
	}
	if bestIdx == -1 {
		return r3.Vector{}, -1, fmt.Errorf("none within %.0fmm of expected position", maxDistMm)
	}
	return centroids[bestIdx], bestIdx, nil
}
