// Package beanjamin: dynamic cup pickup.
//
// pickCupDynamic replaces the static empty_cup grab in setCupForCoffee
// when dynamic_cup_pickup is enabled. It sweeps the poses on the dedicated
// camera-observe switch (camera_observe_pose_switcher_name) one at a time,
// calling a vision service for cup detections at each. As soon as a pose
// yields at least one detection the sweep stops — the next observe pose is
// only tried when the current one failed to find a cup. Detections are lifted
// into world frame and ranked by proximity to the gripper (closest first), and
// the configured approach/grab relative poses (from Config — they are offsets,
// not switch-resident world-frame poses) are composed onto the chosen centroid
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
	"math"
	"sort"
	"time"

	"github.com/golang/geo/r3"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/services/vision"
	"go.viam.com/rdk/spatialmath"
	viz "go.viam.com/rdk/vision"
)

// Pickup item labels — used for logs/spans/errors and as the cache key for
// held-item geometry tracking (held_geometry.go).
const (
	pickupLabelCup   = "cup"
	pickupLabelGlass = "glass"
)

// pickupCandidate is one detected item: its world-frame grasp centroid plus the
// world-frame detected geometry (nil when geometry is unavailable). The geometry
// rides alongside the centroid so the held-item tracker can attach the detected
// shape to the gripper after a successful grab.
type pickupCandidate struct {
	centroid r3.Vector
	geom     spatialmath.Geometry
}

// errNoItemsDetected is returned by findCandidates when the vision frames at
// every observe pose yielded zero detections. pickDynamic recognises this case
// via errors.Is and recovers with a spoken "please place a cup/glass"
// announcement + a wait before re-observing, instead of failing the order
// outright. Shared by cup and glass pickup.
var errNoItemsDetected = errors.New("no items detected")

// noItemRetryDelay is the wait between outer observation attempts when
// findCandidates reports zero detections.
const noItemRetryDelay = 15 * time.Second

// observeDedupMm is the merge radius used to collapse near-duplicate detections
// across multi-vantage observations: two centroids closer than this in world
// frame are treated as the same physical item.
const observeDedupMm = 40.0

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

// rankCentroidsByProximity returns centroids sorted by distance to reference
// ascending (closest first). reference is the gripper's world-frame position, so
// the item nearest the gripper is grabbed first. The returned slice is a new
// allocation; the input is not mutated. Ties keep their original relative order
// (stable sort).
func rankCentroidsByProximity(centroids []r3.Vector, reference r3.Vector) []r3.Vector {
	ranked := append([]r3.Vector(nil), centroids...)
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].Sub(reference).Norm() < ranked[j].Sub(reference).Norm()
	})
	return ranked
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

// gripperWorldPoint returns the world-frame position of the gripper — the
// componentClaws frame the grab actually moves onto a cup/glass. Dynamic pickup
// ranks detected items by proximity to this point (findCandidates), so the item
// nearest the gripper at the observe pose is attempted first.
func (s *beanjaminCoffee) gripperWorldPoint(ctx context.Context) (r3.Vector, error) {
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return r3.Vector{}, err
	}
	pif := referenceframe.NewPoseInFrame(componentClaws, spatialmath.NewZeroPose())
	tf, err := fs.Transform(fsInputs.ToLinearInputs(), pif, referenceframe.World)
	if err != nil {
		return r3.Vector{}, fmt.Errorf("transform gripper frame %q to world: %w", componentClaws, err)
	}
	return tf.(*referenceframe.PoseInFrame).Pose().Point(), nil
}

