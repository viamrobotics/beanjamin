package coffee

// Coffee service configuration: the Config struct and its validation, the typed
// values it carries (relative poses, container dimensions), and the small
// helpers that resolve configured values to their defaults.

import (
	"fmt"
	"time"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/gripper"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/vision"

	"beanjamin/coffee/report"
)

// Config is the attribute set of the viam:beanjamin:coffee service. Field
// semantics and defaults are documented in the README; zero-valued tunables
// fall back to the defaults defined next to the feature that reads them.
type Config struct {
	PoseSwitcherName      string `json:"pose_switcher_name"`
	ClawsPoseSwitcherName string `json:"claws_pose_switcher_name"`
	ArmName               string `json:"arm_name"`
	GripperName           string `json:"gripper_name"`
	SpeechServiceName     string `json:"speech_service_name,omitempty"`
	// HasSeparateBrewButtons selects which coffee machine this arm is driving.
	//
	// false (default): a single toggle switch. The claw holds it down for the
	// whole brew, so BrewTimeSec/LungoBrewTimeSec *are* the dose. Requires the
	// coffee_button_approach/_on/_off claw poses.
	//
	// true: one momentary button per shot size. The claw pokes the button for
	// the ordered drink and steps clear; the machine decides the dose, so the
	// brew times only have to outlast its pour. Requires the espresso_button_
	// and lungo_button_ approach/press claw poses.
	HasSeparateBrewButtons bool `json:"has_separate_brew_buttons,omitempty"`

	// BrewTimeSec / LungoBrewTimeSec are the toggle hold duration, or — under
	// has_separate_brew_buttons — how long to wait out a machine-controlled
	// pour, in which case they must be >= its actual pour or the arm reaches
	// in mid-stream. ButtonPressHoldSec applies only to the button machine.
	BrewTimeSec                float64 `json:"brew_time_sec,omitempty"`
	LungoBrewTimeSec           float64 `json:"lungo_brew_time_sec,omitempty"`
	ButtonPressHoldSec         float64 `json:"button_press_hold_sec,omitempty"`
	GrindTimeSec               float64 `json:"grind_time_sec,omitempty"`
	GripperHoldMinPos          float64 `json:"gripper_hold_min_pos,omitempty"`
	GripperHoldMaxPos          float64 `json:"gripper_hold_max_pos,omitempty"`
	GripperOpenTimeoutSec      float64 `json:"gripper_open_timeout_sec,omitempty"`
	SlowMovementVelDegsPerSec  float64 `json:"slow_movement_vel_degs_per_sec,omitempty"`
	SlowMovementAccDegsPerSec2 float64 `json:"slow_movement_acc_degs_per_sec2,omitempty"`
	PortafilterShakeSec        float64 `json:"portafilter_shake_sec,omitempty"`
	// LockOvershootDegs over-rotates the portafilter lock pivot so the filter
	// still reaches the authored angle after the claws slip on its handle under
	// bayonet load, then unwinds back onto it (Step.PivotExtraDegrees).
	// Defaults to 0 — tune it on the machine.
	LockOvershootDegs     float64 `json:"lock_overshoot_degs,omitempty"`
	SaveMotionRequestsDir string  `json:"save_motion_requests_dir,omitempty"`
	OrderSensorName       string  `json:"order_sensor_name,omitempty"`

	// Optional usage sensor updated during the brew lifecycle via a best-effort
	// read-modify-write: all counters are read with Readings, the changed one is
	// updated, and the full map is written back with DoCommand({"set": {...}}).
	UsageSensorName string `json:"usage_sensor_name,omitempty"`

	CamStorageMuxName string `json:"cam_storage_mux_name,omitempty"`
	DataDir           string `json:"data_dir,omitempty"`
	CanServeDecaf     bool   `json:"can_serve_decaf,omitempty"`

	CanServeIced         bool    `json:"can_serve_iced,omitempty"`
	IceDispenseBoardName string  `json:"ice_board_name,omitempty"`
	IceDispensePinName   string  `json:"ice_pin_name,omitempty"`
	IceDispenseSec       float64 `json:"ice_dispense_sec,omitempty"`
	PourVelDegsPerSec    float64 `json:"pour_vel_degs_per_sec,omitempty"`
	PourAccDegsPerSec2   float64 `json:"pour_acc_degs_per_sec2,omitempty"`

	// IceVisionEnabled watches the glass while ice falls and closes the pin when
	// the surface passes IceStopRowPx, instead of dispensing for a fixed
	// IceDispenseSec (ice_dwell.go). Off by default: the shipping stop row rests
	// on a single observed glass seating, and check_ice_level is how a machine
	// confirms its own value before turning this on.
	IceVisionEnabled bool `json:"ice_vision_enabled,omitempty"`
	// The measurement's pixel geometry. Every one of these is a raw pixel
	// coordinate at the camera's configured resolution — reconfigure the camera
	// to a different frame size and they all silently mean something else.
	// IceStopRowPx is the row the ice surface has to reach; the band scanned is
	// derived from it, never configured above it.
	IceStopRowPx      int     `json:"ice_stop_row_px,omitempty"`
	IceContrastWindow int     `json:"ice_contrast_window,omitempty"`
	IceMinContrast    float64 `json:"ice_min_contrast,omitempty"`
	IceROIX0          int     `json:"ice_roi_x0,omitempty"`
	IceROIX1          int     `json:"ice_roi_x1,omitempty"`
	IceROIY1          int     `json:"ice_roi_y1,omitempty"`
	// The brightness shadow: a second read of the same frame by absolute
	// brightness, logged beside the contrast step and never acted on.
	// IceBrightnessThresh is the row-mean cutoff; unset, the shadow is off, and
	// it deliberately has no default because an absolute cutoff is the one value
	// that does not survive a change in lighting. IceBrightRun is how many
	// consecutive rows must clear it.
	IceBrightnessThresh float64 `json:"ice_brightness_thresh,omitempty"`
	IceBrightRun        int     `json:"ice_bright_run,omitempty"`
	// Dispense loop timing. IceDispenseMaxSec is the ceiling that ends a
	// dispense the measurement never stopped; IceDispenseMinSec holds the pin
	// open before any reading counts, since ice takes seconds to arrive.
	// IceAfterFirstSeenMaxSec is the second ceiling, measured from the first
	// sighting instead of from the pin opening: the absolute one has to clear a
	// whole fill, so only this one is tight enough to bound the overflow when ice
	// is flowing but its surface is never confirmed past the stop row.
	IceDispenseMaxSec       float64 `json:"ice_dispense_max_sec,omitempty"`
	IceDispenseMinSec       float64 `json:"ice_dispense_min_sec,omitempty"`
	IceAfterFirstSeenMaxSec float64 `json:"ice_after_first_seen_max_sec,omitempty"`
	IceCheckIntervalSec     float64 `json:"ice_check_interval_sec,omitempty"`

	// CanServeIcedLatte enables the iced_latte drink: the iced-coffee flow plus
	// a fridge trip for milk (coffee/milk.go). It builds on can_serve_iced — the
	// glass, the ice and the staging area all come from there — and on the
	// fridge-door sweep, so both must be configured alongside it.
	CanServeIcedLatte bool `json:"can_serve_iced_latte,omitempty"`
	// MilkPourSec is how long the bottle is held tilted over the glass: the knob
	// that sets how much milk a latte gets.
	MilkPourSec float64 `json:"milk_pour_sec,omitempty"`

	// Optional Slack notifier (viam:notifications:slack generic service). When
	// set, the coffee service sends a best-effort Slack message via DoCommand
	// for every non-successful order attempt — genuine faults and operator
	// cancels alike. Unset disables notifications.
	SlackNotifierName string `json:"slack_notifier_name,omitempty"`

	// ChoreWheel sets up the weekly chore rota posted by send_weekly_chores.
	// Needs slack_notifier_name. Leave it out to turn the command off.
	ChoreWheel *report.ChoreWheelConfig `json:"chore_wheel,omitempty"`

	// CustomerDetectorName: customer-detector that completed orders are credited
	// to, for "the usual". Unset disables recording.
	CustomerDetectorName string `json:"customer_detector_name,omitempty"`

	// DeliveryHandlerName names a generic service on a peer machine (via a
	// remote, e.g. "delivery-bot:mission-control") that this service can send
	// one-way notifications to with the send_delivery_message DoCommand. The
	// payload is forwarded verbatim as the peer service's DoCommand, so it
	// must be a command that service already understands. Unset disables
	// outbound messaging.
	DeliveryHandlerName string `json:"delivery_handler_name,omitempty"`

	// Conversational, when true, makes the coffee service speak its own
	// status-narrating lines through speech_service_name — initial
	// greetings, almost-ready prompts, order confirmations, rejection
	// quips, etc. When false (the default), the service stays silent
	// except for the drink-ready announcement at cup handoff, leaving
	// everything else for an external orchestrator (e.g. voice-command)
	// to handle.
	Conversational bool `json:"conversational,omitempty"`

	// Vision-driven cup pickup
	// The fields below configure that pipeline and are required.
	CupVisionServiceName          string        `json:"cup_vision_service_name,omitempty"`
	SrcCameraName                 string        `json:"src_camera_name,omitempty"`
	CupApproachRelativePose       *RelativePose `json:"cup_approach_relative_pose,omitempty"`
	CupGrabRelativePose           *RelativePose `json:"cup_grab_relative_pose,omitempty"`
	CameraObservePoseSwitcherName string        `json:"camera_observe_pose_switcher_name,omitempty"`
	// CupPickupMaxAttempts caps how many full observe-and-grab attempts
	// pickCupDynamic will make per order. Each attempt re-detects, then
	// walks the candidate list (closest first), falling through to the
	// next candidate on planning failures. Defaults to 3.
	CupPickupMaxAttempts int `json:"cup_pickup_max_attempts,omitempty"`
	// CupDimensions is the known cup diameter/height the held cup is modeled
	// from (see ContainerDimensions). Required.
	CupDimensions *ContainerDimensions `json:"cup_dimensions,omitempty"`

	// Glass pickup (iced coffee) mirrors cup pickup but with its own vision
	// service and observe-pose switch, tuned for the taller iced-coffee glass.
	// These fields are required when can_serve_iced is set.
	GlassVisionServiceName       string        `json:"glass_vision_service_name,omitempty"`
	GlassObservePoseSwitcherName string        `json:"glass_observe_pose_switcher_name,omitempty"`
	GlassApproachRelativePose    *RelativePose `json:"glass_approach_relative_pose,omitempty"`
	GlassGrabRelativePose        *RelativePose `json:"glass_grab_relative_pose,omitempty"`
	// GlassDimensions is the known glass diameter/height the held glass is
	// modeled from (see ContainerDimensions). Required when can_serve_iced is set.
	GlassDimensions *ContainerDimensions `json:"glass_dimensions,omitempty"`

	// Milk-bottle pickup (iced latte) mirrors cup and glass pickup with its own
	// vision service and observe-pose switch, whose vantages look into the open
	// fridge. These fields are required when can_serve_iced_latte is set.
	MilkVisionServiceName       string        `json:"milk_vision_service_name,omitempty"`
	MilkObservePoseSwitcherName string        `json:"milk_observe_pose_switcher_name,omitempty"`
	MilkApproachRelativePose    *RelativePose `json:"milk_approach_relative_pose,omitempty"`
	MilkGrabRelativePose        *RelativePose `json:"milk_grab_relative_pose,omitempty"`
	// MilkBottleDimensions is the known bottle diameter/height the held bottle is
	// modeled from (see ContainerDimensions). The same offsets that grabbed the
	// bottle put it back, so these are also what the return descent is planned
	// around. Required when can_serve_iced_latte is set.
	MilkBottleDimensions *ContainerDimensions `json:"milk_bottle_dimensions,omitempty"`

	// Serving placement offsets are composed onto the serving-area slot anchor
	// when releasing a finished drink onto the served shelf. The same pair is
	// used for both the hot cup and the iced glass. Both are required.
	ServingApproachRelativePose *RelativePose `json:"serving_approach_relative_pose,omitempty"`
	ServingGrabRelativePose     *RelativePose `json:"serving_grab_relative_pose,omitempty"`

	InputRangeOverride map[string]map[string]JointLimitDegs `json:"input_range_override,omitempty"`

	// FakeMode skips AllowedCollision entries that reference gripper
	// sub-geometries (e.g. "gripper:claws") which only exist on the real
	// ufactory gripper. Set true on fake-hardware test machines; leave
	// unset on the real bot.
	FakeMode bool `json:"fake_mode,omitempty"`

	// MaxBatchSize caps how many drinks a single prepare_order call may
	// enqueue via the optional "count" field. Protects the queue from a
	// runaway voice command ("a hundred lattes") and from an LLM
	// hallucinating a huge count. Defaults to 10 when unset or non-positive.
	MaxBatchSize int `json:"max_batch_size,omitempty"`

	// Fridge-door open (coffee/door.go): swing angle, per-step θ increment, and
	// the frame the gripper aims at / tracks / is allowed to touch (the handle
	// ball; its center is the grasp target). The door obstacle frame itself is a
	// fixed constant in door.go.
	DoorOpenAngleDegs       float64 `json:"door_open_angle_degs,omitempty"`
	DoorPivotDegreesPerStep float64 `json:"door_pivot_degrees_per_step,omitempty"`
	DoorGraspFrameName      string  `json:"door_grasp_frame_name,omitempty"`

	// DoorApproachRelativePose is a RelativePose offset composed onto the grasp
	// frame's center to produce the pre-grasp standoff (like
	// cup_approach_relative_pose onto a detected cup centroid — see
	// composeCupPose), but resolved against the live grasp frame. Its
	// orientation is the base grasp orientation, which DoorGraspYawRatio then
	// yaws through the swing. Required to run open_door.
	DoorApproachRelativePose *RelativePose `json:"door_approach_relative_pose,omitempty"`

	// KeepAlive, when set, runs the idle-purge loop (keepalive.go) that holds the
	// machine's 1 CUP button periodically so it never falls out of brew
	// temperature. Requires HasSeparateBrewButtons. Unset disables it.
	KeepAlive *KeepAlive `json:"keepalive,omitempty"`

	// DoorGraspYawRatio turns the grasp orientation about world Z by ratio x
	// theta as the door swings; see defaultDoorGraspYawRatio. A pointer because
	// 0 and negatives are real settings, not "unset".
	DoorGraspYawRatio *float64 `json:"door_grasp_yaw_ratio,omitempty"`
}

