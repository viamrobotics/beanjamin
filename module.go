package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	viz "github.com/viam-labs/motion-tools/client/client"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/gripper"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"

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

type Step struct {
	PoseName                 string                `json:"pose_name"`
	PauseSec                 float64               `json:"pause_secs,omitempty"`
	LinearConstraint         *StepLinearConstraint `json:"linear_constraint,omitempty"`
	OrientationToleranceDegs *float64              `json:"orientation_tolerance_degs,omitempty"`
	AllowedCollisions        []AllowedCollision    `json:"allowed_collisions,omitempty"`
	PivotFromPose            string                `json:"pivot_from_pose,omitempty"`
	PivotDegreesPerStep      float64               `json:"pivot_degrees_per_step,omitempty"`
	Component                string                `json:"component,omitempty"`

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
	PlaceCup              bool    `json:"place_cup,omitempty"`
	CleanAfterUse         bool    `json:"clean_after_use,omitempty"`
	DialMoveXMM           float64 `json:"dial_move_x_mm,omitempty"`
	DialMoveYMM           float64 `json:"dial_move_y_mm,omitempty"`
	DialMoveZMM           float64 `json:"dial_move_z_mm,omitempty"`
	DialMaxPosition       float64 `json:"dial_max_position,omitempty"`
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

	// Last known absolute dial positions and directions for direction detection
	lastDialX     *float64
	lastDialY     *float64
	lastDialZ     *float64
	lastDialSpeed *float64
	lastDialDirX  float64
	lastDialDirY  float64
	lastDialDirZ  float64
	lastDialDirSpeed float64
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
		return s.getQueue()
	}
	if _, ok := cmd["proceed"]; ok {
		return s.proceedQueue()
	}
	if _, ok := cmd["clear_queue"]; ok {
		return s.clearQueue()
	}
	// Stream deck dial commands
	if v, ok := cmd["dial_move_x"]; ok {
		return s.handleDialMove(ctx, "x", v)
	}
	if v, ok := cmd["dial_move_y"]; ok {
		return s.handleDialMove(ctx, "y", v)
	}
	if v, ok := cmd["dial_move_z"]; ok {
		return s.handleDialMove(ctx, "z", v)
	}
	if v, ok := cmd["dial_move_speed"]; ok {
		return s.handleDialSpeedMove(ctx, v)
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
	err := fmt.Errorf("unknown command, supported commands: cancel, prepare_order, execute_action, get_queue, proceed, clear_queue, dial_move_x/y/z/speed, action")
	s.logger.Warnw("DoCommand", "error", err)
	return nil, err
}

