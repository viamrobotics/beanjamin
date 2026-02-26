package beanjamin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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
	Pose     string  `json:"pose"`
	PauseSec float64 `json:"pause_secs,omitempty"`
}

type Config struct {
	PoseSwitcherName string `json:"pose_switcher_name"`
	Sequence         []Step `json:"sequence"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.PoseSwitcherName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "pose_switcher_name")
	}
	if len(cfg.Sequence) == 0 {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "sequence")
	}
	for i, step := range cfg.Sequence {
		if step.Pose == "" {
			return nil, nil, fmt.Errorf("%s: sequence[%d] is missing required field \"pose\"", path, i)
		}
	}
	return []string{cfg.PoseSwitcherName}, nil, nil
}

type beanjaminCoffee struct {
	resource.AlwaysRebuild

	name      resource.Name
	logger    logging.Logger
	cfg       *Config
	sw       toggleswitch.Switch
	sequence []Step

	mu         sync.Mutex
	cancelCtx  context.Context
	cancelFunc func()
	brewing    atomic.Bool
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
	for i, step := range conf.Sequence {
		if !validSet[step.Pose] {
			cancelFunc()
			return nil, fmt.Errorf("sequence[%d]: pose %q does not exist on switch %q (available: %v)", i, step.Pose, conf.PoseSwitcherName, validPoses)
		}
	}

	s := &beanjaminCoffee{
		name:       name,
		logger:     logger,
		cfg:        conf,
		sw:         sw,
		sequence:   conf.Sequence,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
	}
	return s, nil
}

func (s *beanjaminCoffee) Name() resource.Name {
	return s.name
}

func (s *beanjaminCoffee) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if _, ok := cmd["brew"]; ok {
		return s.brew(ctx)
	}
	if _, ok := cmd["cancel"]; ok {
		return s.cancel()
	}
	return nil, fmt.Errorf("unknown command, supported commands: brew, cancel")
}

func (s *beanjaminCoffee) cancel() (map[string]interface{}, error) {
	if !s.brewing.Load() {
		return nil, errors.New("no brew cycle in progress")
	}
	s.mu.Lock()
	s.cancelFunc()
	s.cancelCtx, s.cancelFunc = context.WithCancel(context.Background())
	s.mu.Unlock()
	s.logger.Infof("brew cycle cancelled")
	return map[string]interface{}{"status": "cancelled"}, nil
}

func (s *beanjaminCoffee) brew(ctx context.Context) (map[string]interface{}, error) {
	if !s.brewing.CompareAndSwap(false, true) {
		return nil, errors.New("brew cycle already in progress")
	}
	defer s.brewing.Store(false)

	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	s.logger.Infof("starting brew cycle with %d steps", len(s.sequence))

	for i, step := range s.sequence {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("brew cancelled at step %d (%q): %w", i, step.Pose, ctx.Err())
		case <-cancelCtx.Done():
			return nil, fmt.Errorf("brew cancelled at step %d (%q)", i, step.Pose)
		default:
		}

		s.logger.Infof("brew step %d/%d: moving to %q", i+1, len(s.sequence), step.Pose)

		_, err := s.sw.DoCommand(ctx, map[string]interface{}{
			"set_position_by_name": step.Pose,
		})
		if err != nil {
			return nil, fmt.Errorf("brew failed at step %d (%q): %w", i, step.Pose, err)
		}

		if step.PauseSec > 0 {
			pause := time.Duration(step.PauseSec * float64(time.Second))
			s.logger.Infof("pausing %s after %q", pause, step.Pose)
			select {
			case <-time.After(pause):
			case <-ctx.Done():
				return nil, fmt.Errorf("brew cancelled during pause after %q: %w", step.Pose, ctx.Err())
			case <-cancelCtx.Done():
				return nil, fmt.Errorf("brew cancelled during pause after %q", step.Pose)
			}
		}
	}

	s.logger.Infof("brew cycle complete")
	return map[string]interface{}{"status": "complete"}, nil
}

func (s *beanjaminCoffee) Close(context.Context) error {
	s.cancelFunc()
	return nil
}