// defaultMaxBatchSize is used when Config.MaxBatchSize is unset or zero.
const defaultMaxBatchSize = 10

// defaultDoorOpenAngleDegs is the fridge-door swing angle when unset.
const defaultDoorOpenAngleDegs = 90

// defaultDoorPivotDegreesPerStep is the per-step θ increment for the door
// sweep when unset.
const defaultDoorPivotDegreesPerStep = 10

// doorOpenAngleDegs returns the configured fridge-door swing angle, defaulting
// to defaultDoorOpenAngleDegs.
func (s *beanjaminCoffee) doorOpenAngleDegs() float64 {
	return orDefault(s.cfg.DoorOpenAngleDegs, defaultDoorOpenAngleDegs)
}

// doorPivotDegreesPerStep returns the configured per-step θ increment for the
// door sweep, defaulting to defaultDoorPivotDegreesPerStep.
func (s *beanjaminCoffee) doorPivotDegreesPerStep() float64 {
	return orDefault(s.cfg.DoorPivotDegreesPerStep, defaultDoorPivotDegreesPerStep)
}

// defaultDoorGraspYawRatio counter-rotates the gripper as the door swings.
//
// Reachability sets this sign, not grasp mechanics: the handle is a ball, so the
// grasp does not constrain wrist roll. The gripper sits behind its tool center
// along -OV, so co-rotating drives the wrist into +y just as the handle travels
// there. Replanning a failed 75-degree sweep offline put +1 out of IK solutions
// at theta=47 and 0 out by theta=75; only -1 reached full open.
const defaultDoorGraspYawRatio = -1