// pickupTarget bundles everything the dynamic pickup pipeline needs to find and
// grab one kind of item. Cups and glasses each build one (cupPickupTarget /
// glassPickupTarget); the pipeline methods (observeVantage, findCandidates,
// tryGrab, recoverToObserve, pickDynamic) are generic over it. This keeps glass
// detection — its own vision service and observe poses, tuned for the taller
// glass — fully independent of cup detection while sharing one implementation.
type pickupTarget struct {
	label            string              // "cup" / "glass" — logs, spans, errors
	vision           vision.Service      // detector for this item
	cameraName       string              // camera frame for centroid->world (shared)
	observeSw        toggleswitch.Switch // switch holding the observe vantages
	observeComponent string              // routing key for executeStep/switchForComponent
	observeHomePose  string              // recovery pose name on observeSw
	approachRel      *RelativePose       // gripper offset for the pre-grab pose
	grabRel          *RelativePose       // gripper offset for the grab pose
	photosPerVantage int                 // vision frames per observe pose
	maxAttempts      int                 // full observe-and-grab attempts
	centroidMinZMm   float64             // floor detection Z to this (0 = disabled)
	noItemSpeak      string              // spoken on "nothing detected" before a retry wait
}

// cupPickupTarget describes dynamic cup pickup. Reproduces the values the cup
// pipeline used before the generic refactor.
func (s *beanjaminCoffee) cupPickupTarget() *pickupTarget {
	return &pickupTarget{
		label:            pickupLabelCup,
		vision:           s.cupVision,
		cameraName:       s.cupCameraName,
		observeSw:        s.cameraObserveSw,
		observeComponent: componentCam,
		observeHomePose:  camPoseCupObserve,
		approachRel:      s.cfg.CupApproachRelativePose,
		grabRel:          s.cfg.CupGrabRelativePose,
		photosPerVantage: pickupPhotosPerVantage(s.cfg.CupPhotosPerVantage),
		maxAttempts:      pickupMaxAttempts(s.cfg.CupPickupMaxAttempts),
		centroidMinZMm:   s.cfg.CupCentroidMinZMm,
		noItemSpeak:      "I don't see a cup yet — please place one on the shelf. Trying again in 15 seconds.",
	}
}

// glassPickupTarget describes dynamic iced-coffee glass pickup: its own vision
// service and observe switch, the shared camera, and grasp offsets tuned for the
// taller glass. The photos-per-vantage and max-attempts knobs are shared with
// cup pickup (cup_photos_per_vantage / cup_pickup_max_attempts) — they are
// item-agnostic operational settings.
func (s *beanjaminCoffee) glassPickupTarget() *pickupTarget {
	return &pickupTarget{
		label:            pickupLabelGlass,
		vision:           s.glassVision,
		cameraName:       s.cupCameraName,
		observeSw:        s.glassObserveSw,
		observeComponent: componentGlassCam,
		observeHomePose:  glassPoseObserve,
		approachRel:      s.cfg.GlassApproachRelativePose,
		grabRel:          s.cfg.GlassGrabRelativePose,
		photosPerVantage: pickupPhotosPerVantage(s.cfg.CupPhotosPerVantage),
		maxAttempts:      pickupMaxAttempts(s.cfg.CupPickupMaxAttempts),
		centroidMinZMm:   s.cfg.GlassCentroidMinZMm,
		noItemSpeak:      "I don't see a glass yet — please place one on the top shelf. Trying again in 15 seconds.",
	}
}

