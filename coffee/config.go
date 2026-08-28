package coffee

// Coffee service configuration: the Config struct and its validation, the typed
// values it carries (steps, relative poses, container dimensions), and the
// small helpers that resolve configured values to their defaults.

import (
	"fmt"
	"time"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/gripper"
	"go.viam.com/rdk/components/sensor"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/vision"
)

type StepLinearConstraint struct {
	LineToleranceMm          float64 `json:"line_tolerance_mm"`
	OrientationToleranceDegs float64 `json:"orientation_tolerance_degs"`
}

type AllowedCollision struct {
	Frame1 string `json:"frame1"`
	Frame2 string `json:"frame2"`
}

type StepMoveOptions struct {
	MaxVelDegsPerSec  float64 `json:"max_vel_degs_per_sec,omitempty"`
	MaxAccDegsPerSec2 float64 `json:"max_acc_degs_per_sec2,omitempty"`
}

type Step struct {
	PoseName            string                `json:"pose_name"`
	Pause               time.Duration         `json:"pause_secs,omitempty"`
	LinearConstraint    *StepLinearConstraint `json:"linear_constraint,omitempty"`
	MoveOptions         *StepMoveOptions      `json:"move_options,omitempty"`
	AllowedCollisions   []AllowedCollision    `json:"allowed_collisions,omitempty"`
	PivotFromPose       string                `json:"pivot_from_pose,omitempty"`
	PivotDegreesPerStep float64               `json:"pivot_degrees_per_step,omitempty"`

	// PivotExtraDegrees rotates past PoseName by this much along the same axis,
	// then unwinds back onto it — all in one planned trajectory. It compensates
	// for the gripper slipping on the portafilter handle while the bayonet is
	// under load: the arm must over-rotate for the filter to seat, and the
	// unwind re-zeroes the grip, since a seated filter out-holds the claws so
	// the handle slides back through them.
	PivotExtraDegrees float64 `json:"pivot_extra_degrees,omitempty"`

	// NoSpill routes this step's move through the level carry (carryHeldLevel)
	// rather than a direct plan
	NoSpill bool `json:"no_spill,omitempty"`

	// PoseSwitch is the switch this step's pose is read from (fetchPose).
	PoseSwitch toggleswitch.Switch `json:"-"`

	// Circular motion: move in small circles around PoseName to distribute
	// material (e.g. coffee grounds) evenly. The motion continues until
	// CircularDurationSec is exceeded.
	CircularRadiusMm     float64 `json:"circular_radius_mm,omitempty"`
	CircularDurationSec  float64 `json:"circular_duration_sec,omitempty"`
	CircularPointsPerRev int     `json:"circular_points_per_rev,omitempty"`
}

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
	BrewTimeSec               float64 `json:"brew_time_sec,omitempty"`
	LungoBrewTimeSec          float64 `json:"lungo_brew_time_sec,omitempty"`
	ButtonPressHoldSec        float64 `json:"button_press_hold_sec,omitempty"`
	GrindTimeSec              float64 `json:"grind_time_sec,omitempty"`
	GripperHoldMinPos         float64 `json:"gripper_hold_min_pos,omitempty"`
	GripperHoldMaxPos         float64 `json:"gripper_hold_max_pos,omitempty"`
	GripperOpenTimeoutSec     float64 `json:"gripper_open_timeout_sec,omitempty"`
	SlowMovementVelDegsPerSec float64 `json:"slow_movement_vel_degs_per_sec,omitempty"`
	PortafilterShakeSec       float64 `json:"portafilter_shake_sec,omitempty"`
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
	CupPhotosPerVantage           int           `json:"cup_photos_per_vantage,omitempty"`
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

	// NoSpillCarry, when true, carries a filled container along a straight line
	// broken into waypoints (one every defaultCarryWaypointSpacingMm) instead of
	// free-planning straight to the goal. Each intermediate waypoint commands the
	// held-item (container) frame with a goal pose cloud that keeps it close to
	// level so the drink doesn't slosh; the final waypoint is pinned exactly (see
	// carryHeldLevel in motion.go). It applies to every free traverse of a filled
	// container: the serving-area placement (placeFullCupOnShelf and the iced
	// glass), carrying the ice-filled glass to staging, and carrying the espresso
	// cup to the pour position. Off by default (those moves free-plan straight to
	// the goal pose).
	NoSpillCarry bool `json:"no_spill_carry,omitempty"`

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
	// orientation is also the grasp orientation the gripper holds through the
	// swing. Required to run open_door.
	DoorApproachRelativePose *RelativePose `json:"door_approach_relative_pose,omitempty"`

	// KeepAlive, when set, runs the idle-purge loop (keepalive.go) that holds the
	// machine's 1 CUP button periodically so it never falls out of brew
	// temperature. Requires HasSeparateBrewButtons. Unset disables it.
	KeepAlive *KeepAlive `json:"keepalive,omitempty"`
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
	if s.cfg != nil && s.cfg.MaxBatchSize > 0 {
		return s.cfg.MaxBatchSize
	}
	return defaultMaxBatchSize
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