// doorGraspYawRatio returns the configured world-Z yaw ratio for the door sweep.
// It cannot use orDefault: that helper treats any non-positive value as unset,
// and 0 (hold orientation fixed) and -1 (counter-rotate) are both real settings.
func (s *beanjaminCoffee) doorGraspYawRatio() float64 {
	if s.cfg.DoorGraspYawRatio != nil {
		return *s.cfg.DoorGraspYawRatio
	}
	return defaultDoorGraspYawRatio
}

// doorGraspFrameName returns the frame the gripper aims at (its center is the
// grasp target), tracks through the sweep, and is allowed to contact. Defaults
// to frameFridgeHandleBall.
func (s *beanjaminCoffee) doorGraspFrameName() string {
	if s.cfg.DoorGraspFrameName != "" {
		return s.cfg.DoorGraspFrameName
	}
	return frameFridgeHandleBall
}

// defaultMilkPourSec is how long the bottle is held tilted over the glass when
// milk_pour_sec is unset.
const defaultMilkPourSec = 4.0

// milkPourDwell returns how long the tilted bottle is held over the glass —
// the configured pour time or the default. This is what sets the milk dose, so
// it is tuned on the machine against the bottle and the glass in use.
func (s *beanjaminCoffee) milkPourDwell() time.Duration {
	return time.Duration(orDefault(s.cfg.MilkPourSec, defaultMilkPourSec) * float64(time.Second))
}

