package coffee

// The iced-latte milk path: open the fridge, take the milk's bottle off its
// spot, pour it into the staged glass, put the bottle back, and shut the
// fridge. Gated by can_serve_iced_latte.
//
// Each milk in milk_options stands at a fixed spot in the fridge, and each spot
// is taught as two claws-switch poses: milk_<spot>_approach in front of the
// bottle and milk_<spot>_grab with the jaws around it. The grab is a straight
// line between them, and the return is the same line run backwards — so the
// bottle has to be put back at its spot. The pour is the pour pivot the
// espresso pour uses (iced.go), and the door is the hinge-arc sweep (door.go).

import (
	"context"
	"fmt"
	"math"
	"time"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"

	"beanjamin/coffee/geom"
)

// milkAreaShieldFrameName is the optional obstacle enclosing the fridge
// interior, the fridge-side counterpart of the clean-cup/glass area shields: a
// hard obstacle while the arm flies to and from the fridge (so it doesn't clip
// the shelves or the door on a free traverse), opened up only on the
// straight-line moves between a spot's approach and grab poses. Inert when the
// frame system doesn't define it, like every other shield.
const milkAreaShieldFrameName = "fridge"

// requireMilk rejects the milk actions on a machine that isn't set up for them.
// Without it the milk config — the milk options, the bottle dimensions — is
// empty, and the failure would surface as an unhelpful pose error partway into
// a motion.
func (s *beanjaminCoffee) requireMilk(action string) error {
	if !s.cfg.CanServeIcedLatte {
		return fmt.Errorf("%s: milk is not configured on this machine (set can_serve_iced_latte)", action)
	}
	return nil
}

// firstMilk is the milk every iced latte and every hand-run milk action uses for
// now: the first in milk_options, which Validate requires on a latte machine.
func (s *beanjaminCoffee) firstMilk() string {
	if len(s.cfg.MilkOptions) == 0 {
		return ""
	}
	return s.cfg.MilkOptions[0].Name
}

// milkSpot returns the fridge spot the named milk stands at.
func (s *beanjaminCoffee) milkSpot(milk string) (string, error) {
	for _, m := range s.cfg.MilkOptions {
		if m.Name == milk {
			return m.Spot, nil
		}
	}
	return "", fmt.Errorf("unknown milk %q", milk)
}

// addMilk is the whole fridge trip for the named milk, run on a staged glass
// with an empty gripper: open the door, fetch the bottle, pour, put the bottle
// back, shut the door. Registered as the add_milk execute_action and spliced
// into serveIced for an iced_latte.
//
// The door is opened once and closed once, so the fridge stands open for the
// pour. Closing it in between would double the sweeps — the slowest and most
// failure-prone part of the sequence — to keep the milk cold for the ~15s the
// pour takes.
func (s *beanjaminCoffee) addMilk(ctx, cancelCtx context.Context, milk string) error {
	if err := s.requireMilk("add_milk"); err != nil {
		return err
	}
	if err := s.openDoor(ctx, cancelCtx); err != nil {
		return s.milkStepErr(milk, err)
	}
	s.setStep(stepAddingMilk)
	if err := s.fetchMilkBottle(ctx, cancelCtx, milk); err != nil {
		return s.milkStepErr(milk, err)
	}
	if err := s.pourMilk(ctx, cancelCtx); err != nil {
		return s.milkStepErr(milk, err)
	}
	if err := s.returnMilkBottle(ctx, cancelCtx); err != nil {
		return s.milkStepErr(milk, err)
	}
	if err := s.closeDoor(ctx, cancelCtx); err != nil {
		return s.milkStepErr(milk, err)
	}
	return nil
}

// backOutOfFridge best-effort recovers from a failed grab: open the jaws (so a
// bottle gripped or half-gripped is left standing on its spot) and back straight
// out to the spot's approach pose, so recovery starts from outside the fridge
// interior rather than inside the shield. Failures are logged, not returned —
// the caller is already returning the error that matters.
func (s *beanjaminCoffee) backOutOfFridge(ctx, cancelCtx context.Context, spot string) {
	logger := s.activeOrderLogger()
	if err := s.openAndVerifyOpen(ctx); err != nil {
		logger.Warnf("fetch_milk: recover: open gripper: %v", err)
		return
	}
	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkApproachPose(spot), nil)); err != nil {
		logger.Warnf("fetch_milk: recover: back out to %q: %v", milkApproachPose(spot), err)
	}
}

