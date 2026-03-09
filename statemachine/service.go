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

	// ResolvePath finds the shortest sequence of state transitions from the current
	// state to the state whose pose name matches targetPose.
	ResolvePath(targetPose string) (intermediates []string, finalPose string, err error)

	// CommitTransition records the new state after a move completes successfully.
	// Unknown pose names are silently ignored.
	CommitTransition(poseName string)

	// InitFromPoseName sets the current state from a pose name. Returns true if the
	// pose name matched a known state, false otherwise.
	InitFromPoseName(poseName string) bool

	// ValidatePath checks that every pose in poseNames is a known state machine state
	// and that each consecutive pair has a direct transition.
	// startPose seeds the initial context ("" means none).
	ValidatePath(poseNames []string, startPose string) error
}

type service struct {
	resource.AlwaysRebuild

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

	transitions := conf.Transitions
	if len(transitions) == 0 {
		transitions = defaultTransitions()
	}

	return &service{
		name:        rawConf.ResourceName(),
		logger:      logger,
		currentPose: "",
		transitions: transitions,
	}, nil
}

func (s *service) Name() resource.Name {
	return s.name
}

// ResolvePath finds the shortest sequence of state transitions from the current
// state to the state whose pose name matches targetPose.
func (s *service) ResolvePath(targetPose string) (intermediates []string, finalPose string, err error) {
	s.mu.Lock()
	current := s.currentPose
	s.mu.Unlock()

	return resolvePath(s.transitions, current, targetPose)
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

// InitFromPoseName sets the current state from a pose name. Returns true if the
// pose name matched a known state, false otherwise.
func (s *service) InitFromPoseName(poseName string) bool {
	if _, ok := s.transitions[poseName]; !ok {
		return false
	}
	s.mu.Lock()
	s.currentPose = poseName
	s.mu.Unlock()
	return true
}

// ValidatePath checks that every pose in poseNames is a known state machine state
// and that each consecutive pair has a direct transition.
func (s *service) ValidatePath(poseNames []string, startPose string) error {
	return validatePath(s.transitions, poseNames, startPose)
}

func (s *service) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if _, ok := cmd["get_state"]; ok {
		return s.getState(), nil
	}
	if nameRaw, ok := cmd["set_state"]; ok {
		return s.setState(nameRaw)
	}
	return nil, errors.New("unknown command, supported commands: get_state, set_state")
}

func (s *service) getState() map[string]interface{} {
	s.mu.Lock()
	pose := s.currentPose
	s.mu.Unlock()

	stateName := pose
	if stateName == "" {
		stateName = "uninitialized"
	}

	allowedNext := []string{}
	if pose != "" {
		if targets, ok := s.transitions[pose]; ok {
			allowedNext = targets
		}
	}

	return map[string]interface{}{
		"state_name":          stateName,
		"allowed_transitions": allowedNext,
	}
}

func (s *service) setState(nameRaw any) (map[string]interface{}, error) {
	name, ok := nameRaw.(string)
	if !ok {
		return nil, fmt.Errorf("set_state: value must be a string, got %T", nameRaw)
	}
	if _, ok := s.transitions[name]; !ok {
		return nil, fmt.Errorf("set_state: %q is not a known state", name)
	}
	s.mu.Lock()
	s.currentPose = name
	s.mu.Unlock()
	s.logger.Infof("state machine: state manually set to %q", name)
	return map[string]interface{}{
		"status":     "ok",
		"state_name": name,
	}, nil
}

func (s *service) Close(context.Context) error {
	return nil
}