// orDefault returns v when it is positive, otherwise def. It backs the
// "configured tunable or default constant" pattern used by the numeric getters.
func orDefault[T ~int | ~float64](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}

// maxBatchSize returns the configured cap on prepare_order count, falling
// back to defaultMaxBatchSize.
func (s *beanjaminCoffee) maxBatchSize() int {
	return orDefault(s.cfg.MaxBatchSize, defaultMaxBatchSize)
}

// defaultCupPickupMaxAttempts is used when Config.CupPickupMaxAttempts is
// unset or zero.
const defaultCupPickupMaxAttempts = 3

// pickupMaxAttempts returns the configured cap on full observe-and-grab
// attempts (cup or glass), falling back to defaultCupPickupMaxAttempts when
// unset or non-positive.
func pickupMaxAttempts(configured int) int {
	return orDefault(configured, defaultCupPickupMaxAttempts)
}

// RelativePose is a 6-DoF offset (translation in millimeters + orientation as
// OrientationVectorDegrees) composed onto a runtime point. Used for
// cup_approach_relative_pose and cup_grab_relative_pose under dynamic cup
// pickup, where the offset is applied to the detected cup centroid rather
// than being a world-frame pose. Kept here (not on the pose switch) so that
// switch-aware tooling (e.g. the test card) doesn't try to drive the arm to
// these as if they were world-frame goals. If a similar offset concept turns
// up in another model later, this can move to a shared package.
type RelativePose struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Z     float64 `json:"z"`
	OX    float64 `json:"o_x"`
	OY    float64 `json:"o_y"`
	OZ    float64 `json:"o_z"`
	Theta float64 `json:"theta"`
}

