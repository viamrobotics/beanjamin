package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	viz "github.com/viam-labs/motion-tools/client/client"
	"go.viam.com/rdk/components/arm"

	"go.viam.com/rdk/components/gripper"
	"go.viam.com/rdk/components/sensor"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	generic "go.viam.com/rdk/services/generic"

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

	CamStorageMuxName    string `json:"cam_storage_mux_name,omitempty"`
	PendingOrderClipsDir string `json:"pending_order_clips_dir,omitempty"`
	CanServeDecaf        bool   `json:"can_serve_decaf,omitempty"`

	InputRangeOverride map[string]map[string]JointLimitDegs `json:"input_range_override,omitempty"`
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
	orderSensorSink        orderSensorSink // optional; named order-sensor from deps, nil if unset
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
		zc, err := generic.FromProvider(deps, conf.CamStorageMuxName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("cam_storage_mux_name %q: %w", conf.CamStorageMuxName, err)
		}
		camStorage = zc
		logger.Infof("cam storage mux %q connected", conf.CamStorageMuxName)
	}

	if conf.PendingOrderClipsDir != "" {
		if err := os.MkdirAll(conf.PendingOrderClipsDir, 0o755); err != nil {
			cancelFunc()
			return nil, fmt.Errorf("pending_order_clips_dir %q: %w", conf.PendingOrderClipsDir, err)
		}
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
		pendingOrderClipsDir: conf.PendingOrderClipsDir,
		gripper:              gripperComp,
		vizEnabled:           vizEnabled,
		cancelCtx:            cancelCtx,
		cancelFunc:           cancelFunc,
		queue:                NewOrderQueue(),
		queueStop:            make(chan struct{}),
		orderSensorSink:      sink,
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
		_, cmdSpan := trace.StartSpan(ctx, "beanjamin::cancel")
		defer cmdSpan.End()
		res, err := s.cancel()
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

// resetWorld rebuilds the cached frame system so any mid-cycle mutations (e.g.
// a portafilter frame reparented to world by lockFilterFrame) are discarded.
// Only callable while nothing is moving AND the queue is paused — i.e. after
// the operator has pressed cancel, or during an inter-order cleanup pause.
func (s *beanjaminCoffee) resetWorld(ctx context.Context) (map[string]interface{}, error) {
	if s.running.Load() {
		return nil, errors.New("reset_world: a sequence is running — send 'cancel' first")
	}
	if !s.paused.Load() {
		return nil, errors.New("reset_world: nothing to reset — run this only after 'cancel' if the portafilter frame is stuck")
	}
	if err := s.resetFrameSystem(ctx); err != nil {
		return nil, fmt.Errorf("reset_world: %w", err)
	}
	s.logger.Infof("reset_world: frame system rebuilt from service — portafilter and all mutated frames restored to config defaults")
	return map[string]interface{}{"status": "reset"}, nil
}

func (s *beanjaminCoffee) cancel() (map[string]interface{}, error) {
	if !s.running.Load() {
		return nil, errors.New("no sequence in progress")
	}
	s.paused.Store(true)
	s.mu.Lock()
	s.cancelFunc()
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())
	s.mu.Unlock()
	s.logger.Infof("sequence cancelled — queue paused, send 'proceed' to resume")
	return map[string]interface{}{"status": "cancelled", "queue": "paused"}, nil
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
