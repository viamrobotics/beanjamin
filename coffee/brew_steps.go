package coffee

// The hot-espresso brew steps prepareDrink sequences through: grind, tamp,
// lock/unlock the portafilter, press the brew button, clean, and the brew-time
// helpers. Each is an execute_action target as well as a prepareDrink step.

import (
	"context"
	"fmt"
	"time"

	"beanjamin/coffee/order"
)

func (s *beanjaminCoffee) grindCoffee(ctx, cancelCtx context.Context) error {
	return s.grindBeans(ctx, cancelCtx, order.BeansRegular)
}

// grindActionName is the execute_action that grinds beans on its own:
// grind_coffee for the regular beans, grind_<beans> for the others.
func grindActionName(beans string) string {
	if beans == order.BeansRegular {
		return "grind_coffee"
	}
	return "grind_" + beans
}

// grindBeans approaches the grinder for beans (grinderPoses), presses its button,
// then circles at the collect pose under the chute to distribute grounds
// evenly while it dispenses.
func (s *beanjaminCoffee) grindBeans(ctx, cancelCtx context.Context, beans string) error {
	poses := grinderPoses(beans)
	label := grindActionName(beans)
	for _, step := range s.grindSteps(poses, grinderButtonCollisions(beans)) {
		// Mark grounds only as we reach the activate pose: the approach move
		// keeps the filter clean, and the grinder dispenses once it's under the
		// chute. From here onward the filter needs cleaning before it goes home.
		if step.PoseName == poses.activate {
			s.handOffEffect(func() { s.portafilterHasGrounds.Store(true) })
		}
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

// grindSteps builds the grind sequence: a free move to the approach pose, a
// straight press of the button at the activate pose, a straight move back to
// the collect pose under the chute, circles there while the grinder
// dispenses, and a straight move back out to the approach. Only the first move
// is free-planned, so the approach can sit further out from the row of
// grinders than the filter has to be to catch the grounds, and the next
// free-planned move (to the tamper) starts from out there too rather than
// sweeping the filter, full of grounds, along the neighbouring grinders.
// buttonCollisions covers only the linear moves: an allowance applies to the
// whole plan, so granting it on the free-planned approach would let the
// planner route the traverse through the button shield.
func (s *beanjaminCoffee) grindSteps(p grinderPoseNames, buttonCollisions []AllowedCollision) []Step {
	slow := s.slowMoveOptions()
	return []Step{
		{PoseName: p.approach, PoseSwitch: s.filterSw},
		{PoseName: p.activate, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint,
			AllowedCollisions: buttonCollisions, MoveOptions: slow},
		{PoseName: p.collect, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint,
			AllowedCollisions: buttonCollisions, MoveOptions: slow},
		{PoseName: p.collect, PoseSwitch: s.filterSw,
			CircularRadiusMm: 8, CircularDurationSec: s.grindDurationSec(), CircularPointsPerRev: 8,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: buttonCollisions, MoveOptions: slow},
		{PoseName: p.approach, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint,
			AllowedCollisions: buttonCollisions, MoveOptions: slow},
	}
}

func (s *beanjaminCoffee) tampGround(ctx, cancelCtx context.Context) error {
	return s.runSteps(ctx, cancelCtx, "tamp_ground", s.tampSteps()...)
}

// tampSteps builds the tamp sequence: a free move to the tamper's approach, a
// straight press, and a straight retreat.
func (s *beanjaminCoffee) tampSteps() []Step {
	return []Step{
		// Carries loose, un-tamped grounds to the tamper: slow it so they don't
		// scatter. Once tamped, the puck is compact, so the retreat runs at the
		// arm's default speed.
		{PoseName: filterPoseTamperApproach, PoseSwitch: s.filterSw, MoveOptions: s.slowMoveOptions()},
		{PoseName: filterPoseTamperActivate, PoseSwitch: s.filterSw, Pause: 3000 * time.Millisecond, LinearConstraint: defaultApproachConstraint,
			MoveOptions: s.slowMoveOptions()},
		{PoseName: filterPoseTamperApproach, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint},
	}
}

func (s *beanjaminCoffee) lockPortaFilter(ctx, cancelCtx context.Context) error {
	if err := s.runSteps(ctx, cancelCtx, "lock_portafilter", s.lockSteps()...); err != nil {
		return err
	}
	if err := s.lockFilterFrame(ctx); err != nil {
		return fmt.Errorf("lock filter frame: %w", err)
	}
	return nil
}

// lockSteps builds the lock sequence: a free move to the group head's
// approach, straight in, then the bayonet pivot.
func (s *beanjaminCoffee) lockSteps() []Step {
	return []Step{
		{PoseName: filterPoseCoffeeApproach, PoseSwitch: s.filterSw},
		{PoseName: filterPoseCoffeeIn, PoseSwitch: s.filterSw, Pause: shortPause, LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions,
			MoveOptions: s.slowMoveOptions()},
		{PoseName: filterPoseCoffeeLockedFinal, PoseSwitch: s.filterSw, PivotFromPose: filterPoseCoffeeIn, PivotDegreesPerStep: 5,
			PivotExtraDegrees: s.cfg.LockOvershootDegs,
			LinearConstraint:  defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions, MoveOptions: s.slowMoveOptions()},
	}
}

func (s *beanjaminCoffee) unlockPortaFilter(ctx, cancelCtx context.Context) error {
	if err := s.unlockFilterFrame(ctx); err != nil {
		return fmt.Errorf("unlock filter frame: %w", err)
	}
	slow := s.slowMoveOptions()
	steps := []Step{
		{PoseName: filterPoseCoffeeIn, PoseSwitch: s.filterSw, PivotFromPose: filterPoseCoffeeLockedFinal, PivotDegreesPerStep: 5,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions, MoveOptions: slow},
	}
	withdraw := Step{PoseName: filterPoseCoffeeApproach, PoseSwitch: s.filterSw,
		LinearConstraint: defaultApproachConstraint, MoveOptions: slow}
	if s.cfg.PortafilterShakeSec > 0 {
		// Shake once at each pose. The two lean opposite ways, so a puck stuck
		// on either side of the basket gets worked from both.
		for i, shakePose := range []string{filterPoseCoffeeShake, filterPoseCoffeeShakeLeft} {
			if i > 0 {
				// Every shake starts from coffee_in, so each one runs the same move.
				steps = append(steps, Step{PoseName: filterPoseCoffeeIn, PoseSwitch: s.filterSw,
					AllowedCollisions: coffeeBrewingCollisions, LinearConstraint: defaultApproachConstraint, MoveOptions: slow})
			}
			steps = append(steps,
				Step{PoseName: shakePose, PoseSwitch: s.filterSw,
					AllowedCollisions: coffeeBrewingCollisions, LinearConstraint: defaultApproachConstraint, MoveOptions: slow},
				// Shake the filter laterally to dislodge the puck.
				Step{PoseName: shakePose, PoseSwitch: s.filterSw,
					CircularRadiusMm: 1, CircularDurationSec: s.cfg.PortafilterShakeSec, CircularPointsPerRev: 8,
					LinearConstraint: defaultApproachConstraint, AllowedCollisions: coffeeBrewingCollisions, MoveOptions: slow},
			)
		}
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
	step := Step{PoseName: poseName, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterGrabCollisions,
		MoveOptions: s.slowMoveOptions()}
	if err := s.executeStep(ctx, cancelCtx, step); err != nil {
		return err
	}
	if err := s.closeGripper(ctx); err != nil {
		return err
	}
	return nil
}

func (s *beanjaminCoffee) releaseFilter(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("release_filter: no gripper configured")
	}
	// Bayonet now holds the filter; arm is committed to leaving it behind.
	// Set the flag before motion so a mid-move cancel still reports it.
	s.handOffEffect(func() { s.portafilterInMachine.Store(true) })
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
	// Filter is firmly back in the claws, out of the machine.
	s.handOffEffect(func() { s.portafilterInMachine.Store(false) })
	return nil
}

// turnCoffeeButtonOn / turnCoffeeButtonOff drive the single-toggle machine:
// the claw holds the switch down for the whole pour and the hold duration is
// the dose. Only reachable when has_separate_brew_buttons is false.
func (s *beanjaminCoffee) turnCoffeeButtonOn(ctx, cancelCtx context.Context) error {
	return s.runSteps(ctx, cancelCtx, "turn_coffee_button_on",
		Step{PoseName: clawPoseCoffeeButtonApproach, PoseSwitch: s.clawsSw},
		Step{PoseName: clawPoseCoffeeButtonOn, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions,
			MoveOptions: s.slowMoveOptions()},
	)
}

func (s *beanjaminCoffee) turnCoffeeButtonOff(ctx, cancelCtx context.Context) error {
	return s.runSteps(ctx, cancelCtx, "turn_coffee_button_off",
		Step{PoseName: clawPoseCoffeeButtonOff, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions,
			MoveOptions: s.slowMoveOptions()},
		Step{PoseName: clawPoseCoffeeButtonApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions,
			MoveOptions: s.slowMoveOptions()},
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
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions, MoveOptions: s.slowMoveOptions()},
		{PoseName: approachPose, PoseSwitch: s.clawsSw,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: clawCoffeeButtonCollisions, MoveOptions: s.slowMoveOptions()},
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
		if order.IsLungo(drink) {
			press = s.pressLungoButton
		}
		if err := press(ctx, cancelCtx); err != nil {
			return fmt.Errorf("brew_coffee: %w", err)
		}
	} else if err := s.turnCoffeeButtonOn(ctx, cancelCtx); err != nil {
		return fmt.Errorf("brew_coffee: %w", err)
	}

	brewTime := s.drinkBrewTime(drink)
	logger.Infof("waiting %s for the %s brew to finish", brewTime, drink)
	brewCtx, done := mergedCancelContext(ctx, cancelCtx) // cancel stops the wait
	defer done()
	if err := s.handOff(brewCtx, move{name: "brew", sleep: brewTime}); err != nil {
		return fmt.Errorf("brew_coffee: %w", err)
	}

	// A poked button self-terminates; only the toggle needs releasing.
	if !buttons {
		if err := s.turnCoffeeButtonOff(ctx, cancelCtx); err != nil {
			return fmt.Errorf("brew_coffee: %w", err)
		}
	}

	// Water has run, so the keep-alive clock restarts (keepalive.go). Here rather
	// than in prepareDrink so it covers every path that pours, including a
	// hand-driven brew_coffee action.
	s.recordMachineActivity()
	return nil
}

// waitOutBrew blocks until d elapses or either context is cancelled.
// Non-positive durations return immediately, so overlapped callers can pass
// brewTime - elapsed unclamped.
func waitOutBrew(ctx, cancelCtx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("cancelled during brew wait: %w", ctx.Err())
	case <-cancelCtx.Done():
		return fmt.Errorf("cancelled during brew wait")
	}
}