// ContainerDimensions is the operator-supplied size of a picked-up container
// (cup or glass), configured as cup_dimensions / glass_dimensions. It defines
// the held-item bounding box: width = depth = DiameterMm and height = HeightMm,
// centered on the grasp centroid (the point the gripper is sent to). The grasp
// centroid itself is unaffected — only the collision/visualization geometry
// comes from here. Round containers (cups/glasses) are well approximated by a
// square-footprint box of the rim diameter, and a known size centered on the
// grasp point avoids a partially-observed point cloud under-reading or skewing
// the box. Every container the arm carries is tracked as a held item
// (held_geometry.go), so these dimensions are required.
type ContainerDimensions struct {
	DiameterMm float64 `json:"diameter_mm"`
	HeightMm   float64 `json:"height_mm"`
}

// validate checks a required ContainerDimensions: it must be present, with a
// positive diameter and height. field is the JSON config key for error messages.
func (d *ContainerDimensions) validate(path, field string) error {
	if d == nil {
		return resource.NewConfigValidationFieldRequiredError(path, field)
	}
	if d.DiameterMm <= 0 {
		return fmt.Errorf("%s: %s.diameter_mm must be > 0", path, field)
	}
	if d.HeightMm <= 0 {
		return fmt.Errorf("%s: %s.height_mm must be > 0", path, field)
	}
	return nil
}

