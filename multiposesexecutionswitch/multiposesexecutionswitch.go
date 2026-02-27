package multiposesexecutionswitch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/golang/geo/r3"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/services/motion"
	"go.viam.com/rdk/spatialmath"
)

var Model = resource.NewModel("viam", "beanjamin", "multi-poses-execution-switch")

func init() {
	resource.RegisterComponent(
		toggleswitch.API,
		Model,
		resource.Registration[toggleswitch.Switch, *Config]{
			Constructor: newMultiPosesExecutionSwitch,
		},
	)
}

type Config struct {
	ReferenceFrame    string               `json:"reference_frame"`
	ComponentName     string               `json:"component_name"`
	Motion            string               `json:"motion"`
	Poses             []PoseConf           `json:"poses"`
	LinearConstraint  *LinearConstraintConf `json:"linear_constraint,omitempty"`
}

type LinearConstraintConf struct {
	LineToleranceMm          float64 `json:"line_tolerance_mm"`
	OrientationToleranceDegs float64 `json:"orientation_tolerance_degs"`
}

type PoseConf struct {
	PoseName     string  `json:"pose_name"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
	Z            float64 `json:"z"`
	OX           float64 `json:"o_x"`
	OY           float64 `json:"o_y"`
	OZ           float64 `json:"o_z"`
	ThetaDegrees float64 `json:"theta_degrees"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.ReferenceFrame == "" {
		cfg.ReferenceFrame = referenceframe.World
	}
	if cfg.ComponentName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "component_name")
	}
	if cfg.Motion == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "motion")
	}
	if len(cfg.Poses) == 0 {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "poses")
	}
	for i, p := range cfg.Poses {
		if p.PoseName == "" {
			return nil, nil, fmt.Errorf("%s: poses[%d] is missing required field \"pose_name\"", path, i)
		}
	}

	reqDeps := []string{cfg.ComponentName}
	if cfg.Motion == "builtin" {
		reqDeps = append(reqDeps, motion.Named("builtin").String())
	} else {
		reqDeps = append(reqDeps, cfg.Motion)
	}

	return reqDeps, nil, nil
}

type multiPosesExecutionSwitch struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name      resource.Name
	logger    logging.Logger
	cfg       *Config
	motion    motion.Service
	poseNames []string

	mu        sync.Mutex
	position  uint32
	executing atomic.Bool
}

func newMultiPosesExecutionSwitch(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (toggleswitch.Switch, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	motionSvc, err := motion.FromProvider(deps, conf.Motion)
	if err != nil {
		return nil, err
	}

	poseNames := make([]string, len(conf.Poses))
	for i, p := range conf.Poses {
		poseNames[i] = p.PoseName
	}

	return &multiPosesExecutionSwitch{
		name:      rawConf.ResourceName(),
		logger:    logger,
		cfg:       conf,
		motion:    motionSvc,
		poseNames: poseNames,
	}, nil
}

func (s *multiPosesExecutionSwitch) Name() resource.Name {
	return s.name
}

func (s *multiPosesExecutionSwitch) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if name, ok := cmd["set_position_by_name"].(string); ok {
		for i, pn := range s.poseNames {
			if pn == name {
				return nil, s.SetPosition(ctx, uint32(i), nil)
			}
		}
		return nil, fmt.Errorf("unknown pose name %q", name)
	}

	if _, ok := cmd["get_current_position_name"]; ok {
		s.mu.Lock()
		pos := s.position
		s.mu.Unlock()
		return map[string]interface{}{
			"position_name": s.poseNames[pos],
		}, nil
	}

	return nil, fmt.Errorf("unknown command, supported commands: set_position_by_name, get_current_position_name")
}

func (s *multiPosesExecutionSwitch) GetNumberOfPositions(ctx context.Context, extra map[string]interface{}) (uint32, []string, error) {
	return uint32(len(s.poseNames)), s.poseNames, nil
}

func (s *multiPosesExecutionSwitch) GetPosition(ctx context.Context, extra map[string]interface{}) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.position, nil
}

func (s *multiPosesExecutionSwitch) SetPosition(ctx context.Context, position uint32, extra map[string]interface{}) error {
	if position > uint32(len(s.poseNames))-1 {
		return fmt.Errorf("requested position %d is greater than highest possible position %d", position, len(s.poseNames)-1)
	}
	return s.goToPosition(ctx, position)
}

// goToPosition moves the component to the pose at the given index.
func (s *multiPosesExecutionSwitch) goToPosition(ctx context.Context, position uint32) error {
	if !s.executing.CompareAndSwap(false, true) {
		return errors.New("switch is currently executing")
	}
	defer s.executing.Store(false)

	pc := s.cfg.Poses[position]

	s.logger.Infof("moving %s to pose %q (index %d)", s.cfg.ComponentName, pc.PoseName, position)

	pose := spatialmath.NewPose(
		r3.Vector{X: pc.X, Y: pc.Y, Z: pc.Z},
		&spatialmath.OrientationVectorDegrees{OX: pc.OX, OY: pc.OY, OZ: pc.OZ, Theta: pc.ThetaDegrees},
	)
	destination := referenceframe.NewPoseInFrame(s.cfg.ReferenceFrame, pose)

	moveReq := motion.MoveReq{
		ComponentName: s.cfg.ComponentName,
		Destination:   destination,
	}
	if s.cfg.LinearConstraint != nil {
		moveReq.Constraints = &motionplan.Constraints{
			LinearConstraint: []motionplan.LinearConstraint{
				{
					LineToleranceMm:          s.cfg.LinearConstraint.LineToleranceMm,
					OrientationToleranceDegs: s.cfg.LinearConstraint.OrientationToleranceDegs,
				},
			},
		}
	}

	_, err := s.motion.Move(ctx, moveReq)
	if err != nil {
		return fmt.Errorf("failed to move to pose %q: %w", pc.PoseName, err)
	}

	s.mu.Lock()
	s.position = position
	s.mu.Unlock()

	return nil
}
