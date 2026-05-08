// Package beanjamin: dynamic cup pickup.
//
// pickCupDynamic replaces the static empty_cup grab in setCupForCoffee
// when dynamic_cup_pickup is enabled. It moves the arm to the configured
// cup_observe_pose (a real world-frame pose on the claws pose switch),
// calls a vision service for cup detections, lifts each centroid into
// world frame, picks the closest detection within range, composes the
// configured approach/grab relative poses (from Config — they are
// offsets, not switch-resident world-frame poses) onto the centroid,
// and feeds the resulting world poses to moveToRawPose.
package beanjamin

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/module/trace"
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
// orientation. The relative pose comes from Config (cup_approach_relative_pose
// / cup_grab_relative_pose) and is interpreted as an offset onto the runtime
// centroid — these are offsets, not absolute world-frame poses.
func composeCupPose(centroidWorld r3.Vector, relative spatialmath.Pose) spatialmath.Pose {
	centroid := spatialmath.NewPoseFromPoint(centroidWorld)
	return spatialmath.Compose(centroid, relative)
}

// relativePoseToSpatial converts a Config RelativePose into a spatialmath.Pose
// suitable for composeCupPose. Translation is millimeters; orientation is
// OrientationVectorDegrees.
func relativePoseToSpatial(r *RelativePose) spatialmath.Pose {
	return spatialmath.NewPose(
		r3.Vector{X: r.X, Y: r.Y, Z: r.Z},
		&spatialmath.OrientationVectorDegrees{OX: r.OX, OY: r.OY, OZ: r.OZ, Theta: r.Theta},
	)
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

	// Frame system inputs are only needed when we lift camera-frame detections
	// into world. Skip the lookup entirely when the operator has declared
	// detections already arrive in world frame.
	var fs *referenceframe.FrameSystem
	var fsInputs referenceframe.FrameSystemInputs
	if !s.cfg.DetectionsAreWorldFrame {
		var err error
		fs, fsInputs, err = s.currentInputs(ctx)
		if err != nil {
			return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %w", err)
		}
	} else {
		s.logger.Infof("dynamic cup pickup: detections_are_world_frame=true — skipping camera-to-world transform")
	}

	centroids := make([]r3.Vector, 0, len(objects))
	for _, obj := range objects {
		if obj.Geometry == nil {
			continue
		}
		local := obj.Geometry.Pose().Point()
		var world r3.Vector
		if s.cfg.DetectionsAreWorldFrame {
			world = local
		} else {
			var err error
			world, err = cameraToWorld(fs, fsInputs, s.cupCameraName, local)
			if err != nil {
				return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %w", err)
			}
		}
		s.logger.Debugf("dynamic cup pickup: detection at camera-local %v -> world %v", local, world)
		centroids = append(centroids, world)
	}
	if len(centroids) == 0 {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %d detection(s) had no usable geometry", len(objects))
	}

	target := r3.Vector{X: s.cfg.ExpectedCupPositionMm.X, Y: s.cfg.ExpectedCupPositionMm.Y, Z: s.cfg.ExpectedCupPositionMm.Z}
	cutoff := s.cfg.CupMaxDistanceFromTargetMm
	s.logger.Infof("dynamic cup pickup: target=(x=%.1f, y=%.1f, z=%.1f) cutoff=%.0fmm — %d candidate(s):",
		target.X, target.Y, target.Z, cutoff, len(centroids))
	for i, c := range centroids {
		d := c.Sub(target).Norm()
		annotation := ""
		if cutoff > 0 && d > cutoff {
			annotation = " [REJECTED — beyond cutoff]"
		}
		s.logger.Infof("  candidate[%d] world=(x=%.1f, y=%.1f, z=%.1f) distance=%.1fmm%s",
			i, c.X, c.Y, c.Z, d, annotation)
	}

	chosen, idx, err := selectCupCentroid(centroids, target, cutoff)
	if err != nil {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %d detection(s) found but %w", len(centroids), err)
	}
	dist := chosen.Sub(target).Norm()
	s.logger.Infof("dynamic cup pickup: chose centroid %d at (x=%.1f, y=%.1f, z=%.1f) — %.1fmm from target",
		idx, chosen.X, chosen.Y, chosen.Z, dist)
	return chosen, nil
}

// pickCupDynamic moves the arm to the configured cup_observe_pose, observes
// the closest cup via the vision service, and executes a side-grab using
// the cup_approach_relative_pose / cup_grab_relative_pose offsets from
// Config composed onto the detected centroid. Called by setCupForCoffee
// when DynamicCupPickup=true.
func (s *beanjaminCoffee) pickCupDynamic(ctx, cancelCtx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "beanjamin::dynamic_cup_pickup")
	defer span.End()

	// Merge cancelCtx into ctx so operator cancel interrupts moveToRawPose
	// and gripper calls. Mirrors motion.go executePivot / executeCircularMotion.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(cancelCtx, func() { cancel() })
	defer stop()
	defer cancel()

	if s.gripper == nil {
		return fmt.Errorf("dynamic_cup_pickup: no gripper configured")
	}

	// 1. Move to observe pose.
	observeStep := Step{PoseName: "cup_observe_pose", Component: "coffee-claws-middle", Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, observeStep); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: observe: %w", err)
	}

	// 2. Observe.
	detectCtx, detectSpan := trace.StartSpan(ctx, "beanjamin::dynamic_cup_pickup::detect")
	centroidWorld, err := s.observeCupCentroid(detectCtx)
	detectSpan.End()
	if err != nil {
		return err
	}

	// 3. Compose configured offsets onto the centroid → world-frame *poseData.
	approachPD := &poseData{
		pose:          composeCupPose(centroidWorld, relativePoseToSpatial(s.cfg.CupApproachRelativePose)),
		refFrame:      referenceframe.World,
		componentName: "coffee-claws-middle",
	}
	grabPD := &poseData{
		pose:          composeCupPose(centroidWorld, relativePoseToSpatial(s.cfg.CupGrabRelativePose)),
		refFrame:      referenceframe.World,
		componentName: "coffee-claws-middle",
	}

	// 5. Approach (free planning).
	if err := s.moveToRawPose(ctx, approachPD, nil, nil, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: approach: %w", err)
	}

	// 6. Open the gripper before descending.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: open gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// 7. Linear move to grab pose.
	if err := s.moveToRawPose(ctx, grabPD, defaultApproachConstraint, nil, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: grab: %w", err)
	}

	// 8. Close the gripper.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: grab gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// 9. Linear retreat back to the approach pose.
	if err := s.moveToRawPose(ctx, approachPD, defaultApproachConstraint, nil, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: retreat: %w", err)
	}
	return nil
}
