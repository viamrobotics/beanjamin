package coffee

// Pose and frame names the brew cycle drives to, and the startup check that
// every pose the enabled configuration needs resolves on its switch.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/golang/geo/r3"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/referenceframe"

	"beanjamin/coffee/order"
)

const (
	//filter pose switches
	filterPoseGrinderApproach            = "grinder_approach"
	filterPoseGrinderActivate            = "grinder_activate"
	filterPoseGrinderCollect             = "grinder_collect"
	filterPoseTamperApproach             = "tamper_approach"
	filterPoseTamperActivate             = "tamper_activate"
	filterPoseCoffeeApproach             = "coffee_approach"
	filterPoseCoffeeIn                   = "coffee_in"
	filterPoseCoffeeLockedFinal          = "coffee_locked_final"
	filterPoseHome                       = "home"
	filterPoseCloseToCleaning            = "close_to_cleaning"
	filterPoseApproachToCleaningScrapper = "approach_to_cleaning_scrapper"
	filterPoseCleaningScrapperActive     = "cleaning_scrapper_active"
	filterPoseApproachToCleaningBrush    = "approach_to_cleaning_brush"
	filterPoseCleaningBrushActive        = "cleaning_brush_active"
	filterPoseCoffeeShake                = "coffee_shake"
	filterPoseCoffeeShakeLeft            = "coffee_shake_left"

	// Keep-alive purge poses (required only when keepalive is configured). Both
	// are filter-frame poses: the arm holds the portafilter while it presses the
	// machine's 1 CUP button, so the portafilter is what gets positioned.
	filterPosePurgeApproach = "purge_approach"
	filterPosePurgePress    = "purge_press"

	//claw pose switches
	clawPoseCoffeeButtonApproach = "coffee_button_approach"
	clawPoseCoffeeButtonOn       = "coffee_button_on"
	clawPoseCoffeeButtonOff      = "coffee_button_off"
	// The portafilter handoff poses are not on the claws switch: they are
	// resolved against the locked portafilter handle (handle_relative_pose.go).
	clawPoseFilterReleased          = "filter_released"
	clawPoseCoffeeLockedFinal       = "coffee_locked_final"
	clawPoseCupReadyForCoffee       = "cup_ready_for_coffee"
	clawPoseCupUnderMachineApproach = "cup_under_machine_approach"

	// Brew-button claw poses, used when has_separate_brew_buttons is set. That
	// machine has one momentary button per shot size, so each size gets its own
	// standoff and press pose and the press is a straight-in linear poke from
	// directly in front of that button.
	clawPoseEspressoButtonApproach = "espresso_button_approach"
	clawPoseEspressoButtonPress    = "espresso_button_press"
	clawPoseLungoButtonApproach    = "lungo_button_approach"
	clawPoseLungoButtonPress       = "lungo_button_press"

	// iced-coffee claw poses (only required when can_serve_iced is set; the
	// glass itself is vision-detected via the glass observe switch).
	clawPoseIceMachineApproach = "ice_machine_approach" // staged in front of the ice chute
	clawPoseIceMachineDispense = "ice_machine_dispense" // glass held under the chute while the pin pulses
	clawPoseStagingApproach    = "staging_approach"     // above the staging area
	clawPoseStaging            = "staging"              // down in the staging area, ready to release the glass
	clawPosePourApproach       = "pour_approach"        // espresso cup upright above the staged glass (pour_approach_relative_pose)
	clawPosePour               = "pour"                 // espresso cup tilted to pour over the ice (pour_relative_pose)

	// iced-latte pour poses, resolved against the staged glass rather than read
	// from the switch (the milk bottle itself is vision-detected via the milk
	// observe switch, and it goes back to the spot it was detected at, so the
	// fridge needs no poses).
	clawPoseMilkPourApproach = "milk_pour_approach" // milk bottle upright above the staged glass (milk_pour_approach_relative_pose)
	clawPoseMilkPour         = "milk_pour"          // milk bottle tilted to pour into the glass (milk_pour_relative_pose)

	// camera pose switches (extra vantages live on
	// the same switch and are enumerated at runtime).
	camPoseCupObserve = "cup_observe"
)

