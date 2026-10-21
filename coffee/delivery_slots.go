package coffee

// Direct-to-cart placement for delivery orders. When the cart is docked, the
// finished drink goes into one of its slots instead of the serving shelf. Before
// the drink is grabbed, the empty-handed arm looks through the delivery-cart
// observe poses until the slots detector sees an AprilTag (locateCartSlot).
// Once the drink is in hand, it is carried level to the hole beside the closest
// tag, lowered in and released (placeHeldInCart). Anything short of a release
// (the cart never docks, no tag is seen, the slot cannot be planned) falls back
// to the shelf, which the cart's existing handoff still covers.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/vision/detection3d"

	"beanjamin/coffee/geom"
)

const (
	// defaultDeliverySlotTagOffsetMm is how far the slot's hole sits from its
	// tag, along the tag's own +Y axis.
	defaultDeliverySlotTagOffsetMm = 75.0
	// defaultDeliveryCartWaitSec is how long a ready drink waits for the cart to
	// dock before it goes to the shelf instead.
	defaultDeliveryCartWaitSec = 30.0
	// defaultDeliverySlotSearchSec is how long the observe poses are retried for
	// a tag once the order is reported ready, covering the cart extending its
	// tray.
	defaultDeliverySlotSearchSec = 20.0
	// deliverySlotRetryPause is the wait between passes over the observe poses.
	deliverySlotRetryPause = time.Second

	deliveryCartPollInterval = time.Second
	deliverySlotLabel        = "delivery-slot"
)

// deliverySlot is one detected cart slot: where the held container is lowered
// into, and the tag id reported to the cart as pickup_position.
type deliverySlot struct {
	point r3.Vector
	tagID int
}

func (s *beanjaminCoffee) deliverySlotTagOffsetMm() float64 {
	return orDefault(s.cfg.DeliverySlotTagOffsetMm, defaultDeliverySlotTagOffsetMm)
}

func (s *beanjaminCoffee) deliveryCartWait() time.Duration {
	return time.Duration(orDefault(s.cfg.DeliveryCartWaitSec, defaultDeliveryCartWaitSec) * float64(time.Second))
}

func (s *beanjaminCoffee) deliverySlotSearch() time.Duration {
	return time.Duration(orDefault(s.cfg.DeliverySlotSearchSec, defaultDeliverySlotSearchSec) * float64(time.Second))
}

// locateCartSlot finds the slot a delivery drink will go into, with the
// gripper still empty so the observe poses are free to tilt the camera down at
// the cart. It waits up to deliveryCartWait for the cart to dock for this
// order, then reports the order ready, which is the cart's cue to extend its
// tray, and keeps looking for a tag for up to deliverySlotSearch while it does.
// found is false with a nil error when the drink should go to the shelf
// instead: the cart never docked or no tag was seen.
func (s *beanjaminCoffee) locateCartSlot(ctx, cancelCtx context.Context, orderID string) (slot deliverySlot, found bool, err error) {
	logger := s.activeOrderLogger()
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	if !s.waitForCart(ctx, orderID) {
		if ctx.Err() != nil {
			return deliverySlot{}, false, ctx.Err()
		}
		logger.Infof("delivery cart: not docked after %s — serving on the shelf", s.deliveryCartWait())
		return deliverySlot{}, false, nil
	}
	s.delivery.markReadyForCart(orderID)
	slot, found, err = s.findDeliverySlot(ctx, cancelCtx, s.deliverySlotSearch(), skipDetectFailures)
	if err == nil && !found {
		logger.Warnf("delivery cart: no slot detected within %s — serving on the shelf", s.deliverySlotSearch())
	}
	return slot, found, err
}

// placeHeldInCart puts the held, filled drink into a slot found earlier by
// locateCartSlot; the cart is docked, so the slot has not moved. placed reports
// whether the drink was released into the cart, and may come with an error
// from the retreat after it. Not placed with a nil error means the drink is
// still held and should go to the shelf; not placed with an error means the arm
// cannot safely fall back.
func (s *beanjaminCoffee) placeHeldInCart(ctx, cancelCtx context.Context, slot deliverySlot) (placed bool, err error) {
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	released, err := s.dropHeldInDeliverySlot(ctx, slot)
	if released {
		if err != nil {
			return true, fmt.Errorf("place_in_cart: %w", err)
		}
		return true, nil
	}
	if errors.Is(err, errMotionPlanning) {
		s.activeOrderLogger().Warnf("delivery cart: slot at %v unreachable — serving on the shelf: %v", slot.point, err)
		return false, nil
	}
	return false, fmt.Errorf("place_in_cart: %w", err)
}

