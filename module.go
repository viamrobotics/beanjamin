package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/motion"

	"beanjamin/statemachine"
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
	PoseName            string                `json:"pose_name"`
	PauseSec            float64               `json:"pause_secs,omitempty"`
	LinearConstraint    *StepLinearConstraint `json:"linear_constraint,omitempty"`
	AllowedCollisions   []AllowedCollision    `json:"allowed_collisions,omitempty"`
	PivotFromPose       string                `json:"pivot_from_pose,omitempty"`
	PivotDegreesPerStep float64               `json:"pivot_degrees_per_step,omitempty"`
}

type Config struct {
	PoseSwitcherName  string            `json:"pose_switcher_name"`
	MotionServiceName string            `json:"motion_service_name"`
	StateMachineName  string            `json:"state_machine_name"`
	Sequences         map[string][]Step `json:"sequences"`
	SpeechServiceName string            `json:"speech_service_name,omitempty"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.PoseSwitcherName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "pose_switcher_name")
	}
	if cfg.MotionServiceName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "motion_service_name")
	}
	if cfg.StateMachineName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "state_machine_name")
	}
	if len(cfg.Sequences) == 0 {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "sequences")
	}
	for name, steps := range cfg.Sequences {
		if len(steps) == 0 {
			return nil, nil, fmt.Errorf("%s: sequence %q is empty", path, name)
		}
		for i, step := range steps {
			if step.PoseName == "" {
				return nil, nil, fmt.Errorf("%s: sequences[%q][%d] is missing required field \"pose_name\"", path, name, i)
			}
			if step.PivotFromPose != "" && step.PivotDegreesPerStep <= 0 {
				return nil, nil, fmt.Errorf(
					"%s: sequences[%q][%d] has pivot_from_pose but pivot_degrees_per_step must be > 0", path, name, i)
			}
		}
	}

	reqDeps := []string{cfg.PoseSwitcherName}
	if cfg.MotionServiceName == "builtin" {
		reqDeps = append(reqDeps, motion.Named("builtin").String())
	} else {
		reqDeps = append(reqDeps, cfg.MotionServiceName)
	}
	reqDeps = append(reqDeps, generic.Named(cfg.StateMachineName).String())

	var optDeps []string
	if cfg.SpeechServiceName != "" {
		optDeps = append(optDeps, generic.Named(cfg.SpeechServiceName).String())
	}
	return reqDeps, optDeps, nil
}

type beanjaminCoffee struct {
	resource.AlwaysRebuild

	name      resource.Name
	logger    logging.Logger
	cfg       *Config
	sw        toggleswitch.Switch
	motion    motion.Service
	speech    resource.Resource // nil when speech_service_name is not configured
	sequences map[string][]Step

	mu         sync.Mutex
	cancelCtx  context.Context
	cancelFunc func()
	running    atomic.Bool

	stateMachine statemachine.Service
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
	sw, ok := switchRes.(toggleswitch.Switch)
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("resource %q is not a switch", conf.PoseSwitcherName)
	}

	motionSvc, err := motion.FromProvider(deps, conf.MotionServiceName)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("motion service %q not found in dependencies: %w", conf.MotionServiceName, err)
	}

	smRes, ok := deps[generic.Named(conf.StateMachineName)]
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("state machine %q not found in dependencies", conf.StateMachineName)
	}
	sm, ok := smRes.(statemachine.Service)
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("resource %q is not a statemachine.Service", conf.StateMachineName)
	}

	_, validPoses, err := sw.GetNumberOfPositions(ctx, nil)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("failed to get positions from switch: %w", err)
	}
	validSet := make(map[string]bool, len(validPoses))
	for _, p := range validPoses {
		validSet[p] = true
	}
	for seqName, steps := range conf.Sequences {
		for i, step := range steps {
			if !validSet[step.PoseName] {
				cancelFunc()
				return nil, fmt.Errorf("sequences[%q][%d]: pose %q does not exist on switch %q (available: %v)", seqName, i, step.PoseName, conf.PoseSwitcherName, validPoses)
			}
			if step.PivotFromPose != "" && !validSet[step.PivotFromPose] {
				cancelFunc()
				return nil, fmt.Errorf("sequences[%q][%d]: pivot_from_pose %q does not exist on switch %q (available: %v)", seqName, i, step.PivotFromPose, conf.PoseSwitcherName, validPoses)
			}
		}
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

	s := &beanjaminCoffee{
		name:         name,
		logger:       logger,
		cfg:          conf,
		sw:           sw,
		motion:       motionSvc,
		speech:       speech,
		sequences:    conf.Sequences,
		cancelCtx:    cancelCtx,
		cancelFunc:   cancelFunc,
		stateMachine: sm,
	}

	// Detect the arm's current pose and initialize the state machine from it,
	if resp, err := sw.DoCommand(ctx, map[string]interface{}{"detect_current_pose": true}); err != nil {
		logger.Warnf("state machine: could not detect arm pose at startup: %v; use set_state to initialize manually", err)
	} else if poseName, ok := resp["pose_name"].(string); ok {
		if sm.InitFromPoseName(poseName) {
			logger.Infof("state machine: auto-initialized from detected arm pose %q", poseName)
		} else {
			logger.Warnf("state machine: detected pose %q does not match any known state; use set_state to initialize manually", poseName)
		}
	}

	return s, nil
}

func (s *beanjaminCoffee) Name() resource.Name {
	return s.name
}

func (s *beanjaminCoffee) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if seqName, ok := cmd["run"].(string); ok {
		steps, exists := s.sequences[seqName]
		if !exists {
			err := fmt.Errorf("unknown sequence %q", seqName)
			s.logger.Warnw("DoCommand", "error", err)
			return nil, err
		}
		if enforceStart, _ := cmd["enforce_start"].(bool); enforceStart {
			if err := s.checkPosition(ctx, steps[0].PoseName); err != nil {
				err = fmt.Errorf("run %q: %w", seqName, err)
				s.logger.Errorw("DoCommand", "error", err)
				return nil, err
			}
		}
		poseNames := make([]string, len(steps))
		for i, step := range steps {
			poseNames[i] = step.PoseName
		}
		if err := s.stateMachine.ValidatePath(poseNames, ""); err != nil {
			s.logger.Errorw("DoCommand", "error", err)
			return nil, err
		}
		res, err := s.runSteps(ctx, seqName, steps)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if seqName, ok := cmd["rewind"].(string); ok {
		steps, exists := s.sequences[seqName]
		if !exists {
			err := fmt.Errorf("unknown sequence %q", seqName)
			s.logger.Warnw("DoCommand", "error", err)
			return nil, err
		}
		if err := s.checkPosition(ctx, steps[len(steps)-1].PoseName); err != nil {
			err = fmt.Errorf("rewind %q: %w", seqName, err)
			s.logger.Errorw("DoCommand", "error", err)
			return nil, err
		}
		reversed := make([]Step, 0, len(steps)-1)
		for i := len(steps) - 2; i >= 0; i-- {
			reversed = append(reversed, steps[i])
		}
		reversedPoseNames := make([]string, len(reversed))
		for i, step := range reversed {
			reversedPoseNames[i] = step.PoseName
		}
		if err := s.stateMachine.ValidatePath(reversedPoseNames, steps[len(steps)-1].PoseName); err != nil {
			s.logger.Errorw("DoCommand", "error", err)
			return nil, err
		}
		res, err := s.runSteps(ctx, seqName+":rewind", reversed)
		if err != nil {
			s.logger.Errorw("DoCommand", "error", err)
		}
		return res, err
	}
	if orderRaw, ok := cmd["prepare_order"]; ok {
		res, err := s.prepareOrder(ctx, orderRaw)
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
	err := fmt.Errorf("unknown command, supported commands: run, rewind, cancel, prepare_order, execute_action")
	s.logger.Warnw("DoCommand", "error", err)
	return nil, err
}

func (s *beanjaminCoffee) checkPosition(ctx context.Context, expected string) error {
	resp, err := s.sw.DoCommand(ctx, map[string]interface{}{
		"get_current_position_name": true,
	})
	if err != nil {
		return fmt.Errorf("failed to get current position: %w", err)
	}
	currentPose, ok := resp["position_name"].(string)
	if !ok {
		return errors.New("unexpected response from get_current_position_name")
	}
	if currentPose != expected {
		return fmt.Errorf("expected switch at %q, but currently at %q", expected, currentPose)
	}
	return nil
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

func (s *beanjaminCoffee) runSteps(ctx context.Context, label string, steps []Step) (map[string]interface{}, error) {
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("a sequence is already running")
	}
	defer s.running.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	s.logger.Infof("starting %s with %d steps", label, len(steps))

	for i, step := range steps {
		s.logger.Infof("%s step %d/%d: %q", label, i+1, len(steps), step.PoseName)
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return nil, fmt.Errorf("%s failed at step %d (%q): %w", label, i+1, step.PoseName, err)
		}
	}

	s.logger.Infof("%s complete", label)
	return map[string]interface{}{"status": "complete"}, nil
}

func (s *beanjaminCoffee) Close(context.Context) error {
	s.cancelFunc()
	return nil
}
