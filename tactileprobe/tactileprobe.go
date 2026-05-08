// Package tactileprobe implements viam:beanjamin:tactile-probe, a tactile
// touch-off calibration service.
//
// The service drives the arm in a chosen direction and treats a MoveToPosition
// failure as a contact event — reading the EEF position at the moment of
// failure as the contact point. It can probe a single surface (e.g. the
// underside of an object) or run a full calibration: probe down to find a
// bottom index surface, probe both sides at a fixed clearance to find the
// horizontal centerline, then compute a button pose from the configured
// object profile (button height above bottom, etc.). The resulting pose can
// optionally be written into a multi-poses-execution-switch via its
// set_pose_value DoCommand for downstream use.
package tactileprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	vmodutils "github.com/erh/vmodutils"
	"github.com/golang/geo/r3"

	"go.viam.com/rdk/components/arm"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/utils"
)

var Model = resource.NewModel("viam", "beanjamin", "tactile-probe")

const (
	defaultProbeMaxTravelMM     = 100.0
	defaultProbeStepMM          = 1.0
	defaultProbeStepPauseMs     = 0
	defaultSideProbeAboveBottom = 5.0
	defaultProbeCount           = 1
	defaultBottomAxis           = "-z"
	defaultCenterAxis           = "y"

	defaultLoadBaselineSamples = 10
	defaultLoadBaselineAlpha   = 0.1
	defaultLoadSettleMs        = 100
)

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newService,
		},
	)
}

type Config struct {
	ArmName          string `json:"arm_name"`
	PoseSwitcherName string `json:"pose_switcher_name,omitempty"`

	ProbeMaxTravelMM float64 `json:"probe_max_travel_mm,omitempty"`
	// ProbeStepMM is how far the arm moves per substep during a probe. Smaller
	// values give finer contact resolution and slower effective speed (each
	// step incurs MoveToPosition + planner latency); larger values reach the
	// surface faster but overshoot more.
	ProbeStepMM float64 `json:"probe_step_mm,omitempty"`
	// ProbeStepPauseMs is an optional sleep between substeps. Combined with
	// ProbeStepMM, gives a rough effective probe speed of step / (move + pause).
	ProbeStepPauseMs int `json:"probe_step_pause_ms,omitempty"`

	// LoadThreshold (Nm) is the abs-delta from the rolling baseline at which a
	// joint load reading is treated as a contact event. If zero, load-based
	// contact detection is disabled and the probe relies solely on
	// MoveToPosition returning an error to declare contact.
	LoadThreshold float64 `json:"load_threshold,omitempty"`
	// LoadBaselineSamples is how many readings are taken before probing begins
	// to establish the joint-load baseline. Default 10.
	LoadBaselineSamples int `json:"load_baseline_samples,omitempty"`
	// LoadBaselineAlpha is the EWMA factor used to drift the baseline as the
	// arm pose changes during probing (so gradual gravity-load shifts don't
	// trigger false contact). Range (0, 1]; default 0.1 (gentle drift).
	LoadBaselineAlpha float64 `json:"load_baseline_alpha,omitempty"`
	// LoadSettleMs is how long to wait before each load reading so the arm
	// has time to physically settle from the previous motion. Without this,
	// load readings capture residual oscillation and decel artifacts that
	// look indistinguishable from real contact. Default 100ms.
	LoadSettleMs int `json:"load_settle_ms,omitempty"`

	Profiles map[string]ObjectProfile `json:"profiles,omitempty"`
}

