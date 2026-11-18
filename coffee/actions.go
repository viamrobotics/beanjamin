package coffee

// execute_action: running one named brew-cycle phase on its own, for stepping
// through a sequence by hand.

import (
	"context"
	"fmt"
)

// actionFuncs is the execute_action surface for this machine's configuration.
// Split out from executeAction so which actions exist is assertable without a
// constructed service (executeAction refreshes the frame system before
// dispatching, so it needs real dependencies).
//
// The three placement actions report a serving slot (for a delivery order's
// pickup_position), but a standalone execute_action has no order to attach it
// to, so their closures drop it and return only the error.
func (s *beanjaminCoffee) actionFuncs() map[string]func(ctx, cancelCtx context.Context) error {
	actions := map[string]func(ctx, cancelCtx context.Context) error{
		"grind_coffee":         s.grindCoffee,
		"tamp_ground":          s.tampGround,
		"lock_portafilter":     s.lockPortaFilter,
		"unlock_portafilter":   s.unlockPortaFilter,
		"release_filter":       s.releaseFilter,
		"grab_filter":          s.grabFilter,
		"brew_coffee":          s.brewCoffee,
		"set_cup_for_coffee":   s.setCupForCoffee,
		"clean_portafilter":    s.cleanPortafilter,
		"fetch_glass":          s.fetchGlass,               // vision-grab a glass off the shelf
		"pulse_ice_pin":        s.pulseIcePin,              // hardware only, no arm motion
		"dispense_ice":         s.dispenseIce,              // arm to chute + pulse + retreat
		"move_to_ice_dispense": s.moveToIceDispense,        // dispense_ice's move half, no pin
		"stage_glass":          s.stageGlass,               // set held glass down, release
		"grab_brewed_cup":      s.grabBrewedCupFromMachine, // retrieve cup from under machine
		"pour_espresso":        s.pourEspresso,             // pour held cup over staged glass
		"grab_staged_glass":    s.grabStagedGlass,          // re-grab the staged glass
		"give_full_cup_to_customer": func(ctx, cancelCtx context.Context) error {
			_, err := s.placeFullCupOnShelf(ctx, cancelCtx)
			return err
		},
		// Manual placement assumes the vessel is full: an operator stepping by hand
		// could be holding either, and treating a filled cup as empty is the
		// expensive mistake.
		"place_held": func(ctx, cancelCtx context.Context) error { // place held vessel in serving area
			_, err := s.placeHeldInServingArea(ctx, cancelCtx, nil, heldFilled)
			return err
		},
		"serve_iced_coffee": func(ctx, cancelCtx context.Context) error { // full sequence end-to-end
			_, err := s.serveIcedCoffee(ctx, cancelCtx)
			return err
		},
		// fetch_milk and add_milk use the first milk in milk_options; return_milk
		// puts back whichever bottle is out.
		"fetch_milk": func(ctx, cancelCtx context.Context) error { // take the bottle off its spot in the open fridge
			return s.fetchMilkBottle(ctx, cancelCtx, s.firstMilk())
		},
		"pour_milk":   s.pourMilk,         // pour held bottle into the staged glass
		"return_milk": s.returnMilkBottle, // set the bottle back on its spot
		"add_milk": func(ctx, cancelCtx context.Context) error { // open fridge + fetch + pour + return + close
			return s.addMilk(ctx, cancelCtx, s.firstMilk())
		},
		"serve_iced_latte": func(ctx, cancelCtx context.Context) error { // iced coffee + milk, end-to-end
			_, err := s.serveIcedLatte(ctx, cancelCtx)
			return err
		},
		// fetch_spice and add_spice use the first spice in spice_options;
		// return_spice puts back whichever shaker is out.
		"fetch_spice": func(ctx, cancelCtx context.Context) error { // take the shaker off its rack spot
			return s.fetchSpiceShaker(ctx, cancelCtx, s.firstSpice())
		},
		"shake_spice":  s.shakeSpice,        // shake the held shaker over the staged glass
		"return_spice": s.returnSpiceShaker, // set the shaker back on its rack spot
		"add_spice": func(ctx, cancelCtx context.Context) error { // fetch + shake + return
			return s.addSpice(ctx, cancelCtx, s.firstSpice())
		},
		"open_door":  s.openDoor,  // grip the fridge handle and swing it open
		"close_door": s.closeDoor, // grip the open door's handle and swing it shut
	}

	if s.cfg.HotWaterLeverGripRelativePose != nil {
		actions["open_hot_water"] = s.openHotWater   // grip the lever and turn the valve open
		actions["close_hot_water"] = s.closeHotWater // grip the lever and turn the valve shut
	}

	// One grind action per extra grinder, named after its beans (grind_decaf).
	for _, beans := range s.cfg.beanNames() {
		actions[grindActionName(beans)] = func(ctx, cancelCtx context.Context) error {
			return s.grindBeans(ctx, cancelCtx, beans)
		}
	}

	// Only register the mug actions when this machine serves mug drinks: without
	// can_serve_mug_drinks the mug vision service and observe switch are nil.
	if s.cfg.CanServeMugDrinks {
		actions["pick_mug"] = s.pickMugDynamic                  // vision-grab a mug off the shelf
		actions["set_mug_for_coffee"] = s.setMugForCoffee       // vision-grab a mug and set it under the group head
		actions["grab_brewed_mug"] = s.grabBrewedMugFromMachine // retrieve mug from under machine
		actions["slide_mug_to_water"] = s.slideMugToWater       // carry the held mug from the group head to the spout
		actions["set_mug_at_water"] = s.setMugAtWater           // set the held mug down at the spout, back away
		actions["regrab_mug_at_water"] = s.regrabMugAtWater     // pick the staged mug back up
		actions["place_mug_at_water"] = s.placeMugAtWater       // vision-grab an empty mug and set it at the spout

		// hot water routine (stub for now)
		actions["dispense_hot_water"] = func(ctx, cancelCtx context.Context) error {
			return s.dispenseHotWater(ctx, cancelCtx, "americano")
		}
		// from a lungo brewed into a mug: grasp, top up with hot water, serve
		actions["serve_americano"] = func(ctx, cancelCtx context.Context) error {
			_, err := s.serveAmericano(ctx, cancelCtx, false, s.grabAndShelve, s.actionPhase(ctx, cancelCtx))
			return err
		}
		// place a mug at the spout, add hot water, serve
		actions["serve_hot_water"] = func(ctx, cancelCtx context.Context) error {
			_, err := s.serveHotWater(ctx, cancelCtx)
			return err
		}
	}

	// Measurement only — no arm motion, no pin. Reading the rim and surface rows
	// off a machine is how its stop row gets confirmed before anything is tuned
	// against it.
	if s.cfg.CanServeIced {
		actions["check_ice_level"] = s.checkIceLevel
	}

	// Only register the button actions this machine actually has, so the
	// unknown-action error lists a set the operator can really run.
	if s.cfg.HasSeparateBrewButtons {
		actions["press_espresso_button"] = s.pressEspressoButton
		actions["press_lungo_button"] = s.pressLungoButton
		actions["brew_lungo"] = s.brewLungo
		// Gated on the machine, not on keepalive, so the purge poses can be verified
		// before the loop is switched on — and never offered on the toggle machine,
		// where holding the switch pours an uncontrolled dose.
		actions["keepalive_purge"] = s.purge
	} else {
		actions["turn_coffee_button_on"] = s.turnCoffeeButtonOn
		actions["turn_coffee_button_off"] = s.turnCoffeeButtonOff
	}

	return actions
}

