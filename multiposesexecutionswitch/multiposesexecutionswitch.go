package multiposesexecutionswitch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/golang/geo/r3"
	commonpb "go.viam.com/api/common/v1"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/motion"
	"go.viam.com/rdk/spatialmath"
)

var Model = resource.NewModel("viam", "beanjamin", "filter-poses-switch")

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
	ReferenceFrame string     `json:"reference_frame"`
	ComponentName  string     `json:"component_name"`
	Motion         string     `json:"motion"`
	Poses          []PoseConf `json:"poses"`
}

type PoseConf struct {
	PoseName    string         `json:"pose_name"`
	PoseValue   *commonpb.Pose `json:"pose_value,omitempty"`
	Baseline    string         `json:"baseline,omitempty"`
	Translation *Translation   `json:"translation,omitempty"`
	Orientation *Orientation   `json:"orientation,omitempty"`
}

type Translation struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type Orientation struct {
	OX    float64 `json:"o_x"`
	OY    float64 `json:"o_y"`
	OZ    float64 `json:"o_z"`
	Theta float64 `json:"theta"`
}

// poseValues holds the resolved absolute coordinates for a pose.
type poseValues struct {
	X, Y, Z    float64
	OX, OY, OZ float64
	Theta      float64
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
	// First pass: collect all names and check for duplicates.
	allNames := make(map[string]bool, len(cfg.Poses))
	for i, p := range cfg.Poses {
		if p.PoseName == "" {
			return nil, nil, fmt.Errorf("%s: poses[%d] is missing required field \"pose_name\"", path, i)
		}
		if allNames[p.PoseName] {
			return nil, nil, fmt.Errorf("%s: poses[%d] has duplicate pose_name %q", path, i, p.PoseName)
		}
		allNames[p.PoseName] = true
	}

	// Second pass: validate fields and baseline references.
	baselineOf := make(map[string]string, len(cfg.Poses))
	for i, p := range cfg.Poses {
		hasPoseValue := p.PoseValue != nil
		hasBaseline := p.Baseline != ""

		if hasPoseValue && hasBaseline {
			return nil, nil, fmt.Errorf("%s: poses[%d] (%q) cannot have both \"pose_value\" and \"baseline\"", path, i, p.PoseName)
		}
		if !hasPoseValue && !hasBaseline {
			return nil, nil, fmt.Errorf("%s: poses[%d] (%q) must have either \"pose_value\" or \"baseline\"", path, i, p.PoseName)
		}

		if !hasBaseline && (p.Translation != nil || p.Orientation != nil) {
			return nil, nil, fmt.Errorf("%s: poses[%d] (%q) \"translation\" and \"orientation\" require \"baseline\"", path, i, p.PoseName)
		}

		if hasBaseline {
			if !allNames[p.Baseline] {
				return nil, nil, fmt.Errorf("%s: poses[%d] (%q) baseline %q not found in poses", path, i, p.PoseName, p.Baseline)
			}
			baselineOf[p.PoseName] = p.Baseline
		}
	}

	// Third pass: detect cycles in baseline references.
	// A pose is "in a cycle" if following its baseline chain leads back to itself.
	// We collect all such poses (in config order) and report them together.
	inCycle := make(map[string]bool)
	for _, p := range cfg.Poses {
		if _, ok := baselineOf[p.PoseName]; !ok {
			continue
		}
		visited := map[string]bool{p.PoseName: true}
		for cur := baselineOf[p.PoseName]; cur != ""; cur = baselineOf[cur] {
			if visited[cur] {
				inCycle[p.PoseName] = true
				break
			}
			visited[cur] = true
		}
	}
	if len(inCycle) > 0 {
		var cycleNames []string
		for _, p := range cfg.Poses {
			if inCycle[p.PoseName] {
				cycleNames = append(cycleNames, fmt.Sprintf("%q", p.PoseName))
			}
		}
		return nil, nil, fmt.Errorf("%s: baseline cycle detected involving poses: %s",
			path, strings.Join(cycleNames, ", "))
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

	name          resource.Name
	logger        logging.Logger
	cfg           *Config
	motion        motion.Service
	poseNames     []string
	resolvedPoses []poseValues

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

	resolvedPoses := resolvePoses(conf.Poses)

	return &multiPosesExecutionSwitch{
		name:          rawConf.ResourceName(),
		logger:        logger,
		cfg:           conf,
		motion:        motionSvc,
		poseNames:     poseNames,
		resolvedPoses: resolvedPoses,
	}, nil
}

// resolvePoses resolves all poses to absolute values by applying baseline
// translations and orientation overrides. Baselines may appear in any order;
// cycles must be rejected by Validate before calling this function.
func resolvePoses(poses []PoseConf) []poseValues {
	resolved := make([]poseValues, len(poses))
	done := make([]bool, len(poses))
	nameToIdx := make(map[string]int, len(poses))
	for i, p := range poses {
		nameToIdx[p.PoseName] = i
	}

	var resolve func(i int)
	resolve = func(i int) {
		if done[i] {
			return
		}
		p := poses[i]
		if p.PoseValue != nil {
			resolved[i] = poseValues{
				X: p.PoseValue.X, Y: p.PoseValue.Y, Z: p.PoseValue.Z,
				OX: p.PoseValue.OX, OY: p.PoseValue.OY, OZ: p.PoseValue.OZ, Theta: p.PoseValue.Theta,
			}
		} else {
			baseIdx := nameToIdx[p.Baseline]
			resolve(baseIdx)
			base := resolved[baseIdx]

			if p.Translation != nil {
				base.X += p.Translation.X
				base.Y += p.Translation.Y
				base.Z += p.Translation.Z
			}

			if p.Orientation != nil {
				base.OX = p.Orientation.OX
				base.OY = p.Orientation.OY
				base.OZ = p.Orientation.OZ
				base.Theta = p.Orientation.Theta
			}

			resolved[i] = base
		}
		done[i] = true
	}

	for i := range poses {
		resolve(i)
	}

	return resolved
}

func (s *multiPosesExecutionSwitch) Name() resource.Name {
	return s.name
}

func (s *multiPosesExecutionSwitch) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
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
		s.mu.Lock()
		pos := s.position
		s.mu.Unlock()
		return map[string]interface{}{
			"position_name": s.poseNames[pos],
		}, nil
	}

	if name, ok := cmd["get_pose_by_name"].(string); ok {
		for i, pn := range s.poseNames {
			if pn == name {
				rp := s.resolvedPoses[i]
				return map[string]interface{}{
					"x":               rp.X,
					"y":               rp.Y,
					"z":               rp.Z,
					"o_x":             rp.OX,
					"o_y":             rp.OY,
					"o_z":             rp.OZ,
					"theta":           rp.Theta,
					"reference_frame": s.cfg.ReferenceFrame,
					"component_name":  s.cfg.ComponentName,
				}, nil
			}
		}
		err := fmt.Errorf("unknown pose name %q", name)
		s.logger.Warnw("DoCommand", "error", err)
		return nil, err
	}

	err := fmt.Errorf("unknown command, supported commands: set_position_by_name, get_current_position_name, get_pose_by_name")
	s.logger.Warnw("DoCommand", "error", err)
	return nil, err
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

	rp := s.resolvedPoses[position]

	s.logger.Infof("moving %s to pose %q (index %d)", s.cfg.ComponentName, s.poseNames[position], position)

	pose := spatialmath.NewPose(
		r3.Vector{X: rp.X, Y: rp.Y, Z: rp.Z},
		&spatialmath.OrientationVectorDegrees{OX: rp.OX, OY: rp.OY, OZ: rp.OZ, Theta: rp.Theta},
	)
	destination := referenceframe.NewPoseInFrame(s.cfg.ReferenceFrame, pose)

	_, err := s.motion.Move(ctx, motion.MoveReq{
		ComponentName: s.cfg.ComponentName,
		Destination:   destination,
	})
	if err != nil {
		return fmt.Errorf("failed to move to pose %q: %w", s.poseNames[position], err)
	}

	s.mu.Lock()
	s.position = position
	s.mu.Unlock()

	return nil
}