type ObjectProfile struct {
	// --- geometry ---

	// ButtonHeightAboveBottomMM is the vertical offset of the button above the
	// bottom index surface (along -bottom_axis). Required.
	ButtonHeightAboveBottomMM float64 `json:"button_height_above_bottom_mm"`

	// ProbeAxisOffsetMM is the scalar distance the probe tip extends past the
	// EEF origin in the probing direction. With bottom_axis=-z and offset=50,
	// the tip is 50mm below the EEF center; reported "surface" positions and
	// any pose/coord output of `calibrate` that refers to the surface are
	// adjusted by this offset. The button-EEF pose itself is unaffected
	// (offset cancels: probing-down lowers the EEF by `offset` more than the
	// surface, so the saved EEF pose for pressing ends up offset-independent).
	ProbeAxisOffsetMM float64 `json:"probe_axis_offset_mm,omitempty"`

	// SideProbeAboveBottomMM is the clearance above the just-found bottom at
	// which the side probes happen. Used by `probe_width` only; `calibrate`
	// no longer side-probes.
	SideProbeAboveBottomMM float64 `json:"side_probe_above_bottom_mm,omitempty"`

	// MaxWidthMM caps the per-side probe travel for `probe_width`. Falls back
	// to the service-level ProbeMaxTravelMM when zero. Unused by `calibrate`.
	MaxWidthMM float64 `json:"max_width_mm,omitempty"`

	// BottomAxis is the direction the arm probes to find the bottom index
	// surface. Default "-z".
	BottomAxis string `json:"bottom_axis,omitempty"`

	// CenterAxis is the world axis along which the operator visually centers
	// the probe over the target before calling `calibrate`. The start pose's
	// coordinate along this axis is captured into CenterAxisValueMM.
	// `probe_width` also probes ±this axis. Default "y".
	CenterAxis string `json:"center_axis,omitempty"`

	// ProbeCount is how many times each surface is probed; results are
	// averaged. Default 1.
	ProbeCount int `json:"probe_count,omitempty"`

	// --- calibration outputs (auto-populated by `calibrate`) ---

	// CenterAxisValueMM is the world-frame coordinate (along CenterAxis) of
	// the start pose at the time `calibrate` last ran. Reflects whatever
	// position the operator visually centered the probe at.
	CenterAxisValueMM float64 `json:"center_axis_value_mm,omitempty"`

	// ProbeAxisValueMM is the world-frame coordinate (along the BottomAxis
	// dimension, signed natively) of the bottom surface measured by the last
	// `calibrate` call. Includes ProbeAxisOffsetMM.
	ProbeAxisValueMM float64 `json:"probe_axis_value_mm,omitempty"`

	// Overrides is a map of world-axis name ("x"/"y"/"z") to fixed value.
	// When present, the corresponding component of the saved button pose is
	// replaced with this value, taking precedence over both the calibrated
	// bottom-axis result and the start-pose-inherited values for the other
	// axes. Useful when you have an exact expected coordinate from CAD or
	// prior calibration and want to lock it in regardless of the probing
	// outcome.
	Overrides map[string]float64 `json:"overrides,omitempty"`
}

func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.ArmName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm_name")
	}
	for name, p := range c.Profiles {
		if p.ButtonHeightAboveBottomMM == 0 {
			return nil, nil, fmt.Errorf("%s: profiles[%q].button_height_above_bottom_mm is required", path, name)
		}
		if p.BottomAxis != "" {
			if _, err := axisVector(p.BottomAxis); err != nil {
				return nil, nil, fmt.Errorf("%s: profiles[%q].bottom_axis: %w", path, name, err)
			}
		}
		if p.CenterAxis != "" {
			if _, err := axisVector(p.CenterAxis); err != nil {
				return nil, nil, fmt.Errorf("%s: profiles[%q].center_axis: %w", path, name, err)
			}
		}
	}
	deps := []string{arm.Named(c.ArmName).String()}
	var optDeps []string
	if c.PoseSwitcherName != "" {
		optDeps = append(optDeps, toggleswitch.Named(c.PoseSwitcherName).String())
	}
	return deps, optDeps, nil
}

func (c *Config) probeMaxTravel() float64 {
	if c.ProbeMaxTravelMM > 0 {
		return c.ProbeMaxTravelMM
	}
	return defaultProbeMaxTravelMM
}
func (c *Config) probeStep() float64 {
	if c.ProbeStepMM > 0 {
		return c.ProbeStepMM
	}
	return defaultProbeStepMM
}
func (c *Config) probeStepPause() time.Duration {
	if c.ProbeStepPauseMs > 0 {
		return time.Duration(c.ProbeStepPauseMs) * time.Millisecond
	}
	return time.Duration(defaultProbeStepPauseMs) * time.Millisecond
}
func (c *Config) loadEnabled() bool { return c.LoadThreshold > 0 }
func (c *Config) loadBaselineSamples() int {
	if c.LoadBaselineSamples > 0 {
		return c.LoadBaselineSamples
	}
	return defaultLoadBaselineSamples
}
func (c *Config) loadBaselineAlpha() float64 {
	if c.LoadBaselineAlpha > 0 && c.LoadBaselineAlpha <= 1 {
		return c.LoadBaselineAlpha
	}
	return defaultLoadBaselineAlpha
}
func (c *Config) loadSettle() time.Duration {
	if c.LoadSettleMs > 0 {
		return time.Duration(c.LoadSettleMs) * time.Millisecond
	}
	return time.Duration(defaultLoadSettleMs) * time.Millisecond
}