// KeepAlive configures the idle-purge loop that holds the espresso machine at
// brew temperature (keepalive.go). Presence enables the loop; nil disables it.
//
// AutoStart must mirror the time programmed into the machine's own Auto Start
// setting, and is also the window's open. Deliberately one number: as two
// settings they drift, and a window opening after Auto Start leaves the machine
// awake long enough to fall into POWER SAVE before anyone can order.
type KeepAlive struct {
	// AutoStart / End bound the window as "HH:MM" local times, half-open.
	AutoStart string `json:"auto_start"`
	End       string `json:"end"`
	// Timezone is a required IANA name, so the window does not depend on host TZ.
	Timezone string `json:"timezone"`
	// Days are three-letter weekday names; defaults to Monday–Friday.
	Days []string `json:"days,omitempty"`

	AfterMin         float64 `json:"after_min,omitempty"`
	CheckIntervalMin float64 `json:"check_interval_min,omitempty"`
	// HoldSec sets the water volume per purge — the knob if the tray fills fast.
	HoldSec float64 `json:"hold_sec,omitempty"`
}

// requireFields returns a field-required error for the first empty value in
// pairs, which alternate config key and value. It keeps Validate's long runs of
// presence checks readable as the lists of field names they really are.
func requireFields(path string, pairs ...any) error {
	for i := 0; i < len(pairs); i += 2 {
		field := pairs[i].(string)
		switch v := pairs[i+1].(type) {
		case string:
			if v == "" {
				return resource.NewConfigValidationFieldRequiredError(path, field)
			}
		case *RelativePose:
			if v == nil {
				return resource.NewConfigValidationFieldRequiredError(path, field)
			}
		default:
			return fmt.Errorf("%s: requireFields: unsupported type %T for %q", path, v, field)
		}
	}
	return nil
}