// fridgeLinearStep is a straight-line move between a bottle's approach and grab
// poses — in to grab or set down, out to retreat. Only these moves pass through
// the interior shield that keeps the free traverse off the shelves; the
// held-item pair is included only while the bottle is in hand. moveOpts is
// s.slowMoveOptions() while the full bottle is held, nil when the gripper is
// empty.
func (s *beanjaminCoffee) fridgeLinearStep(pose string, moveOpts *StepMoveOptions) Step {
	return Step{PoseName: pose, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint,
		Pause: shortPause, AllowedCollisions: s.pickupAreaShieldCollisions(milkAreaShieldFrameName),
		MoveOptions: moveOpts}
}

// attachMilkBottleGeometry models the bottle just grabbed and attaches it to the
// gripper as the held item (see milkBottleBox).
func (s *beanjaminCoffee) attachMilkBottleGeometry(ctx context.Context) error {
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return err
	}
	box, err := s.milkBottleBox(fs, fsInputs)
	if err != nil {
		return err
	}
	return s.attachDetectedGeometry(ctx, pickupLabelMilk, box)
}

// milkBottleBox builds the world-frame box of a bottle the jaws are closed on:
// milk_bottle_dimensions, under the grip point, with its base resting on the
// highest static surface beneath it (the fridge floor or the shelf the spot is
// on). Seating it rather than centering it on the grip point keeps the model
// independent of how high up the bottle the milk_<spot>_grab pose was taught.
//
// The jaws are around the bottle, so the grip point must fall within its
// height. A seated box that misses the grip point means the surface the bottle
// stands on isn't modeled (the box landed on something lower, like the table
// under the fridge) or the grab pose is above the bottle, and both are errors
// rather than a model that is silently wrong.
func (s *beanjaminCoffee) milkBottleBox(fs *referenceframe.FrameSystem, fsInputs referenceframe.FrameSystemInputs) (spatialmath.Geometry, error) {
	dims := s.cfg.MilkBottleDimensions
	box, err := s.configuredContainerBox(fs, fsInputs, pickupLabelMilk, dims, &RelativePose{})
	if err != nil {
		return nil, err
	}
	// Enumerated before the bottle is attached, so it is never taken for the
	// surface it stands on.
	surfaces, err := geom.WorldAnchoredSurfaceBoxes(fs, fsInputs)
	if err != nil {
		return nil, fmt.Errorf("find the surface under the milk bottle: %w", err)
	}
	grip := box.Pose().Point()
	seated, dz, ok := geom.SeatOnSurface(surfaces, box, surfaceRestClearanceMm)
	if !ok {
		return nil, fmt.Errorf("no surface modeled beneath the milk bottle at (x=%.1f, y=%.1f): add the fridge floor to the frame system", grip.X, grip.Y)
	}
	// box was centered on the grip point, so after the shift the grip point sits
	// -dz from the bottle's center.
	if math.Abs(dz) > dims.HeightMm/2 {
		return nil, fmt.Errorf("the milk bottle seated on the surface beneath it (shifted %.1fmm) does not reach the grip point at Z=%.1f: "+
			"check that the surface it stands on is modeled and that the grab pose is within milk_bottle_dimensions' height_mm", dz, grip.Z)
	}
	s.activeOrderLogger().Infof("modeling the held milk bottle (%.0f×%.0f mm) on the surface beneath it: base %.1fmm below the grip point",
		dims.DiameterMm, dims.HeightMm, dims.HeightMm/2-dz)
	return seated, nil
}

// milkStepErr wraps a failure inside the milk sequence, saying so when the
// fridge is left standing open. The arm does not try to shut the door itself
// after a failure: it may still be holding the bottle, and a sweep needs the
// gripper for the handle. So the door stays open, the model keeps recording the
// angle it really is at, and the message tells the operator what to fix.
func (s *beanjaminCoffee) milkStepErr(milk string, err error) error {
	if s.doorOpenDegs != 0 {
		return fmt.Errorf("add_milk: %w (the fridge door is standing open at %.0f° — %s; put the rest of the machine back to its starting state by hand, then shut the door before sending proceed: proceed is what declares the door shut again)", err, s.doorOpenDegs, s.milkPutBackHint(milk))
	}
	return fmt.Errorf("add_milk: %w", err)
}

