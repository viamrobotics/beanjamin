package multiposesexecutionswitch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/golang/geo/r3"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/motion"
	"go.viam.com/rdk/spatialmath"

	"beanjamin/statemachine"
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
	ReferenceFrame   string     `json:"reference_frame"`
	ComponentName    string     `json:"component_name"`
	Motion           string     `json:"motion"`
	StateMachineName string     `json:"state_machine_name"`
	Poses            []PoseConf `json:"poses"`
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
	if cfg.StateMachineName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "state_machine_name")
	}
	if len(cfg.Poses) == 0 {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "poses")
	}
	for i, p := range cfg.Poses {
		if p.PoseName == "" {
			return nil, nil, fmt.Errorf("%s: poses[%d] is missing required field \"pose_name\"", path, i)
		}
	}

	reqDeps := []string{cfg.ComponentName, generic.Named(cfg.StateMachineName).String()}
	if cfg.Motion == "builtin" {
		reqDeps = append(reqDeps, motion.Named("builtin").String())
	} else {
		reqDeps = append(reqDeps, cfg.Motion)
	}

	return reqDeps, nil, nil
}

type multiPosesExecutionSwitch struct {
	resource.AlwaysRebuild

	name         resource.Name
	logger       logging.Logger
	cfg          *Config
	motion       motion.Service
	poseNames    []string
	stateMachine statemachine.Service

	mu         sync.Mutex
	executing  atomic.Bool
	cancelFunc context.CancelFunc
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

	smRes, ok := deps[generic.Named(conf.StateMachineName)]
	if !ok {
		return nil, fmt.Errorf("state machine %q not found in dependencies", conf.StateMachineName)
	}
	sm, ok := smRes.(statemachine.Service)
	if !ok {
		return nil, fmt.Errorf("resource %q is not a statemachine.Service", conf.StateMachineName)
	}

	return &multiPosesExecutionSwitch{
		name:         rawConf.ResourceName(),
		logger:       logger,
		cfg:          conf,
		motion:       motionSvc,
		poseNames:    poseNames,
		stateMachine: sm,
		cancelFunc:   func() {},
	}, nil
}

func (s *multiPosesExecutionSwitch) Name() resource.Name {
	return s.name
}

func (s *multiPosesExecutionSwitch) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if name, ok := cmd["set_position_by_name"].(string); ok {
		for i, pn := range s.poseNames {
			if pn == name {
				if err := s.SetPosition(ctx, uint32(i), nil); err != nil {
					s.logger.Errorw("DoCommand", "error", err)
					return nil, err
				}
				return nil, nil
			}
		}
		err := fmt.Errorf("unknown pose name %q", name)
		s.logger.Warnw("DoCommand", "error", err)
		return nil, err
	}

	if _, ok := cmd["get_current_position_name"]; ok {
		pos, err := s.GetPosition(ctx, nil)
		if err != nil {
			s.logger.Warnw("DoCommand", "error", err)
			return nil, err
		}
		return map[string]interface{}{
			"position_name": s.poseNames[pos],
		}, nil
	}

	if name, ok := cmd["get_pose_by_name"].(string); ok {
		for _, pc := range s.cfg.Poses {
			if pc.PoseName == name {
				return map[string]interface{}{
					"x":               pc.X,
					"y":               pc.Y,
					"z":               pc.Z,
					"o_x":             pc.OX,
					"o_y":             pc.OY,
					"o_z":             pc.OZ,
					"theta_degrees":   pc.ThetaDegrees,
					"reference_frame": s.cfg.ReferenceFrame,
					"component_name":  s.cfg.ComponentName,
				}, nil
			}
		}
		err := fmt.Errorf("unknown pose name %q", name)
		s.logger.Warnw("DoCommand", "error", err)
		return nil, err
	}

	if _, ok := cmd["cancel"]; ok {
		if !s.executing.Load() {
			err := errors.New("no move in progress")
			s.logger.Warnw("DoCommand", "error", err)
			return nil, err
		}
		s.mu.Lock()
		s.cancelFunc()
		s.mu.Unlock()
		return map[string]interface{}{"status": "cancelled"}, nil
	}

	err := fmt.Errorf("unknown command, supported commands: set_position_by_name, get_current_position_name, get_pose_by_name, cancel")
	s.logger.Warnw("DoCommand", "error", err)
	return nil, err
}

func (s *multiPosesExecutionSwitch) Close(context.Context) error {
	s.mu.Lock()
	s.cancelFunc()
	s.mu.Unlock()
	return nil
}

func (s *multiPosesExecutionSwitch) GetNumberOfPositions(ctx context.Context, extra map[string]interface{}) (uint32, []string, error) {
	return uint32(len(s.poseNames)), s.poseNames, nil
}

func (s *multiPosesExecutionSwitch) GetPosition(ctx context.Context, extra map[string]interface{}) (uint32, error) {
	currentPoseName := s.stateMachine.GetState()
	if currentPoseName == "" {
		// State machine uninitialized — detect from current component pose and initialize.
		detected, err := s.detectCurrentPose(ctx)
		if err != nil {
			return 0, fmt.Errorf("state machine uninitialized and pose detection failed: %w", err)
		}
		if err := s.stateMachine.SetState(detected); err != nil {
			return 0, fmt.Errorf("failed to initialize state machine from detected pose: %w", err)
		}
		currentPoseName = detected
	}
	for i, name := range s.poseNames {
		if name == currentPoseName {
			return uint32(i), nil
		}
	}
	return 0, fmt.Errorf("state machine current pose %q is not in this switch's pose list", currentPoseName)
}

