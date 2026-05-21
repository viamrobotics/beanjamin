package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	viz "github.com/viam-labs/motion-tools/client/client"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/gripper"
	"go.viam.com/rdk/components/sensor"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/vision"

	// Register the multi-poses-execution-switch model.
	_ "beanjamin/multiposesexecutionswitch"
)

var Coffee = resource.NewModel("viam", "beanjamin", "coffee")

func init() {
	resource.RegisterService(generic.API, Coffee,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newBeanjaminCoffee,
		},
	)
}

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
	Component           string                `json:"component,omitempty"`

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
	SlowMovementVelDegsPerSec float64 `json:"slow_movement_vel_degs_per_sec,omitempty"`
	PlaceCup                  bool    `json:"place_cup,omitempty"`
	CleanAfterUse             bool    `json:"clean_after_use,omitempty"`
	PortafilterShakeSec       float64 `json:"portafilter_shake_sec,omitempty"`
	SaveMotionRequestsDir     string  `json:"save_motion_requests_dir,omitempty"`
	OrderSensorName           string  `json:"order_sensor_name,omitempty"`

	CamStorageMuxName string `json:"cam_storage_mux_name,omitempty"`
	DataDir           string `json:"data_dir,omitempty"`
	CanServeDecaf     bool   `json:"can_serve_decaf,omitempty"`

	// Conversational, when true, makes the coffee service speak its own
	// status-narrating lines through speech_service_name — initial
	// greetings, almost-ready prompts, order confirmations, rejection
	// quips, etc. When false (the default), the service stays silent
	// except for the drink-ready announcement at cup handoff, leaving
	// everything else for an external orchestrator (e.g. voice-command)
	// to handle.
	Conversational bool `json:"conversational,omitempty"`

	// Dynamic cup pickup. When true, setCupForCoffee uses vision-driven
	// detection to find the cup; when false, the existing static pickup
	// (empty_cup_approach -> empty_cup) is used.
	DynamicCupPickup           bool          `json:"dynamic_cup_pickup,omitempty"`
	CupVisionServiceName       string        `json:"cup_vision_service_name,omitempty"`
	SrcCameraName              string        `json:"src_camera_name,omitempty"`
	ExpectedCupPositionMm      *Vec3Mm       `json:"expected_cup_position_mm,omitempty"`
	CupApproachRelativePose    *RelativePose `json:"cup_approach_relative_pose,omitempty"`
	CupGrabRelativePose        *RelativePose `json:"cup_grab_relative_pose,omitempty"`
	CupMaxDistanceFromTargetMm float64       `json:"cup_max_distance_from_target_mm,omitempty"`
	CupDetectionRetries        int           `json:"cup_detection_retries,omitempty"`
	CupDetectionRetrySleepMs   int           `json:"cup_detection_retry_sleep_ms,omitempty"`
	// CupObserveOffsets are extra observation vantages, each composed onto
	// cup_observe in its local frame ({x:60} = 60mm along the camera's X).
	CupObserveOffsets []RelativePose `json:"cup_observe_offsets,omitempty"`
	// CupPickupMaxAttempts caps how many full observe-and-grab attempts
	// pickCupDynamic will make per order. Each attempt re-detects, then
	// walks the candidate list (closest first), falling through to the
	// next candidate on planning failures. Defaults to 3.
	CupPickupMaxAttempts int `json:"cup_pickup_max_attempts,omitempty"`
	// CupCentroidMinZMm clamps each detection's world-frame Z up to this
	// value when the detected Z falls below it. Use to recover from depth
	// noise that puts the centroid slightly below the physical cup base
	// and trips the planner. Zero (default) disables clamping.
	CupCentroidMinZMm float64 `json:"cup_centroid_min_z_mm,omitempty"`

	// PlaceCupOnShelf, when true, replaces giveFullCupToCustomer with
	// Requires DynamicCupPickup=true.
	PlaceCupOnShelf bool `json:"place_cup_on_shelf,omitempty"`

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

// cupPickupMaxAttempts returns the configured cap on full observe-and-grab
// attempts per order, falling back to defaultCupPickupMaxAttempts.
func (s *beanjaminCoffee) cupPickupMaxAttempts() int {
	if s.cfg != nil && s.cfg.CupPickupMaxAttempts > 0 {
		return s.cfg.CupPickupMaxAttempts
	}
	return defaultCupPickupMaxAttempts
}