// observeVantage captures t.photosPerVantage vision frames at the arm's current
// pose, accumulates every detection from all of them, and lifts each into world
// coordinates. Returns the pickup candidates (world-frame centroid + geometry).
// Returns a nil slice with no error when no frame produced a detection, so the
// sweep in findCandidates can move on to the next observe pose.
func (s *beanjaminCoffee) observeVantage(ctx context.Context, t *pickupTarget) ([]pickupCandidate, error) {
	logger := s.activeOrderLogger()
	photosToTake := t.photosPerVantage

	var objects []*viz.Object
	for photo := 1; photo <= photosToTake; photo++ {
		// Pass an empty camera name so the vision service falls back to its own
		// configured default camera. t.cameraName is still used below to
		// transform detection centroids from the camera frame into world coords.
		objs, err := t.vision.GetObjectPointClouds(ctx, "", nil)
		if err != nil {
			return nil, fmt.Errorf("detect: %w", err)
		}
		logger.Infof("dynamic %s pickup: vision photo %d/%d, found %d detections", t.label, photo, photosToTake, len(objs))
		objects = append(objects, objs...)
	}
	if len(objects) == 0 {
		return nil, nil
	}

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return nil, err
	}

	candidates := make([]pickupCandidate, 0, len(objects))
	for _, obj := range objects {
		if obj.Geometry == nil {
			continue
		}
		local := obj.Geometry.Pose().Point()
		world, err := cameraToWorld(fs, fsInputs, t.cameraName, local)
		if err != nil {
			return nil, err
		}
		// Lift the full detection geometry (not just its centroid) into world so
		// the held-item tracker can attach the detected shape after the grab.
		geomWorld, err := geometryToWorld(fs, fsInputs, t.cameraName, obj.Geometry)
		if err != nil {
			return nil, err
		}
		if floor := t.centroidMinZMm; floor != 0 && world.Z < floor {
			delta := floor - world.Z
			logger.Infof("dynamic %s pickup: flooring centroid Z from %.1f to %.1f (centroid_min_z_mm)",
				t.label, world.Z, floor)
			world.Z = floor
			// Shift the geometry up by the same delta so it stays centered on the
			// (floored) grasp centroid.
			geomWorld = geomWorld.Transform(spatialmath.NewPoseFromPoint(r3.Vector{Z: delta}))
		}
		logger.Debugf("dynamic %s pickup: detection at camera-local %v -> world %v", t.label, local, world)
		candidates = append(candidates, pickupCandidate{centroid: world, geom: geomWorld})
	}
	return candidates, nil
}

