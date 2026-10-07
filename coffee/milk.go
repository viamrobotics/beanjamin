package coffee

// The iced-latte milk path: open the fridge, take the ordered milk's bottle off
// the shelf, pour it into the staged glass, put the bottle back, and shut the
// fridge. Gated by can_serve_iced_latte.
//
// Each milk in milk_options stands at a fixed spot in the fridge, and each spot
// is taught as two claws-switch poses: milk_<spot>_approach in front of the
// bottle and milk_<spot>_grab with the jaws around it. The grab is a straight
// line between them, and the return is the same line run backwards — so the
// bottle has to be put back at its spot. The pour is the fixed-point pivot
// the espresso pour uses (iced.go), and the door is the hinge-arc sweep
// (door.go).

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// milkAreaShieldFrameName is the optional obstacle enclosing the fridge
// interior, the fridge-side counterpart of the clean-cup/glass area shields: a
// hard obstacle while the arm flies to and from the fridge (so it doesn't clip
// the shelves or the door on a free traverse), opened up only on the straight-in
// grab and the straight-out retreat. Inert when the frame system doesn't define
// it, like every other shield.
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

// milkChoiceKey carries execute_action's optional "milk" to the milk actions,
// whose signature has no room for it.
type milkChoiceKey struct{}

// withMilkChoice returns ctx carrying the milk an execute_action names.
func withMilkChoice(ctx context.Context, milk string) context.Context {
	return context.WithValue(ctx, milkChoiceKey{}, milk)
}

// milkChoiceActions are the actions execute_action's "milk" may be passed to.
// return_milk takes none: it puts back whichever bottle is out.
var milkChoiceActions = map[string]bool{
	"fetch_milk":       true,
	"add_milk":         true,
	"serve_iced_latte": true,
}

// checkMilkChoiceAllowed refuses a "milk" on an action that wouldn't use it,
// rather than silently ignoring it.
func checkMilkChoiceAllowed(name string) error {
	if milkChoiceActions[name] {
		return nil
	}
	allowed := make([]string, 0, len(milkChoiceActions))
	for k := range milkChoiceActions {
		allowed = append(allowed, k)
	}
	sort.Strings(allowed)
	return fmt.Errorf("milk is not accepted by action %q, only by: %v", name, allowed)
}

// actionMilk is the milk a hand-run milk action uses: the one execute_action
// named, or the default (first) milk when it named none. It checks the machine
// is set up for milk first, so action gets requireMilk's error rather than a
// vaguer one about the milk list.
func (s *beanjaminCoffee) actionMilk(ctx context.Context, action string) (string, error) {
	if err := s.requireMilk(action); err != nil {
		return "", err
	}
	milk, _ := ctx.Value(milkChoiceKey{}).(string)
	return s.menu().Milk(milk)
}

// refuseDropWhileMilkHeld keeps rewind from opening the jaws on a milk bottle
// wherever the arm happens to be (over the glass, mid-carry): the bottle would
// fall, and it belongs on its spot in the fridge. Only a confirmed hold refuses;
// once the operator has taken the bottle out, rewind goes ahead and its rebuild
// forgets it.
func (s *beanjaminCoffee) refuseDropWhileMilkHeld(ctx context.Context) error {
	if s.heldMilk == "" || s.gripper == nil {
		return nil
	}
	pos, err := s.gripperPos(ctx)
	if err != nil || s.classifyGripper(pos) != gripperHolding {
		// Unreadable: dropHeldContainer won't open the jaws either.
		return nil
	}
	return fmt.Errorf("the gripper is holding a milk bottle — %s, then rewind again", s.milkPutBackHint(s.heldMilk))
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
// bottle gripped or half-gripped is left standing on its spot) and back straight out to the
// spot's approach pose, so rewind starts from outside the fridge interior rather
// than inside the shield. Failures are logged, not returned — the caller is
// already returning the error that matters.
func (s *beanjaminCoffee) backOutOfFridge(ctx, cancelCtx context.Context, spot string) {
	logger := s.activeOrderLogger()
	if err := s.openAndVerifyOpen(ctx); err != nil {
		logger.Warnf("fetch_milk: recover: open gripper: %v", err)
		return
	}
	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkApproachPose(spot))); err != nil {
		logger.Warnf("fetch_milk: recover: back out to %q: %v", milkApproachPose(spot), err)
	}
}