// Vec3Mm is a 3D point in millimeters used for world-frame configuration.
type Vec3Mm struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
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
	if cfg.CamStorageMuxName != "" {
		optDeps = append(optDeps, generic.Named(cfg.CamStorageMuxName).String())
	}

	if cfg.DynamicCupPickup {
		if cfg.CupVisionServiceName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "cup_vision_service_name")
		}
		if cfg.SrcCameraName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "src_camera_name")
		}
		if cfg.ExpectedCupPositionMm == nil {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "expected_cup_position_mm")
		}
		if cfg.CupApproachRelativePose == nil {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "cup_approach_relative_pose")
		}
		if cfg.CupGrabRelativePose == nil {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "cup_grab_relative_pose")
		}
		if cfg.CupDetectionRetries < 0 {
			return nil, nil, fmt.Errorf("%s: cup_detection_retries must be >= 0", path)
		}
		if cfg.CupPickupMaxAttempts < 0 {
			return nil, nil, fmt.Errorf("%s: cup_pickup_max_attempts must be >= 0", path)
		}
		if cfg.CupMaxDistanceFromTargetMm == 0 {
			cfg.CupMaxDistanceFromTargetMm = 300
		}
		reqDeps = append(reqDeps,
			vision.Named(cfg.CupVisionServiceName).String(),
			camera.Named(cfg.SrcCameraName).String(),
		)
	}

	if cfg.PlaceCupOnShelf && !cfg.DynamicCupPickup {
		return nil, nil, fmt.Errorf("%s: place_cup_on_shelf requires dynamic_cup_pickup=true", path)
	}

	return reqDeps, optDeps, nil
}

type beanjaminCoffee struct {
	resource.AlwaysRebuild

	name                   resource.Name
	logger                 logging.Logger
	cfg                    *Config
	filterSw               toggleswitch.Switch
	clawsSw                toggleswitch.Switch
	arm                    arm.Arm
	fsSvc                  framesystem.Service
	cachedFS               *referenceframe.FrameSystem // cached frame system, mutated at lock/unlock
	speech                 resource.Resource           // nil when speech_service_name is not configured
	vizEnabled             bool                        // true when viz_url is configured
	vizConsecutiveFailures int                         // auto-disables viz after repeated failures
	gripper                gripper.Gripper
	camStorage             generic.Service // optional; mux over video stores; nil if cam_storage_mux_name unset
	pendingOrderClipsDir   string          // optional; directory for pending-clip records to survive restarts
	mu                     sync.Mutex
	cancelCtx              context.Context
	cancelFunc             func()
	running                atomic.Bool
	currentStep            atomic.Value // string: current step label for the active order (debug)
	currentOrderID         atomic.Value // string: ID of the order currently being processed; "" when idle
	queue                  *OrderQueue
	queueStop              chan struct{}
	paused                 atomic.Bool
	// portafilterInMachine is true between releaseFilter and grabFilter:
	// the bayonet holds the filter and the arm is free. Cancel uses this
	// to decide whether recovery (re-grip + clean + home) is required.
	portafilterInMachine atomic.Bool
	// portafilterHasGrounds is true once grinding has put grounds in the
	// filter, until cleanPortafilter clears them. Cancel uses this (when
	// portafilterInMachine is false) to drive a clean + home recovery so
	// the filter doesn't get stranded with grounds in it.
	portafilterHasGrounds atomic.Bool
	orderSensorSink       orderSensorSink // optional; named order-sensor from deps, nil if unset
	cupVision             vision.Service  // optional; nil when DynamicCupPickup=false
	cupCameraName         string          // SrcCameraName, validated to exist in cachedFS
	servedShelfTile       atomic.Value    // servedShelfTile holds the latest servedShelfTilePick chosen by
}

func newBeanjaminCoffee(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}
	return NewCoffee(ctx, deps, rawConf.ResourceName(), conf, logger)
}