// observationPoseNames returns the names of every pose on the given observe
// switch.
func (s *beanjaminCoffee) observationPoseNames(ctx context.Context, sw toggleswitch.Switch) ([]string, error) {
	if sw == nil {
		return nil, fmt.Errorf("no observe pose switch configured")
	}
	_, names, err := sw.GetNumberOfPositions(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("enumerate observe poses: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("observe pose switch has no positions")
	}
	return names, nil
}

// findCandidates sweeps the target's observe poses one at a time and returns
// the ranked pickup candidates from the first pose that sees an item.
//
// At each pose it moves the camera there, observes (observeVantage), merges
// near-duplicate detections, and ranks them by proximity to the gripper
// (closest first). The first pose that yields at least one detection wins and
// the remaining poses are not visited — the next observe pose is only tried when
// the current one found nothing. An unreachable pose is logged and skipped.
//
// When no pose produces any detection, findCandidates returns errNoItemsDetected
// so pickDynamic can recover (announce + wait + re-observe).
func (s *beanjaminCoffee) findCandidates(ctx, cancelCtx context.Context, t *pickupTarget) ([]pickupCandidate, error) {
	logger := s.activeOrderLogger()
	poseNames, err := s.observationPoseNames(ctx, t.observeSw)
	if err != nil {
		return nil, fmt.Errorf("dynamic_%s_pickup: %w", t.label, err)
	}

	passes := len(poseNames)
	for i, poseName := range poseNames {
		logger.Infof("dynamic %s pickup: pass %d/%d — moving to observe pose %q", t.label, i+1, passes, poseName)
		// Pause briefly after arriving so the camera frame is stable before
		// detection. t.observeComponent routes the fetch to the right switch.
		step := Step{PoseName: poseName, Component: t.observeComponent, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			logger.Warnf("dynamic %s pickup: pass %d/%d — observe pose %q unreachable, skipping pass: %v", t.label, i+1, passes, poseName, err)
			continue
		}

		detected, err := s.observeVantage(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("dynamic_%s_pickup: pass %d: %w", t.label, i+1, err)
		}
		if len(detected) == 0 {
			logger.Infof("dynamic %s pickup: pass %d/%d found nothing — trying next observe pose", t.label, i+1, passes)
			continue
		}

		// Rank by proximity to the gripperPosition so the item nearest the gripperPosition at
		// this observe pose is attempted first.
		gripperPosition, err := s.gripperWorldPoint(ctx)
		if err != nil {
			return nil, fmt.Errorf("dynamic_%s_pickup: pass %d: %w", t.label, i+1, err)
		}

		// Merge/rank operate on centroids; the geometry is matched back to each
		// ranked centroid afterward (mergeNearbyCentroids averages centroids, so
		// geometry can't be threaded through the merge directly).
		centroids := centroidsOf(detected)
		merged := mergeNearbyCentroids(centroids, observeDedupMm)
		ranked := rankCentroidsByProximity(merged, gripperPosition)
		candidates := candidatesForCentroids(ranked, detected)
		logger.Infof("dynamic %s pickup: pass %d/%d — gripper=(x=%.1f, y=%.1f, z=%.1f) — %d candidate(s) (%d before merge), closest first:",
			t.label, i+1, passes, gripperPosition.X, gripperPosition.Y, gripperPosition.Z, len(candidates), len(detected))
		for j, c := range candidates {
			logger.Infof("  rank[%d] world=(x=%.1f, y=%.1f, z=%.1f) distance=%.1fmm",
				j, c.centroid.X, c.centroid.Y, c.centroid.Z, c.centroid.Sub(gripperPosition).Norm())
		}
		return candidates, nil
	}

	return nil, fmt.Errorf("dynamic_%s_pickup: %w across all %d observe pose(s)", t.label, errNoItemsDetected, passes)
}

// centroidsOf extracts the world-frame centroids from a slice of candidates,
// preserving order. Used to feed the centroid-only merge/rank helpers.
func centroidsOf(candidates []pickupCandidate) []r3.Vector {
	out := make([]r3.Vector, len(candidates))
	for i, c := range candidates {
		out[i] = c.centroid
	}
	return out
}

// candidatesForCentroids pairs each ranked centroid with the geometry of the
// nearest original detection, so the held-item tracker can attach the detected
// shape after the grab. Geometry is matched back by nearest original rather than
// threaded through the merge (which averages centroids).
func candidatesForCentroids(ranked []r3.Vector, originals []pickupCandidate) []pickupCandidate {
	out := make([]pickupCandidate, len(ranked))
	for i, c := range ranked {
		out[i] = pickupCandidate{centroid: c, geom: nearestGeometry(c, originals)}
	}
	return out
}

// nearestGeometry returns the geometry of the original detection whose centroid
// is closest to c, skipping detections with no geometry. Returns nil when no
// original carries a geometry.
func nearestGeometry(c r3.Vector, originals []pickupCandidate) spatialmath.Geometry {
	var best spatialmath.Geometry
	bestDist := math.MaxFloat64
	for _, o := range originals {
		if o.geom == nil {
			continue
		}
		if d := o.centroid.Sub(c).Norm(); d < bestDist {
			bestDist = d
			best = o.geom
		}
	}
	return best
}

// tryGrab attempts a full approach-grab-retreat cycle on one candidate
// centroid. On failure after the approach step, it best-effort restores the
// arm to the target's observe home pose so the caller can attempt a different
// candidate from a known good state.
//
// Returned errors fall into two categories the caller distinguishes via
// errors.Is:
//   - wraps errMotionPlanning → planning failure; try a different candidate.
//   - anything else → execution error or operator cancel; bubble up.
func (s *beanjaminCoffee) tryGrab(ctx, cancelCtx context.Context, t *pickupTarget, cand pickupCandidate) error {
	centroid := cand.centroid
	approachPD := &poseData{
		pose:          composeCupPose(centroid, relativePoseToSpatial(t.approachRel)),
		refFrame:      referenceframe.World,
		componentName: componentClaws,
	}
	grabPD := &poseData{
		pose:          composeCupPose(centroid, relativePoseToSpatial(t.grabRel)),
		refFrame:      referenceframe.World,
		componentName: componentClaws,
	}

	// 1. Approach (free planning). On failure the arm has not moved.
	if err := s.moveToRawPose(ctx, approachPD, nil, nil, nil); err != nil {
		return fmt.Errorf("approach centroid (x=%.1f, y=%.1f, z=%.1f): %w", centroid.X, centroid.Y, centroid.Z, err)
	}

	// 2. Open gripper before descending.
	if err := s.gripper.Open(ctx, nil); err != nil {
		s.recoverToObserve(ctx, cancelCtx, t)
		return fmt.Errorf("open gripper for grab: %w", err)
	}
	time.Sleep(gripperPause)

	// 3. Linear descent to grab pose.
	if err := s.moveToRawPose(ctx, grabPD, defaultApproachConstraint, nil, nil); err != nil {
		s.recoverToObserve(ctx, cancelCtx, t)
		return fmt.Errorf("grab centroid (x=%.1f, y=%.1f, z=%.1f): %w", centroid.X, centroid.Y, centroid.Z, err)
	}

	if err := s.grabAndVerifyHolding(ctx); err != nil {
		s.recoverToObserve(ctx, cancelCtx, t)
		return fmt.Errorf("grab %s: %w", t.label, err)
	}

	// Attach the detected geometry to the gripper while the arm is at the grab
	// pose, so the retreat (and everything until release) plans around the held
	// item. Best-effort: a tracking-attach hiccup degrades to untracked motion
	// (the prior behavior) rather than aborting a physical order mid-grab.
	if err := s.attachDetectedGeometry(ctx, t.label, cand.geom); err != nil {
		s.activeOrderLogger().Warnf("dynamic %s pickup: attach geometry failed, continuing untracked: %v", t.label, err)
	}

	// 5. Linear retreat with the item in hand. A failure here is fatal — we
	// can't drop the item safely by recovering to observe. Strip the
	// errMotionPlanning chain (%v, not %w) so the caller does not treat this
	// as a try-another-candidate planning failure.
	if err := s.moveToRawPose(ctx, approachPD, defaultApproachConstraint, nil, nil); err != nil {
		return fmt.Errorf("retreat with %s grabbed (centroid x=%.1f, y=%.1f, z=%.1f): %v", t.label, centroid.X, centroid.Y, centroid.Z, err)
	}
	return nil
}

// recoverToObserve best-effort returns the arm to the target's observe home
// pose so the next candidate (or the next observation) starts from a known
// state. Errors are logged, not returned — the caller is already returning an
// error.
func (s *beanjaminCoffee) recoverToObserve(ctx, cancelCtx context.Context, t *pickupTarget) {
	logger := s.activeOrderLogger()
	// Close the gripper to a safe configuration before traversing back. A
	// stray open gripper has a larger collision silhouette than a closed one.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		logger.Warnf("dynamic %s pickup: recover: close gripper: %v", t.label, err)
	}
	time.Sleep(gripperPause)

	observeStep := Step{PoseName: t.observeHomePose, Component: t.observeComponent, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, observeStep); err != nil {
		logger.Warnf("dynamic %s pickup: recover to %q: %v", t.label, t.observeHomePose, err)
	}
}

