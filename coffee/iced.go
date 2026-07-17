package coffee

// The iced-coffee path: fetch a glass, dispense ice into it, stage it, grab the
// brewed espresso, pour it over the ice, and serve. Gated by can_serve_iced.

import (
	"context"
	"fmt"
	"time"
)

// serveIcedCoffee finishes an iced_coffee order after the espresso has brewed
// into the cup under the machine. It fetches a separate glass, dispenses ice
// into it via the board pin, sets the glass down in the staging area, retrieves
// the espresso cup, and pours the espresso over the ice. Both finished items
// then go into the serving area at the next round-robin slots: the empty
// espresso cup first, then the iced glass (re-grabbed from staging).
func (s *beanjaminCoffee) serveIcedCoffee(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("serve_iced_coffee: no gripper configured")
	}
	if s.iceBoard == nil {
		return fmt.Errorf("serve_iced_coffee: no ice board configured (set ice_board_name)")
	}

	// 1. Fetch the glass off the top shelf.
	if err := s.fetchGlass(ctx, cancelCtx); err != nil {
		return err
	}
	// 2. Carry the glass to the ice machine and dispense ice.
	if err := s.dispenseIce(ctx, cancelCtx); err != nil {
		return err
	}
	// 3. Set the glass down in the staging area to free the gripper.
	if err := s.stageGlass(ctx, cancelCtx); err != nil {
		return err
	}
	// 4. Retrieve the brewed espresso cup from the machine.
	if err := s.grabBrewedCupFromMachine(ctx, cancelCtx); err != nil {
		return err
	}
	// 5. Pour the espresso over the ice in the staged glass.
	if err := s.pourEspresso(ctx, cancelCtx); err != nil {
		return err
	}
	// 6. Place the now-empty espresso cup in the serving area (round-robin).
	if err := s.placeHeldInServingArea(ctx, cancelCtx); err != nil {
		return err
	}
	// 7. Re-grab the iced glass from the staging area.
	if err := s.grabStagedGlass(ctx, cancelCtx); err != nil {
		return err
	}
	// 8. Place the iced glass in the serving area (next round-robin slot).
	return s.placeHeldInServingArea(ctx, cancelCtx)
}

// grabBrewedCupFromMachine retrieves the brewed cup from under the machine:
// approach -> open gripper -> linear descent + grab -> linear retreat. On return
// the cup is held by the gripper and the arm sits at cup_under_machine_approach.
func (s *beanjaminCoffee) grabBrewedCupFromMachine(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: no gripper configured")
	}
	approachStep := Step{PoseName: clawPoseCupUnderMachineApproach, PoseSwitch: s.clawsSw, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: %w", err)
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: open gripper: %w", err)
	}
	time.Sleep(gripperPause)
	grabStep := Step{PoseName: clawPoseCupReadyForCoffee, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: %w", err)
	}
	if err := s.grabAndVerifyHolding(ctx); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: grab gripper: %w", err)
	}
	// The cup was tracked at pickup and released under the machine; restore its
	// geometry now that it's back in the gripper so the retreat routes around it.
	// grabAndVerifyHolding only returns nil on a confirmed grab, so this never
	// reattaches onto empty jaws.
	if err := s.reattachGeometry(pickupLabelCup); err != nil {
		s.activeOrderLogger().Warnf("grab_brewed_cup_from_machine: reattach cup geometry failed, continuing untracked: %v", err)
	}
	retreatStep := Step{PoseName: clawPoseCupUnderMachineApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause, AllowedCollisions: s.heldItemSurfaceCollisions(heldItemMachineCollisions)}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("grab_brewed_cup_from_machine: %w", err)
	}
	return nil
}