// waitForCart reports whether the cart is docked for this order, polling for
// up to deliveryCartWait. False with ctx done means the order was cancelled.
func (s *beanjaminCoffee) waitForCart(ctx context.Context, orderID string) bool {
	timeout := s.deliveryHeartbeatTimeout()
	deadline := time.Now().Add(s.deliveryCartWait())
	for {
		if s.delivery.dockedFor(orderID, timeout) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(deliveryCartPollInterval):
		}
	}
}

// detectFailures says what a slot search does when the detector errors at an
// observe pose.
type detectFailures int

const (
	// skipDetectFailures logs the error and moves on: during an order the drink
	// is fine on the shelf, so a detector fault must not fail it.
	skipDetectFailures detectFailures = iota
	// returnDetectFailures stops the search with the error, for the
	// troubleshooting commands, where it is the thing being looked for.
	returnDetectFailures
)

// findDeliverySlot passes over the delivery-cart observe poses until one sees a
// slot, starting a new pass until searchFor has elapsed (a zero searchFor makes
// one pass), and returns the slot closest to the gripper there.
func (s *beanjaminCoffee) findDeliverySlot(ctx, cancelCtx context.Context, searchFor time.Duration, onDetectErr detectFailures) (deliverySlot, bool, error) {
	poseNames, err := s.observationPoseNames(ctx, s.deliveryCartObserveSw)
	if err != nil {
		return deliverySlot{}, false, fmt.Errorf("delivery cart: %w", err)
	}
	deadline := time.Now().Add(searchFor)
	for pass := 1; ; pass++ {
		slot, found, err := s.observeDeliverySlotPass(ctx, cancelCtx, poseNames, pass, onDetectErr)
		if err != nil || found {
			return slot, found, err
		}
		if !time.Now().Add(deliverySlotRetryPause).Before(deadline) {
			return deliverySlot{}, false, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return deliverySlot{}, false, ctx.Err()
		case <-time.After(deliverySlotRetryPause):
		}
	}
}

// observeDeliverySlotPass moves the camera through each observe pose and
// returns the slot closest to the gripper at the first pose that sees one. An
// unreachable observe pose is skipped, and so is one where detection fails
// unless onDetectErr says to return the failure.
func (s *beanjaminCoffee) observeDeliverySlotPass(
	ctx, cancelCtx context.Context, poseNames []string, pass int, onDetectErr detectFailures,
) (deliverySlot, bool, error) {
	logger := s.activeOrderLogger()
	for i, name := range poseNames {
		logger.Infof("delivery cart: pass %d, observe pose %d/%d %q", pass, i+1, len(poseNames), name)
		step := Step{PoseName: name, PoseSwitch: s.deliveryCartObserveSw, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			if errors.Is(err, errMotionPlanning) {
				logger.Warnf("delivery cart: observe pose %q unreachable, skipping: %v", name, err)
				continue
			}
			return deliverySlot{}, false, fmt.Errorf("delivery cart: %w", err)
		}
		// The camera must be standing at the pose before the frame is taken.
		if err := s.settle(); err != nil {
			return deliverySlot{}, false, err
		}

		slots, err := s.detectDeliverySlots(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return deliverySlot{}, false, ctx.Err()
			}
			if onDetectErr == returnDetectFailures {
				return deliverySlot{}, false, fmt.Errorf("observe pose %q: %w", name, err)
			}
			logger.Warnf("delivery cart: slot detection failed at %q: %v", name, err)
			continue
		}
		if len(slots) == 0 {
			logger.Infof("delivery cart: no slot seen from %q", name)
			continue
		}
		gripper, err := s.gripperWorldPoint(ctx)
		if err != nil {
			return deliverySlot{}, false, err
		}
		slot := closestDeliverySlot(slots, gripper)
		logger.Infof("delivery cart: %d slot(s) seen from %q; using tag %d at (x=%.1f, y=%.1f, z=%.1f)",
			len(slots), name, slot.tagID, slot.point.X, slot.point.Y, slot.point.Z)
		return slot, true, nil
	}
	return deliverySlot{}, false, ctx.Err()
}