func NewCoffee(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *Config, logger logging.Logger) (resource.Resource, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	switchRes, ok := deps[toggleswitch.Named(conf.PoseSwitcherName)]
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("switch %q not found in dependencies", conf.PoseSwitcherName)
	}
	filterSw, ok := switchRes.(toggleswitch.Switch)
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("resource %q is not a switch", conf.PoseSwitcherName)
	}

	clawSwRes, ok := deps[toggleswitch.Named(conf.ClawsPoseSwitcherName)]
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("claws switch %q not found in dependencies", conf.ClawsPoseSwitcherName)
	}
	clawSw, ok := clawSwRes.(toggleswitch.Switch)
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("resource %q is not a switch", conf.ClawsPoseSwitcherName)
	}

	armComp, err := arm.FromProvider(deps, conf.ArmName)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("arm %q not found in dependencies: %w", conf.ArmName, err)
	}

	gripperComp, err := gripper.FromProvider(deps, conf.GripperName)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("gripper %q not found in dependencies: %w", conf.GripperName, err)
	}

	fsSvc, err := framesystem.FromDependencies(deps)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("frame system service not found in dependencies: %w", err)
	}

	cachedFS, err := framesystem.NewFromService(ctx, fsSvc, nil)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("build initial frame system: %w", err)
	}

	if err := applyJointLimits(logger, cachedFS, conf.InputRangeOverride); err != nil {
		cancelFunc()
		return nil, fmt.Errorf("apply joint limits: %w", err)
	}

	var cupVision vision.Service
	var cupCameraName string
	if conf.DynamicCupPickup {
		visRes, err := vision.FromProvider(deps, conf.CupVisionServiceName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("cup vision service %q: %w", conf.CupVisionServiceName, err)
		}
		cupVision = visRes
		if cachedFS.Frame(conf.SrcCameraName) == nil {
			cancelFunc()
			return nil, fmt.Errorf("src_camera_name %q not found in frame system — add the camera to the frame system fragment", conf.SrcCameraName)
		}
		cupCameraName = conf.SrcCameraName
		logger.Infof("dynamic cup pickup enabled (vision=%q, camera=%q)", conf.CupVisionServiceName, conf.SrcCameraName)
	}

	var speech resource.Resource
	if conf.SpeechServiceName != "" {
		speechRes, ok := deps[generic.Named(conf.SpeechServiceName)]
		if ok {
			speech = speechRes
		}
		if speech != nil {
			logger.Infof("speech service %q connected", conf.SpeechServiceName)
		} else {
			logger.Warnf("speech service %q configured but not available", conf.SpeechServiceName)
		}
	}

	var camStorage generic.Service
	if conf.CamStorageMuxName != "" {
		mux, err := generic.FromProvider(deps, conf.CamStorageMuxName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("cam_storage_mux_name %q: %w", conf.CamStorageMuxName, err)
		}
		camStorage = mux
		logger.Infof("cam storage mux %q connected", conf.CamStorageMuxName)
	}

	var pendingOrderClipsDir string
	if conf.DataDir != "" {
		pendingOrderClipsDir = filepath.Join(conf.DataDir, "pending-clips")
		if err := os.MkdirAll(pendingOrderClipsDir, 0o755); err != nil {
			cancelFunc()
			return nil, fmt.Errorf("data_dir %q: %w", conf.DataDir, err)
		}
		logger.Infof("cam storage: pending-clip records will be written to %s", pendingOrderClipsDir)
	} else {
		logger.Infof("cam storage: no data_dir configured — pending-clip records disabled (interrupted orders will not be recoverable)")
	}

	vizEnabled := false
	if conf.VizURL != "" {
		viz.SetURL(conf.VizURL)
		vizEnabled = true
		logger.Infof("viz client configured at %s", conf.VizURL)
	}

	var sink orderSensorSink
	if conf.OrderSensorName != "" {
		// Same component instance as elsewhere on the robot (not a copy).
		sen, err := sensor.FromProvider(deps, conf.OrderSensorName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("order sensor %q: %w", conf.OrderSensorName, err)
		}
		s, ok := sen.(orderSensorSink)
		if !ok {
			cancelFunc()
			return nil, fmt.Errorf("resource %q must be model viam:beanjamin:order-sensor", conf.OrderSensorName)
		}
		sink = s
		logger.Infof("order sensor %q connected", conf.OrderSensorName)
	}

	s := &beanjaminCoffee{
		name:                 name,
		logger:               logger,
		cfg:                  conf,
		filterSw:             filterSw,
		clawsSw:              clawSw,
		arm:                  armComp,
		fsSvc:                fsSvc,
		cachedFS:             cachedFS,
		speech:               speech,
		camStorage:           camStorage,
		pendingOrderClipsDir: pendingOrderClipsDir,
		gripper:              gripperComp,
		vizEnabled:           vizEnabled,
		cancelCtx:            cancelCtx,
		cancelFunc:           cancelFunc,
		queue:                NewOrderQueue(),
		queueStop:            make(chan struct{}),
		orderSensorSink:      sink,
		cupVision:            cupVision,
		cupCameraName:        cupCameraName,
	}
	go s.processQueue()
	return s, nil
}