// Validate checks the required fields and any feature-gated requirements (e.g.
// can_serve_iced needs the ice board and glass vision pipeline), and returns the
// required and optional dependency names derived from the configured resources.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if err := requireFields(path,
		"pose_switcher_name", cfg.PoseSwitcherName,
		"claws_pose_switcher_name", cfg.ClawsPoseSwitcherName,
		"arm_name", cfg.ArmName,
		"gripper_name", cfg.GripperName,
		// Cup pickup is always vision-driven, so its pipeline is required too.
		"cup_vision_service_name", cfg.CupVisionServiceName,
		"src_camera_name", cfg.SrcCameraName,
		"camera_observe_pose_switcher_name", cfg.CameraObservePoseSwitcherName,
		"cup_approach_relative_pose", cfg.CupApproachRelativePose,
		"cup_grab_relative_pose", cfg.CupGrabRelativePose,
		"serving_approach_relative_pose", cfg.ServingApproachRelativePose,
		"serving_grab_relative_pose", cfg.ServingGrabRelativePose,
	); err != nil {
		return nil, nil, err
	}
	if cfg.CupPickupMaxAttempts < 0 {
		return nil, nil, fmt.Errorf("%s: cup_pickup_max_attempts must be >= 0", path)
	}
	if cfg.ButtonPressHoldSec < 0 {
		return nil, nil, fmt.Errorf("%s: button_press_hold_sec must be >= 0", path)
	}
	// The picked-up cup is always tracked as a held item, and its geometry is
	// modeled from these dimensions, so they must be configured.
	if err := cfg.CupDimensions.validate(path, "cup_dimensions"); err != nil {
		return nil, nil, err
	}

	reqDeps := []string{cfg.PoseSwitcherName, cfg.ClawsPoseSwitcherName, framesystem.PublicServiceName.String(), arm.Named(cfg.ArmName).String(), gripper.Named(cfg.GripperName).String()}

	var optDeps []string
	for _, dep := range []struct {
		name  string
		named func(string) resource.Name
	}{
		{cfg.SpeechServiceName, generic.Named},
		{cfg.OrderSensorName, sensor.Named},
		{cfg.UsageSensorName, sensor.Named},
		{cfg.CamStorageMuxName, generic.Named},
		{cfg.SlackNotifierName, generic.Named},
		{cfg.CustomerDetectorName, generic.Named},
		{cfg.DeliveryHandlerName, generic.Named},
	} {
		if dep.name != "" {
			optDeps = append(optDeps, dep.named(dep.name).String())
		}
	}
	reqDeps = append(reqDeps,
		vision.Named(cfg.CupVisionServiceName).String(),
		camera.Named(cfg.SrcCameraName).String(),
		cfg.CameraObservePoseSwitcherName,
	)

	// Not gated on can_serve_iced: pulse_ice_pin is an execute_action on every
	// machine, so ice_vision_enabled is reachable — and a band that scans nothing
	// would then ride every dispense to its ceiling with nothing in the logs.
	if err := validateIceVision(cfg, path); err != nil {
		return nil, nil, err
	}

	if cfg.CanServeIced {
		// The glass is fetched by its own vision pipeline, reusing the cup camera.
		if err := requireFields(path,
			"ice_board_name", cfg.IceDispenseBoardName,
			"ice_pin_name", cfg.IceDispensePinName,
			"glass_vision_service_name", cfg.GlassVisionServiceName,
			"glass_observe_pose_switcher_name", cfg.GlassObservePoseSwitcherName,
			"glass_approach_relative_pose", cfg.GlassApproachRelativePose,
			"glass_grab_relative_pose", cfg.GlassGrabRelativePose,
		); err != nil {
			return nil, nil, err
		}
		if err := cfg.GlassDimensions.validate(path, "glass_dimensions"); err != nil {
			return nil, nil, err
		}
		reqDeps = append(reqDeps,
			vision.Named(cfg.GlassVisionServiceName).String(),
			cfg.GlassObservePoseSwitcherName,
		)
	}

	if cfg.CanServeIcedLatte {
		// The milk path is the iced flow plus a fridge trip, so it inherits every
		// iced requirement rather than restating them, and additionally needs the
		// handle offset open_door resolves the fridge grasp against.
		if !cfg.CanServeIced {
			return nil, nil, fmt.Errorf("%s: can_serve_iced_latte requires can_serve_iced (the latte is served in the iced glass, over ice)", path)
		}
		if cfg.DoorApproachRelativePose == nil {
			return nil, nil, fmt.Errorf("%s: can_serve_iced_latte requires door_approach_relative_pose (the milk is fetched from behind the fridge door)", path)
		}
		if err := requireFields(path,
			"milk_vision_service_name", cfg.MilkVisionServiceName,
			"milk_observe_pose_switcher_name", cfg.MilkObservePoseSwitcherName,
			"milk_approach_relative_pose", cfg.MilkApproachRelativePose,
			"milk_grab_relative_pose", cfg.MilkGrabRelativePose,
		); err != nil {
			return nil, nil, err
		}
		if err := cfg.MilkBottleDimensions.validate(path, "milk_bottle_dimensions"); err != nil {
			return nil, nil, err
		}
		reqDeps = append(reqDeps,
			vision.Named(cfg.MilkVisionServiceName).String(),
			cfg.MilkObservePoseSwitcherName,
		)
	}

	if cfg.IceDispenseBoardName != "" {
		optDeps = append(optDeps, board.Named(cfg.IceDispenseBoardName).String())
	}

	if err := validateKeepAlive(cfg, path); err != nil {
		return nil, nil, err
	}

	if cfg.ChoreWheel != nil {
		if cfg.SlackNotifierName == "" {
			return nil, nil, fmt.Errorf("%s: chore_wheel requires slack_notifier_name (the wheel is posted through it)", path)
		}
		if err := cfg.ChoreWheel.Validate(path); err != nil {
			return nil, nil, err
		}
	}

	return reqDeps, optDeps, nil
}
