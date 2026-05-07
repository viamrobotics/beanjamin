// Package dialcontrolmotion registers a viam:beanjamin:dial-control-motion
// model that implements the rdk:service:generic API. It translates Stream Deck
// dial inputs into relative arm motions.
//
// Stream Deck delivers absolute dial positions (0..DialMaxPosition) via
// DoCommand. Each call infers a direction (±1) from the change since the last
// reading, converts that into a signed step (mm or deg), and accumulates it
// into a pending bucket per axis. A background drain loop flushes the bucket
// to the arm at DrainIntervalMs, applying a per-axis acceleration multiplier
// derived from how many detents arrived inside the window.
package dialcontrolmotion

import (
	"context"
	"fmt"
	"math"
	"runtime/debug"
	"sync"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/trace"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
	"gonum.org/v1/gonum/num/quat"
)

var Model = resource.NewModel("viam", "beanjamin", "dial-control-motion")

// ModuleVersion is a hand-bumped marker that proves which iteration of this
// model is actually running. Bump it whenever you change behavior so a
// machine's logs reveal whether the new code is loaded.
const ModuleVersion = "v4-ewma-smoothing-2026-05-07"

const (
	defaultMoveMM     float64 = 1.0
	defaultMoveDeg    float64 = 1.0
	defaultDrainMs    int     = 20 // 50 Hz
	defaultMaxPos     float64 = 100
	defaultAccelThr   float64 = 1.0
	defaultAccelMax   float64 = 10.0
	defaultAccelExp   float64 = 1.5
	defaultAccelAlpha float64 = 0.4
)

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newDialControlMotion,
		},
	)
}