func (s *beanjaminCoffee) Name() resource.Name {
	return s.name
}

func (s *beanjaminCoffee) setStep(step string) {
	s.currentStep.Store(step)
	if id, ok := s.currentOrderID.Load().(string); ok && id != "" {
		s.queue.SetStep(id, step)
	}
}

func (s *beanjaminCoffee) Status(ctx context.Context) (map[string]interface{}, error) {
	_, span := trace.StartSpan(ctx, "beanjamin::Status")
	defer span.End()
	orders := s.queue.List()
	// structpb.NewStruct (used by RDK to serialize Status over the wire) only
	// accepts []interface{} for list values, not []map[string]interface{}, so
	// the slice element type must be interface{}.
	orderMaps := make([]interface{}, len(orders))
	for i, o := range orders {
		// structpb.NewStruct rejects []map[string]interface{} as list values,
		// so step_history must also be []interface{}.
		history := make([]interface{}, len(o.StepHistory))
		for j, e := range o.StepHistory {
			history[j] = map[string]interface{}{
				"step":       e.Step,
				"started_at": e.StartedAt.Format(time.RFC3339),
			}
		}
		// Empty string when the order is still pending; the frontend uses
		// completed_at presence as the signal to render the green ready card.
		completedAt := ""
		if !o.CompletedAt.IsZero() {
			completedAt = o.CompletedAt.Format(time.RFC3339)
		}
		orderMaps[i] = map[string]interface{}{
			"id":            o.ID,
			"drink":         o.Drink,
			"customer_name": o.CustomerName,
			"enqueued_at":   o.EnqueuedAt.Format(time.RFC3339),
			"raw_step":      o.RawStep,
			"step_history":  history,
			"completed_at":  completedAt,
		}
	}
	step, _ := s.currentStep.Load().(string)
	resp := map[string]interface{}{
		// count reports pending depth only — orders waiting to be made.
		// Recently-completed orders are visible in `orders` but don't add
		// to depth. Returned as float64 so in-process callers see the
		// same type as gRPC callers (structpb forces all numbers to
		// double on the wire).
		"count":           float64(s.queue.Len()),
		"orders":          orderMaps,
		"is_paused":       s.paused.Load(),
		"is_busy":         s.running.Load(),
		"current_step":    step,
		"can_serve_decaf": s.cfg.CanServeDecaf,
	}
	s.logger.Debugw("Status", "response", resp)
	return resp, nil
}

func (s *beanjaminCoffee) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	ctx, span := trace.StartSpan(ctx, "beanjamin::DoCommand")
	defer span.End()
	if orderRaw, ok := cmd["prepare_order"]; ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::prepare_order")
		defer cmdSpan.End()
		res, err := s.enqueueOrder(ctx, orderRaw)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if actionName, ok := cmd["execute_action"].(string); ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::execute_action["+actionName+"]")
		defer cmdSpan.End()
		res, err := s.executeAction(ctx, actionName)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if _, ok := cmd["cancel"]; ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::cancel")
		defer cmdSpan.End()
		res, err := s.cancel(ctx)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if _, ok := cmd["get_queue"]; ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::get_queue")
		defer cmdSpan.End()
		return s.Status(ctx)
	}
	if _, ok := cmd["proceed"]; ok {
		_, cmdSpan := trace.StartSpan(ctx, "beanjamin::proceed")
		defer cmdSpan.End()
		return s.proceedQueue()
	}
	if _, ok := cmd["clear_queue"]; ok {
		_, cmdSpan := trace.StartSpan(ctx, "beanjamin::clear_queue")
		defer cmdSpan.End()
		return s.clearQueue()
	}
	if _, ok := cmd["cleanup_pending_clips"]; ok {
		_, cmdSpan := trace.StartSpan(ctx, "beanjamin::cleanup_pending_clips")
		defer cmdSpan.End()
		return s.cleanupPendingClips()
	}

	if _, ok := cmd["reset_world"]; ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::reset_world")
		defer cmdSpan.End()
		res, err := s.resetWorld(ctx)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	// Stream deck key commands
	if action, ok := cmd["action"].(string); ok {
		ctx, cmdSpan := trace.StartSpan(ctx, "beanjamin::action["+action+"]")
		defer cmdSpan.End()
		switch action {
		case "open_gripper":
			return s.handleOpenGripper(ctx)
		case "close_gripper":
			return s.handleCloseGripper(ctx)
		default:
			return nil, fmt.Errorf("unknown action %q", action)
		}
	}
	err := fmt.Errorf("unknown command, supported commands: cancel, prepare_order, execute_action, get_queue, proceed, clear_queue, cleanup_pending_clips, reset_world, action")
	s.logger.Warnw("DoCommand", "error", err)
	return nil, err
}

