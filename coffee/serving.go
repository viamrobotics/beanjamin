package coffee

// Cup logistics around the brew: setting an empty cup under the machine and
// placing finished drinks onto the served-drinks shelf (round-robin slot
// selection with planning-failure fallthrough).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
)

// servingAreaShieldCollisions returns the allowed-collision pairs that let the
// gripper bodies, claws, and (while an item is held) the held container pass
// through the serving-area-shield obstacle. The shield stays a hard obstacle on
// the lateral carry so the arm avoids cups already standing on the shelf; these
// pairs are applied only on the linearly constrained descent into a slot and the
// retreat back out, which move straight down/up into the target slot.
//
// The gripper sub-frames (gripper:claws, gripper:case-gripper) only exist on the
// real gripper; filterFakeModeCollisions (applied in moveToRawPose) drops them
// under FakeMode. The held-item pair is gated by heldItemSurfaceCollisions so it
// is omitted once the container has been released (on the retreat).
func (s *beanjaminCoffee) servingAreaShieldCollisions() []AllowedCollision {
	out := []AllowedCollision{
		{Frame1: componentClaws, Frame2: servingAreaShieldFrameName},
		{Frame1: "gripper:claws", Frame2: servingAreaShieldFrameName},
		{Frame1: "gripper:case-gripper", Frame2: servingAreaShieldFrameName},
	}
	return append(out, s.heldItemSurfaceCollisions([]AllowedCollision{
		{Frame1: heldItemFrameName, Frame2: servingAreaShieldFrameName},
	})...)
}

func (s *beanjaminCoffee) setCupForCoffee(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("set_cup_for_coffee: no gripper configured")
	}

	if err := s.pickCupDynamic(ctx, cancelCtx); err != nil {
		return err
	}

	cupPlacementApproach := Step{PoseName: clawPoseCupUnderMachineApproach, PoseSwitch: s.clawsSw, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, cupPlacementApproach); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}
	readyStep := Step{PoseName: clawPoseCupReadyForCoffee, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause, AllowedCollisions: s.heldItemSurfaceCollisions(heldItemMachineCollisions)}
	if err := s.executeStep(ctx, cancelCtx, readyStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Release the cup.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: open gripper: %w", err)
	}
	// Give time for the gripper to open
	time.Sleep(gripperPause)
	// Cup is released under the machine; it no longer travels with the gripper.
	s.detachHeldGeometry()

	// Move away from the cup.
	exitStep := Step{PoseName: clawPoseCupUnderMachineApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, exitStep); err != nil {
		return fmt.Errorf("set_cup_for_coffee: %w", err)
	}

	// Close the gripper after moving away.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("set_cup_for_coffee: close gripper: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

// placeFullCupOnShelf retrieves the brewed cup from cup_ready_for_coffee and
// drops it on the serving-area shelf at the next round-robin slot.
func (s *beanjaminCoffee) placeFullCupOnShelf(ctx, cancelCtx context.Context) error {
	if err := s.grabBrewedCupFromMachine(ctx, cancelCtx); err != nil {
		return err
	}
	return s.placeHeldInServingArea(ctx, cancelCtx)
}

// placeHeldInServingArea drops the item currently held by the gripper into the
// serving area: it walks the serving-area slots in round-robin order starting
// from servingAreaSlotCounter and drops the item in the first slot it can reach
// (tryDropCupInSlot), skipping any whose approach or descent cannot be planned.
// On success the counter advances to the slot after the one used, so the next
// placement starts there. The caller must already be holding the item; shared
// by placeFullCupOnShelf (cups) and serveIcedCoffee (the empty espresso cup and
// the iced glass).
func (s *beanjaminCoffee) placeHeldInServingArea(ctx, cancelCtx context.Context) error {
	logger := s.activeOrderLogger()
	if s.gripper == nil {
		return fmt.Errorf("place_in_serving_area: no gripper configured")
	}

	slots, shelfTopZ, err := s.servingAreaSlots(ctx)
	if err != nil {
		return fmt.Errorf("place_in_serving_area: %w", err)
	}

	n := len(slots)
	start := s.servingAreaSlotCounter.Load()
	var lastErr error
	for off := 0; off < n; off++ {
		idx := slotIndex(start+uint64(off), n)
		logger.Infof("place_in_serving_area: trying slot %d/%d", idx+1, n)
		err := s.tryDropCupInSlot(ctx, slots[idx], shelfTopZ)
		if err == nil {
			// Next placement starts at the slot after the one just used.
			s.servingAreaSlotCounter.Store(start + uint64(off) + 1)
			s.lastServedSlot.Store(int64(idx) + 1)
			logger.Infof("place_in_serving_area: placed item in slot %d/%d", idx+1, n)
			return nil
		}
		lastErr = err

		// Operator cancel always wins.
		if ctx.Err() != nil || cancelCtx.Err() != nil {
			return fmt.Errorf("place_in_serving_area: cancelled: %w", err)
		}

		// Only planning failures (item still held, arm unmoved) are skippable.
		// Anything else — execution error, or any failure after the item was
		// released — bubbles up.
		if !errors.Is(err, errMotionPlanning) {
			return fmt.Errorf("place_in_serving_area: %w", err)
		}
		logger.Warnf("place_in_serving_area: slot %d/%d unreachable — trying next slot: %v", idx+1, n, err)
	}
	return fmt.Errorf("place_in_serving_area: all %d serving-area slot(s) unreachable; last error: %w", n, lastErr)
}

