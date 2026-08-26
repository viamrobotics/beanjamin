package coffee

// The hot-espresso brew steps prepareDrink sequences through: grind, tamp,
// lock/unlock the portafilter, press the brew button, clean, and the brew-time
// helpers. Each is an execute_action target as well as a prepareDrink step.

import (
	"context"
	"fmt"
	"time"
)

func (s *beanjaminCoffee) grindCoffee(ctx, cancelCtx context.Context) error {
	return s.grind(ctx, cancelCtx, filterPoseGrinderApproach, filterPoseGrinderActivate, "grind_coffee")
}

func (s *beanjaminCoffee) grindDecaf(ctx, cancelCtx context.Context) error {
	return s.grind(ctx, cancelCtx, filterPoseDecafGrinderApproach, filterPoseDecafGrinderActivate, "grind_decaf")
}

// grind approaches a grinder chute, circles under it to distribute grounds
// evenly while the grinder dispenses, then returns to the approach pose. The
// approach and activate poses select which grinder (regular vs decaf); label
// identifies the phase in wrapped errors.
func (s *beanjaminCoffee) grind(ctx, cancelCtx context.Context, approachPose, activatePose, label string) error {
	steps := []Step{
		{PoseName: approachPose, PoseSwitch: s.filterSw, Pause: shortPause},
		{PoseName: activatePose, PoseSwitch: s.filterSw, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: approachPose, PoseSwitch: s.filterSw, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: approachPose, PoseSwitch: s.filterSw,
			CircularRadiusMm: 8, CircularDurationSec: s.grindDurationSec(), CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		// Mark grounds only as we reach the activate pose: the approach move
		// keeps the filter clean, and the grinder dispenses once it's under the
		// chute. From here onward a rewind must clean the filter before home.
		if step.PoseName == activatePose {
			s.portafilterHasGrounds.Store(true)
		}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) tampGround(ctx, cancelCtx context.Context) error {
	return s.runSteps(ctx, cancelCtx, "tamp_ground",
		Step{PoseName: filterPoseTamperApproach, PoseSwitch: s.filterSw, Pause: shortPause},
		Step{PoseName: filterPoseTamperActivate, PoseSwitch: s.filterSw, Pause: 3000 * time.Millisecond, LinearConstraint: defaultApproachConstraint},
		Step{PoseName: filterPoseTamperApproach, PoseSwitch: s.filterSw, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
	)
}

func (s *beanjaminCoffee) lockPortaFilter(ctx, cancelCtx context.Context) error {
	if err := s.runSteps(ctx, cancelCtx, "lock_portafilter",
		Step{PoseName: filterPoseCoffeeApproach, PoseSwitch: s.filterSw, Pause: shortPause},
		Step{PoseName: filterPoseCoffeeIn, PoseSwitch: s.filterSw, Pause: shortPause, LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		Step{PoseName: filterPoseCoffeeLockedFinal, PoseSwitch: s.filterSw, PivotFromPose: filterPoseCoffeeIn, PivotDegreesPerStep: 5,
			PivotExtraDegrees: s.cfg.LockOvershootDegs,
			LinearConstraint:  defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
	); err != nil {
		return err
	}
	if err := s.lockFilterFrame(ctx); err != nil {
		return fmt.Errorf("lock filter frame: %w", err)
	}
	return nil
}

func (s *beanjaminCoffee) unlockPortaFilter(ctx, cancelCtx context.Context) error {
	if err := s.unlockFilterFrame(ctx); err != nil {
		return fmt.Errorf("unlock filter frame: %w", err)
	}
	steps := []Step{
		{PoseName: filterPoseCoffeeIn, PoseSwitch: s.filterSw, PivotFromPose: filterPoseCoffeeLockedFinal, PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
	}
	withdraw := Step{PoseName: filterPoseCoffeeApproach, PoseSwitch: s.filterSw, Pause: shortPause,
		LinearConstraint: defaultApproachConstraint}
	if s.cfg.PortafilterShakeSec > 0 {
		steps = append(steps,
			Step{PoseName: filterPoseCoffeeShake, PoseSwitch: s.filterSw, AllowedCollisions: coffeeBrewingCollisions, LinearConstraint: defaultApproachConstraint},
			// Shake the filter laterally to dislodge the puck.
			Step{PoseName: filterPoseCoffeeShake, PoseSwitch: s.filterSw,
				CircularRadiusMm: 2, CircularDurationSec: s.cfg.PortafilterShakeSec, CircularPointsPerRev: 8,
				LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		)
	} else {
		// Withdrawing straight from coffee_in holds the filter inside the
		// actuation area for the first part of the move.
		withdraw.AllowedCollisions = coffeeBrewingCollisions
	}
	return s.runSteps(ctx, cancelCtx, "unlock_portafilter", append(steps, withdraw)...)
}

// moveGripperToPoseWithVerify is the portafilter handoff both release_filter and
// grab_filter perform: open the jaws and wait until they actually read open, move
// the claws linearly to poseName, then close on whatever is there.
//
// The wait is the point. Both moves travel along the filter handle at claw
// clearance, so jaws that have not finished opening drag the filter against the
// bayonet on the way out (release) or strike the handle on the way in (grab).
// Jaw travel scales with clamp force and jaw range, so it cannot be a fixed sleep
// tuned on one build — openAndVerifyOpen polls the actual position and fails loudly
// if the jaws never get there. Callers own the portafilterInMachine flag, whose
// ordering around this call differs by direction.
func (s *beanjaminCoffee) moveGripperToPoseWithVerify(ctx, cancelCtx context.Context, poseName string) error {
	if err := s.openAndVerifyOpen(ctx); err != nil {
		return fmt.Errorf("open gripper: %w", err)
	}
	step := Step{PoseName: poseName, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterGrabCollisions}
	if err := s.executeStep(ctx, cancelCtx, step); err != nil {
		return err
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("grab gripper: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

func (s *beanjaminCoffee) releaseFilter(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("release_filter: no gripper configured")
	}
	// Bayonet now holds the filter; arm is committed to leaving it behind.
	// Set the flag before motion so a rewind after a mid-move cancel recovers.
	s.portafilterInMachine.Store(true)
	if err := s.moveGripperToPoseWithVerify(ctx, cancelCtx, clawPoseFilterReleased); err != nil {
		return fmt.Errorf("release_filter: %w", err)
	}
	return nil
}

func (s *beanjaminCoffee) grabFilter(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("grab_filter: no gripper configured")
	}

	approachStep := Step{PoseName: clawPoseFilterReleased, PoseSwitch: s.clawsSw}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("grab_filter: %w", err)
	}

	if err := s.moveGripperToPoseWithVerify(ctx, cancelCtx, clawPoseCoffeeLockedFinal); err != nil {
		return fmt.Errorf("grab_filter: %w", err)
	}
	// Filter is firmly back in the claws; rewind no longer needs to recover.
	s.portafilterInMachine.Store(false)
	return nil
}

// turnCoffeeButtonOn / turnCoffeeButtonOff drive the single-toggle machine:
// the claw holds the switch down for the whole pour and the hold duration is
// the dose. Only reachable when has_separate_brew_buttons is false.
func (s *beanjaminCoffee) turnCoffeeButtonOn(ctx, cancelCtx context.Context) error {
	return s.runSteps(ctx, cancelCtx, "turn_coffee_button_on",
		Step{PoseName: clawPoseCoffeeButtonApproach, PoseSwitch: s.clawsSw},
		Step{PoseName: clawPoseCoffeeButtonOn, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
	)
}

func (s *beanjaminCoffee) turnCoffeeButtonOff(ctx, cancelCtx context.Context) error {
	return s.runSteps(ctx, cancelCtx, "turn_coffee_button_off",
		Step{PoseName: clawPoseCoffeeButtonOff, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
		Step{PoseName: clawPoseCoffeeButtonApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
	)
}

func (s *beanjaminCoffee) pressEspressoButton(ctx, cancelCtx context.Context) error {
	return s.pressBrewButton(ctx, cancelCtx, clawPoseEspressoButtonApproach, clawPoseEspressoButtonPress, "press_espresso_button")
}

func (s *beanjaminCoffee) pressLungoButton(ctx, cancelCtx context.Context) error {
	return s.pressBrewButton(ctx, cancelCtx, clawPoseLungoButtonApproach, clawPoseLungoButtonPress, "press_lungo_button")
}

// brewButtonSteps is the three-move poke of one momentary brew button: free-plan
// into the button's own standoff, straight in and dwell long enough for the press
// to register, straight back out.
//
// Only the two linear moves carry clawCoffeeButtonCollisions. The buttons sit on
// the machine face behind coffee-machine-buffer-front, so the claw is inside that
// obstacle for the press and the retreat and the planner rejects them without the
// allowance.
//
// The approach must not carry it. An allowed-collision entry is a property of the
// whole plan, not of the neighbourhood of its goal — buildConstraints hands it to
// the planner as a CollisionSpecification covering the entire trajectory. The
// approach is free-planned from wherever the arm happens to be, which mid-brew is
// cup_under_machine_approach, under the machine; allowing coffee-machine-buffer-front
// over that whole traverse lets the planner route straight up through the machine's
// front face, and the claw hits the machine. Free-planned without the allowance it
// plans fine, and turn_coffee_button_on takes the same shape for the same reason.
func (s *beanjaminCoffee) brewButtonSteps(approachPose, pressPose string) []Step {
	return []Step{
		{PoseName: approachPose, PoseSwitch: s.clawsSw},
		{PoseName: pressPose, PoseSwitch: s.clawsSw, Pause: s.buttonPressHold(),
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
		{PoseName: approachPose, PoseSwitch: s.clawsSw,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
	}
}

// pressBrewButton pokes one of the machine's two momentary brew buttons and steps
// clear. The machine doses on its own from there, so there is nothing to release
// afterwards — the arm must clear the button before the pour finishes, not stay
// parked on it.
func (s *beanjaminCoffee) pressBrewButton(ctx, cancelCtx context.Context, approachPose, pressPose, label string) error {
	return s.runSteps(ctx, cancelCtx, label, s.brewButtonSteps(approachPose, pressPose)...)
}

// brewCoffee / brewLungo are the execute_action entry points for each shot size.
func (s *beanjaminCoffee) brewCoffee(ctx, cancelCtx context.Context) error {
	return s.brew(ctx, cancelCtx, "espresso")
}

func (s *beanjaminCoffee) brewLungo(ctx, cancelCtx context.Context) error {
	return s.brew(ctx, cancelCtx, "lungo")
}

// brew starts the pour for the drink's shot size and waits it out. The two
// machine styles differ in how the pour starts and stops, but share the
// cancellable wait:
//
//   - separate buttons: poke the button for this shot size and step clear. The
//     machine decides the dose, so the wait only has to outlast its pour.
//   - single toggle: hold the switch down for the whole wait, then release —
//     here the wait *is* the dose.
func (s *beanjaminCoffee) brew(ctx, cancelCtx context.Context, drink string) error {
	logger := s.activeOrderLogger()
	buttons := s.cfg.HasSeparateBrewButtons

	if buttons {
		press := s.pressEspressoButton
		if isLungoDrink(drink) {
			press = s.pressLungoButton
		}
		if err := press(ctx, cancelCtx); err != nil {
			return fmt.Errorf("brew_coffee: %w", err)
		}
	} else if err := s.turnCoffeeButtonOn(ctx, cancelCtx); err != nil {
		return fmt.Errorf("brew_coffee: %w", err)
	}

	brewTime := s.drinkBrewTime(drink)
	logger.Infof("waiting %s for the %s pour to finish", brewTime, drink)
	select {
	case <-time.After(brewTime):
	case <-ctx.Done():
		return fmt.Errorf("brew_coffee: cancelled during brew wait: %w", ctx.Err())
	case <-cancelCtx.Done():
		return fmt.Errorf("brew_coffee: cancelled during brew wait")
	}

	// A poked button self-terminates; only the toggle needs releasing.
	if !buttons {
		if err := s.turnCoffeeButtonOff(ctx, cancelCtx); err != nil {
			return fmt.Errorf("brew_coffee: %w", err)
		}
	}
	return nil
}

// grindDurationSec returns the configured or default grind duration in seconds.
func (s *beanjaminCoffee) grindDurationSec() float64 {
	return orDefault(s.cfg.GrindTimeSec, defaultGrindTimeSec)
}

// buttonPressHold is how long the claw dwells on a brew button. A momentary
// switch needs real contact time to register, and how much varies with button
// travel and how the claw is shimmed — hence the config knob.
func (s *beanjaminCoffee) buttonPressHold() time.Duration {
	return time.Duration(orDefault(s.cfg.ButtonPressHoldSec, defaultButtonPressHoldSec) * float64(time.Second))
}

// drinkBrewTime returns how long to wait for the machine to finish pouring the
// given drink — the configured value, or the default for that shot size.
func (s *beanjaminCoffee) drinkBrewTime(drink string) time.Duration {
	if isLungoDrink(drink) {
		if s.cfg.LungoBrewTimeSec > 0 {
			return time.Duration(s.cfg.LungoBrewTimeSec * float64(time.Second))
		}
		return defaultLungoBrewTime
	}
	if s.cfg.BrewTimeSec > 0 {
		return time.Duration(s.cfg.BrewTimeSec * float64(time.Second))
	}
	return defaultEspressoBrewTime
}

func (s *beanjaminCoffee) cleanPortafilter(ctx, cancelCtx context.Context) error {
	if err := s.runSteps(ctx, cancelCtx, "clean_portafilter",
		Step{PoseName: filterPoseCloseToCleaning, PoseSwitch: s.filterSw},
		Step{PoseName: filterPoseApproachToCleaningScrapper, PoseSwitch: s.filterSw, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		Step{PoseName: filterPoseCleaningScrapperActive, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		Step{PoseName: filterPoseCleaningScrapperActive, PoseSwitch: s.filterSw, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		Step{PoseName: filterPoseApproachToCleaningScrapper, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		Step{PoseName: filterPoseApproachToCleaningBrush, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		Step{PoseName: filterPoseCleaningBrushActive, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		Step{PoseName: filterPoseCleaningBrushActive, PoseSwitch: s.filterSw, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		Step{PoseName: filterPoseApproachToCleaningBrush, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		Step{PoseName: filterPoseCloseToCleaning, PoseSwitch: s.filterSw, AllowedCollisions: cleaningCollisions, Pause: shortPause},
	); err != nil {
		return err
	}
	s.portafilterHasGrounds.Store(false)
	return nil
}
