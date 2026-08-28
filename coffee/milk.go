package coffee

// The iced-latte milk path: open the fridge, vision-detect the milk bottle
// inside, pour it into the staged glass, put the bottle back where it was
// found, and shut the fridge. Gated by can_serve_iced_latte.
//
// Nothing here is new machinery — it is the three existing patterns composed:
// the cup/glass vision pickup (cup_pickup.go) with its own vision service,
// observe switch and grasp offsets; the fixed-point pour pivot the espresso
// pour uses (iced.go); and the hinge-arc door sweep (door.go).
//
// The one thing neither the cup nor the glass needs is a way back. The bottle
// belongs in the fridge, and where in the fridge it stands changes every time
// somebody puts the milk away — so the return is not an authored pose but the
// grasp itself, replayed: pickup records the world centroid it grabbed at, and
// the return composes the same approach/grab offsets onto that centroid.

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// milkAreaShieldFrameName is the optional obstacle enclosing the fridge
// interior, the fridge-side counterpart of the clean-cup/glass area shields: a
// hard obstacle while the arm flies to and from the fridge (so it doesn't clip
// the shelves or the door on a free traverse), opened up only on the straight-in
// grasp descent and the retreat back out. Inert when the frame system doesn't
// define it, like every other shield.
const milkAreaShieldFrameName = "fridge"

// milkPickupTarget describes dynamic milk-bottle pickup: its own vision service
// and observe switch (whose vantages look into the open fridge), and grasp
// offsets tuned for the bottle. The photos-per-vantage and max-attempts knobs
// are shared with cup pickup (cup_photos_per_vantage / cup_pickup_max_attempts)
// — they are item-agnostic operational settings.
func (s *beanjaminCoffee) milkPickupTarget() *pickupTarget {
	return &pickupTarget{
		label:            pickupLabelMilk,
		vision:           s.milkVision,
		cameraName:       s.cupCameraName,
		observeSw:        s.milkObserveSw,
		observeHomePose:  milkPoseObserve,
		approachRel:      s.cfg.MilkApproachRelativePose,
		grabRel:          s.cfg.MilkGrabRelativePose,
		photosPerVantage: pickupPhotosPerVantage(s.cfg.CupPhotosPerVantage),
		maxAttempts:      pickupMaxAttempts(s.cfg.CupPickupMaxAttempts),
		dims:             s.cfg.MilkBottleDimensions,
		noItemSpeak:      "I can't see the milk in the fridge — could you put the bottle back on its shelf? Trying again in 15 seconds.",
		unreachableSpeak: "I can see the milk but I'm having trouble grabbing it — could you nudge the bottle a little? Trying again in 15 seconds.",
		// Like the tall glass: the bottle's point-cloud centroid Z lands high on
		// the neck, while the box center is a stable mid-body grasp point.
		graspZFromGeom: true,
		shieldFrame:    milkAreaShieldFrameName,
	}
}

// requireMilk rejects the milk actions on a machine that isn't set up for them.
// Without it the milk config — the vision service, the observe switch, the grasp
// offsets — is nil, and the failure would surface as a nil dereference or an
// unhelpful pose error partway into a motion.
func (s *beanjaminCoffee) requireMilk(action string) error {
	if !s.cfg.CanServeIcedLatte {
		return fmt.Errorf("%s: milk is not configured on this machine (set can_serve_iced_latte)", action)
	}
	return nil
}

// addMilk is the whole fridge trip, run on a staged glass with an empty
// gripper: open the door, fetch the bottle, pour, put the bottle back, shut the
// door. Registered as the add_milk execute_action and spliced into serveIced
// for an iced_latte.
//
// The door is opened once and closed once, so the fridge stands open for the
// pour. Closing it in between would double the sweeps — the slowest and most
// failure-prone part of the sequence — to keep the milk cold for the ~15s the
// pour takes.
func (s *beanjaminCoffee) addMilk(ctx, cancelCtx context.Context) error {
	if err := s.requireMilk("add_milk"); err != nil {
		return err
	}
	if err := s.openDoor(ctx, cancelCtx); err != nil {
		return s.milkStepErr(err)
	}
	s.setStep(stepAddingMilk)
	if err := s.fetchMilkBottle(ctx, cancelCtx); err != nil {
		return s.milkStepErr(err)
	}
	if err := s.pourMilk(ctx, cancelCtx); err != nil {
		return s.milkStepErr(err)
	}
	if err := s.returnMilkBottle(ctx, cancelCtx); err != nil {
		return s.milkStepErr(err)
	}
	if err := s.closeDoor(ctx, cancelCtx); err != nil {
		return s.milkStepErr(err)
	}
	return nil
}