// detectDeliverySlots runs the slots detector at the arm's current pose and
// returns every detected slot in world coordinates. Each 3D detection's root
// transform is the tag's pose in the frame the service observed it from (the
// camera), so it is lifted to world through the frame system.
func (s *beanjaminCoffee) detectDeliverySlots(ctx context.Context) ([]deliverySlot, error) {
	logger := s.activeOrderLogger()
	// An empty camera name makes the service use its own configured camera.
	detections, err := s.deliverySlotsVision.GetDetections3D(ctx, "", nil)
	if err != nil {
		return nil, fmt.Errorf("delivery cart: detect slots: %w", err)
	}
	if len(detections) == 0 {
		return nil, nil
	}
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return nil, err
	}
	linearInputs := fsInputs.ToLinearInputs()

	slots := make([]deliverySlot, 0, len(detections))
	snapshot := make([]detectionSnapshotItem, 0, len(detections))
	for i, det := range detections {
		if len(det.Transforms) == 0 {
			continue
		}
		root := det.Transforms[0]
		tf, err := fs.Transform(linearInputs, root.PoseInFrame, referenceframe.World)
		if err != nil {
			return nil, fmt.Errorf("delivery cart: lift detection %d from %q to world: %w", i, root.Parent(), err)
		}
		tagWorld := tf.(*referenceframe.PoseInFrame).Pose()
		tagID, ok := detectionTagID(det)
		if !ok {
			logger.Warnf("delivery cart: detection %q carries no numeric tag id; reporting slot -1", root.Name())
		}
		slots = append(slots, deliverySlot{point: deliverySlotPoint(tagWorld, s.deliverySlotTagOffsetMm()), tagID: tagID})
		if g := root.Geometry(); g != nil {
			snapshot = append(snapshot, detectionSnapshotItem{box: g.Transform(tagWorld)})
		}
	}
	// The boxes are already in world and there are no point clouds to anchor, so
	// the snapshot needs no camera pose.
	s.saveDetectionSnapshot(deliverySlotLabel, fs, fsInputs, spatialmath.NewZeroPose(), snapshot)
	return slots, nil
}

// detectionTagID is the AprilTag id a detection reports as its top
// classification label. -1 and false when there is none.
func detectionTagID(det *detection3d.Detection) (int, bool) {
	if len(det.Classifications) == 0 {
		return -1, false
	}
	id, err := strconv.Atoi(det.Classifications[0].Label())
	if err != nil {
		return -1, false
	}
	return id, true
}

// deliverySlotPoint is the world position of the hole a tag marks: offsetMm
// along the tag's own Y axis, so it follows the cart however it is docked.
func deliverySlotPoint(tagWorld spatialmath.Pose, offsetMm float64) r3.Vector {
	return spatialmath.Compose(tagWorld, spatialmath.NewPoseFromPoint(r3.Vector{Y: offsetMm})).Point()
}

func closestDeliverySlot(slots []deliverySlot, gripper r3.Vector) deliverySlot {
	best := slots[0]
	for _, slot := range slots[1:] {
		if slot.point.Sub(gripper).Norm() < best.point.Sub(gripper).Norm() {
			best = slot
		}
	}
	return best
}

// dropHeldInDeliverySlot carries the held container level to the approach pose
// over the slot, descends slowly in a straight line to the release pose, opens
// the gripper, and retreats straight back to the approach pose. Both poses are
// grip-point poses: delivery_slot_approach_relative_pose and
// delivery_slot_release_relative_pose composed onto the slot point, so their
// orientation is the gripper's.
//
// released reports whether the gripper opened over the slot. Before that, an
// error wrapping errMotionPlanning leaves the drink held, with the arm where it
// grabbed the drink or above the slot.
func (s *beanjaminCoffee) dropHeldInDeliverySlot(ctx context.Context, slot deliverySlot) (released bool, err error) {
	logger := s.activeOrderLogger()
	approach := &poseData{
		pose:          geom.PoseRelativeTo(slot.point, relativePoseToSpatial(s.cfg.DeliverySlotApproachRelativePose)),
		refFrame:      referenceframe.World,
		componentName: gripPoint,
	}
	release := &poseData{
		pose:          geom.PoseRelativeTo(slot.point, relativePoseToSpatial(s.cfg.DeliverySlotReleaseRelativePose)),
		refFrame:      referenceframe.World,
		componentName: gripPoint,
	}
	logger.Infof("delivery cart: slot %v approach_pose=%v release_pose=%v", slot.point, approach.pose, release.pose)

	// The carry commands the held-item frame and converts the grip-point goal
	// into it, so the drink stays level on the way over.
	if err := s.carryHeldLevel(ctx, approach, nil, s.slowMoveOptions()); err != nil {
		return false, fmt.Errorf("approach slot: %w", err)
	}
	if err := s.moveToRawPose(ctx, release, defaultApproachConstraint, nil, s.slowMoveOptions()); err != nil {
		return false, fmt.Errorf("descend into slot: %w", err)
	}

	// A failed open may still have let go, so it is not a fallback to the shelf.
	if err := s.openGripper(ctx); err != nil {
		return false, fmt.Errorf("open gripper to release drink: %v", err)
	}
	if err := s.settle(); err != nil {
		return false, fmt.Errorf("open gripper to release drink: %v", err)
	}
	s.detachHeldGeometry()

	// The gripper is empty: the arm's default speed.
	if err := s.moveToRawPose(ctx, approach, defaultApproachConstraint, nil, nil); err != nil {
		return true, fmt.Errorf("retreat from slot: %w", err)
	}
	if err := s.closeGripper(ctx); err != nil {
		return true, fmt.Errorf("close gripper after release: %w", err)
	}
	return true, nil
}