func (s *beanjaminCoffee) proceedQueue() (map[string]interface{}, error) {
	select {
	case s.queue.proceed <- struct{}{}:
		return map[string]interface{}{"status": "resumed"}, nil
	default:
		return nil, errors.New("not currently paused between orders")
	}
}

func (s *beanjaminCoffee) clearQueue() (map[string]interface{}, error) {
	removed := s.queue.Clear()
	s.logger.Infof("cleared %d orders from queue", removed)
	return map[string]interface{}{"status": "cleared", "removed": removed}, nil
}

// resetCancelWaitTimeout caps how long resetWorld waits for a running sequence
// to observe its cancelled context and return. Generous enough to cover any
// motion-plan cleanup; if exceeded, something is wedged and the operator
// should look at logs rather than have reset_world appear to "succeed".
const resetCancelWaitTimeout = 30 * time.Second

// resetWorld brings the service back to an idle state from anywhere: cancels a
// running sequence (waiting for it to actually stop), clears any pending and
// recently-completed orders, rebuilds the cached frame system from the service
// (discarding mid-cycle mutations like a portafilter frame reparented to world
// by lockFilterFrame), and releases the cancel-induced queue pause so
// processQueue is ready for new orders. Each step is best-effort and skipped
// when not applicable, so it is safe to call from any state.
func (s *beanjaminCoffee) resetWorld(ctx context.Context) (map[string]interface{}, error) {
	cancelled := s.signalCancel()
	if cancelled {
		if err := s.waitForIdle(ctx, resetCancelWaitTimeout); err != nil {
			return nil, fmt.Errorf("reset_world: %w", err)
		}
	}

	removed := s.queue.Clear()

	// reset_world is an operator's "everything is fine, start over" button.
	// Clear the portafilter state flags so a subsequent cancel doesn't try
	// to run recovery against a state that no longer matches reality.
	s.portafilterInMachine.Store(false)
	s.portafilterHasGrounds.Store(false)

	if err := s.resetFrameSystem(ctx); err != nil {
		return nil, fmt.Errorf("reset_world: %w", err)
	}

	unpaused := false
	if s.paused.Load() {
		select {
		case s.queue.proceed <- struct{}{}:
		default:
			// Buffered slot is full — a proceed signal is already pending and
			// will be consumed by processQueue. Either way, the unpause was
			// requested.
		}
		unpaused = true
	}

	s.logger.Infof("reset_world: cancelled=%v cleared=%d unpaused=%v frame_system_reset=true",
		cancelled, removed, unpaused)
	return map[string]interface{}{
		"status":    "reset",
		"cancelled": cancelled,
		"cleared":   removed,
		"unpaused":  unpaused,
	}, nil
}

// signalCancel interrupts any in-flight motion by cancelling the shared
// cancelCtx and pausing the queue. Returns true if a sequence was running.
// Does not wait for the running goroutine to observe the cancellation.
func (s *beanjaminCoffee) signalCancel() bool {
	if !s.running.Load() {
		return false
	}
	s.paused.Store(true)
	s.mu.Lock()
	s.cancelFunc()
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())
	s.mu.Unlock()
	return true
}

