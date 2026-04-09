// Package dialcontrolmotion registers a viam:beanjamin:dial-control-motion
// model that implements the rdk:service:generic API. It translates Stream Deck
// dial inputs into relative arm motions.
package dialcontrolmotion

import (
	"context"
	"fmt"
	"math"
	"sync"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
)

var Model = resource.NewModel("viam", "beanjamin", "dial-control-motion")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newDialControlMotion,
		},
	)
}

type Config struct {
	ArmName               string  `json:"arm_name"`
	DialMoveXMM           float64 `json:"dial_move_x_mm,omitempty"`
	DialMoveYMM           float64 `json:"dial_move_y_mm,omitempty"`
	DialMoveZMM           float64 `json:"dial_move_z_mm,omitempty"`
	DialMoveOrientationMM float64 `json:"dial_move_orientation_mm,omitempty"`
	DialMaxPosition       float64 `json:"dial_max_position,omitempty"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.ArmName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm_name")
	}
	return []string{arm.Named(cfg.ArmName).String()}, nil, nil
}

type dialControlMotion struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger
	cfg    *Config
	arm    arm.Arm

	mu sync.Mutex
	// Last known absolute dial positions and directions for direction detection
	lastDialX              *float64
	lastDialY              *float64
	lastDialZ              *float64
	lastDialSpeed          *float64
	lastDialOrientation    *float64
	lastDialDirX           float64
	lastDialDirY           float64
	lastDialDirZ           float64
	lastDialDirOrientation float64
	lastDialDirSpeed       float64
}

func newDialControlMotion(_ context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	armComp, err := arm.FromProvider(deps, conf.ArmName)
	if err != nil {
		return nil, fmt.Errorf("arm %q not found in dependencies: %w", conf.ArmName, err)
	}

	return &dialControlMotion{
		name:   rawConf.ResourceName(),
		logger: logger,
		cfg:    conf,
		arm:    armComp,
	}, nil
}

func (s *dialControlMotion) Name() resource.Name {
	return s.name
}

func (s *dialControlMotion) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (s *dialControlMotion) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if v, ok := cmd["dial_move_x"]; ok {
		return s.handleDialMove(ctx, "x", v)
	}
	if v, ok := cmd["dial_move_y"]; ok {
		return s.handleDialMove(ctx, "y", v)
	}
	if v, ok := cmd["dial_move_z"]; ok {
		return s.handleDialMove(ctx, "z", v)
	}
	if v, ok := cmd["dial_move_orientation"]; ok {
		return s.handleDialMove(ctx, "orientation", v)
	}
	if v, ok := cmd["dial_move_speed"]; ok {
		return s.handleDialSpeedMove(ctx, v)
	}
	err := fmt.Errorf("unknown command, supported commands: dial_move_x/y/z/orientation/speed")
	s.logger.Warnw("DoCommand", "error", err)
	return nil, err
}

func (s *dialControlMotion) handleMoveArm(ctx context.Context, axis string, mm float64) (map[string]interface{}, error) {
	currentPose, err := s.arm.EndPosition(ctx, map[string]interface{}{})
	if err != nil {
		return nil, fmt.Errorf("failed to get current arm position: %w", err)
	}
	pt := currentPose.Point()
	switch axis {
	case "x":
		pt.X += mm
	case "y":
		pt.Y += mm
	case "z":
		pt.Z += mm
	case "orientation":
		ov := currentPose.Orientation().OrientationVectorDegrees()
		// Normalize the orientation direction vector
		norm := math.Sqrt(ov.OX*ov.OX + ov.OY*ov.OY + ov.OZ*ov.OZ)
		if norm > 0 {
			pt.X += mm * ov.OX / norm
			pt.Y += mm * ov.OY / norm
			pt.Z += mm * ov.OZ / norm
		}
	}
	newPose := spatialmath.NewPose(pt, currentPose.Orientation())
	if err := s.arm.MoveToPosition(ctx, newPose, map[string]interface{}{}); err != nil {
		return nil, fmt.Errorf("failed to move arm: %w", err)
	}
	return map[string]interface{}{"status": "moved", "axis": axis, "mm": mm}, nil
}

func (s *dialControlMotion) handleDialMove(ctx context.Context, axis string, dialValue interface{}) (map[string]interface{}, error) {
	var mm float64
	switch axis {
	case "x":
		mm = s.cfg.DialMoveXMM
	case "y":
		mm = s.cfg.DialMoveYMM
	case "z":
		mm = s.cfg.DialMoveZMM
	case "orientation":
		mm = s.cfg.DialMoveOrientationMM
	}
	if mm == 0 {
		mm = 1
	}
	if dialVal, ok := toFloat64(dialValue); ok {
		s.mu.Lock()
		var last **float64
		var lastDir *float64
		switch axis {
		case "x":
			last = &s.lastDialX
			lastDir = &s.lastDialDirX
		case "y":
			last = &s.lastDialY
			lastDir = &s.lastDialDirY
		case "z":
			last = &s.lastDialZ
			lastDir = &s.lastDialDirZ
		case "orientation":
			last = &s.lastDialOrientation
			lastDir = &s.lastDialDirOrientation
		}
		if *last == nil {
			// First reading — store position and skip move (no direction yet)
			*last = &dialVal
			s.mu.Unlock()
			return map[string]interface{}{"status": "dial_initialized", "axis": axis, "position": dialVal}, nil
		}
		maxPos := s.cfg.DialMaxPosition
		if maxPos == 0 {
			maxPos = 100
		}
		delta := dialVal - **last
		// Correct for rollover: if the jump is more than half the range, it wrapped
		if delta > maxPos/2 {
			delta -= maxPos + 1
		} else if delta < -maxPos/2 {
			delta += maxPos + 1
		}
		var direction float64
		if **last == 0 && *lastDir != 0 {
			// At the zero boundary, the dial can't go lower so the value bounces
			// back up — continue in the last known direction instead of reversing
			direction = *lastDir
		} else if delta < 0 {
			direction = -1
		} else {
			direction = 1
		}
		if direction < 0 {
			mm = -mm
		}
		*lastDir = direction
		**last = dialVal
		s.mu.Unlock()
	}
	return s.handleMoveArm(ctx, axis, mm)
}

func (s *dialControlMotion) handleDialSpeedMove(_ context.Context, dialValue interface{}) (map[string]interface{}, error) {
	const speedStepMM = 1.0
	const minSpeedMM = 0.5

	dialVal, ok := toFloat64(dialValue)
	if !ok {
		return nil, fmt.Errorf("dial_move_speed: invalid value %v", dialValue)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lastDialSpeed == nil {
		s.lastDialSpeed = &dialVal
		return map[string]interface{}{"status": "dial_initialized", "axis": "speed", "position": dialVal}, nil
	}

	maxPos := s.cfg.DialMaxPosition
	if maxPos == 0 {
		maxPos = 100
	}
	delta := dialVal - *s.lastDialSpeed
	if delta > maxPos/2 {
		delta -= maxPos + 1
	} else if delta < -maxPos/2 {
		delta += maxPos + 1
	}

	var direction float64
	if *s.lastDialSpeed == 0 && s.lastDialDirSpeed != 0 {
		direction = s.lastDialDirSpeed
	} else if delta < 0 {
		direction = -1
	} else {
		direction = 1
	}
	s.lastDialDirSpeed = direction
	*s.lastDialSpeed = dialVal

	step := speedStepMM * direction
	applyStep := func(current float64) float64 {
		if current == 0 {
			current = 1
		}
		if v := current + step; v >= minSpeedMM {
			return v
		}
		return minSpeedMM
	}
	s.cfg.DialMoveXMM = applyStep(s.cfg.DialMoveXMM)
	s.cfg.DialMoveYMM = applyStep(s.cfg.DialMoveYMM)
	s.cfg.DialMoveZMM = applyStep(s.cfg.DialMoveZMM)
	s.cfg.DialMoveOrientationMM = applyStep(s.cfg.DialMoveOrientationMM)

	return map[string]interface{}{
		"status":                   "speed_updated",
		"dial_move_x_mm":           s.cfg.DialMoveXMM,
		"dial_move_y_mm":           s.cfg.DialMoveYMM,
		"dial_move_z_mm":           s.cfg.DialMoveZMM,
		"dial_move_orientation_mm": s.cfg.DialMoveOrientationMM,
	}, nil
}

func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