func (p ObjectProfile) bottomAxis() string {
	if p.BottomAxis != "" {
		return p.BottomAxis
	}
	return defaultBottomAxis
}
func (p ObjectProfile) centerAxis() string {
	if p.CenterAxis != "" {
		return p.CenterAxis
	}
	return defaultCenterAxis
}
func (p ObjectProfile) sideProbeAbove() float64 {
	if p.SideProbeAboveBottomMM > 0 {
		return p.SideProbeAboveBottomMM
	}
	return defaultSideProbeAboveBottom
}
func (p ObjectProfile) probeCount() int {
	if p.ProbeCount > 0 {
		return p.ProbeCount
	}
	return defaultProbeCount
}

type service struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger
	cfg    *Config
	cfgMu  sync.Mutex

	arm    arm.Arm
	switch_ toggleswitch.Switch // optional, used to persist calibrated poses
}

func newService(_ context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}
	armComp, err := arm.FromDependencies(deps, conf.ArmName)
	if err != nil {
		return nil, fmt.Errorf("arm %q: %w", conf.ArmName, err)
	}
	var sw toggleswitch.Switch
	if conf.PoseSwitcherName != "" {
		sw, err = toggleswitch.FromDependencies(deps, conf.PoseSwitcherName)
		if err != nil {
			return nil, fmt.Errorf("pose switcher %q: %w", conf.PoseSwitcherName, err)
		}
	}
	return &service{
		name:    rawConf.ResourceName(),
		logger:  logger,
		cfg:     conf,
		arm:     armComp,
		switch_: sw,
	}, nil
}

func (s *service) Name() resource.Name           { return s.name }
func (s *service) Close(_ context.Context) error { return nil }
func (s *service) Status(_ context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (s *service) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if _, ok := cmd["set_profile"]; ok {
		return s.handleSetProfile(ctx, cmd)
	}
	if _, ok := cmd["delete_profile"]; ok {
		return s.handleDeleteProfile(ctx, cmd)
	}
	if _, ok := cmd["list_profiles"]; ok {
		return s.handleListProfiles()
	}
	if _, ok := cmd["probe_bottom"]; ok {
		return s.handleProbeBottom(ctx, cmd)
	}
	if _, ok := cmd["probe_width"]; ok {
		return s.handleProbeWidth(ctx, cmd)
	}
	if _, ok := cmd["calibrate"]; ok {
		return s.handleCalibrate(ctx, cmd)
	}
	return nil, fmt.Errorf("unknown command; supported: set_profile, delete_profile, list_profiles, probe_bottom, probe_width, calibrate")
}

// --- profile management ---

func (s *service) handleSetProfile(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	name, _ := cmd["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("set_profile: name is required")
	}
	body, ok := cmd["profile"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("set_profile: profile object is required")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("set_profile: %w", err)
	}
	var profile ObjectProfile
	if err := json.Unmarshal(b, &profile); err != nil {
		return nil, fmt.Errorf("set_profile: %w", err)
	}
	if profile.ButtonHeightAboveBottomMM == 0 {
		return nil, fmt.Errorf("set_profile: button_height_above_bottom_mm is required")
	}
	if profile.BottomAxis != "" {
		if _, err := axisVector(profile.BottomAxis); err != nil {
			return nil, fmt.Errorf("set_profile: bottom_axis: %w", err)
		}
	}
	if profile.CenterAxis != "" {
		if _, err := axisVector(profile.CenterAxis); err != nil {
			return nil, fmt.Errorf("set_profile: center_axis: %w", err)
		}
	}

	s.cfgMu.Lock()
	if s.cfg.Profiles == nil {
		s.cfg.Profiles = map[string]ObjectProfile{}
	}
	_, replacing := s.cfg.Profiles[name]
	s.cfg.Profiles[name] = profile
	s.cfgMu.Unlock()

	if err := s.persistConfig(ctx); err != nil {
		return nil, fmt.Errorf("persist config: %w", err)
	}
	return map[string]interface{}{"success": true, "name": name, "replaced": replacing}, nil
}

