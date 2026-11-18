package coffee

// Cup logistics around the brew: setting an empty cup under the machine and
// placing finished drinks onto the served-drinks shelf (round-robin slot
// selection with planning-failure fallthrough).

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"

	"beanjamin/coffee/geom"
)

func (s *beanjaminCoffee) setCupForCoffee(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("set_cup_for_coffee: no gripper configured")
	}

	if err := s.pickCupDynamic(ctx, cancelCtx); err != nil {
		return err
	}

	cupPlacementApproach := Step{PoseName: clawPoseCupUnderMachineApproach, PoseSwitch: s.clawsSw}
	if err := s.executeStep(ctx, cancelCtx, cupPlacementApproach); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	// The cup is empty, so the arm's default speed.
	readyStep := Step{PoseName: clawPoseCupReadyForCoffee, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause, AllowedCollisions: s.scene().heldItemSurfaceCollisions(heldItemMachineCollisions)}
	if err := s.executeStep(ctx, cancelCtx, readyStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Release the cup.
	if err := s.openGripper(ctx); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	// Cup is released under the machine; it no longer travels with the gripper.
	s.detachHeldGeometry()

	// Move away from the cup. The gripper is empty, so the arm's default speed.
	exitStep := Step{PoseName: clawPoseCupUnderMachineApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint}
	if err := s.executeStep(ctx, cancelCtx, exitStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Close the gripper after moving away.
	if err := s.closeGripper(ctx); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	return nil
}

// placeFullCupOnShelf retrieves the brewed cup from cup_ready_for_coffee and
// drops it on the serving-area shelf at the next round-robin slot, returning
// the 0-based slot it was placed in.
func (s *beanjaminCoffee) placeFullCupOnShelf(ctx, cancelCtx context.Context) (int, error) {
	return s.placeHeldInServingArea(ctx, cancelCtx, s.grabBrewedCupFromMachine, heldFilled)
}

// grabAndShelve grabs the filled drink and drops it in the next reachable
// serving-area slot.
func (s *beanjaminCoffee) grabAndShelve(ctx, cancelCtx context.Context, grab grabDrink) (int, error) {
	return s.placeHeldInServingArea(ctx, cancelCtx, grab, heldFilled)
}

// heldContents says whether the container being placed still holds liquid. It
// decides only how the traverse to the shelf is planned — a filled container
// routes through the no-spill level carry, an empty one free-plans straight to
// the slot. Held-item tracking, collisions and the descent are identical either
// way.
type heldContents int

const (
	heldFilled heldContents = iota
	heldEmpty
)

// placeHeldInServingArea drops the item currently held by the gripper into the
// serving area (placeHeldInArea). Shared by placeFullCupOnShelf (cups) and
// serveIcedCoffee (the iced glass).
func (s *beanjaminCoffee) placeHeldInServingArea(ctx, cancelCtx context.Context, grab grabDrink, contents heldContents) (int, error) {
	return s.placeHeldInArea(ctx, cancelCtx, s.servingArea(), grab, contents)
}

// placeHeldInArea drops the item currently held by the gripper into area: it
// walks the area's slots in round-robin order starting from its counter and
// drops the item in the first slot it can reach (tryDropCupInSlot), skipping
// any whose approach or descent cannot be planned. On success it returns the
// 0-based slot the item was placed in and advances the counter to the slot
// after it, so the next placement starts there.
//
// grab, when set, takes hold of the item first; nil means it is already held.
// Each slot is tried as one chunk with the grab, so the grab is planned to
// leave the arm room for the drop, and a slot that cannot be planned runs
// nothing — the next slot's chunk records the grab again. Once any of the grab
// has been handed off, it is not repeated.
func (s *beanjaminCoffee) placeHeldInArea(ctx, cancelCtx context.Context, area placementArea, grab grabDrink, contents heldContents) (int, error) {
	logger := s.activeOrderLogger()
	if s.gripper == nil {
		return -1, fmt.Errorf("place_in_%s: no gripper configured", area.frame)
	}

	slots, shelfTopZ, err := s.areaSlots(area)
	if err != nil {
		return -1, fmt.Errorf("place_in_%s: %w", area.frame, err)
	}

	n := len(slots)
	start := area.slots.get()
	var lastErr error
	for off := 0; off < n; off++ {
		idx := geom.SlotIndex(start+uint64(off), n)
		logger.Infof("place_in_%s: trying slot %d/%d", area.frame, idx+1, n)
		err := s.inChunk(fmt.Sprintf("place in slot %d", idx+1), func() error {
			if grab != nil {
				grabbed, err := s.handsOff(func() error { return grab(ctx, cancelCtx) })
				if grabbed {
					grab = nil
				}
				if err != nil {
					return err
				}
			}
			return s.tryDropCupInSlot(ctx, cancelCtx, area, slots[idx], shelfTopZ, contents)
		})
		if err == nil {
			// Next placement starts at the slot after the one just used.
			area.slots.set(logger, start+uint64(off)+1)
			logger.Infof("place_in_%s: placed item in slot %d/%d", area.frame, idx+1, n)
			return idx, nil
		}
		lastErr = err

		// Operator cancel always wins.
		if ctx.Err() != nil || cancelCtx.Err() != nil {
			return -1, fmt.Errorf("place_in_%s: cancelled: %w", area.frame, err)
		}

		// Only planning failures (item still held, arm unmoved) are skippable.
		// Anything else — execution error, or any failure after the item was
		// released — bubbles up.
		if !errors.Is(err, errMotionPlanning) {
			return -1, fmt.Errorf("place_in_%s: %w", area.frame, err)
		}
		logger.Warnf("place_in_%s: slot %d/%d unreachable — trying next slot: %v", area.frame, idx+1, n, err)
	}
	return -1, fmt.Errorf("place_in_%s: all %d slot(s) unreachable; last error: %w", area.frame, n, lastErr)
}

// tryDropCupInSlot drops the held cup at one slot of area: free-plan to the
// approach pose above the slot, descend linearly to the drop pose (placement
// anchor = shelfTopZ + servingAreaDropZOffset, i.e. the held container's
// half-height, so its bottom rests on the shelf regardless of its height),
// release, then retreat linearly and close the gripper.
//
// ServingGrabRelativePose is the grip-point-to-container offset used at release
// (composed onto the placement anchor here) — shared by the hot cup and the
// iced glass so either lands centered on the slot.
//
// Returned errors split like tryGrab so placeHeldInArea can react via
// errors.Is:
//   - wraps errMotionPlanning → the approach or descent could not be planned;
//     the cup is still held and the arm has not committed to the slot, so the
//     caller can try the next slot.
//   - anything else → an execution error, or any failure after the cup was
//     released; bubble up (do not try another slot with an empty gripper).
//
// The raw-pose moves, the level carry and the gripper calls here do not see
// cancelCtx on their own, so it is merged into ctx up front: an operator cancel
// landing during planning, gripper actuation or a gripperPause stops the rest of
// the descent, release and retreat instead of only the trajectory in flight.
func (s *beanjaminCoffee) tryDropCupInSlot(ctx, cancelCtx context.Context, area placementArea, tileWorld r3.Vector, shelfTopZ float64,
	contents heldContents,
) error {
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	logger := s.activeOrderLogger()
	dropAnchor := r3.Vector{
		X: tileWorld.X,
		Y: tileWorld.Y,
		Z: shelfTopZ + s.servingAreaDropZOffset(),
	}
	dropPose := geom.PoseRelativeTo(dropAnchor, relativePoseToSpatial(s.cfg.ServingGrabRelativePose))
	approachPose := geom.PoseRelativeTo(dropAnchor, relativePoseToSpatial(s.cfg.ServingApproachRelativePose))

	approachPD := &poseData{pose: approachPose, refFrame: referenceframe.World, componentName: gripPoint}
	dropPD := &poseData{pose: dropPose, refFrame: referenceframe.World, componentName: gripPoint}
	logger.Infof("shelf placement: slot (x=%.1f, y=%.1f) drop_pose=%v approach_pose=%v",
		tileWorld.X, tileWorld.Y, dropPose, approachPose)

	// 1. Carry the held cup to the approach pose above the slot. A *filled*
	// container is carried with its orientation held near level (carryHeldLevel)
	// so the drink doesn't slosh on the long traverse; an empty one has nothing to
	// spill and plans unconstrained, which is easier to plan. Both
	// wrap planning failures in errMotionPlanning, so on failure the arm has not
	// moved and the cup is still held — the caller can try the next slot.
	carry := func() error { return s.moveToRawPose(ctx, approachPD, nil, nil, nil) }
	if contents == heldFilled {
		carry = func() error { return s.carryHeldLevel(ctx, approachPD, nil, s.spillagePreventionMoveOptions()) }
	}
	if err := carry(); err != nil {
		return fmt.Errorf("approach slot (x=%.1f, y=%.1f): %w", tileWorld.X, tileWorld.Y, err)
	}

	// The cup is held during the approach and descent, so allow its geometry to
	// approach the shelf surface (no-op when tracking is off / nothing attached).
	// The shield pairs additionally let the gripper/claws/held cup descend
	// straight through the area's shield into the target slot — the shield
	// stays a hard obstacle on the lateral carry above so the arm avoids cups
	// already standing there. Build a fresh slice so neither package-level
	// allow-list is aliased by append.
	descentCollisions := append([]AllowedCollision{}, s.scene().heldItemSurfaceCollisions(area.surfaceCollisions)...)
	descentCollisions = append(descentCollisions, s.areaShieldCollisions(area.shieldFrame)...)

	// 2. Linear descent to the drop pose. A planning failure leaves the arm at
	// the approach pose still holding the cup — caller can try the next slot.
	// The descent is a contact move either way; a filled container also needs
	// the spillage-prevention easing.
	descentSpeed := s.slowMoveOptions()
	if contents == heldFilled {
		descentSpeed = s.spillagePreventionMoveOptions()
	}
	if err := s.moveToRawPose(ctx, dropPD, defaultApproachConstraint, descentCollisions, descentSpeed); err != nil {
		return fmt.Errorf("descend into slot (x=%.1f, y=%.1f): %w", tileWorld.X, tileWorld.Y, err)
	}

	// 3. Release the cup. Past this point the cup is committed to this slot;
	// the steps below strip any errMotionPlanning chain (%v) so the caller does
	// not retry another slot with an empty gripper.
	if err := s.openGripper(ctx); err != nil {
		return fmt.Errorf("open gripper to release cup: %w", err)
	}
	// Cup is released onto the shelf; it no longer travels with the gripper.
	s.detachHeldGeometry()

	// 4. Linear retreat back to the approach pose. The cup is released, but the
	// gripper/claws start inside the area's shield, so the shield must stay
	// allowed for the straight-up retreat to plan out of the slot (the held-item
	// pair drops out now that nothing is attached). Nothing is held, so the arm's
	// default speed.
	if err := s.moveToRawPose(ctx, approachPD, defaultApproachConstraint, s.areaShieldCollisions(area.shieldFrame), nil); err != nil {
		return fmt.Errorf("retreat after releasing cup (slot x=%.1f, y=%.1f): %v", tileWorld.X, tileWorld.Y, err)
	}

	// 5. Close the gripper for the next move.
	if err := s.closeGripper(ctx); err != nil {
		return fmt.Errorf("close gripper after release: %w", err)
	}
	return nil
}