const (
	// Frame names
	componentFilter = "filter"
	componentClaws  = "coffee-claws-middle"
	gripPoint       = "grip-point"
)

// glassPoseObserve is the home/recovery observe pose on the glass observe
// switch (parallel to camPoseCupObserve on the cup observe switch).
const glassPoseObserve = "glass_observe"

// milkApproachPose is the claws-switch pose in front of the bottle at a fridge
// spot (MilkOption.Spot), with the door open: where the gripper lines up before
// the straight-in grab, and where it backs out to after setting the bottle down.
func milkApproachPose(spot string) string { return "milk_" + spot + "_approach" }

// milkGrabPose is the claws-switch pose with the gripper around the bottle at a
// fridge spot: grabbed from here, set back here.
func milkGrabPose(spot string) string { return "milk_" + spot + "_grab" }

// milkPoseObserve is the home/recovery observe pose on the milk observe switch,
// looking into the open fridge (parallel to glassPoseObserve).
const milkPoseObserve = "milk_observe"

// requiredPose pairs a pose name with the switch it must resolve on. Used by
// validateConfiguredPoses.
type requiredPose struct {
	sw       toggleswitch.Switch
	poseName string
}

// requiredPoses returns the set of switch poses that the currently-enabled
// configuration can drive the arm to. The core brew cycle (grind → tamp →
// lock → release → brew → grab → unlock → home) always runs, so its poses are
// always required. Cleaning poses are likewise always included: every order
// cleans the portafilter after brewing. Optional features (extra grinders, iced coffee) contribute their
// poses only when their config flag is set.
func (s *beanjaminCoffee) requiredPoses() []requiredPose {
	poses := []requiredPose{
		// step 1: grind (regular)
		{s.filterSw, filterPoseGrinderApproach},
		{s.filterSw, filterPoseGrinderActivate},
		// step 2: tamp
		{s.filterSw, filterPoseTamperApproach},
		{s.filterSw, filterPoseTamperActivate},
		// step 3: lock portafilter
		{s.filterSw, filterPoseCoffeeApproach},
		{s.filterSw, filterPoseCoffeeIn},
		{s.filterSw, filterPoseCoffeeLockedFinal},
		// step 9: home
		{s.filterSw, filterPoseHome},
		// cleaning (post-brew)
		{s.filterSw, filterPoseCloseToCleaning},
		{s.filterSw, filterPoseApproachToCleaningScrapper},
		{s.filterSw, filterPoseCleaningScrapperActive},
		{s.filterSw, filterPoseApproachToCleaningBrush},
		{s.filterSw, filterPoseCleaningBrushActive},
	}

	// step 6: brew. Which claw poses exist depends on the machine. On the
	// button machine both shot sizes are required even if only one is ordered
	// today — the drink is only known per-order, so a switch that can't reach
	// the lungo button is misconfigured regardless of what's queued.
	if s.cfg.HasSeparateBrewButtons {
		poses = append(poses,
			requiredPose{s.clawsSw, clawPoseEspressoButtonApproach},
			requiredPose{s.clawsSw, clawPoseEspressoButtonPress},
			requiredPose{s.clawsSw, clawPoseLungoButtonApproach},
			requiredPose{s.clawsSw, clawPoseLungoButtonPress},
		)
	} else {
		poses = append(poses,
			requiredPose{s.clawsSw, clawPoseCoffeeButtonApproach},
			requiredPose{s.clawsSw, clawPoseCoffeeButtonOn},
			requiredPose{s.clawsSw, clawPoseCoffeeButtonOff},
		)
	}

	// step 8: unlock portafilter. The arm only travels to the shake poses when a
	// shake duration is configured.
	if s.cfg.PortafilterShakeSec > 0 {
		poses = append(poses,
			requiredPose{s.filterSw, filterPoseCoffeeShake},
			requiredPose{s.filterSw, filterPoseCoffeeShakeLeft},
		)
	}

	// Each extra grinder's poses, named after its beans. A grinder listed in
	// bean_options needs all three; the collect pose stays optional only for
	// grinders configured before it existed (the regular grinder, and decaf
	// enabled through the deprecated can_serve_decaf), which collect at their
	// approach pose without one (grinderCollect).
	for _, beans := range s.cfg.beanNames() {
		approach, activate := grinderPoses(beans)
		poses = append(poses,
			requiredPose{s.filterSw, approach},
			requiredPose{s.filterSw, activate},
		)
		if s.cfg.listsBeans(beans) {
			poses = append(poses, requiredPose{s.filterSw, grinderCollectPose(beans)})
		}
	}

	// Gated: requiring these unconditionally would fail construction on every
	// machine that never travels to them.
	if s.cfg.KeepAlive != nil {
		poses = append(poses,
			requiredPose{s.filterSw, filterPosePurgeApproach},
			requiredPose{s.filterSw, filterPosePurgePress},
		)
	}

	poses = append(poses,
		requiredPose{s.clawsSw, clawPoseCupUnderMachineApproach},
		requiredPose{s.clawsSw, clawPoseCupReadyForCoffee},
		requiredPose{s.cameraObserveSw, camPoseCupObserve},
	)

	if s.cfg.CanServeIced {
		// serveIcedCoffee dispenses ice, stages the glass, and pours the
		// espresso over the ice (the cup-retrieval poses above always run). The
		// pour poses are resolved against the staged glass, not read from the
		// switch (glass_relative_poses.go).
		poses = append(poses,
			requiredPose{s.clawsSw, clawPoseIceMachineApproach},
			requiredPose{s.clawsSw, clawPoseIceMachineDispense},
			requiredPose{s.clawsSw, clawPoseStagingApproach},
			requiredPose{s.clawsSw, clawPoseStaging},
			requiredPose{s.glassObserveSw, glassPoseObserve},
		)
	}

	if s.cfg.CanServeIcedLatte {
		// Nothing on the claws switch: the bottle is vision-detected inside the
		// fridge and set back down at the centroid it was grasped at, the door is
		// tracked through its hinge arc, and the pour is resolved against the
		// staged glass (glass_relative_poses.go).
		poses = append(poses,
			requiredPose{s.milkObserveSw, milkPoseObserve},
		)
		// Each milk_options spot's approach and grab, checked now so they are
		// taught and verified before anything moves to them.
		for _, m := range s.cfg.MilkOptions {
			poses = append(poses,
				requiredPose{s.clawsSw, milkApproachPose(m.Spot)},
				requiredPose{s.clawsSw, milkGrabPose(m.Spot)},
			)
		}
	}

	return poses
}

