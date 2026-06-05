// Package beanjamin: dynamic cup pickup.
//
// pickCupDynamic replaces the static empty_cup grab in setCupForCoffee
// when dynamic_cup_pickup is enabled. It sweeps the poses on the dedicated
// camera-observe switch (camera_observe_pose_switcher_name) one at a time,
// calling a vision service for cup detections at each. As soon as a pose
// yields at least one in-range candidate the sweep stops — the next observe
// pose is only tried when the current one failed to find a cup. Detections
// are lifted into world frame, ranked by distance from the configured
// expected position (within the configured cutoff), and the configured
// approach/grab relative poses (from Config — they are offsets, not
// switch-resident world-frame poses) are composed onto the chosen centroid
// and fed to moveToRawPose.
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

// errNoCupsDetected is returned by findCupCandidates when the vision frames
// at every observe pose yielded zero detections. pickCupDynamic recognises
// this case via errors.Is and recovers with a spoken "please place a cup"
// announcement + a wait before re-observing, instead of failing the order
// outright.
var errNoCupsDetected = errors.New("no cups detected")

// noCupsRetryDelay is the wait between outer observation attempts when
// findCupCandidates reports zero detections.
const noCupsRetryDelay = 15 * time.Second

// cupObserveDedupMm is the merge radius used to collapse near-duplicate
// detections across multi-vantage observations: two centroids closer than
// this in world frame are treated as the same physical cup.
const cupObserveDedupMm = 40.0

// mergeNearbyCentroids clusters centroids that fall within mm of an existing
// cluster's running mean and returns one centroid per cluster: the mean of its
// members. First-seen order determines cluster assignment. Input is not mutated.
// mm <= 0 disables merging and returns a copy.
func mergeNearbyCentroids(centroids []r3.Vector, mm float64) []r3.Vector {
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

// observeVantage captures cup_photos_per_vantage vision frames at the arm's
// current pose, accumulates every detection from all of them, and lifts each
// centroid into world coordinates. Returns the pickup centroids (world frame).
// Returns a nil slice with no error when no frame produced a detection, so the
// sweep in findCupCandidates can move on to the next observe pose.
func (s *beanjaminCoffee) observeVantage(ctx context.Context) ([]r3.Vector, error) {
	photosToTake := s.cupPhotosPerVantage()

	var objects []*viz.Object
	for photo := 1; photo <= photosToTake; photo++ {
		// Pass an empty camera name so the vision service falls back to its own
		// configured default camera. s.cupCameraName is still used below to
		// transform detection centroids from the camera frame into world coords.
		objs, err := s.cupVision.GetObjectPointClouds(ctx, "", nil)
		if err != nil {
			return nil, fmt.Errorf("detect: %w", err)
		}
		s.logger.Infof("dynamic cup pickup: vision photo %d/%d, found %d detections", photo, photosToTake, len(objs))
		objects = append(objects, objs...)
	}
	if len(objects) == 0 {
		return nil, nil
	}

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return nil, err
	}

	centroids := make([]r3.Vector, 0, len(objects))
	for _, obj := range objects {
		if obj.Geometry == nil {
			continue
		}
		local := obj.Geometry.Pose().Point()
		world, err := cameraToWorld(fs, fsInputs, s.cupCameraName, local)
		if err != nil {
			return nil, err
		}
		if floor := s.cfg.CupCentroidMinZMm; floor != 0 && world.Z < floor {
			s.logger.Infof("dynamic cup pickup: flooring centroid Z from %.1f to %.1f (cup_centroid_min_z_mm)",
				world.Z, floor)
			world.Z = floor
		}
		s.logger.Debugf("dynamic cup pickup: detection at camera-local %v -> world %v", local, world)
		centroids = append(centroids, world)
	}
	return centroids, nil
}

