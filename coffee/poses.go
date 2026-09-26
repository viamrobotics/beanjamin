package coffee

// Pose and frame names the brew cycle drives to, and the startup check that
// every pose the enabled configuration needs resolves on its switch.

import (
	"context"
	"fmt"

	"github.com/golang/geo/r3"
	toggleswitch "go.viam.com/rdk/components/switch"
)

const (
	//filter pose switches
	filterPoseGrinderApproach            = "grinder_approach"
	filterPoseGrinderActivate            = "grinder_activate"
	filterPoseDecafGrinderApproach       = "decaf_grinder_approach"
	filterPoseDecafGrinderActivate       = "decaf_grinder_activate"
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
	clawPoseCoffeeButtonApproach    = "coffee_button_approach"
	clawPoseCoffeeButtonOn          = "coffee_button_on"
	clawPoseCoffeeButtonOff         = "coffee_button_off"
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
	clawPosePourApproach       = "pour_approach"        // espresso cup upright above the staged glass
	clawPosePour               = "pour"                 // espresso cup tilted to pour over the ice

	// iced-latte claw poses (only required when can_serve_iced_latte is set; the
	// milk bottle itself is vision-detected via the milk observe switch, and it
	// goes back to the spot it was detected at, so the fridge needs no poses).
	clawPoseMilkPourApproach = "milk_pour_approach" // milk bottle upright above the staged glass
	clawPoseMilkPour         = "milk_pour"          // milk bottle tilted to pour into the glass

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
// always required. Cleaning poses are likewise always included: the
// recovery path in rewind() runs cleanPortafilter whenever the portafilter
// holds grounds, which is the case for every order once grinding starts. Optional features (decaf, iced coffee) contribute their
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
		// step 4: release filter
		{s.clawsSw, clawPoseFilterReleased},
		// step 7: grab filter
		{s.clawsSw, clawPoseCoffeeLockedFinal},
		// step 9: home
		{s.filterSw, filterPoseHome},
		// cleaning (post-brew and rewind recovery)
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

	if s.cfg.CanServeDecaf {
		poses = append(poses,
			requiredPose{s.filterSw, filterPoseDecafGrinderApproach},
			requiredPose{s.filterSw, filterPoseDecafGrinderActivate},
		)
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
		// espresso over the ice (the cup-retrieval poses above always run).
		poses = append(poses,
			requiredPose{s.clawsSw, clawPoseIceMachineApproach},
			requiredPose{s.clawsSw, clawPoseIceMachineDispense},
			requiredPose{s.clawsSw, clawPoseStagingApproach},
			requiredPose{s.clawsSw, clawPoseStaging},
			requiredPose{s.clawsSw, clawPosePourApproach},
			requiredPose{s.clawsSw, clawPosePour},
			requiredPose{s.glassObserveSw, glassPoseObserve},
		)
	}

	if s.cfg.CanServeIcedLatte {
		// Only the pour is authored: the bottle is vision-detected inside the
		// fridge and set back down at the centroid it was grasped at, and the door
		// is tracked through its hinge arc rather than driven to switch poses.
		poses = append(poses,
			requiredPose{s.clawsSw, clawPoseMilkPourApproach},
			requiredPose{s.clawsSw, clawPoseMilkPour},
			requiredPose{s.milkObserveSw, milkPoseObserve},
		)
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
	s.logger.Infof("pose validation: %d configured pose(s) resolved and non-zero", len(poses))
	return nil
}