// validateConfiguredPoses checks, for the currently-enabled configuration,
// that every switch pose the service can move to actually resolves on its pose
// switch and is non-zero. A missing pose surfaces as a get_pose_by_name error
// from the switch; an all-zero translation indicates an unset/placeholder pose
// that would silently drive the arm to the base origin. Called once at
// construction so a misconfigured switch fails fast instead of mid-order.
func (s *beanjaminCoffee) validateConfiguredPoses(ctx context.Context) error {
	poses := s.requiredPoses()
	for _, rp := range poses {
		pd, err := s.fetchPose(ctx, rp.sw, rp.poseName)
		if err != nil {
			return fmt.Errorf("pose validation: required pose %q on %q switch: %w", rp.poseName, rp.sw.Name().ShortName(), err)
		}
		if pd.pose.Point() == (r3.Vector{}) {
			return fmt.Errorf("pose validation: required pose %q on %q switch resolves to a zero position — is it configured?", rp.poseName, rp.sw.Name().ShortName())
		}
	}
	// The filter handoff poses are derived rather than read off a switch;
	// resolving them here fails at startup, not at the first release, when the
	// handle frame is missing.
	for _, poseName := range handleRelativePoses {
		if _, err := s.handleRelativeGripPoseData(ctx, poseName); err != nil {
			return fmt.Errorf("pose validation: %w", err)
		}
	}
	s.logger.Infof("pose validation: %d configured pose(s) resolved and non-zero", len(poses))
	return s.resolveGrinderCollectPoses(ctx)
}