// grindDurationSec returns the configured or default grind duration in seconds.
func (s *beanjaminCoffee) grindDurationSec() float64 {
	return orDefault(s.cfg.GrindTimeSec, defaultGrindTimeSec)
}

// buttonPressHold is how long the claw dwells on a brew button. A momentary
// switch needs real contact time to register, and how much varies with button
// travel and how the claw is shimmed — hence the config knob.
func (s *beanjaminCoffee) buttonPressHold() time.Duration {
	return time.Duration(s.cfg.ButtonPressHoldSec * float64(time.Second))
}

// drinkBrewTime returns how long to wait for the machine to finish pouring the
// given drink — the configured value, or the default for that shot size.
func (s *beanjaminCoffee) drinkBrewTime(drink string) time.Duration {
	configured, def := s.cfg.BrewTimeSec, defaultEspressoBrewTime
	if order.IsLungo(drink) {
		configured, def = s.cfg.LungoBrewTimeSec, defaultLungoBrewTime
	}
	return time.Duration(orDefault(configured, def.Seconds()) * float64(time.Second))
}

func (s *beanjaminCoffee) cleanPortafilter(ctx, cancelCtx context.Context) error {
	if err := s.runSteps(ctx, cancelCtx, "clean_portafilter", s.cleanSteps()...); err != nil {
		return err
	}
	s.handOffEffect(func() { s.portafilterHasGrounds.Store(false) })
	return nil
}