func (s *service) handleDeleteProfile(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	name, _ := cmd["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("delete_profile: name is required")
	}
	s.cfgMu.Lock()
	_, ok := s.cfg.Profiles[name]
	if ok {
		delete(s.cfg.Profiles, name)
	}
	s.cfgMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("delete_profile: %q not found", name)
	}
	if err := s.persistConfig(ctx); err != nil {
		return nil, fmt.Errorf("persist config: %w", err)
	}
	return map[string]interface{}{"success": true, "name": name}, nil
}

func (s *service) handleListProfiles() (map[string]interface{}, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	out := make(map[string]interface{}, len(s.cfg.Profiles))
	for name, p := range s.cfg.Profiles {
		out[name] = p
	}
	return map[string]interface{}{"profiles": out}, nil
}

// --- probe primitives ---

// probeOnce drives the arm up to `dir * maxTravel` mm from the current pose,
// in substeps of `probe_step_mm`. Contact is detected by either:
//   - MoveToPosition returning an error (the arm's built-in collision halt), or
//   - the joint-load reading rising above a baseline by more than
//     `load_threshold` Nm (only if load_threshold > 0).
//
// The EEF pose at the moment of contact is returned. Whether contact happens
// or not, the arm is finally moved back to the original `start` pose so
// subsequent probe iterations start from a known position.
func (s *service) probeOnce(ctx context.Context, dir r3.Vector, maxTravel float64) (spatialmath.Pose, error) {
	start, err := s.arm.EndPosition(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("EndPosition (start): %w", err)
	}

	var baseline []float64
	if s.cfg.loadEnabled() {
		baseline, err = s.establishLoadBaseline(ctx, s.cfg.loadBaselineSamples())
		if err != nil {
			return nil, fmt.Errorf("establish load baseline: %w", err)
		}
		s.logger.Infof("probe load baseline (n=%d): %v", s.cfg.loadBaselineSamples(), baseline)
	}

	step := s.cfg.probeStep()
	pause := s.cfg.probeStepPause()
	nSteps := int(math.Ceil(maxTravel / step))

	contact, err := s.driveSubsteps(ctx, start, dir, step, maxTravel, nSteps, pause, baseline)

	// Always retract to start, regardless of outcome.
	if retErr := s.arm.MoveToPosition(ctx, start, nil); retErr != nil {
		s.logger.Warnf("probe: failed to retract to start: %v", retErr)
		// If the substep loop already returned an error, prefer that.
		if err == nil {
			return nil, fmt.Errorf("retract to start: %w", retErr)
		}
	}
	return contact, err
}

// driveSubsteps walks along dir in step-sized increments, returning the
// contact pose on the first substep where either MoveToPosition errors or the
// joint-load reading exceeds the baseline by more than load_threshold. If
// `baseline` is nil, load polling is skipped. Returns an error if full max
// travel completed without contact.
func (s *service) driveSubsteps(
	ctx context.Context,
	start spatialmath.Pose,
	dir r3.Vector,
	step, maxTravel float64,
	nSteps int,
	pause time.Duration,
	baseline []float64,
) (spatialmath.Pose, error) {
	threshold := s.cfg.LoadThreshold
	alpha := s.cfg.loadBaselineAlpha()
	usingLoad := baseline != nil

	for i := 1; i <= nSteps; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		traveled := math.Min(float64(i)*step, maxTravel)
		target := translatePose(start, dir.Mul(traveled))

		if err := s.arm.MoveToPosition(ctx, target, nil); err != nil {
			contact, posErr := s.arm.EndPosition(ctx, nil)
			if posErr != nil {
				return nil, fmt.Errorf("EndPosition (after contact at substep %d/%d): %w", i, nSteps, posErr)
			}
			s.logger.Infof("probe contact (move error) at substep %d/%d: requested %s, recorded %s (err: %v)",
				i, nSteps, formatPoint(target.Point()), formatPoint(contact.Point()), err)
			return contact, nil
		}

		if usingLoad {
			load, err := s.readSettledLoad(ctx)
			if err != nil {
				s.logger.Warnf("probe substep %d: read_load failed: %v", i, err)
			} else if len(load) != len(baseline) {
				s.logger.Warnf("probe substep %d: load length %d != baseline length %d", i, len(load), len(baseline))
			} else {
				maxAbsDelta, joint := maxAbsDelta(load, baseline)
				s.logger.Debugf("probe substep %d/%d: load=%v baseline=%v max_delta=%.3f Nm (j%d)",
					i, nSteps, load, baseline, maxAbsDelta, joint)
				if maxAbsDelta > threshold {
					contact, posErr := s.arm.EndPosition(ctx, nil)
					if posErr != nil {
						return nil, fmt.Errorf("EndPosition (after load contact at substep %d/%d): %w", i, nSteps, posErr)
					}
					s.logger.Infof("probe contact (load) at substep %d/%d: requested %s, recorded %s (j%d delta=%.3f Nm > threshold %.3f)",
						i, nSteps, formatPoint(target.Point()), formatPoint(contact.Point()), joint, maxAbsDelta, threshold)
					return contact, nil
				}
				// No contact — drift baseline toward the latest sample so
				// gradual gravity-load shifts get absorbed.
				for j := range baseline {
					baseline[j] = (1-alpha)*baseline[j] + alpha*load[j]
				}
			}
		}

		if pause > 0 {
			time.Sleep(pause)
		}
	}
	return nil, fmt.Errorf("no contact within %.2f mm (%d substeps)", maxTravel, nSteps)
}

