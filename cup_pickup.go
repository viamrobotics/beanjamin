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

// errNoCupsDetected is returned by observeCupCandidates when every
// vantage's vision call (including its zero-detection retries) yielded
// zero detections. pickCupDynamic recognises this case via errors.Is and
// recovers with a spoken "please place a cup" announcement + a wait
// before re-observing, instead of failing the order outright.
var errNoCupsDetected = errors.New("no cups detected")

// noCupsRetryDelay is the wait between outer observation attempts when
// observeCupCandidates reports zero detections.
const noCupsRetryDelay = 15 * time.Second

// cupObserveDedupMm is the merge radius used to collapse near-duplicate
// detections across multi-vantage observations: two centroids closer than
// this in world frame are treated as the same physical cup.
const cupObserveDedupMm = 40.0

// dedupeNearbyCentroids returns a copy of centroids with near-duplicates
// removed: any centroid within mm of a kept centroid is dropped. First
// occurrence wins. Input is not mutated. mm <= 0 disables deduplication.
func dedupeNearbyCentroids(centroids []r3.Vector, mm float64) []r3.Vector {
	if mm <= 0 || len(centroids) <= 1 {
		return append([]r3.Vector(nil), centroids...)
	}
	out := make([]r3.Vector, 0, len(centroids))
	for _, c := range centroids {
		dup := false
		for _, k := range out {
			if c.Sub(k).Norm() < mm {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

// dedupeNearbyGeometries collapses geometries whose pose points sit within
// mm of an already-kept geometry's pose point in world frame. Behaves like
// dedupeNearbyCentroids; first occurrence wins. mm <= 0 disables.
func dedupeNearbyGeometries(geoms []spatialmath.Geometry, mm float64) []spatialmath.Geometry {
	if mm <= 0 || len(geoms) <= 1 {
		return append([]spatialmath.Geometry(nil), geoms...)
	}
	out := make([]spatialmath.Geometry, 0, len(geoms))
	for _, g := range geoms {
		gp := g.Pose().Point()
		dup := false
		for _, k := range out {
			if gp.Sub(k.Pose().Point()).Norm() < mm {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, g)
		}
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

// observeOnce calls the vision service for cup detections at the arm's
// current pose, retries on empty results per the configured retry policy,
// lifts each detection's centroid into world coordinates, and partitions
// the results by shelf-top Z when hasShelfCfg is true. Returns the pickup
// centroids and the on-shelf geometries (in world frame). Returns nil
// results with no error when no detections remain after retries, so
// observeCupCandidates can move on to the next vantage.
func (s *beanjaminCoffee) observeOnce(ctx context.Context, shelfTopZ float64, hasShelfCfg bool) ([]r3.Vector, []spatialmath.Geometry, error) {
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
			return nil, nil, fmt.Errorf("detect: %w", err)
		}
		s.logger.Infof("dynamic cup pickup: vision attempt %d/%d, found %d detections", attempt, maxAttempts, len(objs))
		if len(objs) > 0 {
			objects = objs
			break
		}
		if attempt < maxAttempts {
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return nil, nil, fmt.Errorf("cancelled during retry: %w", ctx.Err())
			}
		}
	}
	if len(objects) == 0 {
		return nil, nil, nil
	}

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return nil, nil, err
	}

	centroids := make([]r3.Vector, 0, len(objects))
	var onShelfCups []spatialmath.Geometry
	if hasShelfCfg {
		onShelfCups = make([]spatialmath.Geometry, 0, len(objects))
	}

	for _, obj := range objects {
		if obj.Geometry == nil {
			continue
		}
		local := obj.Geometry.Pose().Point()
		world, err := cameraToWorld(fs, fsInputs, s.cupCameraName, local)
		if err != nil {
			return nil, nil, err
		}
		if floor := s.cfg.CupCentroidMinZMm; floor != 0 && world.Z < floor {
			s.logger.Infof("dynamic cup pickup: flooring centroid Z from %.1f to %.1f (cup_centroid_min_z_mm)",
				world.Z, floor)
			world.Z = floor
		}

		// Detections whose centroid sits above the shelf top surface are
		// already-served cups; lift their geometry to world frame for the
		// shelf-tile occupancy check and exclude them from pickup ranking.
		if hasShelfCfg && world.Z > shelfTopZ {
			gif := referenceframe.NewGeometriesInFrame(s.cupCameraName, []spatialmath.Geometry{obj.Geometry})
			worldGifTF, err := fs.Transform(fsInputs.ToLinearInputs(), gif, referenceframe.World)
			if err != nil {
				return nil, nil, fmt.Errorf("transform geometry to world: %w", err)
			}
			geos := worldGifTF.(*referenceframe.GeometriesInFrame).Geometries()
			if len(geos) > 0 {
				onShelfCups = append(onShelfCups, geos[0])
			}
			s.logger.Debugf("dynamic cup pickup: detection world=%v above shelf-top Z=%.1fmm — on-shelf, excluded from pickup",
				world, shelfTopZ)
			continue
		}

		s.logger.Debugf("dynamic cup pickup: detection at camera-local %v -> world %v", local, world)
		centroids = append(centroids, world)
	}
	return centroids, onShelfCups, nil
}

// observeCupCandidates drives the arm through the configured cup_observe
// pose plus any CupObserveAlternates poses, calls vision at each, merges
// detections across passes (collapsing near-duplicates within
// cupObserveDedupMm in world frame), lifts everything into world
// coordinates, filters by CupMaxDistanceFromTargetMm, and returns the
// pickup candidates sorted by distance to ExpectedCupPositionMm.
//
// The caller is expected to have already moved the arm to cup_observe;
// pass 0 reuses that position, and passes 1..N drive to each named pose
// in CupObserveAlternates via the claws switch. An unreachable pose is
// logged and skipped.
func (s *beanjaminCoffee) observeCupCandidates(ctx, cancelCtx context.Context) ([]r3.Vector, error) {
	hasShelfCfg := s.cfg.PlaceCupOnShelf
	var (
		shelfPose spatialmath.Pose
		shelfDims r3.Vector
		shelfTopZ float64
	)
	if hasShelfCfg {
		pose, dims, err := s.shelfTopGeometry(ctx)
		if err != nil {
			return nil, fmt.Errorf("dynamic_cup_pickup: %w", err)
		}
		shelfPose = pose
		shelfDims = dims
		shelfTopZ = pose.Point().Z + dims.Z/2
	}

	passes := 1 + len(s.cfg.CupObserveAlternates)
	allCentroids := make([]r3.Vector, 0)
	allOnShelf := make([]spatialmath.Geometry, 0)
	totalDetections := 0
	for i := range passes {
		if i > 0 {
			poseName := s.cfg.CupObserveAlternates[i-1]
			const msg = "I couldn't quite find the cup, looking harder."
			if sayErr := s.sayAlways(ctx, msg); sayErr != nil {
				s.logger.Warnf("dynamic cup pickup: announcement failed: %v", sayErr)
			}
			s.logger.Infof("dynamic cup pickup: pass %d/%d — moving to %q", i+1, passes, poseName)
			step := Step{PoseName: poseName, Component: componentClaws, Pause: shortPause}
			if err := s.executeStep(ctx, cancelCtx, step); err != nil {
				s.logger.Warnf("dynamic cup pickup: pass %d/%d — pose %q unreachable, skipping pass: %v", i+1, passes, poseName, err)
				continue
			}
		} else {
			s.logger.Infof("dynamic cup pickup: pass %d/%d — observing at cup_observe", i+1, passes)
		}

		passCentroids, passOnShelf, err := s.observeOnce(ctx, shelfTopZ, hasShelfCfg)
		if err != nil {
			return nil, fmt.Errorf("dynamic_cup_pickup: pass %d: %w", i+1, err)
		}
		totalDetections += len(passCentroids) + len(passOnShelf)
		s.logger.Infof("dynamic cup pickup: pass %d/%d contributed %d pickup, %d on-shelf",
			i+1, passes, len(passCentroids), len(passOnShelf))
		allCentroids = append(allCentroids, passCentroids...)
		allOnShelf = append(allOnShelf, passOnShelf...)

		// Early-bail: if we already have a pickup candidate, the extra
		// vantages aren't needed for the grab itself. The on-shelf bucket
		// loses any data the unvisited vantages would have contributed,
		// which can mask occluded shelf cups — but the alternative is
		// always paying the multi-vantage motion cost on orders where the
		// base view already saw the cup.
		if len(allCentroids) > 0 && i < passes-1 {
			s.logger.Infof("dynamic cup pickup: pass %d/%d found %d pickup candidate(s) — skipping remaining %d vantage(s)",
				i+1, passes, len(allCentroids), passes-i-1)
			break
		}
	}

	centroids := dedupeNearbyCentroids(allCentroids, cupObserveDedupMm)
	onShelfCups := dedupeNearbyGeometries(allOnShelf, cupObserveDedupMm)
	if len(centroids) != len(allCentroids) || len(onShelfCups) != len(allOnShelf) {
		s.logger.Infof("dynamic cup pickup: dedup collapsed %d->%d pickup, %d->%d on-shelf (radius=%.0fmm)",
			len(allCentroids), len(centroids), len(allOnShelf), len(onShelfCups), cupObserveDedupMm)
	}

	if hasShelfCfg {
		s.logger.Infof("dynamic cup pickup: shelf partition — %d pickup candidate(s), %d on-shelf cup(s) (threshold Z=%.1fmm)",
			len(centroids), len(onShelfCups), shelfTopZ)
		if err := s.selectShelfTile(shelfPose, shelfDims, onShelfCups); err != nil {
			return nil, err
		}
	}

	if len(centroids) == 0 {
		if totalDetections == 0 {
			return nil, fmt.Errorf("dynamic_cup_pickup: %w across %d vantage(s)", errNoCupsDetected, passes)
		}
		if hasShelfCfg && len(onShelfCups) > 0 {
			return nil, fmt.Errorf("dynamic_cup_pickup: all %d detection(s) classified as on-shelf (above Z=%.1fmm); no empty-cup candidate", len(onShelfCups), shelfTopZ)
		}
		return nil, fmt.Errorf("dynamic_cup_pickup: %d detection(s) had no usable geometry", totalDetections)
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

	observeStep := Step{PoseName: clawPoseCupObserve, Component: componentClaws, Pause: shortPause}
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
		observeStep := Step{PoseName: clawPoseCupObserve, Component: componentClaws, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, observeStep); err != nil {
			return fmt.Errorf("dynamic_cup_pickup: observe: %w", err)
		}

		detectCtx, detectSpan := trace.StartSpan(ctx, "beanjamin::dynamic_cup_pickup::detect")
		candidates, err := s.observeCupCandidates(detectCtx, cancelCtx)
		detectSpan.End()
		if err != nil {
			// "No cups detected" is recoverable: there may be no cups,
			// or the vision service is having a bd ady. Announce + wait,
			// thenre-observe on the next outer iteration. Bail on any
			// other failure (out-of-range, all-on-shelf, etc.)
			// — re-observing won't change those.
			if errors.Is(err, errNoCupsDetected) && attempt < maxAttempts {
				recoverStep := Step{PoseName: clawPoseCupObserve, Component: componentClaws, Pause: shortPause}
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
