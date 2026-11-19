package coffee

// The vessel a coffee is brewed into: a cup for an espresso or lungo, a mug for
// an Americano. Placing it under the group head and taking it back out are the
// same routine for both, parameterized by vessel.

import (
	"context"
	"fmt"
)

// vessel is what the routine needs to know about the container on the way
// under the group head and back out: how it is picked, the claws poses in
// front of and under the spout, and how its held geometry is modeled when
// nothing was cached.
type vessel struct {
	label            string                                     // pickupLabelCup / pickupLabelMug
	pick             func(ctx, cancelCtx context.Context) error // pickCupDynamic / pickMugDynamic
	underApproach    string                                     // claws pose in front of the group head
	readyForCoffee   string                                     // claws pose under the group head
	attachConfigured func(ctx context.Context) error            // fallback when nothing is cached
	dimsField        string                                     // config key attachConfigured models from, for messages
}

// cupVessel is the espresso cup: picked by the cup vision pipeline and modeled
// from cup_dimensions.
func (s *beanjaminCoffee) cupVessel() vessel {
	return vessel{
		label:            pickupLabelCup,
		pick:             s.pickCupDynamic,
		underApproach:    clawPoseCupUnderMachineApproach,
		readyForCoffee:   clawPoseCupReadyForCoffee,
		attachConfigured: s.attachConfiguredCupGeometry,
		dimsField:        "cup_dimensions",
	}
}

// mugVessel is the Americano's mug: picked by its own vision pipeline and
// modeled from mug_dimensions plus its handle (can_serve_mug_drinks).
func (s *beanjaminCoffee) mugVessel() vessel {
	return vessel{
		label:            pickupLabelMug,
		pick:             s.pickMugDynamic,
		underApproach:    clawPoseMugUnderMachineApproach,
		readyForCoffee:   clawPoseMugReadyForCoffee,
		attachConfigured: s.attachConfiguredMugGeometry,
		dimsField:        "mug_dimensions",
	}
}

// approachBrewedVessel sends the claws to the vessel's under-machine approach
// pose and opens the gripper, ready to take the brewed vessel.
func (s *beanjaminCoffee) approachBrewedVessel(ctx, cancelCtx context.Context, v vessel) error {
	name := "approach_brewed_" + v.label
	approachStep := Step{PoseName: v.underApproach, PoseSwitch: s.clawsSw}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := s.openGripper(ctx); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// graspBrewedInPlace takes the vessel once the gripper is open above it
// (approachBrewedVessel): linear descent + grab, restoring its held geometry.
// There is no retreat; the arm is left at v.readyForCoffee holding the vessel.
func (s *beanjaminCoffee) graspBrewedInPlace(ctx, cancelCtx context.Context, v vessel) error {
	name := "grasp_brewed_" + v.label
	grabStep := Step{PoseName: v.readyForCoffee, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause,
		MoveOptions: s.slowMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := s.grabAndVerifyHolding(ctx, "brewed "+v.label); err != nil {
		return fmt.Errorf("%s: grab gripper: %w", name, err)
	}
	// Put the vessel's cached geometry back on the gripper so the retreat routes
	// around it.
	if err := s.reattachGeometry(v.label); err != nil {
		s.activeOrderLogger().Warnf("%s: reattach %s geometry failed, continuing untracked: %v", name, v.label, err)
	}
	// With nothing cached (a vessel not picked by vision), model it from its
	// configured dimensions instead.
	if !s.scene().heldItemAttached {
		if err := v.attachConfigured(ctx); err != nil {
			s.activeOrderLogger().Warnf("%s: model %s from %s failed, continuing untracked: %v", name, v.label, v.dimsField, err)
		}
	}
	return nil
}

// retreatBrewedVessel is the linear retreat with the vessel in hand, back to
// v.underApproach, at the spillage-prevention speed.
func (s *beanjaminCoffee) retreatBrewedVessel(ctx, cancelCtx context.Context, v vessel) error {
	retreatStep := Step{PoseName: v.underApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: s.scene().heldItemSurfaceCollisions(heldItemMachineCollisions),
		MoveOptions: s.spillagePreventionMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("grasp_brewed_%s: %w", v.label, err)
	}
	return nil
}

// grabBrewedMugFromMachine retrieves the brewed mug from under the machine:
// approach, open, linear descent and grab, then the retreat to
// mug_under_machine_approach.
func (s *beanjaminCoffee) grabBrewedMugFromMachine(ctx, cancelCtx context.Context) error {
	v := s.mugVessel()
	if err := s.approachBrewedVessel(ctx, cancelCtx, v); err != nil {
		return err
	}
	if err := s.graspBrewedInPlace(ctx, cancelCtx, v); err != nil {
		return err
	}
	return s.retreatBrewedVessel(ctx, cancelCtx, v)
}