// pickupPhotosPerVantage returns the number of vision frames to capture at each
// observation pose, defaulting to 1.
func pickupPhotosPerVantage(configured int) int {
	return orDefault(configured, 1)
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

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.PoseSwitcherName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "pose_switcher_name")
	}
	if cfg.ClawsPoseSwitcherName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "claws_pose_switcher_name")
	}
	if cfg.ArmName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm_name")
	}
	if cfg.GripperName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "gripper_name")
	}
	reqDeps := []string{cfg.PoseSwitcherName, cfg.ClawsPoseSwitcherName, framesystem.PublicServiceName.String(), arm.Named(cfg.ArmName).String(), gripper.Named(cfg.GripperName).String()}

	var optDeps []string
	if cfg.SpeechServiceName != "" {
		optDeps = append(optDeps, generic.Named(cfg.SpeechServiceName).String())
	}
	if cfg.OrderSensorName != "" {
		optDeps = append(optDeps, sensor.Named(cfg.OrderSensorName).String())
	}
	if cfg.UsageSensorName != "" {
		optDeps = append(optDeps, sensor.Named(cfg.UsageSensorName).String())
	}
	if cfg.CamStorageMuxName != "" {
		optDeps = append(optDeps, generic.Named(cfg.CamStorageMuxName).String())
	}
	if cfg.SlackNotifierName != "" {
		optDeps = append(optDeps, generic.Named(cfg.SlackNotifierName).String())
	}
	if cfg.CustomerDetectorName != "" {
		optDeps = append(optDeps, generic.Named(cfg.CustomerDetectorName).String())
	}
	if cfg.DeliveryHandlerName != "" {
		optDeps = append(optDeps, generic.Named(cfg.DeliveryHandlerName).String())
	}

	if cfg.CupVisionServiceName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "cup_vision_service_name")
	}
	if cfg.SrcCameraName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "src_camera_name")
	}
	if cfg.CameraObservePoseSwitcherName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "camera_observe_pose_switcher_name")
	}
	if cfg.CupApproachRelativePose == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "cup_approach_relative_pose")
	}
	if cfg.CupGrabRelativePose == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "cup_grab_relative_pose")
	}
	if cfg.ServingApproachRelativePose == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "serving_approach_relative_pose")
	}
	if cfg.ServingGrabRelativePose == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "serving_grab_relative_pose")
	}
	if cfg.CupPhotosPerVantage < 0 {
		return nil, nil, fmt.Errorf("%s: cup_photos_per_vantage must be >= 0", path)
	}
	if cfg.CupPickupMaxAttempts < 0 {
		return nil, nil, fmt.Errorf("%s: cup_pickup_max_attempts must be >= 0", path)
	}
	// The picked-up cup is always tracked as a held item, and its geometry is
	// modeled from these dimensions, so they must be configured.
	if err := cfg.CupDimensions.validate(path, "cup_dimensions"); err != nil {
		return nil, nil, err
	}
	reqDeps = append(reqDeps,
		vision.Named(cfg.CupVisionServiceName).String(),
		camera.Named(cfg.SrcCameraName).String(),
		cfg.CameraObservePoseSwitcherName,
	)

	if cfg.CanServeIced {
		if cfg.IceDispenseBoardName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "ice_board_name")
		}
		if cfg.IceDispensePinName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "ice_pin_name")
		}
		// Iced coffee fetches a glass via its own vision pipeline (the glass is
		// always vision-detected, reusing the cup camera src_camera_name).
		if cfg.GlassVisionServiceName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "glass_vision_service_name")
		}
		if cfg.GlassObservePoseSwitcherName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "glass_observe_pose_switcher_name")
		}
		if cfg.GlassApproachRelativePose == nil {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "glass_approach_relative_pose")
		}
		if cfg.GlassGrabRelativePose == nil {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "glass_grab_relative_pose")
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
		if cfg.MilkVisionServiceName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "milk_vision_service_name")
		}
		if cfg.MilkObservePoseSwitcherName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "milk_observe_pose_switcher_name")
		}
		if cfg.MilkApproachRelativePose == nil {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "milk_approach_relative_pose")
		}
		if cfg.MilkGrabRelativePose == nil {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "milk_grab_relative_pose")
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

	return reqDeps, optDeps, nil
}