// cleanSteps builds the cleaning sequence: knock the puck out on the scraper,
// brush the basket, and back off.
func (s *beanjaminCoffee) cleanSteps() []Step {
	slow := s.slowMoveOptions()
	return []Step{
		{PoseName: filterPoseCloseToCleaning, PoseSwitch: s.filterSw},
		{PoseName: filterPoseApproachToCleaningScrapper, PoseSwitch: s.filterSw, AllowedCollisions: cleaningCollisions},
		{PoseName: filterPoseCleaningScrapperActive, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, MoveOptions: slow},
		{PoseName: filterPoseCleaningScrapperActive, PoseSwitch: s.filterSw, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8, MoveOptions: slow},
		{PoseName: filterPoseApproachToCleaningScrapper, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, MoveOptions: slow},
		// Scraper to brush is a transfer with no contact, so it runs at the arm's default speed.
		{PoseName: filterPoseApproachToCleaningBrush, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions},
		{PoseName: filterPoseCleaningBrushActive, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, MoveOptions: slow},
		{PoseName: filterPoseCleaningBrushActive, PoseSwitch: s.filterSw, AllowedCollisions: cleaningCollisions, CircularRadiusMm: 3, CircularDurationSec: 2.5, CircularPointsPerRev: 8, MoveOptions: slow},
		{PoseName: filterPoseApproachToCleaningBrush, PoseSwitch: s.filterSw, LinearConstraint: defaultApproachConstraint, AllowedCollisions: cleaningCollisions, MoveOptions: slow},
		{PoseName: filterPoseCloseToCleaning, PoseSwitch: s.filterSw, AllowedCollisions: cleaningCollisions},
	}
}

const (
	defaultEspressoBrewTime = 8 * time.Second
	defaultLungoBrewTime    = 15 * time.Second
	defaultGrindTimeSec     = 7.5
	// defaultIceDispenseSec is how long the ice pin is held HIGH when
	// ice_dispense_sec is unset.
	defaultIceDispenseSec = 5.0
)
