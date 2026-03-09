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
		resource.Registration[resource.Resource, resource.NoNativeConfig]{
			Constructor: newService,
		},
	)
}

// Service is the interface for the standalone state machine component.
type Service interface {
	resource.Resource

	// ResolvePath finds the shortest sequence of state transitions from the current
	// state to any state whose pose name matches targetPose.
	ResolvePath(targetPose string) (intermediates []int, finalStateIdx int, err error)

	// CommitTransition records the new state index after a move completes successfully.
	CommitTransition(newStateIdx int)

	// InitFromPoseName sets the state index from a pose name. Returns true if the
	// pose name matched a known state, false otherwise.
	InitFromPoseName(poseName string) bool
}

type service struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger
	mu     sync.Mutex
	idx    int // -1 = uninitialized
}

func newService(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	return &service{
		name:   rawConf.ResourceName(),
		logger: logger,
		idx:    -1,
	}, nil
}

func (s *service) Name() resource.Name {
	return s.name
}

// ResolvePath finds the shortest sequence of state transitions from the current
// state to any state whose pose name matches targetPose.
func (s *service) ResolvePath(targetPose string) (intermediates []int, finalStateIdx int, err error) {
	s.mu.Lock()
	current := s.idx
	s.mu.Unlock()

	return ResolvePath(current, targetPose)
}

// CommitTransition records the new state index after a move completes successfully.
func (s *service) CommitTransition(newStateIdx int) {
	s.mu.Lock()
	s.idx = newStateIdx
	s.mu.Unlock()
}

// InitFromPoseName sets the state index from a pose name. Returns true if the
// pose name matched a known state, false otherwise.
func (s *service) InitFromPoseName(poseName string) bool {
	idx := InferIndex(poseName)
	if idx < 0 {
		return false
	}
	s.mu.Lock()
	s.idx = idx
	s.mu.Unlock()
	return true
}

func (s *service) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	if _, ok := cmd["get_state"]; ok {
		return s.getState(), nil
	}
	if idxRaw, ok := cmd["set_state_index"]; ok {
		return s.setStateIndex(idxRaw)
	}
	return nil, errors.New("unknown command, supported commands: get_state, set_state_index")
}

func (s *service) getState() map[string]any {
	s.mu.Lock()
	idx := s.idx
	s.mu.Unlock()

	stateName := "uninitialized"
	if idx >= 0 && idx < len(statePoseNames) {
		stateName = statePoseNames[idx]
	}

	var allowedNext []map[string]any
	if idx >= 0 {
		for _, nextIdx := range validTransitions[idx] {
			allowedNext = append(allowedNext, map[string]any{
				"index": nextIdx,
				"name":  statePoseNames[nextIdx],
			})
		}
	}

	return map[string]any{
		"state_index":         idx,
		"state_name":          stateName,
		"allowed_transitions": allowedNext,
	}
}

func (s *service) setStateIndex(idxRaw any) (map[string]any, error) {
	var idx int
	switch v := idxRaw.(type) {
	case float64:
		idx = int(v)
	case int:
		idx = v
	default:
		return nil, fmt.Errorf("set_state_index: value must be an integer, got %T", idxRaw)
	}
	if idx < 0 || idx >= len(statePoseNames) {
		return nil, fmt.Errorf("set_state_index: %d is out of range [0, %d]", idx, len(statePoseNames)-1)
	}
	s.mu.Lock()
	s.idx = idx
	s.mu.Unlock()
	s.logger.Infof("state machine: state manually set to index %d (%q)", idx, statePoseNames[idx])
	return map[string]any{
		"status":      "ok",
		"state_index": idx,
		"state_name":  statePoseNames[idx],
	}, nil
}

func (s *service) Close(context.Context) error {
	return nil
}