// readSettledLoad sleeps load_settle_ms (so the arm physically settles after
// the most recent motion) and then reads the load.
func (s *service) readSettledLoad(ctx context.Context) ([]float64, error) {
	settle := s.cfg.loadSettle()
	if settle > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(settle):
		}
	}
	return s.readLoad(ctx)
}

// readLoad calls arm.DoCommand({"load": true}) and parses the numeric array.
func (s *service) readLoad(ctx context.Context) ([]float64, error) {
	resp, err := s.arm.DoCommand(ctx, map[string]interface{}{"load": true})
	if err != nil {
		return nil, fmt.Errorf("arm.DoCommand: %w", err)
	}
	raw, ok := resp["load"]
	if !ok {
		return nil, fmt.Errorf("response missing %q key (got %v)", "load", resp)
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("load field is %T, expected []interface{}", raw)
	}
	out := make([]float64, len(arr))
	for i, v := range arr {
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("load[%d] is %T, expected float64", i, v)
		}
		out[i] = f
	}
	return out, nil
}

// establishLoadBaseline takes n stationary load readings and returns the mean
// per joint. The arm should be at rest (post-positioning, pre-probe).
func (s *service) establishLoadBaseline(ctx context.Context, n int) ([]float64, error) {
	if n < 1 {
		n = 1
	}
	var sum []float64
	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		load, err := s.readSettledLoad(ctx)
		if err != nil {
			return nil, fmt.Errorf("baseline sample %d/%d: %w", i+1, n, err)
		}
		if sum == nil {
			sum = make([]float64, len(load))
		} else if len(load) != len(sum) {
			return nil, fmt.Errorf("baseline sample %d: load length changed (%d → %d)", i+1, len(sum), len(load))
		}
		for j, v := range load {
			sum[j] += v
		}
	}
	for j := range sum {
		sum[j] /= float64(n)
	}
	return sum, nil
}

// maxAbsDelta returns max over j of |a[j]-b[j]| and the index of the joint
// where it occurred. Slices are assumed equal-length and non-empty.
func maxAbsDelta(a, b []float64) (float64, int) {
	max := 0.0
	idx := 0
	for j := range a {
		d := math.Abs(a[j] - b[j])
		if d > max {
			max = d
			idx = j
		}
	}
	return max, idx
}