// pickCupDynamic picks an empty cup via the dynamic pipeline. Called by
// setCupForCoffee when DynamicCupPickup=true.
func (s *beanjaminCoffee) pickCupDynamic(ctx, cancelCtx context.Context) error {
	return s.pickDynamic(ctx, cancelCtx, s.cupPickupTarget())
}

// pickGlassDynamic picks an iced-coffee glass via the dynamic pipeline (its own
// vision service + observe switch). Called by fetchGlass when
// DynamicGlassPickup=true.
func (s *beanjaminCoffee) pickGlassDynamic(ctx, cancelCtx context.Context) error {
	return s.pickDynamic(ctx, cancelCtx, s.glassPickupTarget())
}

// pickDynamic sweeps the target's observe poses (findCandidates, stopping at the
// first pose that sees a reachable item) and walks the ranked candidate list
// grabbing the first reachable one. Falls through to the next candidate on
// planning failures and re-observes (up to t.maxAttempts attempts) when no
// observe pose sees an item or all candidates in a batch fail planning. Shared
// by cup and glass pickup.
func (s *beanjaminCoffee) pickDynamic(ctx, cancelCtx context.Context, t *pickupTarget) error {
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(ctx, "beanjamin::dynamic_pickup::"+t.label)
	defer span.End()

	// Merge cancelCtx into ctx so operator cancel interrupts moveToRawPose
	// and gripper calls. Mirrors motion.go executePivot / executeCircularMotion.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(cancelCtx, func() { cancel() })
	defer stop()
	defer cancel()

	if s.gripper == nil {
		return fmt.Errorf("dynamic_%s_pickup: no gripper configured", t.label)
	}

	maxAttempts := t.maxAttempts
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// findCandidates sweeps the observe poses itself (stopping at the first
		// that sees an item), so there is no separate pre-move here.
		detectCtx, detectSpan := trace.StartSpan(ctx, "beanjamin::dynamic_pickup::"+t.label+"::detect")
		candidates, err := s.findCandidates(detectCtx, cancelCtx, t)
		detectSpan.End()
		if err != nil {
			// "Nothing detected" is recoverable: there may be no item, or the
			// vision service is having a bad day. Announce + wait, then
			// re-observe on the next outer iteration. Bail on any other failure
			// (e.g. an unreachable observe pose or a transform error) —
			// re-observing won't change those.
			if errors.Is(err, errNoItemsDetected) && attempt < maxAttempts {
				recoverStep := Step{PoseName: t.observeHomePose, Component: t.observeComponent, Pause: shortPause}
				if mvErr := s.executeStep(ctx, cancelCtx, recoverStep); mvErr != nil {
					logger.Warnf("dynamic %s pickup: return to %q before retry wait: %v", t.label, t.observeHomePose, mvErr)
				}
				if sayErr := s.sayAlways(ctx, t.noItemSpeak); sayErr != nil {
					logger.Warnf("dynamic %s pickup: announcement failed: %v", t.label, sayErr)
				}
				logger.Infof("dynamic %s pickup: nothing detected on attempt %d/%d — waiting %s before retry",
					t.label, attempt, maxAttempts, noItemRetryDelay)
				select {
				case <-time.After(noItemRetryDelay):
				case <-ctx.Done():
					return fmt.Errorf("dynamic_%s_pickup: cancelled during no-item wait: %w", t.label, ctx.Err())
				}
				lastErr = err
				continue
			}
			return err
		}

		logger.Infof("dynamic %s pickup: attempt %d/%d — %d candidate(s) to try", t.label, attempt, maxAttempts, len(candidates))
		for i, cand := range candidates {
			err := s.tryGrab(ctx, cancelCtx, t, cand)
			if err == nil {
				return nil
			}
			lastErr = err

			// Operator cancel always wins.
			if ctx.Err() != nil {
				return fmt.Errorf("dynamic_%s_pickup: cancelled: %w", t.label, err)
			}

			if !errors.Is(err, errMotionPlanning) && !errors.Is(err, errGripMissed) {
				return err
			}
			logger.Warnf("dynamic %s pickup: attempt %d, candidate %d/%d failed (planning or grab miss) — trying next: %v",
				t.label, attempt, i+1, len(candidates), err)
		}
	}
	return fmt.Errorf("dynamic_%s_pickup: exhausted %d attempt(s); last error: %w", t.label, maxAttempts, lastErr)
}
