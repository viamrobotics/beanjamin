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
	PoseSwitcherName          string  `json:"pose_switcher_name"`
	ClawsPoseSwitcherName     string  `json:"claws_pose_switcher_name"`
	ArmName                   string  `json:"arm_name"`
	GripperName               string  `json:"gripper_name"`
	SpeechServiceName         string  `json:"speech_service_name,omitempty"`
	VizURL                    string  `json:"viz_url,omitempty"`
	BrewTimeSec               float64 `json:"brew_time_sec,omitempty"`
	LungoBrewTimeSec          float64 `json:"lungo_brew_time_sec,omitempty"`
	GrindTimeSec              float64 `json:"grind_time_sec,omitempty"`
	GripperHoldMinPos         float64 `json:"gripper_hold_min_pos,omitempty"`
	GripperHoldMaxPos         float64 `json:"gripper_hold_max_pos,omitempty"`
	SlowMovementVelDegsPerSec float64 `json:"slow_movement_vel_degs_per_sec,omitempty"`
	PortafilterShakeSec       float64 `json:"portafilter_shake_sec,omitempty"`
	SaveMotionRequestsDir     string  `json:"save_motion_requests_dir,omitempty"`
	OrderSensorName           string  `json:"order_sensor_name,omitempty"`

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

	// Optional Slack notifier (viam:notifications:slack generic service). When
	// set, the coffee service sends a best-effort Slack message via DoCommand
	// for every non-successful order attempt — genuine faults and operator
	// cancels alike. Unset disables notifications.
	SlackNotifierName string `json:"slack_notifier_name,omitempty"`

	// CustomerDetectorName: customer-detector that completed orders are credited
	// to, for "the usual". Unset disables recording.
	CustomerDetectorName string `json:"customer_detector_name,omitempty"`

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
	// CupDimensions optionally overrides the cup size derived from the
	// detection point cloud with a known diameter/height (see
	// ContainerDimensions). Unset keeps the point-cloud-derived size.
	CupDimensions *ContainerDimensions `json:"cup_dimensions,omitempty"`

	// Glass pickup (iced coffee) mirrors cup pickup but with its own vision
	// service and observe-pose switch, tuned for the taller iced-coffee glass.
	// These fields are required when can_serve_iced is set.
	GlassVisionServiceName       string        `json:"glass_vision_service_name,omitempty"`
	GlassObservePoseSwitcherName string        `json:"glass_observe_pose_switcher_name,omitempty"`
	GlassApproachRelativePose    *RelativePose `json:"glass_approach_relative_pose,omitempty"`
	GlassGrabRelativePose        *RelativePose `json:"glass_grab_relative_pose,omitempty"`
	// GlassDimensions optionally overrides the glass size derived from the
	// detection point cloud with a known diameter/height (see
	// ContainerDimensions). Unset keeps the point-cloud-derived size.
	GlassDimensions *ContainerDimensions `json:"glass_dimensions,omitempty"`

	// Serving placement offsets are composed onto the serving-area slot anchor
	// when releasing a finished drink onto the served shelf. The same pair is
	// used for both the hot cup and the iced glass. Both are required.
	ServingApproachRelativePose *RelativePose `json:"serving_approach_relative_pose,omitempty"`
	ServingGrabRelativePose     *RelativePose `json:"serving_grab_relative_pose,omitempty"`

	// TrackHeldGeometry, when true, attaches the vision-detected geometry of a
	// picked-up cup/glass to the gripper frame in the cached frame system, so
	// motion planning routes around the held item until it is set down (see
	// held_geometry.go). The geometry comes from the pickup vision detection.
	// Off by default.
	TrackHeldGeometry bool `json:"track_held_geometry,omitempty"`

	// NoSpillCarry, when true, carries the brewed cup from under the machine to
	// the serving-area shelf along a straight line broken into waypoints (one
	// every defaultCarryWaypointSpacingMm). Each waypoint commands the held-item
	// (container) frame, interpolating from the container's upright start pose to
	// the approach pose, with a goal pose cloud keeping it close to level so the
	// held drink doesn't slosh (see carryHeldLevel in motion.go). Only affects the
	// carry move in placeFullCupOnShelf; it commands the held-item frame, so it
	// requires TrackHeldGeometry=true. Off by default (the carry free-plans
	// straight to the approach pose).
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
}

// defaultMaxBatchSize is used when Config.MaxBatchSize is unset or zero.
const defaultMaxBatchSize = 10

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

// ContainerDimensions is an optional, operator-supplied size for a picked-up
// container (cup or glass). When set on the coffee service config
// (cup_dimensions / glass_dimensions), it replaces the size derived from the
// detection point cloud: the held-item bounding box is built with
// width = depth = DiameterMm and height = HeightMm, centered on the grasp
// centroid (the point the gripper is sent to) rather than on the point-cloud
// midpoint. The grasp centroid itself is unaffected — only the
// collision/visualization geometry changes. Round containers (cups/glasses)
// are well approximated by a square-footprint box of the rim diameter, and a
// known size centered on the grasp point avoids the point cloud under-reading or
// skewing the box for a partially-observed container. Unset (the default) keeps
// the point-cloud-derived dimensions.
type ContainerDimensions struct {
	DiameterMm float64 `json:"diameter_mm"`
	HeightMm   float64 `json:"height_mm"`
}

// validate checks an optional ContainerDimensions override: a nil override is
// allowed (point-cloud dimensions are used), but when present both diameter and
// height must be positive. field is the JSON config key for error messages.
func (d *ContainerDimensions) validate(path, field string) error {
	if d == nil {
		return nil
	}
	if d.DiameterMm <= 0 {
		return fmt.Errorf("%s: %s.diameter_mm must be > 0", path, field)
	}
	if d.HeightMm <= 0 {
		return fmt.Errorf("%s: %s.height_mm must be > 0", path, field)
	}
	return nil
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
	if err := cfg.CupDimensions.validate(path, "cup_dimensions"); err != nil {
		return nil, nil, err
	}
	if err := cfg.GlassDimensions.validate(path, "glass_dimensions"); err != nil {
		return nil, nil, err
	}
	reqDeps = append(reqDeps,
		vision.Named(cfg.CupVisionServiceName).String(),
		camera.Named(cfg.SrcCameraName).String(),
		cfg.CameraObservePoseSwitcherName,
	)

	if cfg.NoSpillCarry && !cfg.TrackHeldGeometry {
		return nil, nil, fmt.Errorf("%s: no_spill_carry commands the held-item (container) frame, so it requires track_held_geometry=true", path)
	}

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
		reqDeps = append(reqDeps,
			vision.Named(cfg.GlassVisionServiceName).String(),
			cfg.GlassObservePoseSwitcherName,
		)
	}
	if cfg.IceDispenseBoardName != "" {
		optDeps = append(optDeps, board.Named(cfg.IceDispenseBoardName).String())
	}

	return reqDeps, optDeps, nil
}
