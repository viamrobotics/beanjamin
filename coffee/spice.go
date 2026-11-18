package coffee

// The spice path: take the ordered spice's shaker off the rack, shake a dash
// over the staged glass, and put the shaker back. Offered on the iced latte
// when spice_options is set.
//
// Each spice in spice_options stands at a fixed spot on an open rack, and each
// spot is taught as two claws-switch poses: spice_<spot>_approach in front of
// the shaker and spice_<spot>_grab with the jaws around it. The grab is a
// straight line between them and the return is the same line run backwards, so
// the shaker has to go back to its own spot. The shake poses are resolved
// against the staged glass (glass_relative_poses.go), like the milk pour.

import (
	"context"
	"fmt"
)

// spiceAreaShieldFrameName is the optional obstacle enclosing the spice rack:
// a hard obstacle while the arm flies to and from the rack, opened up only on
// the straight-line moves between a spot's approach and grab poses. Inert when
// the frame system doesn't define it.
const spiceAreaShieldFrameName = "spice-rack"

const (
	// defaultSpiceShakeCount is how many times the tilted shaker is jerked up
	// and back down when spice_shake_count is unset.
	defaultSpiceShakeCount = 3
	// defaultSpiceShakeAmplitudeMm is how far each shake lifts the shaker when
	// spice_shake_amplitude_mm is unset.
	defaultSpiceShakeAmplitudeMm = 20.0
)

func (s *beanjaminCoffee) spiceShakeCount() int {
	return orDefault(s.cfg.SpiceShakeCount, defaultSpiceShakeCount)
}

func (s *beanjaminCoffee) spiceShakeAmplitudeMm() float64 {
	return orDefault(s.cfg.SpiceShakeAmplitudeMm, defaultSpiceShakeAmplitudeMm)
}

// requireSpice rejects the spice actions on a machine that isn't set up for
// them. Without spice_options the shaker dimensions and shake poses are unset,
// and the failure would surface as an unhelpful pose error partway into a
// motion.
func (s *beanjaminCoffee) requireSpice(action string) error {
	if len(s.cfg.SpiceOptions) == 0 {
		return fmt.Errorf("%s: no spice is configured on this machine (set spice_options)", action)
	}
	return nil
}

// firstSpice is the spice the hand-run spice actions use: the first in
// spice_options. An order carries its own spice.
func (s *beanjaminCoffee) firstSpice() string {
	if len(s.cfg.SpiceOptions) == 0 {
		return ""
	}
	return s.cfg.SpiceOptions[0].Name
}

// spiceSpot returns the rack spot the named spice stands at.
func (s *beanjaminCoffee) spiceSpot(spice string) (string, error) {
	for _, sp := range s.cfg.SpiceOptions {
		if sp.Name == spice {
			return sp.Spot, nil
		}
	}
	return "", fmt.Errorf("unknown spice %q", spice)
}

// addSpice is the whole spice trip for the named spice, run on a staged glass
// with an empty gripper: fetch the shaker, shake it over the glass, put it
// back. Registered as the add_spice execute_action and run by finishIced for a
// spiced iced latte. One chunk, so the grasp on the rack leaves the wrist room
// for the shake tilt.
func (s *beanjaminCoffee) addSpice(ctx, cancelCtx context.Context, spice string) error {
	if err := s.requireSpice("add_spice"); err != nil {
		return err
	}
	s.setStep(stepAddingSpice)
	if err := s.inChunk("fetch, shake and return spice", func() error {
		if err := s.fetchSpiceShaker(ctx, cancelCtx, spice); err != nil {
			return err
		}
		if err := s.shakeSpice(ctx, cancelCtx); err != nil {
			return err
		}
		return s.returnSpiceShaker(ctx, cancelCtx)
	}); err != nil {
		if s.scene().heldSpice != "" {
			return fmt.Errorf("add_spice: %w (%s)", err, s.spicePutBackHint(spice))
		}
		return fmt.Errorf("add_spice: %w", err)
	}
	return nil
}

// rackLinearStep is a straight-line move between a shaker's approach and grab
// poses. Only these moves pass through the rack shield that keeps the free
// traverse off the rack. moveOpts is s.slowMoveOptions() while the shaker is
// held, nil when the gripper is empty.
func (s *beanjaminCoffee) rackLinearStep(pose string, moveOpts *StepMoveOptions) Step {
	return Step{PoseName: pose, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint,
		Pause: shortPause, AllowedCollisions: s.areaShieldCollisions(spiceAreaShieldFrameName),
		MoveOptions: moveOpts}
}