type Config struct {
	ArmName string `json:"arm_name"`

	DialMoveXMM           float64 `json:"dial_move_x_mm,omitempty"`
	DialMoveYMM           float64 `json:"dial_move_y_mm,omitempty"`
	DialMoveZMM           float64 `json:"dial_move_z_mm,omitempty"`
	DialMoveOrientationMM float64 `json:"dial_move_orientation_mm,omitempty"`

	DialMoveRXDeg float64 `json:"dial_move_rx_deg,omitempty"`
	DialMoveRYDeg float64 `json:"dial_move_ry_deg,omitempty"`
	DialMoveRZDeg float64 `json:"dial_move_rz_deg,omitempty"`

	DialMaxPosition float64 `json:"dial_max_position,omitempty"`

	// DrainIntervalMs controls how often accumulated dial input is flushed to
	// the arm. Detents that arrive between flushes are summed, so faster
	// turning produces proportionally larger moves per flush.
	DrainIntervalMs int `json:"drain_interval_ms,omitempty"`

	// Acceleration: movement multiplier grows with detent rate. The raw per-window
	// count is smoothed via EWMA across drain windows so the multiplier ramps up
	// and decays gracefully instead of snapping per-flush.
	//
	//   smoothed = alpha * count + (1 - alpha) * smoothed_prev
	//   multiplier = clamp((smoothed / AccelThresholdCount)^AccelExponent, 1, AccelMaxMultiplier)
	//
	// AccelSmoothingAlpha in (0, 1]: 1 = no smoothing (instant), smaller = smoother/laggier.
	AccelThresholdCount float64 `json:"accel_threshold_count,omitempty"`
	AccelMaxMultiplier  float64 `json:"accel_max_multiplier,omitempty"`
	AccelExponent       float64 `json:"accel_exponent,omitempty"`
	AccelSmoothingAlpha float64 `json:"accel_smoothing_alpha,omitempty"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.ArmName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm_name")
	}
	return []string{arm.Named(cfg.ArmName).String()}, nil, nil
}

func (cfg *Config) maxPosition() float64 {
	if cfg.DialMaxPosition > 0 {
		return cfg.DialMaxPosition
	}
	return defaultMaxPos
}

func (cfg *Config) drainInterval() time.Duration {
	if cfg.DrainIntervalMs > 0 {
		return time.Duration(cfg.DrainIntervalMs) * time.Millisecond
	}
	return time.Duration(defaultDrainMs) * time.Millisecond
}

func (cfg *Config) accelThresholdCount() float64 {
	if cfg.AccelThresholdCount > 0 {
		return cfg.AccelThresholdCount
	}
	return defaultAccelThr
}

func (cfg *Config) accelMaxMultiplier() float64 {
	if cfg.AccelMaxMultiplier > 0 {
		return cfg.AccelMaxMultiplier
	}
	return defaultAccelMax
}

func (cfg *Config) accelExponent() float64 {
	if cfg.AccelExponent > 0 {
		return cfg.AccelExponent
	}
	return defaultAccelExp
}

func (cfg *Config) accelSmoothingAlpha() float64 {
	if cfg.AccelSmoothingAlpha > 0 && cfg.AccelSmoothingAlpha <= 1 {
		return cfg.AccelSmoothingAlpha
	}
	return defaultAccelAlpha
}

func (cfg *Config) moveMM(axis string) float64 {
	switch axis {
	case "x":
		if cfg.DialMoveXMM > 0 {
			return cfg.DialMoveXMM
		}
	case "y":
		if cfg.DialMoveYMM > 0 {
			return cfg.DialMoveYMM
		}
	case "z":
		if cfg.DialMoveZMM > 0 {
			return cfg.DialMoveZMM
		}
	case "orientation":
		if cfg.DialMoveOrientationMM > 0 {
			return cfg.DialMoveOrientationMM
		}
	}
	return defaultMoveMM
}

func (cfg *Config) moveDeg(axis string) float64 {
	switch axis {
	case "rx":
		if cfg.DialMoveRXDeg > 0 {
			return cfg.DialMoveRXDeg
		}
	case "ry":
		if cfg.DialMoveRYDeg > 0 {
			return cfg.DialMoveRYDeg
		}
	case "rz":
		if cfg.DialMoveRZDeg > 0 {
			return cfg.DialMoveRZDeg
		}
	}
	return defaultMoveDeg
}

type dialControlMotion struct {
	resource.AlwaysRebuild

	name       resource.Name
	logger     logging.Logger
	cfg        *Config
	arm        arm.Arm
	cancelCtx  context.Context
	cancelFunc func()

	// Direction-inference state for each axis. Stream Deck sends absolute
	// positions, so we reconstruct ±1 from successive readings.
	dialMu        sync.Mutex
	lastDial      map[string]*float64
	lastDirection map[string]float64

	pendingMu     sync.Mutex
	pendingMoves  map[string]float64 // axis -> accumulated signed delta (mm or deg)
	pendingCounts map[string]int     // axis -> detent count this window

	// smoothedCounts is owned exclusively by drainLoop; it carries an EWMA of
	// per-axis detent count across drain windows so the acceleration multiplier
	// ramps up and decays instead of snapping per-flush.
	smoothedCounts map[string]float64
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

	cancelCtx, cancelFunc := context.WithCancel(context.Background())
	s := &dialControlMotion{
		name:          rawConf.ResourceName(),
		logger:        logger,
		cfg:           conf,
		arm:           armComp,
		cancelCtx:     cancelCtx,
		cancelFunc:    cancelFunc,
		lastDial:      make(map[string]*float64),
		lastDirection: make(map[string]float64),
		pendingMoves:   make(map[string]float64),
		pendingCounts:  make(map[string]int),
		smoothedCounts: make(map[string]float64),
	}

	logger.Infow("dial-control-motion starting",
		"module_version", ModuleVersion,
		"vcs_revision", buildRevision(),
		"drain_interval_ms", conf.DrainIntervalMs,
		"accel_threshold_count", conf.AccelThresholdCount,
		"accel_max_multiplier", conf.AccelMaxMultiplier,
		"accel_exponent", conf.AccelExponent,
		"resolved_drain_interval", s.cfg.drainInterval(),
		"resolved_accel_threshold", s.cfg.accelThresholdCount(),
		"resolved_accel_max", s.cfg.accelMaxMultiplier(),
		"resolved_accel_exponent", s.cfg.accelExponent(),
		"resolved_accel_smoothing_alpha", s.cfg.accelSmoothingAlpha(),
	)

	go s.drainLoop()
	return s, nil
}

// buildRevision pulls the git commit (and dirty flag) baked in by `go build`
// from the binary's BuildInfo. Returns "unknown" outside a VCS tree.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, modified := "unknown", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	return rev + modified
}

func (s *dialControlMotion) Name() resource.Name {
	return s.name
}

func (s *dialControlMotion) Close(_ context.Context) error {
	s.cancelFunc()
	return nil
}

func (s *dialControlMotion) Status(ctx context.Context) (map[string]interface{}, error) {
	_, span := trace.StartSpan(ctx, "dial-control-motion::Status")
	defer span.End()
	return map[string]interface{}{}, nil
}

var supportedAxes = map[string]string{
	"dial_move_x":           "x",
	"dial_move_y":           "y",
	"dial_move_z":           "z",
	"dial_move_orientation": "orientation",
	"dial_move_rx":          "rx",
	"dial_move_ry":          "ry",
	"dial_move_rz":          "rz",
}

func (s *dialControlMotion) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	_, span := trace.StartSpan(ctx, "dial-control-motion::DoCommand")
	defer span.End()

	if _, ok := cmd["dial_move_speed"]; ok {
		err := fmt.Errorf("dial_move_speed has been removed; the module now uses automatic acceleration. Configure accel_threshold_count / accel_max_multiplier / accel_exponent instead")
		s.logger.Warnw("DoCommand", "error", err)
		return nil, err
	}

	for key, axis := range supportedAxes {
		if v, ok := cmd[key]; ok {
			return s.handleDialMove(axis, v)
		}
	}

	err := fmt.Errorf("unknown command, supported commands: dial_move_x/y/z/orientation/rx/ry/rz")
	s.logger.Warnw("DoCommand", "error", err)
	return nil, err
}

// handleDialMove converts an absolute dial reading into a signed step and
// enqueues it. It does NOT call the arm; drainLoop flushes accumulated moves.
func (s *dialControlMotion) handleDialMove(axis string, dialValue interface{}) (map[string]interface{}, error) {
	dialVal, ok := toFloat64(dialValue)
	if !ok {
		return nil, fmt.Errorf("%s: invalid dial value %v", axis, dialValue)
	}

	s.dialMu.Lock()
	last, seen := s.lastDial[axis]
	if !seen || last == nil {
		v := dialVal
		s.lastDial[axis] = &v
		s.dialMu.Unlock()
		return map[string]interface{}{"status": "dial_initialized", "axis": axis, "position": dialVal}, nil
	}

	maxPos := s.cfg.maxPosition()
	delta := dialVal - *last
	// Rollover correction: if the jump is more than half the range, it wrapped.
	if delta > maxPos/2 {
		delta -= maxPos + 1
	} else if delta < -maxPos/2 {
		delta += maxPos + 1
	}

	lastDir := s.lastDirection[axis]
	var direction float64
	if *last == 0 && lastDir != 0 {
		// At the zero boundary, the dial can't go lower so the value bounces
		// back up — continue in the last known direction instead of reversing.
		direction = lastDir
	} else if delta < 0 {
		direction = -1
	} else {
		direction = 1
	}
	s.lastDirection[axis] = direction
	*last = dialVal
	s.dialMu.Unlock()

	var step float64
	switch axis {
	case "rx", "ry", "rz":
		step = s.cfg.moveDeg(axis) * direction
	default:
		step = s.cfg.moveMM(axis) * direction
	}

	s.pendingMu.Lock()
	s.pendingMoves[axis] += step
	s.pendingCounts[axis]++
	pendingForAxis := s.pendingMoves[axis]
	countForAxis := s.pendingCounts[axis]
	s.pendingMu.Unlock()

	s.logger.Infow("dial detent queued",
		"module_version", ModuleVersion,
		"axis", axis,
		"dial_value", dialVal,
		"direction", direction,
		"step", step,
		"pending", pendingForAxis,
		"count", countForAxis,
	)

	return map[string]interface{}{"status": "queued", "axis": axis, "step": step}, nil
}

// drainLoop ticks at drain_interval_ms. Every tick it advances the per-axis
// smoothed-count EWMA (so multiplier decays even on quiet windows), and if any
// motion was accumulated this window, flushes it to the arm.
func (s *dialControlMotion) drainLoop() {
	ticker := time.NewTicker(s.cfg.drainInterval())
	defer ticker.Stop()

	for {
		select {
		case <-s.cancelCtx.Done():
			return
		case <-ticker.C:
			s.pendingMu.Lock()
			pending := s.pendingMoves
			counts := s.pendingCounts
			s.pendingMoves = make(map[string]float64)
			s.pendingCounts = make(map[string]int)
			s.pendingMu.Unlock()

			s.advanceSmoothedCounts(counts)

			if len(pending) == 0 {
				continue
			}

			multipliers := make(map[string]float64, len(pending))
			for axis := range pending {
				multipliers[axis] = s.accelMultiplier(s.smoothedCounts[axis])
			}
			s.logger.Infow("dial drain flush",
				"module_version", ModuleVersion,
				"pending", pending,
				"counts", counts,
				"smoothed", s.smoothedCounts,
				"multipliers", multipliers,
			)
			if err := s.flushMoves(pending, multipliers); err != nil {
				s.logger.Warnw("arm move failed", "error", err)
			}
		}
	}
}

// advanceSmoothedCounts updates the per-axis EWMA based on this window's raw
// counts. Active axes (count > 0) ease toward count; previously-active axes
// with no detents this window decay toward zero. Tiny residuals are pruned.
func (s *dialControlMotion) advanceSmoothedCounts(counts map[string]int) {
	alpha := s.cfg.accelSmoothingAlpha()

	for axis, c := range counts {
		s.smoothedCounts[axis] = alpha*float64(c) + (1-alpha)*s.smoothedCounts[axis]
	}
	for axis, sm := range s.smoothedCounts {
		if _, active := counts[axis]; active {
			continue
		}
		decayed := (1 - alpha) * sm
		if decayed < 0.05 {
			delete(s.smoothedCounts, axis)
		} else {
			s.smoothedCounts[axis] = decayed
		}
	}
}

// accelMultiplier returns the movement multiplier for a given smoothed count.
// At smoothed = threshold the multiplier is exactly 1×; below threshold it is
// also pinned to 1× (we never slow motion below base).
func (s *dialControlMotion) accelMultiplier(smoothed float64) float64 {
	threshold := s.cfg.accelThresholdCount()
	if smoothed <= threshold {
		return 1.0
	}
	multiplier := math.Pow(smoothed/threshold, s.cfg.accelExponent())
	return math.Min(multiplier, s.cfg.accelMaxMultiplier())
}

// flushMoves applies all accumulated deltas in a single EndPosition /
// MoveToPosition round-trip. Translation axes update the position vector;
// rotation axes are composed onto the orientation quaternion. Multipliers
// (one per axis) are pre-computed by drainLoop from the smoothed EWMA.
func (s *dialControlMotion) flushMoves(pending, multipliers map[string]float64) error {
	currentPose, err := s.arm.EndPosition(s.cancelCtx, map[string]interface{}{})
	if err != nil {
		return fmt.Errorf("failed to get arm position: %w", err)
	}

	pt := currentPose.Point()
	ori := currentPose.Orientation()

	mul := func(axis string) float64 {
		if m, ok := multipliers[axis]; ok {
			return m
		}
		return 1.0
	}

	for axis, delta := range pending {
		if axis == "rx" || axis == "ry" || axis == "rz" {
			continue
		}
		delta *= mul(axis)
		switch axis {
		case "x":
			pt.X += delta
		case "y":
			pt.Y += delta
		case "z":
			pt.Z += delta
		case "orientation":
			ov := ori.OrientationVectorRadians()
			pt = r3.Vector{
				X: pt.X + ov.OX*delta,
				Y: pt.Y + ov.OY*delta,
				Z: pt.Z + ov.OZ*delta,
			}
		}
	}

	for _, axis := range []string{"rx", "ry", "rz"} {
		delta, ok := pending[axis]
		if !ok {
			continue
		}
		delta *= mul(axis)
		newPose, err := rotatePose(spatialmath.NewPose(pt, ori), axis, delta)
		if err != nil {
			return err
		}
		pt = newPose.Point()
		ori = newPose.Orientation()
	}

	return s.arm.MoveToPosition(s.cancelCtx, spatialmath.NewPose(pt, ori), map[string]interface{}{})
}

// rotatePose applies a rotation of deg degrees around the given world axis
// (rx/ry/rz) by left-multiplying the rotation quaternion onto the pose's
// orientation.
func rotatePose(pose spatialmath.Pose, axis string, deg float64) (spatialmath.Pose, error) {
	theta := deg * math.Pi / 180.0
	half := theta / 2.0
	cosHalf, sinHalf := math.Cos(half), math.Sin(half)

	var rotQ quat.Number
	switch axis {
	case "rx":
		rotQ = quat.Number{Real: cosHalf, Imag: sinHalf}
	case "ry":
		rotQ = quat.Number{Real: cosHalf, Jmag: sinHalf}
	case "rz":
		rotQ = quat.Number{Real: cosHalf, Kmag: sinHalf}
	default:
		return nil, fmt.Errorf("unknown rotation axis %q", axis)
	}

	currentQ := pose.Orientation().Quaternion()
	newQ := quat.Mul(rotQ, quat.Number{
		Real: currentQ.Real,
		Imag: currentQ.Imag,
		Jmag: currentQ.Jmag,
		Kmag: currentQ.Kmag,
	})

	mag := quat.Abs(newQ)
	if mag > 1e-10 {
		newQ = quat.Scale(1.0/mag, newQ)
	}

	w := math.Min(1.0, math.Max(-1.0, newQ.Real))
	theta2 := 2.0 * math.Acos(w)
	sinT := math.Sin(theta2 / 2.0)

	var ori spatialmath.Orientation
	if sinT < 1e-10 {
		ori = &spatialmath.R4AA{Theta: 0, RX: 1}
	} else {
		ori = &spatialmath.R4AA{
			Theta: theta2,
			RX:    newQ.Imag / sinT,
			RY:    newQ.Jmag / sinT,
			RZ:    newQ.Kmag / sinT,
		}
	}

	return spatialmath.NewPose(pose.Point(), ori), nil
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
