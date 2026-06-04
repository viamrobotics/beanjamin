// Package beanjamin: dynamic cup pickup.
//
// pickCupDynamic replaces the static empty_cup grab in setCupForCoffee
// when dynamic_cup_pickup is enabled. It drives the arm through every pose on
// the dedicated cup-observe switch (cup_observe_pose_switcher_name),
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

// cupObserveHomePose is the pose name on the cup-observe switch used as the
// home / recovery position (it is also swept like any other observe pose).
const cupObserveHomePose = "cup_observe"

// mergeNearbyCentroids clusters centroids that fall within mm of an existing
// cluster's running mean and returns one centroid per cluster: the mean of its
// members. Merging the detections of the same physical cup seen from several
// vantages yields a better position estimate than any single detection. First-seen
// order determines cluster assignment. Input is not mutated. mm <= 0 disables
// merging and returns a copy.
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
	photos := s.cupPhotosPerVantage()
	maxEmptyAttempts := s.cfg.CupDetectionRetries + 1
	sleep := time.Duration(s.cfg.CupDetectionRetrySleepMs) * time.Millisecond
	if sleep <= 0 {
		sleep = 250 * time.Millisecond
	}

	// Take cup_photos_per_vantage frames at this pose and accumulate every
	// detection from all of them — feeding repeated detections of the same cup
	// into the cross-vantage merge averages out per-frame centroid noise. Within
	// a single photo, retry up to CupDetectionRetries times if the frame comes
	// back empty (a transient miss); a non-empty frame is used as-is.
	var objects []*viz.Object
	for photo := 1; photo <= photos; photo++ {
		var objs []*viz.Object
		for attempt := 1; attempt <= maxEmptyAttempts; attempt++ {
			// Pass an empty camera name so the vision service falls back to its own
			// configured default camera. s.cupCameraName is still used below to
			// transform detection centroids from the camera frame into world coords.
			got, err := s.cupVision.GetObjectPointClouds(ctx, "", nil)
			if err != nil {
				return nil, nil, fmt.Errorf("detect: %w", err)
			}
			objs = got
			if len(objs) > 0 {
				break
			}
			if attempt < maxEmptyAttempts {
				select {
				case <-time.After(sleep):
				case <-ctx.Done():
					return nil, nil, fmt.Errorf("cancelled during retry: %w", ctx.Err())
				}
			}
		}
		s.logger.Infof("dynamic cup pickup: vision photo %d/%d, found %d detections", photo, photos, len(objs))
		objects = append(objects, objs...)
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

// cupObservations is the merged result of sweeping every observation vantage:
// empty-cup pickup candidates (below the shelf split) and already-on-shelf cup
// geometries (above it), each merged/deduped across vantages in world frame.
// rawDetections is the count of usable detections summed across all vantages,
// before the cross-vantage merge; vantages is the number of poses swept. Both
// are used only for logging.
type cupObservations struct {
	pickup        []r3.Vector
	onShelf       []spatialmath.Geometry
	rawDetections int
	vantages      int
}

// observeAllVantages drives the claws to every pose on the dedicated cup-observe
// switch (CupObservePoseSwitcherName), calls observeOnce at each, and merges the
// detections across all passes into a single cupObservations. It always visits
// every reachable vantage so the on-shelf occupancy map and the pickup centroids
// reflect every angle: a cup occluded from one view is still seen (and counted)
// from another. This is what keeps us from dropping a fresh cup on top of one a
// single view missed. An unreachable pose is logged and skipped.
func (s *beanjaminCoffee) observeAllVantages(ctx, cancelCtx context.Context, shelfTopZ float64, hasShelfCfg bool) (cupObservations, error) {
	if s.cupObserveSw == nil {
		return cupObservations{}, fmt.Errorf("no cup observe pose switch configured")
	}
	_, poseNames, err := s.cupObserveSw.GetNumberOfPositions(ctx, nil)
	if err != nil {
		return cupObservations{}, fmt.Errorf("enumerate cup observe poses: %w", err)
	}
	if len(poseNames) == 0 {
		return cupObservations{}, fmt.Errorf("cup observe pose switch has no positions")
	}

	passes := len(poseNames)
	allCentroids := make([]r3.Vector, 0)
	allOnShelf := make([]spatialmath.Geometry, 0)
	totalDetections := 0
	for i, poseName := range poseNames {
		s.logger.Infof("dynamic cup pickup: pass %d/%d — moving to observe pose %q", i+1, passes, poseName)
		// Pause briefly after arriving so the camera frame is stable before
		// detection. The selector routes the fetch to the cup-observe switch.
		step := Step{PoseName: poseName, Component: s.cfg.CupObservePoseSwitcherName, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			s.logger.Warnf("dynamic cup pickup: pass %d/%d — observe pose %q unreachable, skipping pass: %v", i+1, passes, poseName, err)
			continue
		}

		passCentroids, passOnShelf, err := s.observeOnce(ctx, shelfTopZ, hasShelfCfg)
		if err != nil {
			return cupObservations{}, fmt.Errorf("pass %d: %w", i+1, err)
		}
		totalDetections += len(passCentroids) + len(passOnShelf)
		s.logger.Infof("dynamic cup pickup: pass %d/%d contributed %d pickup, %d on-shelf",
			i+1, passes, len(passCentroids), len(passOnShelf))
		allCentroids = append(allCentroids, passCentroids...)
		allOnShelf = append(allOnShelf, passOnShelf...)
	}

	pickup := mergeNearbyCentroids(allCentroids, cupObserveDedupMm)
	onShelf := dedupeNearbyGeometries(allOnShelf, cupObserveDedupMm)
	s.logger.Infof("dynamic cup pickup: detected %d distinct cup(s) across %d vantage(s) — %d pickup candidate(s), %d on-shelf (raw %d before merge)",
		len(pickup)+len(onShelf), passes, len(pickup), len(onShelf), totalDetections)
	return cupObservations{pickup: pickup, onShelf: onShelf, rawDetections: totalDetections, vantages: passes}, nil
}

// observeCupCandidates orchestrates a full cup observation: it resolves the
// shelf config, sweeps every vantage and merges the detections
// (observeAllVantages), selects a free shelf tile from the merged on-shelf set
// when shelf placement is enabled, and returns the pickup candidates filtered
// by CupMaxDistanceFromTargetMm and sorted by distance to ExpectedCupPositionMm.
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

	obs, err := s.observeAllVantages(ctx, cancelCtx, shelfTopZ, hasShelfCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic_cup_pickup: %w", err)
	}
	centroids := obs.pickup
	onShelfCups := obs.onShelf
	passes := obs.vantages

	if hasShelfCfg {
		s.logger.Infof("dynamic cup pickup: shelf partition — %d pickup candidate(s), %d on-shelf cup(s) (threshold Z=%.1fmm)",
			len(centroids), len(onShelfCups), shelfTopZ)
		if err := s.selectShelfTile(shelfPose, shelfDims, onShelfCups); err != nil {
			return nil, err
		}
	}

	if len(centroids) == 0 {
		if obs.rawDetections == 0 {
			return nil, fmt.Errorf("dynamic_cup_pickup: %w across %d vantage(s)", errNoCupsDetected, passes)
		}
		if hasShelfCfg && len(onShelfCups) > 0 {
			return nil, fmt.Errorf("dynamic_cup_pickup: all %d detection(s) classified as on-shelf (above Z=%.1fmm); no empty-cup candidate", len(onShelfCups), shelfTopZ)
		}
		return nil, fmt.Errorf("dynamic_cup_pickup: %d detection(s) had no usable geometry", obs.rawDetections)
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

	observeStep := Step{PoseName: cupObserveHomePose, Component: s.cfg.CupObservePoseSwitcherName, Pause: shortPause}
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
		// observeCupCandidates drives the arm to every observe pose itself,
		// so there is no separate pre-move here.
		detectCtx, detectSpan := trace.StartSpan(ctx, "beanjamin::dynamic_cup_pickup::detect")
		candidates, err := s.observeCupCandidates(detectCtx, cancelCtx)
		detectSpan.End()
		if err != nil {
			// "No cups detected" is recoverable: there may be no cups,
			// or the vision service is having a bad day. Announce + wait,
			// then re-observe on the next outer iteration. Bail on any
			// other failure (out-of-range, all-on-shelf, etc.)
			// — re-observing won't change those.
			if errors.Is(err, errNoCupsDetected) && attempt < maxAttempts {
				recoverStep := Step{PoseName: cupObserveHomePose, Component: s.cfg.CupObservePoseSwitcherName, Pause: shortPause}
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