// executeAction runs one named action, holding the arm for its duration.
// withGlass takes the portafilter out of the frame system for the call and, when
// nothing is modeled in the gripper, puts a configured glass in its place — for
// stepping the iced sequence with the filter lifted off by hand. It is refused
// on any action outside withGlassActions.
func (s *beanjaminCoffee) executeAction(ctx context.Context, name string, withGlass bool) (map[string]any, error) {
	actions := s.actionFuncs()

	action, ok := actions[name]
	if !ok {
		names := make([]string, 0, len(actions))
		for k := range actions {
			names = append(names, k)
		}
		return nil, fmt.Errorf("unknown action %q, available actions: %v", name, names)
	}

	// Before the run gate, so a rejected flag costs no state.
	if withGlass {
		if err := checkWithGlassAllowed(name); err != nil {
			return nil, err
		}
	}

	cancelCtx, err := s.lease.claimManual("execute_action " + name)
	if err != nil {
		return nil, err
	}
	defer s.lease.release()
	defer s.startDirectRun(ctx, cancelCtx, "execute_action "+name)()

	// Pick up any out-of-band frame-system edits before planning. Guarded so a
	// held item or locked filter established by a prior action call (manual
	// step-by-step sequences span separate DoCommands) is preserved.
	if err := s.refreshFrameSystemIfClean(ctx); err != nil {
		return nil, fmt.Errorf("refresh frame system before action %q: %w", name, err)
	}
	// After the refresh, or it would be put straight back.
	if withGlass {
		restore, err := s.swapFilterForGlass(ctx)
		if err != nil {
			return nil, err
		}
		// Restore on every exit path: the next action's refresh is skipped while
		// anything is held, which would leave the swap in place for good.
		defer restore()
	}

	s.logger.Infof("executing action %q", name)

	if err := action(ctx, cancelCtx); err != nil {
		return nil, err
	}

	s.logger.Infof("action %q complete", name)
	return map[string]any{"status": "complete", "action": name}, nil
}