// milkStepErr wraps a failure inside the milk sequence, saying so when the
// fridge is left standing open. The arm does not try to shut the door itself
// after a failure: it may still be holding the bottle, and a sweep needs the
// gripper for the handle. So the door stays open, the model keeps recording the
// angle it really is at, and the message tells the operator what to fix.
func (s *beanjaminCoffee) milkStepErr(err error) error {
	if s.doorOpenDegs != 0 {
		return fmt.Errorf("add_milk: %w (the fridge door is standing open at %.0f° — take the bottle out of the gripper if it's holding one, shut the door by hand, then rewind)", err, s.doorOpenDegs)
	}
	return fmt.Errorf("add_milk: %w", err)
}

// fetchMilkBottle vision-detects the milk bottle inside the fridge and grabs it,
// leaving it held by the gripper. The door must already be open — the observe
// vantages look through the opening, and a shut door is an obstacle between the
// camera and the milk.
//
// The centroid it was grasped at is recorded for returnMilkBottle. Recording it
// only on success means a failed pickup can't leave a stale position behind for
// a later return to drive at.
func (s *beanjaminCoffee) fetchMilkBottle(ctx, cancelCtx context.Context) error {
	if err := s.requireMilk("fetch_milk"); err != nil {
		return err
	}
	centroid, err := s.pickDynamic(ctx, cancelCtx, s.milkPickupTarget())
	if err != nil {
		return fmt.Errorf("fetch_milk: %w", err)
	}
	s.milkGraspCentroid = &centroid
	s.activeOrderLogger().Infof("fetch_milk: bottle in hand, pickup position recorded at (x=%.1f, y=%.1f, z=%.1f)",
		centroid.X, centroid.Y, centroid.Z)
	return nil
}

// pourMilk carries the held bottle over the staged glass and tilts it to pour,
// dwells for milkPourDwell so the glass fills, then returns it upright before
// moving away. Same fixed-point pivot as pourEspresso: the claws rotate the
// bottle in place so the stream stays over the glass, and the staged glass
// remains a hard obstacle throughout — the bottle must clear it, never drive
// into it. How much milk the latte gets is the dwell, not the tilt.
func (s *beanjaminCoffee) pourMilk(ctx, cancelCtx context.Context) error {
	if err := s.requireMilk("pour_milk"); err != nil {
		return err
	}
	// The bottle is full here — carry it level to the pour position so it doesn't
	// slosh before the tilt (NoSpill honored only with no_spill_carry).
	approachStep := Step{PoseName: clawPoseMilkPourApproach, PoseSwitch: s.clawsSw, Pause: shortPause, NoSpill: true}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("pour_milk: %w", err)
	}
	pourStep := Step{PoseName: clawPoseMilkPour, PoseSwitch: s.clawsSw, PivotFromPose: clawPoseMilkPourApproach, PivotDegreesPerStep: 5,
		MoveOptions: s.pourMoveOptions(), Pause: s.milkPourDwell()}
	if err := s.executeStep(ctx, cancelCtx, pourStep); err != nil {
		return fmt.Errorf("pour_milk: %w", err)
	}
	// Return upright along the same pivot so any residual drip stays over the glass.
	uprightStep := Step{PoseName: clawPoseMilkPourApproach, PoseSwitch: s.clawsSw, PivotFromPose: clawPoseMilkPour, PivotDegreesPerStep: 5,
		MoveOptions: s.pourMoveOptions(), Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, uprightStep); err != nil {
		return fmt.Errorf("pour_milk: %w", err)
	}
	s.incrementSensorReading(ctx, s.usageSensor, "milk", "milk_pours", 1)
	return nil
}