// fridgeLinearStep is a straight-line move between a bottle's approach and grab
// poses — in to grab or set down, out to retreat. Only these moves pass through
// the interior shield that keeps the free traverse off the shelves; the
// held-item pair is included only while the bottle is in hand.
func (s *beanjaminCoffee) fridgeLinearStep(pose string) Step {
	return Step{PoseName: pose, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint,
		Pause: shortPause, AllowedCollisions: s.pickupAreaShieldCollisions(milkAreaShieldFrameName)}
}

// milkStepErr wraps a failure inside the milk sequence, saying so when the
// fridge is left standing open. The arm does not try to shut the door itself
// after a failure: it may still be holding the bottle, and a sweep needs the
// gripper for the handle. So the door stays open, the model keeps recording the
// angle it really is at, and the message tells the operator what to fix.
func (s *beanjaminCoffee) milkStepErr(milk string, err error) error {
	if s.doorOpenDegs != 0 {
		return fmt.Errorf("add_milk: %w (the fridge door is standing open at %.0f° — %s; rewind to recover the arm, then shut the door by hand before sending proceed: proceed is what declares the door shut again)", err, s.doorOpenDegs, s.milkPutBackHint(milk))
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

// fetchMilkBottle takes the named milk's bottle off the fridge shelf, leaving
// it held by the gripper: fly to its spot's approach pose, open, straight in to
// the grab pose, grab, straight back out to the approach pose. The door must
// already be open — the approach is through the opening.
//
// Which milk is out is recorded only once the bottle is confirmed in hand and
// modeled, so a failed grab can't leave a return pointed at a bottle the
// gripper never took.
// A grab that fails once the arm has started in backs it out to the approach
// pose with the jaws open, so recovery starts from outside the fridge.
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
	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkGrabPose(spot))); err != nil {
		s.backOutOfFridge(ctx, cancelCtx, spot)
		return fmt.Errorf("fetch_milk: %w", err)
	}
	if err := s.grabAndVerifyHolding(ctx); err != nil {
		s.backOutOfFridge(ctx, cancelCtx, spot)
		return fmt.Errorf("fetch_milk: grab the %s milk: %w", milk, err)
	}
	// Model the bottle from milk_bottle_dimensions, centered on the grip point,
	// so the retreat and the carry to the glass route around it. Without the
	// model nothing that follows can run — the pour poses and the level carry
	// are resolved against it — so a failure lets go of the bottle where it
	// stands and backs out, rather than leaving it half-taken.
	if err := s.attachConfiguredGeometry(ctx, pickupLabelMilk, s.cfg.MilkBottleDimensions, &RelativePose{}); err != nil {
		s.backOutOfFridge(ctx, cancelCtx, spot)
		return fmt.Errorf("fetch_milk: model the %s milk bottle: %w", milk, err)
	}
	s.heldMilk = milk
	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkApproachPose(spot))); err != nil {
		return fmt.Errorf("fetch_milk: retreat with the %s milk: %w", milk, err)
	}
	s.activeOrderLogger().Infof("fetch_milk: %s milk in hand", milk)
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
	// The bottle is full and open here, so carry it level (NoSpill), which also
	// defaults the move to the slow tier.
	approachStep := Step{PoseName: clawPoseMilkPourApproach, PoseSwitch: s.clawsSw, Pause: shortPause, NoSpill: true}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("pour_milk: %w", err)
	}
	pourStep := Step{PoseName: clawPoseMilkPour, PoseSwitch: s.clawsSw, PivotFromPose: clawPoseMilkPourApproach, PivotDegreesPerStep: 5,
		MoveOptions: s.pourMoveOptions(), Pause: s.milkPourDwell()}
	if err := s.executeStep(ctx, cancelCtx, pourStep); err != nil {
		return fmt.Errorf("pour_milk: %w", err)
	}
	// Return upright along the same pivot so any residual drip stays over the
	// glass, at the default (slow) pivot speed so the milk left in the bottle
	// doesn't slosh out.
	uprightStep := Step{PoseName: clawPoseMilkPourApproach, PoseSwitch: s.clawsSw, PivotFromPose: clawPoseMilkPour, PivotDegreesPerStep: 5,
		Pause: shortPause}
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

	// The bottle still holds milk, so carry it level back to the fridge.
	approachStep := Step{PoseName: milkApproachPose(spot), PoseSwitch: s.clawsSw, Pause: shortPause, NoSpill: true}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("return_milk: approach the shelf: %w", err)
	}

	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkGrabPose(spot))); err != nil {
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

	if err := s.executeStep(ctx, cancelCtx, s.fridgeLinearStep(milkApproachPose(spot))); err != nil {
		return fmt.Errorf("return_milk: retreat after releasing the bottle: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("return_milk: close gripper after release: %w", err)
	}
	time.Sleep(gripperPause)
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