// milkPutBackHint tells the operator where a bottle taken out of the gripper by
// hand goes: its own spot, named, since that is where the arm reaches for it
// next time.
func (s *beanjaminCoffee) milkPutBackHint(milk string) string {
	spot, err := s.milkSpot(milk)
	if err != nil {
		return "if the gripper is holding a milk bottle, take it out and stand it back on its own spot in the fridge"
	}
	return fmt.Sprintf("if the gripper is holding the %s milk, take it out and stand it back on its %s spot in the fridge, where the arm reaches for it next time", milk, spot)
}

// fetchMilkBottle takes the named milk's bottle off its spot in the fridge,
// leaving it held by the gripper: fly to the spot's approach pose, open,
// straight in to the grab pose, grab, straight back out to the approach pose.
// The door must already be open — the approach is through the opening.
//
// Which milk is out is recorded only once the bottle is confirmed in hand and
// modeled, so a failed grab can't leave a return pointed at a bottle the
// gripper never took. A grab that fails once the arm has started in backs it
// out to the approach pose with the jaws open, so recovery starts from outside
// the fridge.
func (s *beanjaminCoffee) fetchMilkBottle(ctx, cancelCtx context.Context, milk string) error {
	if err := s.requireMilk("fetch_milk"); err != nil {
		return err
	}
	if s.gripper == nil {
		return fmt.Errorf("fetch_milk: no gripper configured")
	}
	if s.heldMilk != "" {
		return fmt.Errorf("fetch_milk: the %s milk is already out of the fridge — run return_milk first", s.heldMilk)
	}
	// Merge cancelCtx in so an operator cancel also interrupts the gripper calls,
	// which take a single context (executeStep merges it for the moves).
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()
	spot, err := s.milkSpot(milk)
	if err != nil {
		return fmt.Errorf("fetch_milk: %w", err)
	}
	approachStep := Step{PoseName: milkApproachPose(spot), PoseSwitch: s.clawsSw, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("fetch_milk: %w", err)
	}
	// Verified open, not a fixed pause: jaws still closing in on the way in would
	// strike the bottle.
	if err := s.openAndVerifyOpen(ctx); err != nil {
		return fmt.Errorf("fetch_milk: open gripper: %w", err)
	}
	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkGrabPose(spot), nil)); err != nil {
		s.backOutOfFridge(ctx, cancelCtx, spot)
		return fmt.Errorf("fetch_milk: %w", err)
	}
	if err := s.grabAndVerifyHolding(ctx); err != nil {
		s.backOutOfFridge(ctx, cancelCtx, spot)
		return fmt.Errorf("fetch_milk: grab the %s milk: %w", milk, err)
	}
	// Model the bottle from milk_bottle_dimensions, standing on the surface under
	// it, so the retreat and the carry to the glass route around it. Without the
	// model nothing that follows can run — the pour poses and the level carry
	// are resolved against it — so a failure lets go of the bottle where it
	// stands and backs out, rather than leaving it half-taken.
	if err := s.attachMilkBottleGeometry(ctx); err != nil {
		s.backOutOfFridge(ctx, cancelCtx, spot)
		return fmt.Errorf("fetch_milk: model the %s milk bottle: %w", milk, err)
	}
	s.heldMilk = milk
	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkApproachPose(spot), s.slowMoveOptions())); err != nil {
		return fmt.Errorf("fetch_milk: retreat with the %s milk: %w", milk, err)
	}
	s.activeOrderLogger().Infof("fetch_milk: %s milk in hand", milk)
	return nil
}