// setHeldSpice records which spice shaker is off the rack, "" for none.
func (s *beanjaminCoffee) setHeldSpice(spice string) {
	if err := s.editScene(func(sc *sceneState) error {
		sc.heldSpice = spice
		return nil
	}); err != nil {
		s.activeOrderLogger().Errorf("record the spice off the rack as %q: %v", spice, err)
	}
}

// spicePutBackHint tells the operator where a shaker taken out of the gripper
// by hand goes: its own spot, where the arm reaches for it next time.
func (s *beanjaminCoffee) spicePutBackHint(spice string) string {
	spot, err := s.spiceSpot(spice)
	if err != nil {
		return "if the gripper is holding a spice shaker, take it out and stand it back on its own spot on the rack"
	}
	return fmt.Sprintf("if the gripper is holding the %s shaker, take it out and stand it back on its %s spot on the rack, where the arm reaches for it next time", spice, spot)
}

// attachSpiceShakerGeometry models the shaker just grabbed and attaches it to
// the gripper as the held item, standing on the surface beneath it.
func (s *beanjaminCoffee) attachSpiceShakerGeometry(ctx context.Context) error {
	at, err := s.expectedPlacement(ctx)
	if err != nil {
		return err
	}
	dims := s.cfg.SpiceShakerDimensions
	var baseBelowGripMm float64
	if err := s.editScene(func(sc *sceneState) error {
		var err error
		baseBelowGripMm, err = sc.attachSeatedContainer(at, pickupLabelSpice, "spice shaker", "spice_shaker_dimensions", dims)
		return err
	}); err != nil {
		return err
	}
	s.activeOrderLogger().Infof("attached the held spice shaker (%.0f×%.0f mm) on the surface beneath it: base %.1fmm below the grip point",
		dims.DiameterMm, dims.HeightMm, baseBelowGripMm)
	return nil
}

// fetchSpiceShaker takes the named spice's shaker off its rack spot, leaving it
// held by the gripper: fly to the spot's approach pose, open, straight in to
// the grab pose, grab, straight back out.
//
// Which spice is out is recorded once the grab is confirmed, so a failed grab
// can't leave a return pointed at a shaker the gripper never took.
func (s *beanjaminCoffee) fetchSpiceShaker(ctx, cancelCtx context.Context, spice string) error {
	if err := s.requireSpice("fetch_spice"); err != nil {
		return err
	}
	if s.gripper == nil {
		return fmt.Errorf("fetch_spice: no gripper configured")
	}
	if s.scene().heldSpice != "" {
		return fmt.Errorf("fetch_spice: the %s shaker is already off the rack — run return_spice first", s.scene().heldSpice)
	}
	// Merge cancelCtx in so an operator cancel also interrupts the gripper calls.
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()
	spot, err := s.spiceSpot(spice)
	if err != nil {
		return fmt.Errorf("fetch_spice: %w", err)
	}
	approachStep := Step{PoseName: spiceApproachPose(spot), PoseSwitch: s.clawsSw, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("fetch_spice: %w", err)
	}
	// Verified open: jaws still closing on the way in would knock the shaker over.
	if err := s.openAndVerifyOpen(ctx); err != nil {
		return fmt.Errorf("fetch_spice: open gripper: %w", err)
	}
	if err := s.executeStep(ctx, cancelCtx, s.rackLinearStep(spiceGrabPose(spot), nil)); err != nil {
		return fmt.Errorf("fetch_spice: %w", err)
	}
	if err := s.grabAndVerifyHolding(ctx, spice+" shaker"); err != nil {
		return fmt.Errorf("fetch_spice: grab the %s shaker: %w", spice, err)
	}
	s.setHeldSpice(spice)
	// The shake poses are resolved against the held item, so nothing after this
	// can run without the model.
	if err := s.attachSpiceShakerGeometry(ctx); err != nil {
		return fmt.Errorf("fetch_spice: model the %s shaker: %w", spice, err)
	}
	if err := s.executeStep(ctx, cancelCtx, s.rackLinearStep(spiceApproachPose(spot), s.slowMoveOptions())); err != nil {
		return fmt.Errorf("fetch_spice: retreat with the %s shaker: %w", spice, err)
	}
	s.activeOrderLogger().Infof("fetch_spice: %s shaker in hand", spice)
	return nil
}

