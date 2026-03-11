package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/gripper"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
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

type Step struct {
	PoseName            string                `json:"pose_name"`
	PauseSec            float64               `json:"pause_secs,omitempty"`
	LinearConstraint    *StepLinearConstraint `json:"linear_constraint,omitempty"`
	AllowedCollisions   []AllowedCollision    `json:"allowed_collisions,omitempty"`
	PivotFromPose       string                `json:"pivot_from_pose,omitempty"`
	PivotDegreesPerStep float64               `json:"pivot_degrees_per_step,omitempty"`
}

type Config struct {
	PoseSwitcherName      string            `json:"pose_switcher_name"`
	ClawsPoseSwitcherName string            `json:"claws_pose_switcher_name"`
	ArmName               string            `json:"arm_name"`
	GripperName           string            `json:"gripper_name"`
	Sequences             map[string][]Step `json:"sequences"`
	SpeechServiceName     string            `json:"speech_service_name,omitempty"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.PoseSwitcherName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "pose_switcher_name")
	}
	if cfg.ArmName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm_name")
	}
	if cfg.GripperName != "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "gripper_name")
	}
	if cfg.GripperName != "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "gripper_name")
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

	if cfg.ClawsPoseSwitcherName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "claws_pose_switcher_name")
	}

	reqDeps := []string{cfg.PoseSwitcherName, cfg.ClawsPoseSwitcherName, framesystem.PublicServiceName.String(), arm.Named(cfg.ArmName).String()}

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
	clawsSw   toggleswitch.Switch
	arm       arm.Arm
	fsSvc     framesystem.Service
	cachedFS  *referenceframe.FrameSystem // cached frame system, mutated at lock/unlock
	speech    resource.Resource           // nil when speech_service_name is not configured
	gripper   gripper.Gripper             // nil when gripper_name is not configured
	sequences map[string][]Step

	mu         sync.Mutex
	cancelCtx  context.Context
	cancelFunc func()
	running    atomic.Bool
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

	clawsRes, ok := deps[toggleswitch.Named(conf.ClawsPoseSwitcherName)]
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("claws switch %q not found in dependencies", conf.ClawsPoseSwitcherName)
	}
	clawsSw, ok := clawsRes.(toggleswitch.Switch)
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
		name:       name,
		logger:     logger,
		cfg:        conf,
		sw:         sw,
		clawsSw:    clawsSw,
		arm:        armComp,
		fsSvc:      fsSvc,
		cachedFS:   cachedFS,
		speech:     speech,
		gripper:    gripperComp,
		sequences:  conf.Sequences,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
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
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s cancelled at step %d (%q): %w", label, i, step.PoseName, ctx.Err())
		case <-cancelCtx.Done():
			return nil, fmt.Errorf("%s cancelled at step %d (%q)", label, i, step.PoseName)
		default:
		}

		s.logger.Infof("%s step %d/%d: moving to %q", label, i+1, len(steps), step.PoseName)

		if err := s.moveToPose(ctx, s.sw, step); err != nil {
			return nil, fmt.Errorf("%s failed at step %d (%q): %w", label, i, step.PoseName, err)
		}

		if step.PauseSec > 0 {
			pause := time.Duration(step.PauseSec * float64(time.Second))
			s.logger.Infof("pausing %s after %q", pause, step.PoseName)
			select {
			case <-time.After(pause):
			case <-ctx.Done():
				return nil, fmt.Errorf("%s cancelled during pause after %q: %w", label, step.PoseName, ctx.Err())
			case <-cancelCtx.Done():
				return nil, fmt.Errorf("%s cancelled during pause after %q", label, step.PoseName)
			}
		}
	}

	s.logger.Infof("%s complete", label)
	return map[string]interface{}{"status": "complete"}, nil
}

func (s *beanjaminCoffee) Close(context.Context) error {
	s.cancelFunc()
	return nil
}