func (s *beanjaminCoffee) getQueue() (map[string]interface{}, error) {
	orders := s.queue.List()
	names := make([]string, len(orders))
	for i, o := range orders {
		names[i] = o.CustomerName
	}
	return map[string]interface{}{
		"count":     len(orders),
		"orders":    names,
		"is_paused": s.paused.Load(),
	}, nil
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

func (s *beanjaminCoffee) handleMoveArm(ctx context.Context, axis string, mm float64) (map[string]interface{}, error) {
	currentPose, err := s.arm.EndPosition(ctx, map[string]interface{}{})
	if err != nil {
		return nil, fmt.Errorf("failed to get current arm position: %w", err)
	}
	pt := currentPose.Point()
	switch axis {
	case "x":
		pt.X += mm
	case "y":
		pt.Y += mm
	case "z":
		pt.Z += mm
	}
	newPose := spatialmath.NewPose(pt, currentPose.Orientation())
	if err := s.arm.MoveToPosition(ctx, newPose, map[string]interface{}{}); err != nil {
		return nil, fmt.Errorf("failed to move arm: %w", err)
	}
	return map[string]interface{}{"status": "moved", "axis": axis, "mm": mm}, nil
}

func (s *beanjaminCoffee) handleDialMove(ctx context.Context, axis string, dialValue interface{}) (map[string]interface{}, error) {
	if s.arm == nil {
		return nil, fmt.Errorf("no arm configured")
	}
	var mm float64
	switch axis {
	case "x":
		mm = s.cfg.DialMoveXMM
	case "y":
		mm = s.cfg.DialMoveYMM
	case "z":
		mm = s.cfg.DialMoveZMM
	}
	if mm == 0 {
		mm = 1
	}
	if dialVal, ok := toFloat64(dialValue); ok {
		var last **float64
		var lastDir *float64
		switch axis {
		case "x":
			last = &s.lastDialX
			lastDir = &s.lastDialDirX
		case "y":
			last = &s.lastDialY
			lastDir = &s.lastDialDirY
		case "z":
			last = &s.lastDialZ
			lastDir = &s.lastDialDirZ
		}
		if *last == nil {
			// First reading — store position and skip move (no direction yet)
			*last = &dialVal
			return map[string]interface{}{"status": "dial_initialized", "axis": axis, "position": dialVal}, nil
		}
		maxPos := s.cfg.DialMaxPosition
		if maxPos == 0 {
			maxPos = 100
		}
		delta := dialVal - **last
		// Correct for rollover: if the jump is more than half the range, it wrapped
		if delta > maxPos/2 {
			delta -= maxPos + 1
		} else if delta < -maxPos/2 {
			delta += maxPos + 1
		}
		var direction float64
		if **last == 0 && *lastDir != 0 {
			// At the zero boundary, the dial can't go lower so the value bounces
			// back up — continue in the last known direction instead of reversing
			direction = *lastDir
		} else if delta < 0 {
			direction = -1
		} else {
			direction = 1
		}
		if direction < 0 {
			mm = -mm
		}
		*lastDir = direction
		**last = dialVal
	}
	return s.handleMoveArm(ctx, axis, mm)
}

func (s *beanjaminCoffee) handleDialSpeedMove(_ context.Context, dialValue interface{}) (map[string]interface{}, error) {
	const speedStepMM = 1.0
	const minSpeedMM = 0.5

	dialVal, ok := toFloat64(dialValue)
	if !ok {
		return nil, fmt.Errorf("dial_move_speed: invalid value %v", dialValue)
	}

	if s.lastDialSpeed == nil {
		s.lastDialSpeed = &dialVal
		return map[string]interface{}{"status": "dial_initialized", "axis": "speed", "position": dialVal}, nil
	}

	maxPos := s.cfg.DialMaxPosition
	if maxPos == 0 {
		maxPos = 100
	}
	delta := dialVal - *s.lastDialSpeed
	if delta > maxPos/2 {
		delta -= maxPos + 1
	} else if delta < -maxPos/2 {
		delta += maxPos + 1
	}

	var direction float64
	if *s.lastDialSpeed == 0 && s.lastDialDirSpeed != 0 {
		direction = s.lastDialDirSpeed
	} else if delta < 0 {
		direction = -1
	} else {
		direction = 1
	}
	s.lastDialDirSpeed = direction
	*s.lastDialSpeed = dialVal

	step := speedStepMM * direction
	applyStep := func(current float64) float64 {
		if current == 0 {
			current = 1
		}
		if v := current + step; v >= minSpeedMM {
			return v
		}
		return minSpeedMM
	}
	s.cfg.DialMoveXMM = applyStep(s.cfg.DialMoveXMM)
	s.cfg.DialMoveYMM = applyStep(s.cfg.DialMoveYMM)
	s.cfg.DialMoveZMM = applyStep(s.cfg.DialMoveZMM)

	return map[string]interface{}{
		"status":          "speed_updated",
		"dial_move_x_mm": s.cfg.DialMoveXMM,
		"dial_move_y_mm": s.cfg.DialMoveYMM,
		"dial_move_z_mm": s.cfg.DialMoveZMM,
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

func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func (s *beanjaminCoffee) Close(context.Context) error {
	close(s.queueStop)
	s.cancelFunc()
	return nil
}
