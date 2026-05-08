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

	Profiles map[string]ObjectProfile `json:"profiles,omitempty"`
}

type ObjectProfile struct {
	// ButtonHeightAboveBottomMM is the vertical offset of the button above the
	// bottom index surface. Required.
	ButtonHeightAboveBottomMM float64 `json:"button_height_above_bottom_mm"`

	// SideProbeAboveBottomMM is the clearance above the just-found bottom at
	// which the side probes happen.
	SideProbeAboveBottomMM float64 `json:"side_probe_above_bottom_mm,omitempty"`

	// MaxWidthMM caps the per-side probe travel so the arm doesn't fly past
	// the object if collision detection somehow misses. Falls back to the
	// service-level ProbeMaxTravelMM when zero.
	MaxWidthMM float64 `json:"max_width_mm,omitempty"`

	// BottomAxis is the direction the arm probes to find the bottom index
	// surface. Default "-z".
	BottomAxis string `json:"bottom_axis,omitempty"`

	// CenterAxis is the axis along which the arm probes to find the object's
	// horizontal centerline. The probe is run in both +CenterAxis and
	// -CenterAxis directions; their midpoint is the center. Default "y".
	CenterAxis string `json:"center_axis,omitempty"`

	// ProbeCount is how many times each surface is probed; results are
	// averaged. Default 1.
	ProbeCount int `json:"probe_count,omitempty"`
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
// in substeps of `probe_step_mm`. The first substep that errors is treated
// as a contact event; the EEF pose at that moment is returned. Whether
// contact happens or not, the arm is finally moved back to the original
// `start` pose so subsequent probe iterations start from a known position.
func (s *service) probeOnce(ctx context.Context, dir r3.Vector, maxTravel float64) (spatialmath.Pose, error) {
	start, err := s.arm.EndPosition(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("EndPosition (start): %w", err)
	}

	step := s.cfg.probeStep()
	pause := s.cfg.probeStepPause()
	nSteps := int(math.Ceil(maxTravel / step))

	contact, err := s.driveSubsteps(ctx, start, dir, step, maxTravel, nSteps, pause)

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

// driveSubsteps does the inner loop: walk along dir in step-sized increments,
// returning the contact pose on the first errored substep, or an error if the
// full max travel completed without contact.
func (s *service) driveSubsteps(
	ctx context.Context,
	start spatialmath.Pose,
	dir r3.Vector,
	step, maxTravel float64,
	nSteps int,
	pause time.Duration,
) (spatialmath.Pose, error) {
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
			s.logger.Infof("probe contact at substep %d/%d: requested %s, recorded %s (move error: %v)",
				i, nSteps, formatPoint(target.Point()), formatPoint(contact.Point()), err)
			return contact, nil
		}

		if pause > 0 {
			time.Sleep(pause)
		}
	}
	return nil, fmt.Errorf("no contact within %.2f mm (%d substeps)", maxTravel, nSteps)
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
	return map[string]interface{}{
		"contact_mean": pointToMap(mean),
		"contacts":     pointsToMaps(contacts),
		"axis":         profile.bottomAxis(),
		"probe_count":  len(contacts),
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

func (s *service) handleCalibrate(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	profile, err := s.resolveProfile(cmd)
	if err != nil {
		return nil, err
	}
	saveAs, _ := cmd["save_as"].(string)

	start, err := s.arm.EndPosition(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("EndPosition (start): %w", err)
	}

	// 1. Probe bottom.
	bottomDir, _ := axisVector(profile.bottomAxis())
	bottomContacts, err := s.probeRepeat(ctx, bottomDir, s.cfg.probeMaxTravel(), profile.probeCount())
	if err != nil {
		return nil, fmt.Errorf("bottom probe: %w", err)
	}
	bottomMean := meanPoint(bottomContacts)

	// 2. Move to side-probe height. Take the start XY, replace the bottom-axis
	//    coord with the just-found bottom value, then offset by
	//    sideProbeAbove in the opposite of the probe direction (i.e. "up").
	startPt := start.Point()
	sideHeightPt := replaceAlong(bottomDir, startPt, bottomMean)
	sideHeightOffset := bottomDir.Mul(-profile.sideProbeAbove())
	sideHeightPt = r3.Vector{
		X: sideHeightPt.X + sideHeightOffset.X,
		Y: sideHeightPt.Y + sideHeightOffset.Y,
		Z: sideHeightPt.Z + sideHeightOffset.Z,
	}
	if err := s.arm.MoveToPosition(ctx, spatialmath.NewPose(sideHeightPt, start.Orientation()), nil); err != nil {
		return nil, fmt.Errorf("move to side-probe height: %w", err)
	}

	// 3. Probe both sides along center axis.
	centerDir, _ := axisVector(profile.centerAxis())
	maxWidth := s.cfg.probeMaxTravel()
	if profile.MaxWidthMM > 0 && profile.MaxWidthMM < maxWidth {
		maxWidth = profile.MaxWidthMM
	}
	posContacts, err := s.probeRepeat(ctx, centerDir, maxWidth, profile.probeCount())
	if err != nil {
		return nil, fmt.Errorf("+%s probe: %w", profile.centerAxis(), err)
	}
	negContacts, err := s.probeRepeat(ctx, centerDir.Mul(-1), maxWidth, profile.probeCount())
	if err != nil {
		return nil, fmt.Errorf("-%s probe: %w", profile.centerAxis(), err)
	}
	posMean := meanPoint(posContacts)
	negMean := meanPoint(negContacts)

	// Center along the configured axis. Other components are taken from start.
	centerVal := (componentAlong(centerDir, posMean) + componentAlong(centerDir, negMean)) / 2

	// 4. Compose button pose:
	//    - center axis component = centerVal
	//    - bottom axis component = bottomMean.<axis> + ButtonHeightAboveBottomMM * (-bottomDir)
	//    - other axis component = start
	//    - orientation = start
	buttonPt := startPt
	buttonPt = setAlong(centerDir, buttonPt, centerVal)
	bottomVal := componentAlong(bottomDir, bottomMean) + profile.ButtonHeightAboveBottomMM*(-1)
	buttonPt = setAlong(bottomDir, buttonPt, bottomVal)

	buttonPose := spatialmath.NewPose(buttonPt, start.Orientation())

	// 5. Move arm back to start (clean exit, regardless of save_as).
	if err := s.arm.MoveToPosition(ctx, start, nil); err != nil {
		s.logger.Warnf("calibrate: failed to return to start: %v", err)
	}

	result := map[string]interface{}{
		"button_pose":  poseToMap(buttonPose),
		"bottom_mean":  pointToMap(bottomMean),
		"center_value": centerVal,
		"profile":      profileNameOrEmpty(cmd),
	}

	// 6. Optional persist via switch.set_pose_value.
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

func profileNameOrEmpty(cmd map[string]interface{}) string {
	n, _ := cmd["profile"].(string)
	return n
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

// replaceAlong returns a copy of `target`'s position with the `axis` component
// replaced with that of `source`. Used to move the arm to the same horizontal
// XY as the start pose but with the just-found Z (or whatever axis is "down").
func replaceAlong(axis r3.Vector, target, source r3.Vector) r3.Vector {
	switch {
	case axis.X != 0:
		target.X = source.X
	case axis.Y != 0:
		target.Y = source.Y
	case axis.Z != 0:
		target.Z = source.Z
	}
	return target
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