// pourMilk carries the held bottle over the staged glass and tilts it to pour,
// dwells for milkPourDwell so the glass fills, then returns it upright before
// moving away. Same pour pivot as pourEspresso: the claws tilt the bottle
// while moving it along the straight line between the authored approach and
// pour points (lifting it as it tips, if milk_pour_relative_pose sits higher),
// so the stream stays over the glass, and the staged glass
// remains a hard obstacle throughout — the bottle must clear it, never drive
// into it. How much milk the latte gets is the dwell, not the tilt.
func (s *beanjaminCoffee) pourMilk(ctx, cancelCtx context.Context) error {
	if err := s.requireMilk("pour_milk"); err != nil {
		return err
	}
	// The bottle is full and open here, so carry it level (NoSpill) and slow.
	approachStep := Step{PoseName: clawPoseMilkPourApproach, PoseSwitch: s.clawsSw, Pause: shortPause, NoSpill: true,
		MoveOptions: s.slowMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("pour_milk: %w", err)
	}
	// Held to pourConstraint so the planner can't detour the open bottle off
	// the glass; a tilt it can't finish is executed as far as it planned.
	pourStep := Step{PoseName: clawPoseMilkPour, PoseSwitch: s.clawsSw, PivotFromPose: clawPoseMilkPourApproach, PivotDegreesPerStep: 5,
		LinearConstraint: pourConstraint, AcceptPartialPlan: true, MoveOptions: s.pourMoveOptions(), Pause: s.milkPourDwell()}
	if err := s.executeStep(ctx, cancelCtx, pourStep); err != nil {
		return fmt.Errorf("pour_milk: %w", err)
	}
	// Return upright along the same pivot (lowering the bottle as it rights,
	// mirroring the pour) so any residual drip stays over the glass, at the
	// slow speed so the milk left in the bottle doesn't slosh out. The return is
	// constrained the same way but must complete: carrying a still-tilted bottle
	// away would pour milk along the whole carry.
	uprightStep := Step{PoseName: clawPoseMilkPourApproach, PoseSwitch: s.clawsSw, PivotFromPose: clawPoseMilkPour, PivotDegreesPerStep: 5,
		LinearConstraint: pourConstraint, Pause: shortPause, MoveOptions: s.slowMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, uprightStep); err != nil {
		return fmt.Errorf("pour_milk: %w", err)
	}
	s.incrementSensorReading(ctx, s.usageSensor, "milk", "milk_pours", 1)
	return nil
}

// returnMilkBottle puts the bottle fetchMilkBottle took back on its spot:
// carry it level to the spot's approach pose, straight in to the grab pose,
// release, straight back out to the approach pose, close the jaws. The reverse
// of the fetch, on the same two poses.
//
// The record of which milk is out is cleared on release, so a second return
// with no intervening fetch fails loudly instead of driving an empty gripper at
// a spot where the bottle already stands.
func (s *beanjaminCoffee) returnMilkBottle(ctx, cancelCtx context.Context) error {
	if err := s.requireMilk("return_milk"); err != nil {
		return err
	}
	if s.gripper == nil {
		return fmt.Errorf("return_milk: no gripper configured")
	}
	milk := s.heldMilk
	if milk == "" {
		return fmt.Errorf("return_milk: no milk is out of the fridge — run fetch_milk first")
	}
	// As in fetchMilkBottle: let an operator cancel reach the gripper calls too.
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()
	spot, err := s.milkSpot(milk)
	if err != nil {
		return fmt.Errorf("return_milk: %w", err)
	}
	s.activeOrderLogger().Infof("return_milk: putting the %s milk back at the %s spot", milk, spot)

	// The bottle still holds milk, so carry it level and slow back to the fridge.
	approachStep := Step{PoseName: milkApproachPose(spot), PoseSwitch: s.clawsSw, Pause: shortPause, NoSpill: true,
		MoveOptions: s.slowMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("return_milk: approach the shelf: %w", err)
	}
	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkGrabPose(spot), s.slowMoveOptions())); err != nil {
		return fmt.Errorf("return_milk: set the bottle on the shelf: %w", err)
	}
	// Verified open before backing out: jaws still on the bottle would drag it off
	// the shelf. On failure the bottle is still recorded as held and still
	// modeled in the gripper, which is what the operator will find.
	if err := s.openAndVerifyOpen(ctx); err != nil {
		return fmt.Errorf("return_milk: release the bottle: %w", err)
	}
	// The bottle is standing on the shelf; it no longer travels with the gripper.
	s.detachHeldGeometry()
	s.heldMilk = ""

	// The gripper starts inside the interior shield, so it stays allowed for the
	// straight-out retreat (the held-item pair drops out now that nothing is held).
	// The gripper is empty, so the arm's default speed.
	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkApproachPose(spot), nil)); err != nil {
		return fmt.Errorf("return_milk: retreat after releasing the bottle: %w", err)
	}
	if err := s.closeGripper(ctx); err != nil {
		return fmt.Errorf("return_milk: %w", err)
	}
	return nil
}

// defaultMilkPourSec is how long the bottle is held tilted over the glass when
// milk_pour_sec is unset.
const defaultMilkPourSec = 4.0

// milkPourDwell returns how long the tilted bottle is held over the glass —
// the configured pour time or the default. This is what sets the milk dose, so
// it is tuned on the machine against the bottle and the glass in use.
func (s *beanjaminCoffee) milkPourDwell() time.Duration {
	return time.Duration(orDefault(s.cfg.MilkPourSec, defaultMilkPourSec) * float64(time.Second))
}
