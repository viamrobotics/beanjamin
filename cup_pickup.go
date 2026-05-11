// Package beanjamin: dynamic cup pickup.
//
// pickCupDynamic replaces the static empty_cup grab in setCupForCoffee
// when dynamic_cup_pickup is enabled. It moves the arm to the configured
// cup_observe (a real world-frame pose on the claws pose switch),
// calls a vision service for cup detections, lifts each centroid into
// world frame, ranks detections by distance from the configured
// expected position (within the configured cutoff), composes the
// configured approach/grab relative poses (from Config — they are
// offsets, not switch-resident world-frame poses) onto the chosen
// centroid, and feeds the resulting world poses to moveToRawPose.
//
// On a planning failure, pickCupDynamic falls through to the next
// candidate cup and re-observes the workspace after each batch is
// exhausted, bounded by CupPickupMaxAttempts.
package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	viz "go.viam.com/rdk/vision"
)

// rankCupCentroids returns centroids sorted by distance to target ascending,
// dropping any beyond maxDistMm. maxDistMm == 0 disables the cutoff. The
// returned slice is a new allocation; the input is not mutated. Ties keep
// their original relative order (stable sort).
func rankCupCentroids(centroids []r3.Vector, target r3.Vector, maxDistMm float64) []r3.Vector {
	inRange := make([]r3.Vector, 0, len(centroids))
	for _, c := range centroids {
		if maxDistMm > 0 && c.Sub(target).Norm() > maxDistMm {
			continue
		}
		inRange = append(inRange, c)
	}
	sort.SliceStable(inRange, func(i, j int) bool {
		return inRange[i].Sub(target).Norm() < inRange[j].Sub(target).Norm()
	})
	return inRange
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

// observeCupCandidates calls the vision service for cup detections, retries
// on empty results per the configured retry policy, lifts each detection's
// centroid into world coordinates, filters out detections beyond
// CupMaxDistanceFromTargetMm, and returns the survivors sorted by distance to
// ExpectedCupPositionMm (closest first). Returns an error if no usable
// in-range centroids remain.
func (s *beanjaminCoffee) observeCupCandidates(ctx context.Context) ([]r3.Vector, error) {
	maxAttempts := s.cfg.CupDetectionRetries + 1
	sleep := time.Duration(s.cfg.CupDetectionRetrySleepMs) * time.Millisecond
	if sleep <= 0 {
		sleep = 250 * time.Millisecond
	}

	var objects []*viz.Object
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Pass an empty camera name so the vision service falls back to its own
		// configured default camera. s.cupCameraName is still used below to
		// transform detection centroids from the camera frame into world coords.
		objs, err := s.cupVision.GetObjectPointClouds(ctx, "", nil)
		if err != nil {
			return nil, fmt.Errorf("dynamic_cup_pickup: detect: %w", err)
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
				return nil, fmt.Errorf("dynamic_cup_pickup: cancelled during retry: %w", ctx.Err())
			}
		}
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("dynamic_cup_pickup: no cups detected after %d attempts", maxAttempts)
	}

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return nil, fmt.Errorf("dynamic_cup_pickup: %w", err)
	}

	centroids := make([]r3.Vector, 0, len(objects))
	for _, obj := range objects {
		if obj.Geometry == nil {
			continue
		}
		local := obj.Geometry.Pose().Point()
		world, err := cameraToWorld(fs, fsInputs, s.cupCameraName, local)
		if err != nil {
			return nil, fmt.Errorf("dynamic_cup_pickup: %w", err)
		}
		if floor := s.cfg.CupCentroidMinZMm; floor != 0 && world.Z < floor {
			s.logger.Infof("dynamic cup pickup: flooring centroid Z from %.1f to %.1f (cup_centroid_min_z_mm)",
				world.Z, floor)
			world.Z = floor
		}
		s.logger.Debugf("dynamic cup pickup: detection at camera-local %v -> world %v", local, world)
		centroids = append(centroids, world)
	}
	if len(centroids) == 0 {
		return nil, fmt.Errorf("dynamic_cup_pickup: %d detection(s) had no usable geometry", len(objects))
	}

	target := r3.Vector{X: s.cfg.ExpectedCupPositionMm.X, Y: s.cfg.ExpectedCupPositionMm.Y, Z: s.cfg.ExpectedCupPositionMm.Z}
	cutoff := s.cfg.CupMaxDistanceFromTargetMm
	s.logger.Infof("dynamic cup pickup: target=(x=%.1f, y=%.1f, z=%.1f) cutoff=%.0fmm — %d raw candidate(s):",
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

	ranked := rankCupCentroids(centroids, target, cutoff)
	if len(ranked) == 0 {
		return nil, fmt.Errorf("dynamic_cup_pickup: %d detection(s) found but none within %.0fmm of target", len(centroids), cutoff)
	}
	s.logger.Infof("dynamic cup pickup: %d in-range candidate(s) (closest first):", len(ranked))
	for i, c := range ranked {
		s.logger.Infof("  rank[%d] world=(x=%.1f, y=%.1f, z=%.1f) distance=%.1fmm",
			i, c.X, c.Y, c.Z, c.Sub(target).Norm())
	}
	return ranked, nil
}