// tryDropCupInSlot drops the held cup at one serving-area slot: free-plan to the
// approach pose above the slot, descend linearly to the drop pose (placement
// anchor = shelfTopZ + servingAreaDropZOffset, i.e. the held container's
// half-height, so its bottom rests on the shelf regardless of its height),
// release, then retreat linearly and close the gripper.
//
// ServingGrabRelativePose is the claws-to-container offset used at release
// (composed onto the placement anchor here) — shared by the hot cup and the
// iced glass so either lands centered on the slot.
//
// Returned errors split like tryGrab so placeHeldInServingArea can react via
// errors.Is:
//   - wraps errMotionPlanning → the approach or descent could not be planned;
//     the cup is still held and the arm has not committed to the slot, so the
//     caller can try the next slot.
//   - anything else → an execution error, or any failure after the cup was
//     released; bubble up (do not try another slot with an empty gripper).
func (s *beanjaminCoffee) tryDropCupInSlot(ctx context.Context, tileWorld r3.Vector, shelfTopZ float64) error {
	logger := s.activeOrderLogger()
	dropAnchor := r3.Vector{
		X: tileWorld.X,
		Y: tileWorld.Y,
		Z: shelfTopZ + s.servingAreaDropZOffset(),
	}
	dropPose := composeCupPose(dropAnchor, relativePoseToSpatial(s.cfg.ServingGrabRelativePose))
	approachPose := composeCupPose(dropAnchor, relativePoseToSpatial(s.cfg.ServingApproachRelativePose))

	approachPD := &poseData{pose: approachPose, refFrame: referenceframe.World, componentName: componentClaws}
	dropPD := &poseData{pose: dropPose, refFrame: referenceframe.World, componentName: componentClaws}
	logger.Infof("shelf placement: slot (x=%.1f, y=%.1f) drop_pose=%v approach_pose=%v",
		tileWorld.X, tileWorld.Y, dropPose, approachPose)

	// 1. Carry the held cup to the approach pose above the slot. With
	// no_spill_carry set, step through level-pinned waypoints (carryHeldLevel)
	// so the drink doesn't slosh on the long traverse; otherwise free-plan
	// straight there. Both wrap planning failures in errMotionPlanning, so on
	// failure the arm has not moved and the cup is still held — the caller can
	// try the next slot.
	carry := func() error { return s.moveToRawPose(ctx, approachPD, nil, nil, nil) }
	if s.cfg.NoSpillCarry {
		carry = func() error { return s.carryHeldLevel(ctx, approachPD) }
	}
	if err := carry(); err != nil {
		return fmt.Errorf("approach slot (x=%.1f, y=%.1f): %w", tileWorld.X, tileWorld.Y, err)
	}

	// The cup is held during the approach and descent, so allow its geometry to
	// approach the shelf surface (no-op when tracking is off / nothing attached).
	// The shield pairs additionally let the gripper/claws/held cup descend
	// straight through the serving-area-shield into the target slot — the shield
	// stays a hard obstacle on the lateral carry above so the arm avoids cups
	// already on the shelf. Build a fresh slice so neither package-level
	// allow-list is aliased by append.
	descentCollisions := append([]AllowedCollision{}, s.heldItemSurfaceCollisions(heldItemServingAreaCollisions)...)
	descentCollisions = append(descentCollisions, s.servingAreaShieldCollisions()...)

	// 2. Linear descent to the drop pose. A planning failure leaves the arm at
	// the approach pose still holding the cup — caller can try the next slot.
	if err := s.moveToRawPose(ctx, dropPD, defaultApproachConstraint, descentCollisions, nil); err != nil {
		return fmt.Errorf("descend into slot (x=%.1f, y=%.1f): %w", tileWorld.X, tileWorld.Y, err)
	}

	// 3. Release the cup. Past this point the cup is committed to this slot;
	// the steps below strip any errMotionPlanning chain (%v) so the caller does
	// not retry another slot with an empty gripper.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("open gripper to release cup: %w", err)
	}
	time.Sleep(gripperPause)
	// Cup is released onto the shelf; it no longer travels with the gripper.
	s.detachHeldGeometry()

	// 4. Linear retreat back to the approach pose. The cup is released, but the
	// gripper/claws start inside the serving-area-shield, so the shield must stay
	// allowed for the straight-up retreat to plan out of the slot (the held-item
	// pair drops out now that nothing is attached).
	if err := s.moveToRawPose(ctx, approachPD, defaultApproachConstraint, s.servingAreaShieldCollisions(), nil); err != nil {
		return fmt.Errorf("retreat after releasing cup (slot x=%.1f, y=%.1f): %v", tileWorld.X, tileWorld.Y, err)
	}

	// 5. Close the gripper for the next move.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("close gripper after release: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}
