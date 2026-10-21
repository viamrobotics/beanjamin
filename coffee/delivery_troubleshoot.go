package coffee

// Troubleshooting DoCommands for direct-to-cart placement, run by hand outside
// an order: detect_delivery_slot runs the slot search alone, and
// place_in_delivery_cart runs the search and then the placement with a cup
// modeled in the gripper. Neither waits for the cart nor touches the delivery
// ledger, and a detector error is returned rather than reported as no slot.

import (
	"context"
	"fmt"
)

func (s *beanjaminCoffee) requireDeliverySlots() error {
	if s.deliverySlotsVision == nil {
		return fmt.Errorf("delivery cart placement is not configured (set delivery_slots_vision_service_name and delivery_cart_observe_pose_switcher_name)")
	}
	return nil
}

// detectDeliverySlotCommand moves through the delivery-cart observe poses and
// reports the slot an order would place into. The arm stays at the observe pose
// that saw it.
func (s *beanjaminCoffee) detectDeliverySlotCommand(ctx context.Context) (map[string]any, error) {
	if err := s.requireDeliverySlots(); err != nil {
		return nil, err
	}
	cancelCtx, err := s.lease.claimManual("detect_delivery_slot")
	if err != nil {
		return nil, err
	}
	defer s.lease.release()
	if err := s.refreshFrameSystemIfClean(ctx); err != nil {
		return nil, fmt.Errorf("detect_delivery_slot: refresh frame system: %w", err)
	}

	slot, found, err := s.findDeliverySlot(ctx, cancelCtx, 0, returnDetectFailures)
	if err != nil {
		return nil, fmt.Errorf("detect_delivery_slot: %w", err)
	}
	return deliverySlotResponse(slot, found), nil
}

// placeInDeliveryCartCommand runs an order's cart placement with a stand-in for
// the drink: the configured cup is modeled in the gripper at its current pose
// (unless something is already modeled there), the observe poses find the
// slot, and the held cup is lowered in and released. Start with the gripper
// level, since the modeled cup is upright as of the call. A modeled cup is
// dropped again if the placement fails before the release.
func (s *beanjaminCoffee) placeInDeliveryCartCommand(ctx context.Context) (map[string]any, error) {
	if err := s.requireDeliverySlots(); err != nil {
		return nil, err
	}
	if s.gripper == nil {
		return nil, fmt.Errorf("place_in_delivery_cart: no gripper configured")
	}
	cancelCtx, err := s.lease.claimManual("place_in_delivery_cart")
	if err != nil {
		return nil, err
	}
	defer s.lease.release()
	if err := s.refreshFrameSystemIfClean(ctx); err != nil {
		return nil, fmt.Errorf("place_in_delivery_cart: refresh frame system: %w", err)
	}
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()

	if !s.heldItemAttached {
		if err := s.attachConfiguredCupGeometry(ctx); err != nil {
			return nil, fmt.Errorf("place_in_delivery_cart: model held cup: %w", err)
		}
	}
	released := false
	defer func() {
		if !released {
			s.detachHeldGeometry()
		}
	}()

	slot, found, err := s.findDeliverySlot(ctx, cancelCtx, 0, returnDetectFailures)
	if err != nil {
		return nil, fmt.Errorf("place_in_delivery_cart: %w", err)
	}
	if !found {
		return deliverySlotResponse(slot, false), nil
	}
	released, err = s.dropHeldInDeliverySlot(ctx, slot)
	if err != nil {
		return nil, fmt.Errorf("place_in_delivery_cart (released=%v): %w", released, err)
	}
	resp := deliverySlotResponse(slot, true)
	resp["placed"] = true
	return resp, nil
}

func deliverySlotResponse(slot deliverySlot, found bool) map[string]any {
	if !found {
		return map[string]any{"found": false}
	}
	return map[string]any{
		"found":  true,
		"tag_id": slot.tagID,
		"x":      slot.point.X,
		"y":      slot.point.Y,
		"z":      slot.point.Z,
	}
}