// shakeSpice carries the held shaker upright over the staged glass, tips it
// over (the same pour pivot the milk uses), jerks it up by
// spice_shake_amplitude_mm and back down spice_shake_count times so the spice
// falls through the holes, and rights it again before moving away. Every move
// over the glass is held to pourConstraint so the shaker stays above it.
func (s *beanjaminCoffee) shakeSpice(ctx, cancelCtx context.Context) error {
	if err := s.requireSpice("shake_spice"); err != nil {
		return err
	}
	// Carried upright so nothing falls out on the way to the glass.
	approachStep := Step{PoseName: clawPoseSpiceShakeApproach, PoseSwitch: s.clawsSw, Pause: shortPause, NoSpill: true,
		MoveOptions: s.slowMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("shake_spice: %w", err)
	}
	tiltStep := Step{PoseName: clawPoseSpiceShake, PoseSwitch: s.clawsSw, PivotFromPose: clawPoseSpiceShakeApproach, PivotDegreesPerStep: 5,
		LinearConstraint: pourConstraint, MoveOptions: s.pourMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, tiltStep); err != nil {
		return fmt.Errorf("shake_spice: %w", err)
	}
	// The shakes run at the arm's default speed: it is the abrupt stop at the
	// bottom of each one that knocks the spice out.
	for i := range s.spiceShakeCount() {
		up := Step{PoseName: clawPoseSpiceShakeLift, PoseSwitch: s.clawsSw, LinearConstraint: pourConstraint}
		down := Step{PoseName: clawPoseSpiceShake, PoseSwitch: s.clawsSw, LinearConstraint: pourConstraint}
		for _, st := range []Step{up, down} {
			if err := s.executeStep(ctx, cancelCtx, st); err != nil {
				return fmt.Errorf("shake_spice: shake %d: %w", i+1, err)
			}
		}
	}
	// Righted along the same pivot so whatever is still falling lands in the
	// glass, not along the carry back to the rack.
	uprightStep := Step{PoseName: clawPoseSpiceShakeApproach, PoseSwitch: s.clawsSw, PivotFromPose: clawPoseSpiceShake, PivotDegreesPerStep: 5,
		LinearConstraint: pourConstraint, Pause: shortPause, MoveOptions: s.slowMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, uprightStep); err != nil {
		return fmt.Errorf("shake_spice: %w", err)
	}
	// heldSpice is empty only for a hand-run shake_spice with no fetch_spice
	// before it, where there's no telling which shaker was used.
	if spice := s.scene().heldSpice; spice != "" {
		s.countUsage(ctx, spice+" dashes", spiceDashCounter(spice), 1)
	}
	return nil
}

// returnSpiceShaker puts the shaker fetchSpiceShaker took back on its spot:
// carry it upright to the spot's approach pose, straight in to the grab pose,
// release, straight back out, close the jaws.
//
// The record of which spice is out is cleared on release, so a second return
// with no intervening fetch fails loudly instead of driving an empty gripper
// at a spot where the shaker already stands.
func (s *beanjaminCoffee) returnSpiceShaker(ctx, cancelCtx context.Context) error {
	if err := s.requireSpice("return_spice"); err != nil {
		return err
	}
	if s.gripper == nil {
		return fmt.Errorf("return_spice: no gripper configured")
	}
	spice := s.scene().heldSpice
	if spice == "" {
		return fmt.Errorf("return_spice: no spice shaker is off the rack — run fetch_spice first")
	}
	ctx, done := mergedCancelContext(ctx, cancelCtx)
	defer done()
	spot, err := s.spiceSpot(spice)
	if err != nil {
		return fmt.Errorf("return_spice: %w", err)
	}
	s.activeOrderLogger().Infof("return_spice: putting the %s shaker back at the %s spot", spice, spot)

	approachStep := Step{PoseName: spiceApproachPose(spot), PoseSwitch: s.clawsSw, Pause: shortPause, NoSpill: true,
		MoveOptions: s.slowMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("return_spice: approach the rack: %w", err)
	}
	if err := s.executeStep(ctx, cancelCtx, s.rackLinearStep(spiceGrabPose(spot), s.slowMoveOptions())); err != nil {
		return fmt.Errorf("return_spice: set the shaker on the rack: %w", err)
	}
	// Verified open before backing out: jaws still on the shaker would drag it
	// off the rack.
	if err := s.openAndVerifyOpen(ctx); err != nil {
		return fmt.Errorf("return_spice: release the shaker: %w", err)
	}
	s.detachHeldGeometry()
	s.setHeldSpice("")

	if err := s.executeStep(ctx, cancelCtx, s.rackLinearStep(spiceApproachPose(spot), nil)); err != nil {
		return fmt.Errorf("return_spice: retreat after releasing the shaker: %w", err)
	}
	if err := s.closeGripper(ctx); err != nil {
		return fmt.Errorf("return_spice: %w", err)
	}
	return nil
}
