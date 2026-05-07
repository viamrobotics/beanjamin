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
	"context"
	"fmt"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	viz "go.viam.com/rdk/vision"
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

// composeCupPose builds a world-frame target pose by composing a relative
// pose (translation + orientation) onto a centroid point with identity
// orientation. The relative pose is read from the claws pose switch under
// names like cup_grab_relative_pose / cup_approach_relative_pose; its
// reference_frame field is intentionally ignored — these poses are
// interpreted as offsets from the runtime centroid, not absolute poses.
func composeCupPose(centroidWorld r3.Vector, relative spatialmath.Pose) spatialmath.Pose {
	centroid := spatialmath.NewPoseFromPoint(centroidWorld)
	return spatialmath.Compose(centroid, relative)
}

// cameraToWorld lifts a point given in the camera's local frame into the
// world frame. The vision service returns object geometry in the camera
// frame; this function uses the frame system Transform to convert.
func cameraToWorld(
	fs *referenceframe.FrameSystem,
	fsInputs referenceframe.FrameSystemInputs,
	cameraFrame string,
	point r3.Vector,
) (r3.Vector, error) {
	pif := referenceframe.NewPoseInFrame(cameraFrame, spatialmath.NewPoseFromPoint(point))
	tf, err := fs.Transform(fsInputs.ToLinearInputs(), pif, referenceframe.World)
	if err != nil {
		return r3.Vector{}, fmt.Errorf("transform %q to world: %w", cameraFrame, err)
	}
	worldPose := tf.(*referenceframe.PoseInFrame)
	return worldPose.Pose().Point(), nil
}

// observeCupCentroid calls the vision service for cup detections, retries
// on empty results per the configured retry policy, lifts each detection's
// centroid into world coordinates, and returns the closest in-range
// centroid (per selectCupCentroid). Returns an error if no cups are found
// after all retries, or if all detections fall outside the configured
// max distance.
func (s *beanjaminCoffee) observeCupCentroid(ctx context.Context) (r3.Vector, error) {
	maxAttempts := s.cfg.CupDetectionRetries + 1
	sleep := time.Duration(s.cfg.CupDetectionRetrySleepMs) * time.Millisecond
	if sleep <= 0 {
		sleep = 250 * time.Millisecond
	}

	var objects []*viz.Object
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		objs, err := s.cupVision.GetObjectPointClouds(ctx, s.cupCameraName, nil)
		if err != nil {
			return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: detect: %w", err)
		}
		s.logger.Infof("dynamic cup pickup: attempt %d/%d, found %d detections", attempt, maxAttempts, len(objs))
		if len(objs) > 0 {
			objects = objs
			break
		}
		if attempt < maxAttempts {
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: cancelled during retry: %w", ctx.Err())
			}
		}
	}
	if len(objects) == 0 {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: no cups detected after %d attempts", maxAttempts)
	}

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %w", err)
	}

	centroids := make([]r3.Vector, 0, len(objects))
	for _, obj := range objects {
		if obj.Geometry == nil {
			continue
		}
		local := obj.Geometry.Pose().Point()
		world, err := cameraToWorld(fs, fsInputs, s.cupCameraName, local)
		if err != nil {
			return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %w", err)
		}
		s.logger.Debugf("dynamic cup pickup: detection at camera-local %v -> world %v", local, world)
		centroids = append(centroids, world)
	}
	if len(centroids) == 0 {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %d detection(s) had no usable geometry", len(objects))
	}

	target := r3.Vector{X: s.cfg.ExpectedCupPositionMm.X, Y: s.cfg.ExpectedCupPositionMm.Y, Z: s.cfg.ExpectedCupPositionMm.Z}
	chosen, idx, err := selectCupCentroid(centroids, target, s.cfg.CupMaxDistanceFromTargetMm)
	if err != nil {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %d detection(s) found but %w", len(centroids), err)
	}
	dist := chosen.Sub(target).Norm()
	s.logger.Infof("dynamic cup pickup: chose centroid %d at (x=%.1f, y=%.1f, z=%.1f) — %.1fmm from target",
		idx, chosen.X, chosen.Y, chosen.Z, dist)
	return chosen, nil
}
