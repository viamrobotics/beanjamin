package statemachine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

// Model is the Viam model identifier for the state machine service.
var Model = resource.NewModel("viam", "beanjamin", "state-machine")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newService,
		},
	)
}

// Config holds optional configuration for the state machine service.
type Config struct {
	Transitions map[string][]string `json:"transitions,omitempty"`
}

// Validate checks the configuration.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if len(cfg.Transitions) == 0 {
		return nil, nil, nil
	}
	// Build set of known states.
	known := make(map[string]bool, len(cfg.Transitions))
	for name := range cfg.Transitions {
		if name == "uninitialized" {
			return nil, nil, fmt.Errorf("%s: \"uninitialized\" is a reserved state name", path)
		}
		known[name] = true
	}
	// Verify all transition targets are known states.
	for name, targets := range cfg.Transitions {
		for _, target := range targets {
			if !known[target] {
				return nil, nil, fmt.Errorf("%s: transition from %q targets unknown state %q", path, name, target)
			}
		}
	}
	return nil, nil, nil
}

// Service is the interface for the standalone state machine component.
type Service interface {
	resource.Resource

	// GetState returns the current pose name, or "" if uninitialized.
	GetState() string

	// SetState sets the current state to the given pose name.
	// Returns an error if the pose name is not a known state.
	SetState(poseName string) error

	// ResolvePath finds the shortest sequence of state transitions from the current
	// state to the state whose pose name matches targetPose.
	ResolvePath(targetPose string) (intermediates []string, finalPose string, err error)

	// CommitTransition records the new state after a move completes successfully.
	// Unknown pose names are silently ignored.
	CommitTransition(poseName string)

	// ValidatePath checks that every pose in poseNames is a known state machine state
	// and that each consecutive pair has a direct transition.
	// startPose seeds the initial context ("" means none).
	ValidatePath(poseNames []string, startPose string) error
}

type service struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name        resource.Name
	logger      logging.Logger
	mu          sync.Mutex
	currentPose string // "" = uninitialized
	transitions map[string][]string
}

func newService(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}
	return &service{
		name:        rawConf.ResourceName(),
		logger:      logger,
		currentPose: "",
		transitions: conf.Transitions,
	}, nil
}

func (s *service) Name() resource.Name {
	return s.name
}

// GetState returns the current pose name, or "" if uninitialized.
func (s *service) GetState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentPose
}

// ResolvePath finds the shortest sequence of state transitions from the current
// state to the state whose pose name matches targetPose.
func (s *service) ResolvePath(targetPose string) (intermediates []string, finalPose string, err error) {
	return resolvePath(s.transitions, s.GetState(), targetPose)
}

// CommitTransition records the new state after a move completes successfully.
// Unknown pose names are silently ignored.
func (s *service) CommitTransition(poseName string) {
	if _, ok := s.transitions[poseName]; !ok {
		return
	}
	s.mu.Lock()
	s.currentPose = poseName
	s.mu.Unlock()
}

// SetState sets the current state to the given pose name.
// Returns an error if the pose name is not a known state.
func (s *service) SetState(poseName string) error {
	if _, ok := s.transitions[poseName]; !ok {
		return fmt.Errorf("set_state: %q is not a known state", poseName)
	}
	s.mu.Lock()
	s.currentPose = poseName
	s.mu.Unlock()
	s.logger.Infof("state machine: state set to %q", poseName)
	return nil
}

// ValidatePath checks that every pose in poseNames is a known state machine state
// and that each consecutive pair has a direct transition.
func (s *service) ValidatePath(poseNames []string, startPose string) error {
	return validatePath(s.transitions, poseNames, startPose)
}

func (s *service) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, errors.New("state machine does not support DoCommand; use GetState, SetState, ResolvePath, CommitTransition, and ValidatePath methods instead")
}