// grinderPoses names the filter-switch poses at the grinder for beans: the
// regular grinder's grinder_approach / grinder_activate, and for every other
// beans <beans>_grinder_approach / <beans>_grinder_activate.
func grinderPoses(beans string) (approach, activate string) {
	if beans == "" || beans == order.BeansRegular {
		return filterPoseGrinderApproach, filterPoseGrinderActivate
	}
	return beans + "_" + filterPoseGrinderApproach, beans + "_" + filterPoseGrinderActivate
}

// grinderCollectPose names the optional filter-switch pose the filter sits at
// under beans' grinder chute while it dispenses: grinder_collect, or
// <beans>_grinder_collect. Without it the filter collects at the approach pose
// (grinderCollect).
func grinderCollectPose(beans string) string {
	if beans == "" || beans == order.BeansRegular {
		return filterPoseGrinderCollect
	}
	return beans + "_" + filterPoseGrinderCollect
}

// grinderCollect is the pose the filter collects grounds at for beans: its
// collect pose when the switch has one, else its approach pose, which is where
// every grinder collected before collect poses existed.
func (s *beanjaminCoffee) grinderCollect(beans string) string {
	if s.collectPoses[beans] {
		return grinderCollectPose(beans)
	}
	approach, _ := grinderPoses(beans)
	return approach
}

// resolveGrinderCollectPoses records which grinders have a collect pose on the
// filter switch. Optional, so a missing one is not an error; one that resolves
// to a zero position is, like any other configured pose.
func (s *beanjaminCoffee) resolveGrinderCollectPoses(ctx context.Context) error {
	s.collectPoses = map[string]bool{}
	// The switch lists its pose names; asking it for an unknown one by name
	// would work too, but logs a warning on the switch for every grinder that
	// simply has no collect pose.
	_, names, err := s.filterSw.GetNumberOfPositions(ctx, nil)
	if err != nil {
		return fmt.Errorf("pose validation: list poses on %q switch: %w", s.filterSw.Name().ShortName(), err)
	}
	for _, beans := range append([]string{order.BeansRegular}, s.cfg.beanNames()...) {
		name := grinderCollectPose(beans)
		if !slices.Contains(names, name) {
			s.logger.Debugf("pose validation: no %q pose; %s grinds collect at the approach pose", name, beans)
			continue
		}
		pd, err := s.fetchPose(ctx, s.filterSw, name)
		if err != nil {
			return fmt.Errorf("pose validation: pose %q on %q switch: %w", name, s.filterSw.Name().ShortName(), err)
		}
		if pd.pose.Point() == (r3.Vector{}) {
			return fmt.Errorf("pose validation: pose %q on %q switch resolves to a zero position — is it configured?", name, s.filterSw.Name().ShortName())
		}
		s.collectPoses[beans] = true
		s.logger.Infof("pose validation: %s grinds collect at %q", beans, name)
	}
	return nil
}

// grinderShieldFrame names the obstacle guarding the button of beans'
// grinder: grinder-button-shield for the regular grinder, else
// <beans>-grinder-button-shield with the underscores in beans as hyphens.
func grinderShieldFrame(beans string) string {
	if beans == "" || beans == order.BeansRegular {
		return "grinder-button-shield"
	}
	return strings.ReplaceAll(beans, "_", "-") + "-grinder-button-shield"
}

// checkGrinderShields fails when an extra grinder's button shield is missing
// from the frame system. Grinding is allowed to touch the shield by name, so a
// misnamed one (usually a fragment prefix that doesn't match the beans name)
// would otherwise only surface mid-order, as a grind that can't reach the
// button.
func checkGrinderShields(fs *referenceframe.FrameSystem, beans []string) error {
	for _, b := range beans {
		if name := grinderShieldFrame(b); fs.Frame(name) == nil {
			return fmt.Errorf("bean_options %q: grinder button shield %q is not in the frame system — "+
				"it is either disabled (check fragment_mods) or missing (apply the grinder fragment with prefix %q)",
				b, name, strings.TrimSuffix(name, "-grinder-button-shield"))
		}
	}
	return nil
}