// probeRepeat runs probeOnce `count` times and returns the per-iteration
// contact poses. Each iteration retracts to its own start, so subsequent
// iterations begin from the same pose as the first.
func (s *service) probeRepeat(ctx context.Context, dir r3.Vector, maxTravel float64, count int) ([]spatialmath.Pose, error) {
	if count < 1 {
		count = 1
	}
	out := make([]spatialmath.Pose, 0, count)
	for i := 0; i < count; i++ {
		p, err := s.probeOnce(ctx, dir, maxTravel)
		if err != nil {
			return nil, fmt.Errorf("probe iteration %d/%d: %w", i+1, count, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// --- top-level commands ---

func (s *service) handleProbeBottom(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	profile, err := s.resolveProfile(cmd)
	if err != nil {
		return nil, err
	}
	dir, _ := axisVector(profile.bottomAxis())
	contacts, err := s.probeRepeat(ctx, dir, s.cfg.probeMaxTravel(), profile.probeCount())
	if err != nil {
		return nil, err
	}
	mean := meanPoint(contacts)
	// Adjust by probe-axis offset to report the actual surface position. The
	// EEF contact and the surface differ by `offset` along the probe direction.
	surface := r3.Vector{
		X: mean.X + dir.X*profile.ProbeAxisOffsetMM,
		Y: mean.Y + dir.Y*profile.ProbeAxisOffsetMM,
		Z: mean.Z + dir.Z*profile.ProbeAxisOffsetMM,
	}
	return map[string]interface{}{
		"contact_mean":             pointToMap(mean),
		"contacts":                 pointsToMaps(contacts),
		"surface_mean":             pointToMap(surface),
		"surface_value":            worldCoordAlong(dir, surface),
		"axis":                     profile.bottomAxis(),
		"probe_axis_offset_mm":     profile.ProbeAxisOffsetMM,
		"probe_count":              len(contacts),
	}, nil
}

func (s *service) handleProbeWidth(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	profile, err := s.resolveProfile(cmd)
	if err != nil {
		return nil, err
	}
	dirPos, _ := axisVector(profile.centerAxis())
	dirNeg := dirPos.Mul(-1)
	maxTravel := s.cfg.probeMaxTravel()
	if profile.MaxWidthMM > 0 && profile.MaxWidthMM < maxTravel {
		maxTravel = profile.MaxWidthMM
	}

	posContacts, err := s.probeRepeat(ctx, dirPos, maxTravel, profile.probeCount())
	if err != nil {
		return nil, fmt.Errorf("+%s probe: %w", profile.centerAxis(), err)
	}
	negContacts, err := s.probeRepeat(ctx, dirNeg, maxTravel, profile.probeCount())
	if err != nil {
		return nil, fmt.Errorf("-%s probe: %w", profile.centerAxis(), err)
	}

	posMean := meanPoint(posContacts)
	negMean := meanPoint(negContacts)
	center := r3.Vector{
		X: (posMean.X + negMean.X) / 2,
		Y: (posMean.Y + negMean.Y) / 2,
		Z: (posMean.Z + negMean.Z) / 2,
	}
	return map[string]interface{}{
		"axis":          profile.centerAxis(),
		"positive_mean": pointToMap(posMean),
		"negative_mean": pointToMap(negMean),
		"center":        pointToMap(center),
	}, nil
}

// handleCalibrate runs the simplified single-probe calibration. The operator
// is expected to have visually centered the probe over the target along
// `center_axis` and parked the arm at the desired probing pose+orientation
// before calling this. The service:
//
//  1. Records the start pose (orientation + non-bottom-axis coordinates carry
//     into the saved button pose).
//  2. Probes along `bottom_axis` until contact (averaged over `probe_count`).
//  3. Composes the button pose: bottom-axis component = contact + button
//     height (the probe-axis offset cancels for the EEF coords); all other
//     components and orientation inherited from the start pose.
//  4. Returns the arm to the start pose.
//  5. Updates the named profile in cloud config with the captured
//     `center_axis_value` (start pose's coord along center_axis) and
//     `probe_axis_value` (the surface coord along bottom_axis, including the
//     probe-axis offset). Skipped if the profile was passed inline.
//  6. Optionally writes the button pose into the configured switch under
//     `save_as`.
func (s *service) handleCalibrate(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	profileName, _ := cmd["profile"].(string)
	profile, err := s.resolveProfile(cmd)
	if err != nil {
		return nil, err
	}
	saveAs, _ := cmd["save_as"].(string)

	start, err := s.arm.EndPosition(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("EndPosition (start): %w", err)
	}
	startPt := start.Point()

	bottomDir, _ := axisVector(profile.bottomAxis())
	centerDir, _ := axisVector(profile.centerAxis())

	// 1. Probe bottom.
	bottomContacts, err := s.probeRepeat(ctx, bottomDir, s.cfg.probeMaxTravel(), profile.probeCount())
	if err != nil {
		return nil, fmt.Errorf("bottom probe: %w", err)
	}
	bottomEEFMean := meanPoint(bottomContacts)

	// Surface position = EEF contact + bottom_axis * probe_axis_offset.
	surface := r3.Vector{
		X: bottomEEFMean.X + bottomDir.X*profile.ProbeAxisOffsetMM,
		Y: bottomEEFMean.Y + bottomDir.Y*profile.ProbeAxisOffsetMM,
		Z: bottomEEFMean.Z + bottomDir.Z*profile.ProbeAxisOffsetMM,
	}

	// 2. Compose button pose. Saved pose's bottom-axis component:
	//        contact_eef + button_height - probe_axis_offset
	//    which equals surface + button_height = the button's world coord
	//    (because surface = contact_eef - probe_axis_offset along the probe
	//    direction). The saved pose thus represents the button's location;
	//    downstream callers (or a press motion) handle any further offset
	//    needed to actually contact the button with the tip.
	buttonPt := startPt
	bottomEEFVal := componentAlong(bottomDir, bottomEEFMean) -
		profile.ButtonHeightAboveBottomMM +
		profile.ProbeAxisOffsetMM
	buttonPt = setAlong(bottomDir, buttonPt, bottomEEFVal)

	// Apply manual per-axis overrides from the profile. Overridden values
	// take precedence over both calibrated (bottom_axis) and inherited
	// (start-pose) values. Keyed by axis name in world frame: "x", "y", "z".
	if v, ok := profile.Overrides["x"]; ok {
		buttonPt.X = v
	}
	if v, ok := profile.Overrides["y"]; ok {
		buttonPt.Y = v
	}
	if v, ok := profile.Overrides["z"]; ok {
		buttonPt.Z = v
	}

	buttonPose := spatialmath.NewPose(buttonPt, start.Orientation())

	// 3. Move arm back to start.
	if err := s.arm.MoveToPosition(ctx, start, nil); err != nil {
		s.logger.Warnf("calibrate: failed to return to start: %v", err)
	}

	// 4. Capture calibration values.
	centerAxisValue := worldCoordAlong(centerDir, startPt)
	probeAxisValue := worldCoordAlong(bottomDir, surface)

	result := map[string]interface{}{
		"button_pose":         poseToMap(buttonPose),
		"contact_mean":        pointToMap(bottomEEFMean),
		"surface_mean":        pointToMap(surface),
		"center_axis":         profile.centerAxis(),
		"center_axis_value":   centerAxisValue,
		"bottom_axis":         profile.bottomAxis(),
		"probe_axis_value":    probeAxisValue,
		"probe_axis_offset":   profile.ProbeAxisOffsetMM,
		"profile":             profileName,
	}

	// 5. Update the named profile with the captured values, and persist.
	if profileName != "" {
		s.cfgMu.Lock()
		p := s.cfg.Profiles[profileName]
		p.CenterAxisValueMM = centerAxisValue
		p.ProbeAxisValueMM = probeAxisValue
		s.cfg.Profiles[profileName] = p
		s.cfgMu.Unlock()
		if err := s.persistConfig(ctx); err != nil {
			s.logger.Warnf("calibrate: failed to persist updated profile: %v", err)
			result["profile_persist_error"] = err.Error()
		} else {
			result["profile_updated"] = true
		}
	}

	// 6. Optional persist of the button pose into the switch.
	if saveAs != "" {
		if s.switch_ == nil {
			return result, fmt.Errorf("save_as requested but pose_switcher_name is not configured")
		}
		body := poseToMap(buttonPose)
		body["name"] = saveAs
		_, err := s.switch_.DoCommand(ctx, map[string]interface{}{"set_pose_value": body})
		if err != nil {
			return result, fmt.Errorf("persist pose %q via switch: %w", saveAs, err)
		}
		result["saved_as"] = saveAs
	}
	return result, nil
}

// resolveProfile reads either a profile name (must exist) or an inline profile
// object from the DoCommand body.
func (s *service) resolveProfile(cmd map[string]interface{}) (ObjectProfile, error) {
	if name, ok := cmd["profile"].(string); ok && name != "" {
		s.cfgMu.Lock()
		profile, found := s.cfg.Profiles[name]
		s.cfgMu.Unlock()
		if !found {
			return ObjectProfile{}, fmt.Errorf("profile %q not found", name)
		}
		return profile, nil
	}
	if obj, ok := cmd["profile"].(map[string]interface{}); ok {
		b, err := json.Marshal(obj)
		if err != nil {
			return ObjectProfile{}, err
		}
		var profile ObjectProfile
		if err := json.Unmarshal(b, &profile); err != nil {
			return ObjectProfile{}, err
		}
		return profile, nil
	}
	return ObjectProfile{}, fmt.Errorf("profile (string name or inline object) is required")
}

// --- persistence ---

func (s *service) persistConfig(ctx context.Context) error {
	s.cfgMu.Lock()
	b, err := json.Marshal(s.cfg)
	s.cfgMu.Unlock()
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	var attrMap utils.AttributeMap
	if err := json.Unmarshal(b, &attrMap); err != nil {
		return fmt.Errorf("attribute map: %w", err)
	}
	return vmodutils.UpdateComponentCloudAttributesFromModuleEnv(ctx, s.name, attrMap, s.logger)
}

// --- helpers ---

func axisVector(axis string) (r3.Vector, error) {
	switch strings.TrimSpace(strings.ToLower(axis)) {
	case "+x", "x":
		return r3.Vector{X: 1}, nil
	case "-x":
		return r3.Vector{X: -1}, nil
	case "+y", "y":
		return r3.Vector{Y: 1}, nil
	case "-y":
		return r3.Vector{Y: -1}, nil
	case "+z", "z":
		return r3.Vector{Z: 1}, nil
	case "-z":
		return r3.Vector{Z: -1}, nil
	default:
		return r3.Vector{}, fmt.Errorf("unknown axis %q (expected +x, -x, +y, -y, +z, -z)", axis)
	}
}

func translatePose(p spatialmath.Pose, delta r3.Vector) spatialmath.Pose {
	pt := p.Point()
	return spatialmath.NewPose(r3.Vector{X: pt.X + delta.X, Y: pt.Y + delta.Y, Z: pt.Z + delta.Z}, p.Orientation())
}

// componentAlong returns the projection of v onto axis (axis must be unit-ish:
// our axisVector() always returns ±1 along a single axis, so this just
// extracts the relevant coordinate with the right sign).
func componentAlong(axis r3.Vector, v r3.Vector) float64 {
	return axis.X*v.X + axis.Y*v.Y + axis.Z*v.Z
}

// worldCoordAlong returns the world-frame coordinate of v along the dimension
// pointed at by axis (signed natively, ignoring axis's sign). E.g. for axis
// = -z and v.Z = 277, returns 277 (not -277). Used for output values that the
// human will read as a coordinate ("the surface is at z=277") rather than as
// a directional projection.
func worldCoordAlong(axis r3.Vector, v r3.Vector) float64 {
	switch {
	case axis.X != 0:
		return v.X
	case axis.Y != 0:
		return v.Y
	case axis.Z != 0:
		return v.Z
	}
	return 0
}

// setAlong replaces the component of `pt` along `axis` so that the projection
// becomes `value`. Other components are left untouched. Assumes `axis` is a
// unit vector along one of x/y/z (which axisVector enforces).
func setAlong(axis r3.Vector, pt r3.Vector, value float64) r3.Vector {
	switch {
	case axis.X != 0:
		pt.X = value * axis.X
	case axis.Y != 0:
		pt.Y = value * axis.Y
	case axis.Z != 0:
		pt.Z = value * axis.Z
	}
	return pt
}

func meanPoint(poses []spatialmath.Pose) r3.Vector {
	var s r3.Vector
	for _, p := range poses {
		pt := p.Point()
		s.X += pt.X
		s.Y += pt.Y
		s.Z += pt.Z
	}
	n := float64(len(poses))
	if n == 0 {
		return r3.Vector{}
	}
	return r3.Vector{X: s.X / n, Y: s.Y / n, Z: s.Z / n}
}

func pointToMap(v r3.Vector) map[string]interface{} {
	return map[string]interface{}{"x": v.X, "y": v.Y, "z": v.Z}
}

func pointsToMaps(poses []spatialmath.Pose) []map[string]interface{} {
	out := make([]map[string]interface{}, len(poses))
	for i, p := range poses {
		out[i] = pointToMap(p.Point())
	}
	return out
}

func poseToMap(p spatialmath.Pose) map[string]interface{} {
	pt := p.Point()
	ov := p.Orientation().OrientationVectorDegrees()
	return map[string]interface{}{
		"x":     pt.X,
		"y":     pt.Y,
		"z":     pt.Z,
		"o_x":   ov.OX,
		"o_y":   ov.OY,
		"o_z":   ov.OZ,
		"theta": ov.Theta,
	}
}

func formatPoint(v r3.Vector) string {
	return fmt.Sprintf("(%.2f, %.2f, %.2f)", v.X, v.Y, v.Z)
}