// observationPoseNames returns the names of every pose on the camera-observe
// switch.
func (s *beanjaminCoffee) observationPoseNames(ctx context.Context) ([]string, error) {
	if s.cameraObserveSw == nil {
		return nil, fmt.Errorf("no camera observe pose switch configured")
	}
	_, names, err := s.cameraObserveSw.GetNumberOfPositions(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("enumerate camera observe poses: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("camera observe pose switch has no positions")
	}
	return names, nil
}

// findCupCandidates sweeps the camera-observe poses one at a time and returns
// the ranked pickup candidates from the first pose that sees a reachable cup.
//
// At each pose it moves the camera there, observes (observeVantage), merges
// near-duplicate detections, and ranks them by distance to
// ExpectedCupPositionMm within CupMaxDistanceFromTargetMm. The first pose that
// yields at least one in-range candidate wins and the remaining poses are not
// visited — the next observe pose is only tried when the current one failed to
// find a usable cup. An unreachable pose is logged and skipped.
//
// When no pose yields an in-range candidate, the error distinguishes two cases
// so pickCupDynamic can react: errNoCupsDetected (recoverable: announce + wait
// + re-observe) when no pose produced any detection at all, versus a plain
// "none within cutoff" error (non-recoverable) when detections were seen but
// all fell outside the cutoff.
func (s *beanjaminCoffee) findCupCandidates(ctx, cancelCtx context.Context) ([]r3.Vector, error) {
	poseNames, err := s.observationPoseNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("dynamic_cup_pickup: %w", err)
	}

	target := r3.Vector{X: s.cfg.ExpectedCupPositionMm.X, Y: s.cfg.ExpectedCupPositionMm.Y, Z: s.cfg.ExpectedCupPositionMm.Z}
	cutoff := s.cfg.CupMaxDistanceFromTargetMm

	passes := len(poseNames)
	totalDetections := 0
	for i, poseName := range poseNames {
		s.logger.Infof("dynamic cup pickup: pass %d/%d — moving to observe pose %q", i+1, passes, poseName)
		// Pause briefly after arriving so the camera frame is stable before
		// detection. "cam" routes the fetch to the camera-observe switch.
		step := Step{PoseName: poseName, Component: componentCam, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			s.logger.Warnf("dynamic cup pickup: pass %d/%d — observe pose %q unreachable, skipping pass: %v", i+1, passes, poseName, err)
			continue
		}

		centroids, err := s.observeVantage(ctx)
		if err != nil {
			return nil, fmt.Errorf("dynamic_cup_pickup: pass %d: %w", i+1, err)
		}
		totalDetections += len(centroids)
		if len(centroids) == 0 {
			s.logger.Infof("dynamic cup pickup: pass %d/%d found no cup — trying next observe pose", i+1, passes)
			continue
		}

		merged := mergeNearbyCentroids(centroids, cupObserveDedupMm)
		s.logger.Infof("dynamic cup pickup: pass %d/%d — target=(x=%.1f, y=%.1f, z=%.1f) cutoff=%.0fmm — %d candidate(s) (%d before merge):",
			i+1, passes, target.X, target.Y, target.Z, cutoff, len(merged), len(centroids))
		for j, c := range merged {
			d := c.Sub(target).Norm()
			annotation := ""
			if cutoff > 0 && d > cutoff {
				annotation = " [REJECTED — beyond cutoff]"
			}
			s.logger.Infof("  candidate[%d] world=(x=%.1f, y=%.1f, z=%.1f) distance=%.1fmm%s",
				j, c.X, c.Y, c.Z, d, annotation)
		}

		ranked := rankCupCentroids(merged, target, cutoff)
		if len(ranked) == 0 {
			s.logger.Infof("dynamic cup pickup: pass %d/%d saw %d cup(s) but none within %.0fmm of target — trying next observe pose",
				i+1, passes, len(merged), cutoff)
			continue
		}

		s.logger.Infof("dynamic cup pickup: pass %d/%d — %d in-range candidate(s) (closest first):", i+1, passes, len(ranked))
		for j, c := range ranked {
			s.logger.Infof("  rank[%d] world=(x=%.1f, y=%.1f, z=%.1f) distance=%.1fmm",
				j, c.X, c.Y, c.Z, c.Sub(target).Norm())
		}
		return ranked, nil
	}

	if totalDetections == 0 {
		return nil, fmt.Errorf("dynamic_cup_pickup: %w across all %d observe pose(s)", errNoCupsDetected, passes)
	}
	return nil, fmt.Errorf("dynamic_cup_pickup: %d detection(s) across all observe poses but none within %.0fmm of target", totalDetections, cutoff)
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
		componentName: componentClaws,
	}
	grabPD := &poseData{
		pose:          composeCupPose(centroid, relativePoseToSpatial(s.cfg.CupGrabRelativePose)),
		refFrame:      referenceframe.World,
		componentName: componentClaws,
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

	observeStep := Step{PoseName: camPoseCupObserve, Component: componentCam, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, observeStep); err != nil {
		s.logger.Warnf("dynamic cup pickup: recover to cup_observe: %v", err)
	}
}

// pickCupDynamic sweeps the camera-observe poses (findCupCandidates, stopping
// at the first pose that sees a reachable cup) and walks the ranked candidate
// list grabbing the first reachable cup. Falls through to the next candidate on
// planning failures and re-observes (up to CupPickupMaxAttempts attempts) when
// no observe pose sees a cup or all candidates in a batch fail planning. Called
// by setCupForCoffee when DynamicCupPickup=true.
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
		// findCupCandidates sweeps the observe poses itself (stopping at the
		// first that sees a cup), so there is no separate pre-move here.
		detectCtx, detectSpan := trace.StartSpan(ctx, "beanjamin::dynamic_cup_pickup::detect")
		candidates, err := s.findCupCandidates(detectCtx, cancelCtx)
		detectSpan.End()
		if err != nil {
			// "No cups detected" is recoverable: there may be no cups,
			// or the vision service is having a bad day. Announce + wait,
			// then re-observe on the next outer iteration. Bail on any
			// other failure (e.g. detections all beyond the cutoff) —
			// re-observing won't change those.
			if errors.Is(err, errNoCupsDetected) && attempt < maxAttempts {
				recoverStep := Step{PoseName: camPoseCupObserve, Component: componentCam, Pause: shortPause}
				if mvErr := s.executeStep(ctx, cancelCtx, recoverStep); mvErr != nil {
					s.logger.Warnf("dynamic cup pickup: return to cup_observe before retry wait: %v", mvErr)
				}
				const msg = "I don't see a cup yet — please place one on the shelf. Trying again in 15 seconds."
				if sayErr := s.sayAlways(ctx, msg); sayErr != nil {
					s.logger.Warnf("dynamic cup pickup: announcement failed: %v", sayErr)
				}
				s.logger.Infof("dynamic cup pickup: no cups detected on attempt %d/%d — waiting %s before retry",
					attempt, maxAttempts, noCupsRetryDelay)
				select {
				case <-time.After(noCupsRetryDelay):
				case <-ctx.Done():
					return fmt.Errorf("dynamic_cup_pickup: cancelled during no-cups wait: %w", ctx.Err())
				}
				lastErr = err
				continue
			}
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
