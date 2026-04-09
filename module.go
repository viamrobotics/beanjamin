package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	viz "github.com/viam-labs/motion-tools/client/client"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/gripper"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	generic "go.viam.com/rdk/services/generic"

	// Register the filter-poses-switch model.
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
	PoseSwitcherName      string  `json:"pose_switcher_name"`
	ClawsPoseSwitcherName string  `json:"claws_pose_switcher_name"`
	ArmName               string  `json:"arm_name"`
	GripperName           string  `json:"gripper_name"`
	SpeechServiceName     string  `json:"speech_service_name,omitempty"`
	VizURL                string  `json:"viz_url,omitempty"`
	BrewTimeSec           float64 `json:"brew_time_sec,omitempty"`
	LungoBrewTimeSec      float64 `json:"lungo_brew_time_sec,omitempty"`
	PlaceCup              bool    `json:"place_cup,omitempty"`
	CleanAfterUse         bool    `json:"clean_after_use,omitempty"`
	SaveMotionRequestsDir string  `json:"save_motion_requests_dir,omitempty"`
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
	mu                     sync.Mutex
	cancelCtx              context.Context
	cancelFunc             func()
	running                atomic.Bool
	queue                  *OrderQueue
	queueStop              chan struct{}
	paused                 atomic.Bool
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

	vizEnabled := false
	if conf.VizURL != "" {
		viz.SetURL(conf.VizURL)
		vizEnabled = true
		logger.Infof("viz client configured at %s", conf.VizURL)
	}

	s := &beanjaminCoffee{
		name:       name,
		logger:     logger,
		cfg:        conf,
		filterSw:   filterSw,
		clawsSw:    clawSw,
		arm:        armComp,
		fsSvc:      fsSvc,
		cachedFS:   cachedFS,
		speech:     speech,
		gripper:    gripperComp,
		vizEnabled: vizEnabled,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
		queue:      NewOrderQueue(),
		queueStop:  make(chan struct{}),
	}
	go s.processQueue()
	return s, nil
}

func (s *beanjaminCoffee) Name() resource.Name {
	return s.name
}

func (s *beanjaminCoffee) Status(ctx context.Context) (map[string]interface{}, error) {
	orders := s.queue.List()
	names := make([]string, len(orders))
	for i, o := range orders {
		names[i] = o.CustomerName
	}
	return map[string]interface{}{
		"count":      len(orders),
		"orders":     names,
		"is_paused":  s.paused.Load(),
		"is_running": s.running.Load(),
	}, nil
}

func (s *beanjaminCoffee) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if orderRaw, ok := cmd["prepare_order"]; ok {
		res, err := s.enqueueOrder(ctx, orderRaw)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if actionName, ok := cmd["execute_action"].(string); ok {
		res, err := s.executeAction(ctx, actionName)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if _, ok := cmd["cancel"]; ok {
		res, err := s.cancel()
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if _, ok := cmd["get_queue"]; ok {
		return s.Status(ctx)
	}
	if _, ok := cmd["proceed"]; ok {
		return s.proceedQueue()
	}
	if _, ok := cmd["clear_queue"]; ok {
		return s.clearQueue()
	}
	// Stream deck key commands
	if action, ok := cmd["action"].(string); ok {
		switch action {
		case "open_gripper":
			return s.handleOpenGripper(ctx)
		case "close_gripper":
			return s.handleCloseGripper(ctx)
		default:
			return nil, fmt.Errorf("unknown action %q", action)
		}
	}
	err := fmt.Errorf("unknown command, supported commands: cancel, prepare_order, execute_action, get_queue, proceed, clear_queue, action")
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

func (s *beanjaminCoffee) cancel() (map[string]interface{}, error) {
	if !s.running.Load() {
		return nil, errors.New("no sequence in progress")
	}
	s.mu.Lock()
	s.cancelFunc()
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())
	s.mu.Unlock()
	s.logger.Infof("sequence cancelled")
	return map[string]interface{}{"status": "cancelled"}, nil
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