// tryGrabCup attempts a full approach-grab-retreat cycle on one candidate
// centroid. On failure after the approach step, it best-effort restores the
// arm to cup_observe so the caller can attempt a different candidate from a
// known good state.
//
// Returned errors fall into two categories the caller distinguishes via
// errors.Is:
//   - wraps errMotionPlanning → planning failure; try a different candidate.
//   - anything else → execution error or operator cancel; bubble up.
func (s *beanjaminCoffee) tryGrabCup(ctx, cancelCtx context.Context, centroid r3.Vector) error {
	approachPD := &poseData{
		pose:          composeCupPose(centroid, relativePoseToSpatial(s.cfg.CupApproachRelativePose)),
		refFrame:      referenceframe.World,
		componentName: "coffee-claws-middle",
	}
	grabPD := &poseData{
		pose:          composeCupPose(centroid, relativePoseToSpatial(s.cfg.CupGrabRelativePose)),
		refFrame:      referenceframe.World,
		componentName: "coffee-claws-middle",
	}

	// 1. Approach (free planning). On failure the arm has not moved.
	if err := s.moveToRawPose(ctx, approachPD, nil, nil, nil); err != nil {
		return fmt.Errorf("approach centroid (x=%.1f, y=%.1f, z=%.1f): %w", centroid.X, centroid.Y, centroid.Z, err)
	}

	// 2. Open gripper before descending.
	if err := s.gripper.Open(ctx, nil); err != nil {
		s.recoverToObserve(ctx, cancelCtx)
		return fmt.Errorf("open gripper for grab: %w", err)
	}
	time.Sleep(gripperPause)

	// 3. Linear descent to grab pose.
	if err := s.moveToRawPose(ctx, grabPD, defaultApproachConstraint, nil, nil); err != nil {
		s.recoverToObserve(ctx, cancelCtx)
		return fmt.Errorf("grab centroid (x=%.1f, y=%.1f, z=%.1f): %w", centroid.X, centroid.Y, centroid.Z, err)
	}

	// 4. Close the gripper on the cup.
	//
	// TODO: verify the gripper actually picked up a cup before continuing.
	// gripper.IsHoldingSomething is not usable here because the real robot
	// permanently grips the claws extension, so the call returns true
	// regardless of whether a cup is between the claws. 
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		s.recoverToObserve(ctx, cancelCtx)
		return fmt.Errorf("close gripper on cup: %w", err)
	}
	time.Sleep(gripperPause)

	// 5. Linear retreat with the cup in hand. A failure here is fatal — we
	// can't drop the cup safely by recovering to observe. Strip the
	// errMotionPlanning chain (%v, not %w) so the caller does not treat this
	// as a try-another-cup planning failure.
	if err := s.moveToRawPose(ctx, approachPD, defaultApproachConstraint, nil, nil); err != nil {
		return fmt.Errorf("retreat with cup grabbed (centroid x=%.1f, y=%.1f, z=%.1f): %v", centroid.X, centroid.Y, centroid.Z, err)
	}
	return nil
}

// recoverToObserve best-effort returns the arm to cup_observe so the next
// candidate (or the next observation) starts from a known state. Errors are
// logged, not returned — the caller is already returning an error.
func (s *beanjaminCoffee) recoverToObserve(ctx, cancelCtx context.Context) {
	// Close the gripper to a safe configuration before traversing back. A
	// stray open gripper has a larger collision silhouette than a closed one.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		s.logger.Warnf("dynamic cup pickup: recover: close gripper: %v", err)
	}
	time.Sleep(gripperPause)

	observeStep := Step{PoseName: "cup_observe", Component: "coffee-claws-middle", Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, observeStep); err != nil {
		s.logger.Warnf("dynamic cup pickup: recover to cup_observe: %v", err)
	}
}

// pickCupDynamic moves the arm to cup_observe, observes the cup workspace,
// and walks the ranked candidate list grabbing the first reachable cup.
// Falls through to the next candidate on planning failures and re-observes
// (up to CupPickupMaxAttempts attempts) when the gripper closes on empty air
// or all candidates in a batch fail planning. Called by setCupForCoffee when
// DynamicCupPickup=true.
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

	maxAttempts := s.cupPickupMaxAttempts()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Move to observe pose at the start of every attempt. Redundant on
		// attempts that follow a recoverToObserve, but harmless — the plan
		// to a pose the arm already occupies is trivial.
		observeStep := Step{PoseName: "cup_observe", Component: "coffee-claws-middle", Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, observeStep); err != nil {
			return fmt.Errorf("dynamic_cup_pickup: observe: %w", err)
		}

		detectCtx, detectSpan := trace.StartSpan(ctx, "beanjamin::dynamic_cup_pickup::detect")
		candidates, err := s.observeCupCandidates(detectCtx)
		detectSpan.End()
		if err != nil {
			// Either no detections or none in range — re-observing won't
			// reveal cups that aren't there. Bail.
			return err
		}

		s.logger.Infof("dynamic cup pickup: attempt %d/%d — %d candidate(s) to try", attempt, maxAttempts, len(candidates))
		for i, centroid := range candidates {
			err := s.tryGrabCup(ctx, cancelCtx, centroid)
			if err == nil {
				return nil
			}
			lastErr = err

			// Operator cancel always wins.
			if ctx.Err() != nil {
				return fmt.Errorf("dynamic_cup_pickup: cancelled: %w", err)
			}

			if !errors.Is(err, errMotionPlanning) {
				return err
			}
			s.logger.Warnf("dynamic cup pickup: attempt %d, candidate %d/%d planning failed — trying next: %v",
				attempt, i+1, len(candidates), err)
		}
	}
	return fmt.Errorf("dynamic_cup_pickup: exhausted %d attempt(s); last error: %w", maxAttempts, lastErr)
}