// milkReturnPoses returns the standoff and set-down grip-point poses for putting
// the bottle back at centroid. They are the pickup's own offsets composed onto
// the recorded grasp centroid, so the bottle is set down exactly where the grab
// lifted it from and the descent retraces the retreat.
func (s *beanjaminCoffee) milkReturnPoses(centroid r3.Vector) (approach, place spatialmath.Pose) {
	return composeCupPose(centroid, relativePoseToSpatial(s.cfg.MilkApproachRelativePose)),
		composeCupPose(centroid, relativePoseToSpatial(s.cfg.MilkGrabRelativePose))
}

// returnMilkBottle puts the bottle back on the fridge shelf at the centroid
// fetchMilkBottle recorded: carry to the standoff, linear descent onto the
// shelf, release, linear retreat, close the jaws. The reverse of the pickup's
// approach-grab-retreat, against the same offsets.
//
// The recorded position is cleared on release, so a second return with no
// intervening pickup fails loudly instead of driving an empty gripper at a spot
// where the bottle already stands.
func (s *beanjaminCoffee) returnMilkBottle(ctx, cancelCtx context.Context) error {
	if err := s.requireMilk("return_milk"); err != nil {
		return err
	}
	if s.gripper == nil {
		return fmt.Errorf("return_milk: no gripper configured")
	}
	if s.milkGraspCentroid == nil {
		return fmt.Errorf("return_milk: no recorded pickup position — run fetch_milk first")
	}

	// Merge cancelCtx into ctx so operator cancel interrupts the raw moves and
	// the gripper calls (the same merge pickDynamic does — these are raw-pose
	// moves, not executeStep, so nothing else checks cancelCtx for us).
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(cancelCtx, func() { cancel() })
	defer stop()
	defer cancel()

	centroid := *s.milkGraspCentroid
	approachPose, placePose := s.milkReturnPoses(centroid)
	approachPD := &poseData{pose: approachPose, refFrame: referenceframe.World, componentName: gripPoint}
	placePD := &poseData{pose: placePose, refFrame: referenceframe.World, componentName: gripPoint}
	s.activeOrderLogger().Infof("return_milk: putting the bottle back at (x=%.1f, y=%.1f, z=%.1f)",
		centroid.X, centroid.Y, centroid.Z)

	// The bottle still holds milk, so carry it level back to the fridge under
	// no_spill_carry — the same choice tryDropCupInSlot makes for a full cup.
	carry := func() error { return s.moveToRawPose(ctx, approachPD, nil, nil, nil) }
	if s.cfg.NoSpillCarry {
		carry = func() error { return s.carryHeldLevel(ctx, approachPD, nil, nil) }
	}
	if err := carry(); err != nil {
		return fmt.Errorf("return_milk: approach the shelf: %w", err)
	}

	// The bottle is held through the descent, so let its geometry approach the
	// fridge surfaces it legitimately gets close to on the way down, and let the
	// gripper and bottle pass through the interior shield that keeps the free
	// traverse clear of the shelves.
	descentCollisions := append([]AllowedCollision{}, s.heldItemSurfaceCollisions(heldItemFridgeCollisions)...)
	descentCollisions = append(descentCollisions, s.pickupAreaShieldCollisions(milkAreaShieldFrameName)...)
	if err := s.moveToRawPose(ctx, placePD, defaultApproachConstraint, descentCollisions, nil); err != nil {
		return fmt.Errorf("return_milk: descend onto the shelf: %w", err)
	}

	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("return_milk: open gripper: %w", err)
	}
	time.Sleep(gripperPause)
	// The bottle is standing on the shelf; it no longer travels with the gripper.
	s.detachHeldGeometry()
	s.milkGraspCentroid = nil

	// The gripper starts inside the interior shield, so it stays allowed for the
	// straight-up retreat (the held-item pair drops out now that nothing is held).
	if err := s.moveToRawPose(ctx, approachPD, defaultApproachConstraint, s.pickupAreaShieldCollisions(milkAreaShieldFrameName), nil); err != nil {
		return fmt.Errorf("return_milk: retreat after releasing the bottle: %v", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("return_milk: close gripper after release: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}