// grabStagedGlass picks the iced glass back up from the staging area: approach
// -> open -> linear descent + grab -> linear retreat, leaving the glass held by
// the gripper. The reverse of stageGlass.
func (s *beanjaminCoffee) grabStagedGlass(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("grab_staged_glass: no gripper configured")
	}
	approachStep := Step{PoseName: clawPoseStagingApproach, PoseSwitch: s.clawsSw, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("grab_staged_glass: %w", err)
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("grab_staged_glass: open gripper: %w", err)
	}
	time.Sleep(gripperPause)
	// Descend onto the glass; it stays a world obstacle, but allow the jaws to
	// contact it for this step (the rest of the arm still routes around it).
	grabStep := Step{PoseName: clawPoseStaging, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause, AllowedCollisions: s.stagedGlassGrabCollisions()}
	if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
		return fmt.Errorf("grab_staged_glass: %w", err)
	}
	if err := s.grabAndVerifyHolding(ctx); err != nil {
		return fmt.Errorf("grab_staged_glass: grab gripper: %w", err)
	}
	// Glass is back in the gripper: drop the world obstacle, then restore it as a
	// held item (obstacle first, so the held glass doesn't collide with its double).
	s.removeStagedGlassObstacle()
	if err := s.reattachGeometry(pickupLabelGlass); err != nil {
		s.activeOrderLogger().Warnf("grab_staged_glass: reattach glass geometry failed, continuing untracked: %v", err)
	}
	retreatStep := Step{PoseName: clawPoseStagingApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("grab_staged_glass: %w", err)
	}
	return nil
}

// fetchGlass vision-detects an iced-coffee glass off the top shelf and grabs it,
// leaving it held by the gripper (see pickGlassDynamic). Iced coffee always uses
// vision glass pickup — there is no static fallback (can_serve_iced requires
// glass_vision_service_name and glass_observe_pose_switcher_name).
func (s *beanjaminCoffee) fetchGlass(ctx, cancelCtx context.Context) error {
	if err := s.pickGlassDynamic(ctx, cancelCtx); err != nil {
		return fmt.Errorf("fetch_glass: %w", err)
	}
	return nil
}

// dispenseIce carries the held glass to the ice machine, holds it under the
// chute, pulses the ice pin HIGH for iceDispenseSec, then retreats. The pin is
// always driven back LOW — including on cancel — so the ice machine can't be
// left running.
func (s *beanjaminCoffee) dispenseIce(ctx, cancelCtx context.Context) error {
	approachStep := Step{PoseName: clawPoseIceMachineApproach, PoseSwitch: s.clawsSw, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("dispense_ice: %w", err)
	}
	dispenseStep := Step{PoseName: clawPoseIceMachineDispense, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, dispenseStep); err != nil {
		return fmt.Errorf("dispense_ice: %w", err)
	}

	if err := s.pulseIcePin(ctx, cancelCtx); err != nil {
		return fmt.Errorf("dispense_ice: %w", err)
	}
	s.incrementSensorReading(ctx, s.usageSensor, "ice machine", "ice_dispenses", 1)

	retreatStep := Step{PoseName: clawPoseIceMachineApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
		return fmt.Errorf("dispense_ice: %w", err)
	}
	return nil
}

func (s *beanjaminCoffee) pulseIcePin(ctx, cancelCtx context.Context) error {
	if s.iceBoard == nil {
		return fmt.Errorf("pulse_ice_pin: no ice board configured (set ice_board_name)")
	}
	logger := s.activeOrderLogger()
	pinName := s.icePinName()
	pin, err := s.iceBoard.GPIOPinByName(pinName)
	if err != nil {
		return fmt.Errorf("pulse_ice_pin: get pin %q: %w", pinName, err)
	}
	dwell := time.Duration(s.iceDispenseSec() * float64(time.Second))
	logger.Infof("dispensing ice: pin %q HIGH for %s", pinName, dwell)
	if err := pin.Set(ctx, true, nil); err != nil {
		return fmt.Errorf("pulse_ice_pin: set pin %q high: %w", pinName, err)
	}
	// Drive the pin LOW with a fresh context so the write still lands if ctx is
	// already cancelled.
	stop := func() error {
		if err := pin.Set(context.Background(), false, nil); err != nil {
			return fmt.Errorf("pulse_ice_pin: set pin %q low: %w", pinName, err)
		}
		return nil
	}
	select {
	case <-time.After(dwell):
	case <-ctx.Done():
		_ = stop()
		return fmt.Errorf("pulse_ice_pin: cancelled during dispense: %w", ctx.Err())
	case <-cancelCtx.Done():
		_ = stop()
		return fmt.Errorf("pulse_ice_pin: cancelled during dispense")
	}
	return stop()
}