func (s *multiPosesExecutionSwitch) SetPosition(ctx context.Context, position uint32, extra map[string]interface{}) error {
	if position > uint32(len(s.poseNames))-1 {
		return fmt.Errorf("requested position %d is greater than highest possible position %d", position, len(s.poseNames)-1)
	}
	return s.goToPosition(ctx, position)
}

// detectCurrentPose queries the component's current pose via the motion service and
// finds the closest configured pose within tolerance. Returns the pose name, or an
// error if no pose matches.
func (s *multiPosesExecutionSwitch) detectCurrentPose(ctx context.Context) (string, error) {
	poseInFrame, err := s.motion.GetPose(ctx, s.cfg.ComponentName, s.cfg.ReferenceFrame, nil, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get component pose: %w", err)
	}
	currentPose := poseInFrame.Pose()

	const positionToleranceMm = 5.0
	const orientationToleranceDeg = 5.0

	bestName := ""
	bestPosDist := math.MaxFloat64

	cp := currentPose.Point()

	for _, pc := range s.cfg.Poses {
		target := spatialmath.NewPose(
			r3.Vector{X: pc.X, Y: pc.Y, Z: pc.Z},
			&spatialmath.OrientationVectorDegrees{OX: pc.OX, OY: pc.OY, OZ: pc.OZ, Theta: pc.ThetaDegrees},
		)

		tp := target.Point()
		dx, dy, dz := cp.X-tp.X, cp.Y-tp.Y, cp.Z-tp.Z
		posDist := math.Sqrt(dx*dx + dy*dy + dz*dz)
		if posDist > positionToleranceMm {
			continue
		}

		q1 := currentPose.Orientation().Quaternion()
		q2 := target.Orientation().Quaternion()
		dot := q1.Real*q2.Real + q1.Imag*q2.Imag + q1.Jmag*q2.Jmag + q1.Kmag*q2.Kmag
		oriAngleDeg := 2 * math.Acos(math.Min(1.0, math.Abs(dot))) * 180 / math.Pi
		if oriAngleDeg > orientationToleranceDeg {
			continue
		}

		if posDist < bestPosDist {
			bestPosDist = posDist
			bestName = pc.PoseName
		}
	}

	if bestName == "" {
		return "", fmt.Errorf(
			"arm position does not match any configured pose within %.1fmm / %.1f°; use set_state to initialize manually",
			positionToleranceMm, orientationToleranceDeg,
		)
	}
	return bestName, nil
}

// poseConfigByName finds the PoseConf with the given name from cfg.Poses.
func (s *multiPosesExecutionSwitch) poseConfigByName(name string) (*PoseConf, bool) {
	for i := range s.cfg.Poses {
		if s.cfg.Poses[i].PoseName == name {
			return &s.cfg.Poses[i], true
		}
	}
	return nil, false
}

// movePose executes a single motion move to the given pose configuration.
func (s *multiPosesExecutionSwitch) movePose(ctx context.Context, pc *PoseConf) error {
	pose := spatialmath.NewPose(
		r3.Vector{X: pc.X, Y: pc.Y, Z: pc.Z},
		&spatialmath.OrientationVectorDegrees{OX: pc.OX, OY: pc.OY, OZ: pc.OZ, Theta: pc.ThetaDegrees},
	)
	destination := referenceframe.NewPoseInFrame(s.cfg.ReferenceFrame, pose)

	_, err := s.motion.Move(ctx, motion.MoveReq{
		ComponentName: s.cfg.ComponentName,
		Destination:   destination,
	})
	if err != nil {
		return fmt.Errorf("failed to move to pose %q: %w", pc.PoseName, err)
	}
	return nil
}

// goToPosition moves the component to the pose at the given index, routing
// through state-machine intermediates when a state machine is configured.
func (s *multiPosesExecutionSwitch) goToPosition(ctx context.Context, position uint32) error {
	ctx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	if !s.executing.CompareAndSwap(false, true) {
		s.mu.Unlock()
		cancel()
		return errors.New("switch is currently executing")
	}
	s.cancelFunc = cancel
	s.mu.Unlock()

	defer s.executing.Store(false)
	defer cancel()

	targetPose := s.cfg.Poses[position]
	s.logger.Infof("moving %s to pose %q (index %d)", s.cfg.ComponentName, targetPose.PoseName, position)

	intermediates, finalPose, err := s.stateMachine.ResolvePath(targetPose.PoseName)
	if err != nil {
		s.logger.Warnf("state machine ResolvePath failed for %q: %v", targetPose.PoseName, err)
		return err
	}

	// Execute intermediate poses.
	for _, name := range intermediates {
		pc, ok := s.poseConfigByName(name)
		if !ok {
			return fmt.Errorf("intermediate pose %q is not configured on this switch", name)
		}
		s.logger.Infof("routing through intermediate pose %q", name)
		if err := s.movePose(ctx, pc); err != nil {
			return err
		}
		s.stateMachine.CommitTransition(name)
	}
	// Execute the final target pose.
	if err := s.movePose(ctx, &targetPose); err != nil {
		return err
	}
	s.stateMachine.CommitTransition(finalPose)
	return nil
}
