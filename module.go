package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"beanjamin/speechclient"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
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

type Step struct {
	PoseName string  `json:"pose_name"`
	PauseSec float64 `json:"pause_secs,omitempty"`
}

type Config struct {
	PoseSwitcherName  string            `json:"pose_switcher_name"`
	Sequences         map[string][]Step `json:"sequences"`
	SpeechServiceName string            `json:"speech_service_name,omitempty"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.PoseSwitcherName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "pose_switcher_name")
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
		}
	}
	var optDeps []string
	if cfg.SpeechServiceName != "" {
		optDeps = append(optDeps, cfg.SpeechServiceName)
	}
	return []string{cfg.PoseSwitcherName}, optDeps, nil
}

type beanjaminCoffee struct {
	resource.AlwaysRebuild

	name      resource.Name
	logger    logging.Logger
	cfg       *Config
	sw        toggleswitch.Switch
	speech    speechclient.Speech // nil when speech_service_name is not configured
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
		}
	}

	var speech speechclient.Speech
	if conf.SpeechServiceName != "" {
		speechRes, ok := deps[speechclient.Named(conf.SpeechServiceName)]
		if ok {
			speech, _ = speechRes.(speechclient.Speech)
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
		speech:     speech,
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
			return nil, fmt.Errorf("unknown sequence %q", seqName)
		}
		if enforceStart, _ := cmd["enforce_start"].(bool); enforceStart {
			if err := s.checkPosition(ctx, steps[0].PoseName); err != nil {
				return nil, fmt.Errorf("run %q: %w", seqName, err)
			}
		}
		return s.runSteps(ctx, seqName, steps)
	}
	if seqName, ok := cmd["rewind"].(string); ok {
		steps, exists := s.sequences[seqName]
		if !exists {
			return nil, fmt.Errorf("unknown sequence %q", seqName)
		}
		if err := s.checkPosition(ctx, steps[len(steps)-1].PoseName); err != nil {
			return nil, fmt.Errorf("rewind %q: %w", seqName, err)
		}
		reversed := make([]Step, 0, len(steps)-1)
		for i := len(steps) - 2; i >= 0; i-- {
			reversed = append(reversed, steps[i])
		}
		return s.runSteps(ctx, seqName+":rewind", reversed)
	}
	if orderRaw, ok := cmd["prepare_order"]; ok {
		return s.prepareOrder(ctx, orderRaw)
	}
	if _, ok := cmd["cancel"]; ok {
		return s.cancel()
	}
	return nil, fmt.Errorf("unknown command, supported commands: run, rewind, cancel, prepare_order")
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

		_, err := s.sw.DoCommand(ctx, map[string]interface{}{
			"set_position_by_name": step.PoseName,
		})
		if err != nil {
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