// waitForIdle polls until s.running flips back to false (meaning the cancelled
// sequence has fully unwound through its defers) or the timeout / ctx expires.
func (s *beanjaminCoffee) waitForIdle(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for s.running.Load() {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for sequence to stop", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

const cancelAnnouncement = "Cancelling the current order. I'll clean up if needed and return to home. Click proceed when you're ready for the next order."

// cancel interrupts any running sequence and drives whichever recovery the
// current state requires so the operator does not need a follow-up reset_world:
//   - portafilter locked in the machine (post-releaseFilter, pre-grabFilter):
//     grab → unlock → clean → home.
//   - portafilter held by the arm with grounds in it (post-grindCoffee,
//     pre-cleanPortafilter, and not in the machine): clean → home.
//   - otherwise: no recovery motion (queue paused, frame system reset).
//
// The frame system is rebuilt at the end to discard any lockFilterFrame
// mutation. The queue is left paused with its pending orders intact; send
// 'proceed' to resume. If recovery motion fails, the frame system is left
// untouched and the flags remain set so a subsequent cancel can retry.
//
// Known limitation: a cancel that fires mid-lockPortaFilter (between the
// motion entering the machine and releaseFilter's gripper.Open) may try to
// route the arm away while the bayonet is partially engaged. There is no
// safe automated recovery for that narrow window — the operator must
// intervene manually.
func (s *beanjaminCoffee) cancel(ctx context.Context) (map[string]interface{}, error) {
	cancelled := s.signalCancel()
	if cancelled {
		if err := s.waitForIdle(ctx, resetCancelWaitTimeout); err != nil {
			return nil, fmt.Errorf("cancel: %w", err)
		}
	}

	// Take exclusive ownership of the arm before any recovery motion so
	// other commands (execute_action, prepare_order consumer) can't race.
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("cancel: another sequence is running")
	}
	defer s.running.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	// Announce the cancellation up front so the operator hears what's
	// happening before any recovery motion begins. Only speak when there
	// is something to actually cancel/recover — silence on a no-op cancel.
	if cancelled || s.portafilterInMachine.Load() || s.portafilterHasGrounds.Load() {
		if err := s.sayAlways(ctx, cancelAnnouncement); err != nil {
			s.logger.Warnf("cancel: failed to announce cancellation: %v", err)
		}
	}

	recovered := false
	switch {
	case s.portafilterInMachine.Load():
		s.logger.Infof("cancel: portafilter is in the machine — running recovery (grab → unlock → clean → home)")
		s.setStep("Recovering filter")
		if err := s.grabFilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("cancel: recovery grab_filter: %w", err)
		}
		s.setStep("Unlocking portafilter")
		if err := s.unlockPortaFilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("cancel: recovery unlock_portafilter: %w", err)
		}
		s.setStep("Cleaning")
		if err := s.cleanPortafilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("cancel: recovery clean_portafilter: %w", err)
		}
		s.setStep("Finishing up")
		homeStep := Step{PoseName: "home", Component: "filter"}
		if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
			return nil, fmt.Errorf("cancel: recovery home: %w", err)
		}
		s.portafilterInMachine.Store(false)
		recovered = true
	case s.portafilterHasGrounds.Load():
		s.logger.Infof("cancel: portafilter has grounds — running recovery (clean → home)")
		s.setStep("Cleaning")
		if err := s.cleanPortafilter(ctx, cancelCtx); err != nil {
			return nil, fmt.Errorf("cancel: recovery clean_portafilter: %w", err)
		}
		s.setStep("Finishing up")
		homeStep := Step{PoseName: "home", Component: "filter"}
		if err := s.executeStep(ctx, cancelCtx, homeStep); err != nil {
			return nil, fmt.Errorf("cancel: recovery home: %w", err)
		}
		// cleanPortafilter already cleared portafilterHasGrounds on success.
		recovered = true
	}

	if err := s.resetFrameSystem(ctx); err != nil {
		return nil, fmt.Errorf("cancel: %w", err)
	}

	s.currentStep.Store("")
	s.logger.Infof("cancel: cancelled=%v recovered=%v — queue paused, send 'proceed' to resume",
		cancelled, recovered)
	return map[string]interface{}{
		"status":    "cancelled",
		"cancelled": cancelled,
		"recovered": recovered,
		"queue":     "paused",
	}, nil
}

func (s *beanjaminCoffee) handleOpenGripper(ctx context.Context) (map[string]interface{}, error) {
	if s.gripper == nil {
		return nil, fmt.Errorf("no gripper configured")
	}
	if err := s.gripper.Open(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to open gripper: %w", err)
	}
	return map[string]interface{}{"status": "opened"}, nil
}

func (s *beanjaminCoffee) handleCloseGripper(ctx context.Context) (map[string]interface{}, error) {
	if s.gripper == nil {
		return nil, fmt.Errorf("no gripper configured")
	}
	grabbed, err := s.gripper.Grab(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to close gripper: %w", err)
	}
	return map[string]interface{}{"status": "closed", "grabbed": grabbed}, nil
}

func (s *beanjaminCoffee) Close(context.Context) error {
	close(s.queueStop)
	s.cancelFunc()
	return nil
}
