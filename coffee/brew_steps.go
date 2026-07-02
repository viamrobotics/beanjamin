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
		{PoseName: approachPose, Component: componentFilter, Pause: shortPause},
		{PoseName: activatePose, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: approachPose, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: approachPose, Component: componentFilter,
			CircularRadiusMm: 8, CircularDurationSec: s.grindDurationSec(), CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		// Mark grounds only as we reach the activate pose: the approach move
		// keeps the filter clean, and the grinder dispenses once it's under the
		// chute. From here onward any cancel must clean the filter before home.
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
		Step{PoseName: filterPoseTamperApproach, Component: componentFilter, Pause: shortPause},
		Step{PoseName: filterPoseTamperActivate, Component: componentFilter, Pause: 3000 * time.Millisecond, LinearConstraint: defaultApproachConstraint},
		Step{PoseName: filterPoseTamperApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
	)
}

func (s *beanjaminCoffee) lockPortaFilter(ctx, cancelCtx context.Context) error {
	if err := s.runSteps(ctx, cancelCtx, "lock_portafilter",
		Step{PoseName: filterPoseCoffeeApproach, Component: componentFilter, Pause: shortPause},
		Step{PoseName: filterPoseCoffeeIn, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		Step{PoseName: filterPoseCoffeeLockedFinal, Component: componentFilter, PivotFromPose: filterPoseCoffeeIn, PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
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
	return s.runSteps(ctx, cancelCtx, "unlock_portafilter",
		Step{PoseName: filterPoseCoffeeIn, Component: componentFilter, PivotFromPose: filterPoseCoffeeLockedFinal, PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		Step{PoseName: filterPoseCoffeeShake, Component: componentFilter, AllowedCollisions: coffeeBrewingCollisions, LinearConstraint: defaultApproachConstraint},
		// Shake the filter laterally to dislodge the puck.
		Step{PoseName: filterPoseCoffeeShake, Component: componentFilter,
			CircularRadiusMm: 4, CircularDurationSec: s.cfg.PortafilterShakeSec, CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		Step{PoseName: filterPoseCoffeeApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
	)
}

func (s *beanjaminCoffee) releaseFilter(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("release_filter: no gripper configured")
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("release_filter: open gripper: %w", err)
	}
	// Bayonet now holds the filter; arm is committed to leaving it behind.
	// Set the flag before motion so a mid-move cancel still triggers recovery.
	s.portafilterInMachine.Store(true)
	step := Step{PoseName: clawPoseFilterReleased, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterGrabCollisions}
	if err := s.executeStep(ctx, cancelCtx, step); err != nil {
		return fmt.Errorf("release_filter: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("release_filter: grab gripper: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

func (s *beanjaminCoffee) grabFilter(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("grab_filter: no gripper configured")
	}

	approachStep := Step{PoseName: clawPoseFilterReleased, Component: componentClaws}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("grab_filter: %w", err)
	}

	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("grab_filter: open gripper: %w", err)
	}

	alignStep := Step{PoseName: clawPoseCoffeeLockedFinal, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterGrabCollisions}
	if err := s.executeStep(ctx, cancelCtx, alignStep); err != nil {
		return fmt.Errorf("grab_filter: %w", err)
	}

	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("grab_filter: grab gripper: %w", err)
	}
	// Filter is firmly back in the claws; cancel no longer needs to recover.
	s.portafilterInMachine.Store(false)
	time.Sleep(gripperPause)
	return nil
}

func (s *beanjaminCoffee) turnCoffeeButtonOn(ctx, cancelCtx context.Context) error {
	return s.runSteps(ctx, cancelCtx, "turn_coffee_button_on",
		Step{PoseName: clawPoseCoffeeButtonApproach, Component: componentClaws},
		Step{PoseName: clawPoseCoffeeButtonOn, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
	)
}

func (s *beanjaminCoffee) turnCoffeeButtonOff(ctx, cancelCtx context.Context) error {
	return s.runSteps(ctx, cancelCtx, "turn_coffee_button_off",
		Step{PoseName: clawPoseCoffeeButtonOff, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
		Step{PoseName: clawPoseCoffeeButtonApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
	)
}

// brewCoffee is the execute_action entry point — uses the espresso default brew time.
func (s *beanjaminCoffee) brewCoffee(ctx, cancelCtx context.Context) error {
	return s.brew(ctx, cancelCtx, s.drinkBrewTime("espresso"))
}

// brew presses the coffee button, waits for the given duration, then releases.
func (s *beanjaminCoffee) brew(ctx, cancelCtx context.Context, brewTime time.Duration) error {
	logger := s.activeOrderLogger()
	if err := s.turnCoffeeButtonOn(ctx, cancelCtx); err != nil {
		return fmt.Errorf("brew_coffee: %w", err)
	}
	logger.Infof("waiting %s for coffee to brew", brewTime)
	select {
	case <-time.After(brewTime):
	case <-ctx.Done():
		return fmt.Errorf("brew_coffee: cancelled during brew wait: %w", ctx.Err())
	case <-cancelCtx.Done():
		return fmt.Errorf("brew_coffee: cancelled during brew wait")
	}
	if err := s.turnCoffeeButtonOff(ctx, cancelCtx); err != nil {
		return fmt.Errorf("brew_coffee: %w", err)
	}
	return nil
}

// grindDurationSec returns the configured or default grind duration in seconds.
func (s *beanjaminCoffee) grindDurationSec() float64 {
	return orDefault(s.cfg.GrindTimeSec, defaultGrindTimeSec)
}

// drinkBrewTime returns the configured or default brew duration for the given drink.
func (s *beanjaminCoffee) drinkBrewTime(drink string) time.Duration {
	switch drink {
	case "lungo", "decaf_lungo":
		if s.cfg.LungoBrewTimeSec > 0 {
			return time.Duration(s.cfg.LungoBrewTimeSec * float64(time.Second))
		}
		return defaultLungoBrewTime
	default:
		if s.cfg.BrewTimeSec > 0 {
			return time.Duration(s.cfg.BrewTimeSec * float64(time.Second))
		}
		return defaultEspressoBrewTime
	}
}

func (s *beanjaminCoffee) cleanPortafilter(ctx, cancelCtx context.Context) error {
	if err := s.runSteps(ctx, cancelCtx, "clean_portafilter",
		Step{PoseName: filterPoseCloseToCleaning, Component: componentFilter},
		Step{PoseName: filterPoseApproachToCleaningScrapper, Component: componentFilter, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		Step{PoseName: filterPoseCleaningScrapperActive, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		Step{PoseName: filterPoseCleaningScrapperActive, Component: componentFilter, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		Step{PoseName: filterPoseApproachToCleaningScrapper, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		Step{PoseName: filterPoseApproachToCleaningBrush, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		Step{PoseName: filterPoseCleaningBrushActive, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		Step{PoseName: filterPoseCleaningBrushActive, Component: componentFilter, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		Step{PoseName: filterPoseApproachToCleaningBrush, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		Step{PoseName: filterPoseCloseToCleaning, Component: componentFilter, AllowedCollisions: cleaningCollisions, Pause: shortPause},
	); err != nil {
		return err
	}
	s.portafilterHasGrounds.Store(false)
	return nil
}