// stageGlass sets the held glass down in the staging area and releases it,
// freeing the gripper to retrieve the espresso cup and pour; the glass is
// re-grabbed afterward (grabStagedGlass) and placed in the serving area.
func (s *beanjaminCoffee) stageGlass(ctx, cancelCtx context.Context) error {
	// The glass is full of ice here — carry it level to staging so nothing bounces
	// out on the traverse from the ice machine (NoSpill honored only with no_spill_carry).
	approachStep := Step{PoseName: clawPoseStagingApproach, PoseSwitch: s.clawsSw, Pause: shortPause, NoSpill: true}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("stage_glass: %w", err)
	}
	placeStep := Step{PoseName: clawPoseStaging, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause, AllowedCollisions: s.heldItemSurfaceCollisions(heldItemStagingCollisions)}
	if err := s.executeStep(ctx, cancelCtx, placeStep); err != nil {
		return fmt.Errorf("stage_glass: %w", err)
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("stage_glass: open gripper: %w", err)
	}
	time.Sleep(gripperPause)
	// Glass is set down; keep it as a static world obstacle (rather than dropping
	// it) so the cup-retrieval, pour, and serving moves route around it.
	if err := s.stageGlassAsObstacle(ctx); err != nil {
		return fmt.Errorf("stage_glass: %w", err)
	}
	exitStep := Step{PoseName: clawPoseStagingApproach, PoseSwitch: s.clawsSw, LinearConstraint: defaultApproachConstraint, Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, exitStep); err != nil {
		return fmt.Errorf("stage_glass: %w", err)
	}
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("stage_glass: close gripper: %w", err)
	}
	time.Sleep(gripperPause)
	return nil
}

// pourEspresso moves the held espresso cup above the staged glass and tilts it
// to pour the espresso over the ice (the tilt geometry lives in the pour pose),
// dwells so the cup drains, then returns it upright before moving away.
func (s *beanjaminCoffee) pourEspresso(ctx, cancelCtx context.Context) error {
	// The cup is full of espresso here — carry it level to the pour position so it
	// doesn't slosh before the pour tilt (NoSpill honored only with no_spill_carry).
	approachStep := Step{PoseName: clawPosePourApproach, PoseSwitch: s.clawsSw, Pause: shortPause, NoSpill: true}
	if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
		return fmt.Errorf("pour_espresso: %w", err)
	}
	// Tilt to pour as a fixed-point pivot: the claws rotate the cup in place
	// (slerp waypoints follow the geodesic between the upright and poured
	// orientations — a pure rotation about the world X axis, since both share
	// OX=0) so the stream stays over the glass instead of spilling. The staged
	// glass stays a hard obstacle here — the cup must clear it, never drive in.
	// Pivots default to the slow-movement velocity; override it so the pour
	// isn't dragged out (tune via pour_vel_degs_per_sec).
	pourStep := Step{PoseName: clawPosePour, PoseSwitch: s.clawsSw, PivotFromPose: clawPosePourApproach, PivotDegreesPerStep: 5,
		MoveOptions: s.pourMoveOptions(), Pause: pourPause}
	if err := s.executeStep(ctx, cancelCtx, pourStep); err != nil {
		return fmt.Errorf("pour_espresso: %w", err)
	}
	// Return upright along the same pivot so any residual drip stays over the glass.
	uprightStep := Step{PoseName: clawPosePourApproach, PoseSwitch: s.clawsSw, PivotFromPose: clawPosePour, PivotDegreesPerStep: 5,
		MoveOptions: s.pourMoveOptions(), Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, uprightStep); err != nil {
		return fmt.Errorf("pour_espresso: %w", err)
	}
	return nil
}

// iceDispenseSec returns the configured or default ice-dispense duration in seconds.
func (s *beanjaminCoffee) iceDispenseSec() float64 {
	return orDefault(s.cfg.IceDispenseSec, defaultIceDispenseSec)
}

// defaultPourVelDegsPerSec is the max joint velocity for the pour tilt and
// return-upright pivots when pour_vel_degs_per_sec is unset.
const defaultPourVelDegsPerSec = 60.0

// pourMoveOptions returns the per-step MoveOptions applied to the pour pivots,
// using the configured pour velocity or the default. We want the speed and acceleration to be higher
// so it tilts faster and reduces spills.
func (s *beanjaminCoffee) pourMoveOptions() *StepMoveOptions {
	return &StepMoveOptions{
		MaxVelDegsPerSec:  orDefault(s.cfg.PourVelDegsPerSec, defaultPourVelDegsPerSec),
		MaxAccDegsPerSec2: s.cfg.PourAccDegsPerSec2,
	}
}

// icePinName returns the ice-machine board pin name. Validate requires it to be
// set whenever can_serve_iced is enabled, which is the only path that reaches a
// dispense, so it is always non-empty here.
func (s *beanjaminCoffee) icePinName() string {
	return s.cfg.IceDispensePinName
}
