package beanjamin

import (
	"context"
	"errors"
	"fmt"
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

type Config struct {
	PoseSwitcherName string `json:"pose_switcher_name"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.PoseSwitcherName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "switch_name")
	}
	return []string{cfg.PoseSwitcherName}, nil, nil
}

type beanjaminCoffee struct {
	resource.AlwaysRebuild

	name      resource.Name
	logger    logging.Logger
	cfg       *Config
	sw        toggleswitch.Switch
	poseNames []string

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

	_, poseNames, err := sw.GetNumberOfPositions(ctx, nil)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("failed to get positions from switch: %w", err)
	}

	s := &beanjaminCoffee{
		name:       name,
		logger:     logger,
		cfg:        conf,
		sw:         sw,
		poseNames:  poseNames,
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
	return nil, fmt.Errorf("unknown command, supported commands: brew")
}

func (s *beanjaminCoffee) brew(ctx context.Context) (map[string]interface{}, error) {
	if !s.brewing.CompareAndSwap(false, true) {
		return nil, errors.New("brew cycle already in progress")
	}
	defer s.brewing.Store(false)

	s.logger.Infof("starting brew cycle with %d steps", len(s.poseNames))

	// Pause durations after each pose completes. Adjust these as you test the flow.
	pauseAfter := map[string]time.Duration{
		// "grinder_approach":  0 * time.Second,
		// "grinder_activate":  10 * time.Second, // wait for grinder to finish
		// "tamper_approach":   0 * time.Second,
		// "tamper_activate":   3 * time.Second,  // wait for tamp pressure
		// "coffee_approach":   0 * time.Second,
		// "coffee_in":         0 * time.Second,
		// "coffee_locked":     25 * time.Second, // wait for espresso to pull
	}

	for i, poseName := range s.poseNames {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("brew cancelled at step %d (%q): %w", i, poseName, ctx.Err())
		case <-s.cancelCtx.Done():
			return nil, fmt.Errorf("brew cancelled at step %d (%q): component closing", i, poseName)
		default:
		}

		s.logger.Infof("brew step %d/%d: moving to %q", i+1, len(s.poseNames), poseName)

		_, err := s.sw.DoCommand(ctx, map[string]interface{}{
			"set_position_by_name": poseName,
		})
		if err != nil {
			return nil, fmt.Errorf("brew failed at step %d (%q): %w", i, poseName, err)
		}

		if pause, ok := pauseAfter[poseName]; ok && pause > 0 {
			s.logger.Infof("pausing %s after %q", pause, poseName)
			select {
			case <-time.After(pause):
			case <-ctx.Done():
				return nil, fmt.Errorf("brew cancelled during pause after %q: %w", poseName, ctx.Err())
			case <-s.cancelCtx.Done():
				return nil, fmt.Errorf("brew cancelled during pause after %q: component closing", poseName)
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
