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
	steps := []Step{
		{PoseName: filterPoseGrinderApproach, Component: componentFilter, Pause: shortPause},
		{PoseName: filterPoseGrinderActivate, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseGrinderApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		// Circle under the grinder chute to distribute grounds evenly while the grinder dispenses.
		{PoseName: filterPoseGrinderApproach, Component: componentFilter,
			CircularRadiusMm: 8, CircularDurationSec: s.grindDurationSec(), CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		// Mark grounds only as we reach the activate pose: the approach move
		// keeps the filter clean, and the grinder dispenses once it's under the
		// chute. From here onward any cancel must clean the filter before home.
		if step.PoseName == filterPoseGrinderActivate {
			s.portafilterHasGrounds.Store(true)
		}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("grind_coffee: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) grindDecaf(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: filterPoseDecafGrinderApproach, Component: componentFilter, Pause: shortPause},
		{PoseName: filterPoseDecafGrinderActivate, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseDecafGrinderApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
		// Circle under the decaf grinder chute to distribute grounds evenly while the grinder dispenses.
		{PoseName: filterPoseDecafGrinderApproach, Component: componentFilter,
			CircularRadiusMm: 8, CircularDurationSec: s.grindDurationSec(), CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		// Mark grounds only as we reach the activate pose: the approach move
		// keeps the filter clean, and the grinder dispenses once it's under the
		// chute. From here onward any cancel must clean the filter before home.
		if step.PoseName == filterPoseDecafGrinderActivate {
			s.portafilterHasGrounds.Store(true)
		}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("grind_decaf: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) tampGround(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: filterPoseTamperApproach, Component: componentFilter, Pause: shortPause},
		{PoseName: filterPoseTamperActivate, Component: componentFilter, Pause: 3000 * time.Millisecond, LinearConstraint: defaultApproachConstraint},
		{PoseName: filterPoseTamperApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("tamp_ground: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) lockPortaFilter(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: filterPoseCoffeeApproach, Component: componentFilter, Pause: shortPause},
		{PoseName: filterPoseCoffeeIn, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: filterPoseCoffeeLockedFinal, Component: componentFilter, PivotFromPose: filterPoseCoffeeIn, PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("lock_portafilter: %w", err)
		}
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
		{PoseName: filterPoseCoffeeIn, Component: componentFilter, PivotFromPose: filterPoseCoffeeLockedFinal, PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: filterPoseCoffeeShake, Component: componentFilter, AllowedCollisions: coffeeBrewingCollisions, LinearConstraint: defaultApproachConstraint},
		// Shake the filter laterally to dislodge the puck.
		{PoseName: filterPoseCoffeeShake, Component: componentFilter,
			CircularRadiusMm: 4, CircularDurationSec: s.cfg.PortafilterShakeSec, CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions},
		{PoseName: filterPoseCoffeeApproach, Component: componentFilter, Pause: shortPause, LinearConstraint: defaultApproachConstraint},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("unlock_portafilter: %w", err)
		}
	}
	return nil
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
	steps := []Step{
		{PoseName: clawPoseCoffeeButtonApproach, Component: componentClaws},
		{PoseName: clawPoseCoffeeButtonOn, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("turn_coffee_button_on: %w", err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) turnCoffeeButtonOff(ctx, cancelCtx context.Context) error {
	steps := []Step{
		{PoseName: clawPoseCoffeeButtonOff, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
		{PoseName: clawPoseCoffeeButtonApproach, Component: componentClaws, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("turn_coffee_button_off: %w", err)
		}
	}
	return nil
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
	if s.cfg.GrindTimeSec > 0 {
		return s.cfg.GrindTimeSec
	}
	return defaultGrindTimeSec
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
	steps := []Step{
		{PoseName: filterPoseCloseToCleaning, Component: componentFilter},
		{PoseName: filterPoseApproachToCleaningScrapper, Component: componentFilter, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: filterPoseCleaningScrapperActive, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		{PoseName: filterPoseCleaningScrapperActive, Component: componentFilter, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		{PoseName: filterPoseApproachToCleaningScrapper, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: filterPoseApproachToCleaningBrush, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: filterPoseCleaningBrushActive, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		{PoseName: filterPoseCleaningBrushActive, Component: componentFilter, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8},
		{PoseName: filterPoseApproachToCleaningBrush, Component: componentFilter, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, Pause: shortPause},
		{PoseName: filterPoseCloseToCleaning, Component: componentFilter, AllowedCollisions: cleaningCollisions, Pause: shortPause},
	}
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("clean_portafilter: %w", err)
		}
	}
	s.portafilterHasGrounds.Store(false)
	return nil
}
