package dialcontrolmotion

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/testutils/inject"
)

const floatTol = 1e-9

func almostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// newTestService builds a dialControlMotion with the maps initialized and a
// test logger, but no arm — enough to exercise every routine that doesn't call
// the hardware (handleDialMove, accelMultiplier, advanceSmoothedCounts, the
// axis-mode commands).
func newTestService(t *testing.T, cfg *Config) *dialControlMotion {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	return &dialControlMotion{
		logger:         logging.NewTestLogger(t),
		cfg:            cfg,
		cancelCtx:      context.Background(),
		lastDial:       make(map[string]*float64),
		lastDirection:  make(map[string]float64),
		pendingMoves:   make(map[string]float64),
		pendingCounts:  make(map[string]int),
		smoothedCounts: make(map[string]float64),
		axisMode:       axisModeTranslation,
	}
}

func TestIsRotationAxis(t *testing.T) {
	rotation := []string{"rx", "ry", "rz"}
	translation := []string{"x", "y", "z", "orientation", "", "RX"}
	for _, a := range rotation {
		if !isRotationAxis(a) {
			t.Errorf("isRotationAxis(%q) = false, want true", a)
		}
	}
	for _, a := range translation {
		if isRotationAxis(a) {
			t.Errorf("isRotationAxis(%q) = true, want false", a)
		}
	}
}

func TestConfigMaxPositionAndDrainInterval(t *testing.T) {
	if got := (&Config{}).maxPosition(); got != defaultMaxPos {
		t.Errorf("default maxPosition = %v, want %v", got, defaultMaxPos)
	}
	if got := (&Config{DialMaxPosition: 250}).maxPosition(); got != 250 {
		t.Errorf("configured maxPosition = %v, want 250", got)
	}
	if got := (&Config{}).drainInterval().Milliseconds(); got != int64(defaultDrainMs) {
		t.Errorf("default drainInterval = %dms, want %dms", got, defaultDrainMs)
	}
	if got := (&Config{DrainIntervalMs: 40}).drainInterval().Milliseconds(); got != 40 {
		t.Errorf("configured drainInterval = %dms, want 40ms", got)
	}
}

// TestConfigAccelResolution covers the three-way resolution shared by
// accelThresholdCount/MaxMultiplier/Exponent: rotation axes prefer their
// rotation override, else the translation value, else the default.
func TestConfigAccelResolution(t *testing.T) {
	tests := []struct {
		name             string
		cfg              *Config
		axis             string
		wantThr, wantMax float64
		wantExp          float64
	}{
		{"all defaults, translation", &Config{}, "x", defaultAccelThr, defaultAccelMax, defaultAccelExp},
		{"all defaults, rotation", &Config{}, "rx", defaultAccelThr, defaultAccelMax, defaultAccelExp},
		{
			"translation configured applies to translation axis",
			&Config{AccelThresholdCount: 3, AccelMaxMultiplier: 20, AccelExponent: 2},
			"y", 3, 20, 2,
		},
		{
			"translation configured also backs rotation axis when no rotation override",
			&Config{AccelThresholdCount: 3, AccelMaxMultiplier: 20, AccelExponent: 2},
			"rx", 3, 20, 2,
		},
		{
			"rotation override wins for rotation axis",
			&Config{
				AccelThresholdCount: 3, AccelMaxMultiplier: 20, AccelExponent: 2,
				AccelRotationThresholdCount: 5, AccelRotationMaxMultiplier: 30, AccelRotationExponent: 4,
			},
			"rz", 5, 30, 4,
		},
		{
			"rotation override does not leak into translation axis",
			&Config{
				AccelRotationThresholdCount: 5, AccelRotationMaxMultiplier: 30, AccelRotationExponent: 4,
			},
			"x", defaultAccelThr, defaultAccelMax, defaultAccelExp,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.accelThresholdCount(tt.axis); got != tt.wantThr {
				t.Errorf("accelThresholdCount(%q) = %v, want %v", tt.axis, got, tt.wantThr)
			}
			if got := tt.cfg.accelMaxMultiplier(tt.axis); got != tt.wantMax {
				t.Errorf("accelMaxMultiplier(%q) = %v, want %v", tt.axis, got, tt.wantMax)
			}
			if got := tt.cfg.accelExponent(tt.axis); got != tt.wantExp {
				t.Errorf("accelExponent(%q) = %v, want %v", tt.axis, got, tt.wantExp)
			}
		})
	}
}

// TestConfigAccelSmoothingAlpha covers the (0,1] range guard: out-of-range
// values fall through to the next source rather than being used.
func TestConfigAccelSmoothingAlpha(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		axis string
		want float64
	}{
		{"default", &Config{}, "x", defaultAccelAlpha},
		{"in-range translation", &Config{AccelSmoothingAlpha: 0.5}, "x", 0.5},
		{"alpha of 1 is allowed", &Config{AccelSmoothingAlpha: 1}, "x", 1},
		{"alpha above 1 rejected -> default", &Config{AccelSmoothingAlpha: 1.5}, "x", defaultAccelAlpha},
		{"alpha of 0 rejected -> default", &Config{AccelSmoothingAlpha: 0}, "x", defaultAccelAlpha},
		{"rotation override wins", &Config{AccelSmoothingAlpha: 0.5, AccelRotationSmoothingAlpha: 0.3}, "rx", 0.3},
		{"invalid rotation override falls back to translation", &Config{AccelSmoothingAlpha: 0.5, AccelRotationSmoothingAlpha: 2}, "rx", 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.accelSmoothingAlpha(tt.axis); got != tt.want {
				t.Errorf("accelSmoothingAlpha(%q) = %v, want %v", tt.axis, got, tt.want)
			}
		})
	}
}

func TestConfigMoveMMAndDeg(t *testing.T) {
	full := &Config{
		DialMoveXMM: 2, DialMoveYMM: 3, DialMoveZMM: 4, DialMoveOrientationMM: 5,
		DialMoveRXDeg: 6, DialMoveRYDeg: 7, DialMoveRZDeg: 8,
	}
	mmCases := map[string]float64{"x": 2, "y": 3, "z": 4, "orientation": 5, "unknown": defaultMoveMM}
	for axis, want := range mmCases {
		if got := full.moveMM(axis); got != want {
			t.Errorf("moveMM(%q) = %v, want %v", axis, got, want)
		}
	}
	degCases := map[string]float64{"rx": 6, "ry": 7, "rz": 8, "x": defaultMoveDeg}
	for axis, want := range degCases {
		if got := full.moveDeg(axis); got != want {
			t.Errorf("moveDeg(%q) = %v, want %v", axis, got, want)
		}
	}
	// Unset fields fall back to the defaults.
	if got := (&Config{}).moveMM("x"); got != defaultMoveMM {
		t.Errorf("default moveMM(x) = %v, want %v", got, defaultMoveMM)
	}
	if got := (&Config{}).moveDeg("rz"); got != defaultMoveDeg {
		t.Errorf("default moveDeg(rz) = %v, want %v", got, defaultMoveDeg)
	}
}

func TestConfigValidate(t *testing.T) {
	if _, _, err := (&Config{}).Validate("p"); err == nil {
		t.Error("Validate with empty arm_name: expected error, got nil")
	}
	deps, _, err := (&Config{ArmName: "myarm"}).Validate("p")
	if err != nil {
		t.Fatalf("Validate with arm_name: unexpected error %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("Validate deps = %v, want exactly one (the arm)", deps)
	}
}

func TestToFloat64(t *testing.T) {
	tests := []struct {
		in     any
		want   float64
		wantOK bool
	}{
		{float64(3.5), 3.5, true},
		{float32(2), 2, true},
		{int(7), 7, true},
		{int64(9), 9, true},
		{"nope", 0, false},
		{nil, 0, false},
		{true, 0, false},
	}
	for _, tt := range tests {
		got, ok := toFloat64(tt.in)
		if ok != tt.wantOK || (ok && got != tt.want) {
			t.Errorf("toFloat64(%#v) = (%v, %v), want (%v, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

// TestAccelMultiplier exercises the clamp((smoothed/threshold)^exp, 1, max)
// curve with the default translation config (thr=1, exp=1.5, max=10).
func TestAccelMultiplier(t *testing.T) {
	s := newTestService(t, &Config{})
	tests := []struct {
		smoothed float64
		want     float64
	}{
		{0, 1},                // below threshold pins to 1
		{0.5, 1},              // still below
		{1, 1},                // exactly at threshold
		{2, math.Pow(2, 1.5)}, // ~2.828
		{4, 8},                // 4^1.5 = 8
		{9, 10},               // 27 capped at max
		{1000, 10},            // far past, capped
	}
	for _, tt := range tests {
		if got := s.accelMultiplier(tt.smoothed, "x"); !almostEqual(got, tt.want, 1e-6) {
			t.Errorf("accelMultiplier(%v) = %v, want %v", tt.smoothed, got, tt.want)
		}
	}

	// A rotation override caps rotation axes independently.
	sr := newTestService(t, &Config{AccelRotationMaxMultiplier: 3})
	if got := sr.accelMultiplier(1000, "rx"); got != 3 {
		t.Errorf("rotation accelMultiplier cap = %v, want 3", got)
	}
	if got := sr.accelMultiplier(1000, "x"); got != defaultAccelMax {
		t.Errorf("translation accelMultiplier cap = %v, want %v", got, defaultAccelMax)
	}
}

// TestAdvanceSmoothedCounts covers the EWMA ramp on active axes, decay on
// quiet axes, and pruning of tiny residuals. Alpha defaults to 0.4.
func TestAdvanceSmoothedCounts(t *testing.T) {
	s := newTestService(t, &Config{})

	// Ramp: first window with count 5 -> 0.4*5 + 0.6*0 = 2.0.
	s.advanceSmoothedCounts(map[string]int{"x": 5})
	if got := s.smoothedCounts["x"]; !almostEqual(got, 2.0, floatTol) {
		t.Fatalf("after 1 window smoothed[x] = %v, want 2.0", got)
	}
	// Second window, still count 5 -> 0.4*5 + 0.6*2.0 = 3.2 (ramps toward 5).
	s.advanceSmoothedCounts(map[string]int{"x": 5})
	if got := s.smoothedCounts["x"]; !almostEqual(got, 3.2, floatTol) {
		t.Fatalf("after 2 windows smoothed[x] = %v, want 3.2", got)
	}
	// Quiet window: x decays by (1-alpha) -> 0.6*3.2 = 1.92.
	s.advanceSmoothedCounts(map[string]int{})
	if got := s.smoothedCounts["x"]; !almostEqual(got, 1.92, floatTol) {
		t.Fatalf("after decay smoothed[x] = %v, want 1.92", got)
	}

	// Pruning: a residual that decays below 0.05 is deleted entirely.
	s.smoothedCounts["y"] = 0.08 // -> 0.6*0.08 = 0.048 < 0.05
	s.smoothedCounts["z"] = 0.1  // -> 0.6*0.1  = 0.06  >= 0.05, kept
	s.advanceSmoothedCounts(map[string]int{})
	if _, ok := s.smoothedCounts["y"]; ok {
		t.Errorf("smoothed[y] = %v, want pruned (absent)", s.smoothedCounts["y"])
	}
	if got, ok := s.smoothedCounts["z"]; !ok || !almostEqual(got, 0.06, floatTol) {
		t.Errorf("smoothed[z] = %v (present=%v), want 0.06", got, ok)
	}
}

func TestHandleDialMove_InitializesOnFirstReading(t *testing.T) {
	s := newTestService(t, &Config{})
	res, err := s.handleDialMove("x", 50.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res["status"] != "dial_initialized" {
		t.Errorf("status = %v, want dial_initialized", res["status"])
	}
	if len(s.pendingMoves) != 0 {
		t.Errorf("pendingMoves = %v, want empty on init", s.pendingMoves)
	}
}

func TestHandleDialMove_IncrementAndDecrement(t *testing.T) {
	s := newTestService(t, &Config{})
	if _, err := s.handleDialMove("x", 50.0); err != nil { // init
		t.Fatal(err)
	}

	res, err := s.handleDialMove("x", 51.0)
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "queued" || res["step"].(float64) != defaultMoveMM {
		t.Errorf("increment: status=%v step=%v, want queued/%v", res["status"], res["step"], defaultMoveMM)
	}
	if got := s.pendingMoves["x"]; got != defaultMoveMM {
		t.Errorf("pending after +1 = %v, want %v", got, defaultMoveMM)
	}

	// A second detent in the same direction accumulates.
	if _, err := s.handleDialMove("x", 52.0); err != nil {
		t.Fatal(err)
	}
	if got := s.pendingMoves["x"]; got != 2*defaultMoveMM {
		t.Errorf("pending after +2 = %v, want %v", got, 2*defaultMoveMM)
	}

	// Reverse direction yields a negative step.
	res, err = s.handleDialMove("x", 51.0)
	if err != nil {
		t.Fatal(err)
	}
	if res["step"].(float64) != -defaultMoveMM {
		t.Errorf("decrement step = %v, want %v", res["step"], -defaultMoveMM)
	}
}

func TestHandleDialMove_NoChangeAwayFromBoundary(t *testing.T) {
	s := newTestService(t, &Config{})
	if _, err := s.handleDialMove("x", 50.0); err != nil { // init
		t.Fatal(err)
	}
	res, err := s.handleDialMove("x", 50.0) // same value, no prior direction
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "no_change" {
		t.Errorf("status = %v, want no_change", res["status"])
	}
	if len(s.pendingMoves) != 0 {
		t.Errorf("pendingMoves = %v, want empty", s.pendingMoves)
	}
}

// TestHandleDialMove_Rollover verifies the wrap correction: a raw jump larger
// than half the range is reinterpreted as a short move across the 0/max seam.
func TestHandleDialMove_Rollover(t *testing.T) {
	// maxPos=100. 98 -> 1 is a raw delta of -97; corrected to +4 (wrapped up
	// past the max), so direction is positive and the detent count is 4.
	s := newTestService(t, &Config{})
	if _, err := s.handleDialMove("x", 98.0); err != nil {
		t.Fatal(err)
	}
	res, err := s.handleDialMove("x", 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if res["step"].(float64) != defaultMoveMM {
		t.Errorf("rollover-up step = %v, want +%v (positive direction)", res["step"], defaultMoveMM)
	}
	if got := s.pendingCounts["x"]; got != 4 {
		t.Errorf("rollover detent count = %v, want 4", got)
	}

	// The mirror case: 2 -> 99 is raw +97, corrected to -4 (wrapped down).
	s2 := newTestService(t, &Config{})
	if _, err := s2.handleDialMove("x", 2.0); err != nil {
		t.Fatal(err)
	}
	res, err = s2.handleDialMove("x", 99.0)
	if err != nil {
		t.Fatal(err)
	}
	if res["step"].(float64) != -defaultMoveMM {
		t.Errorf("rollover-down step = %v, want -%v (negative direction)", res["step"], defaultMoveMM)
	}
}

// TestHandleDialMove_SaturationAtMax verifies that holding the dial against the
// upper limit (Stream Deck retransmits maxPos) keeps synthesizing detents in
// the last direction instead of stalling.
func TestHandleDialMove_SaturationAtMax(t *testing.T) {
	s := newTestService(t, &Config{})
	if _, err := s.handleDialMove("x", 99.0); err != nil { // init
		t.Fatal(err)
	}
	if _, err := s.handleDialMove("x", 100.0); err != nil { // move to max, direction +1
		t.Fatal(err)
	}
	res, err := s.handleDialMove("x", 100.0) // held at max, same value
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "queued" || res["step"].(float64) != defaultMoveMM {
		t.Errorf("held-at-max: status=%v step=%v, want queued/+%v", res["status"], res["step"], defaultMoveMM)
	}
}

func TestHandleDialMove_RotationModeRoutesAxis(t *testing.T) {
	s := newTestService(t, &Config{})
	s.axisMode = axisModeRotation
	res, err := s.handleDialMove("x", 50.0) // should be routed to rx
	if err != nil {
		t.Fatal(err)
	}
	if res["axis"] != "rx" {
		t.Errorf("rotation-mode axis = %v, want rx", res["axis"])
	}
}

func TestHandleDialMove_InvalidValue(t *testing.T) {
	s := newTestService(t, &Config{})
	if _, err := s.handleDialMove("x", "not-a-number"); err == nil {
		t.Error("expected error for non-numeric dial value, got nil")
	}
}

// TestHandleDialMove_ZeroBoundaryBounce covers the case where the dial sits at
// the lower limit and Stream Deck reports it bouncing back up: motion continues
// in the last (downward) direction rather than reversing.
func TestHandleDialMove_ZeroBoundaryBounce(t *testing.T) {
	s := newTestService(t, &Config{})
	if _, err := s.handleDialMove("x", 1.0); err != nil { // init at 1
		t.Fatal(err)
	}
	if _, err := s.handleDialMove("x", 0.0); err != nil { // down to 0, direction -1
		t.Fatal(err)
	}
	res, err := s.handleDialMove("x", 1.0) // bounces back to 1 while pinned at floor
	if err != nil {
		t.Fatal(err)
	}
	if res["step"].(float64) != -defaultMoveMM {
		t.Errorf("zero-bounce step = %v, want -%v (continue downward)", res["step"], defaultMoveMM)
	}
}

func TestRotatePose_PreservesPositionAndUnknownAxis(t *testing.T) {
	start := spatialmath.NewPose(r3.Vector{X: 10, Y: 20, Z: 30}, &spatialmath.R4AA{Theta: 0, RX: 1})

	got, err := rotatePose(start, "rx", 90)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d := got.Point().Sub(start.Point()).Norm(); d > floatTol {
		t.Errorf("position moved by %.6f mm, want 0 (rotation pivots in place)", d)
	}

	if _, err := rotatePose(start, "rw", 90); err == nil {
		t.Error("expected error for unknown rotation axis, got nil")
	}
}

func TestRotatePose_NinetyDegreesAboutX(t *testing.T) {
	start := spatialmath.NewPose(r3.Vector{}, &spatialmath.R4AA{Theta: 0, RX: 1})
	got, err := rotatePose(start, "rx", 90)
	if err != nil {
		t.Fatal(err)
	}
	aa := got.Orientation().AxisAngles()
	if !almostEqual(aa.Theta, math.Pi/2, 1e-6) {
		t.Errorf("rotation angle = %.6f rad, want %.6f (90°)", aa.Theta, math.Pi/2)
	}
	// Axis should be +X.
	if !almostEqual(aa.RX, 1, 1e-6) || !almostEqual(aa.RY, 0, 1e-6) || !almostEqual(aa.RZ, 0, 1e-6) {
		t.Errorf("rotation axis = (%.4f, %.4f, %.4f), want (1, 0, 0)", aa.RX, aa.RY, aa.RZ)
	}
}

func TestRotatePose_ZeroDegreesIsIdentity(t *testing.T) {
	start := spatialmath.NewPose(r3.Vector{X: 1, Y: 2, Z: 3}, &spatialmath.R4AA{Theta: 0, RX: 1})
	got, err := rotatePose(start, "rz", 0)
	if err != nil {
		t.Fatal(err)
	}
	if aa := got.Orientation().AxisAngles(); math.Abs(aa.Theta) > 1e-6 {
		t.Errorf("zero-degree rotation produced angle %.6f rad, want ~0", aa.Theta)
	}
}

func TestAxisModeCommands(t *testing.T) {
	s := newTestService(t, &Config{})

	// toggle flips translation -> rotation -> translation.
	res, _ := s.toggleAxisMode()
	if res["axis_mode"] != axisModeRotation {
		t.Errorf("after 1st toggle = %v, want rotation", res["axis_mode"])
	}
	res, _ = s.toggleAxisMode()
	if res["axis_mode"] != axisModeTranslation {
		t.Errorf("after 2nd toggle = %v, want translation", res["axis_mode"])
	}

	// set accepts the two valid modes and rejects anything else.
	if _, err := s.setAxisMode(axisModeRotation); err != nil {
		t.Errorf("setAxisMode(rotation) unexpected error: %v", err)
	}
	if s.axisMode != axisModeRotation {
		t.Errorf("axisMode = %v after set, want rotation", s.axisMode)
	}
	if _, err := s.setAxisMode("sideways"); err == nil {
		t.Error("setAxisMode(sideways) expected error, got nil")
	}
	if _, err := s.setAxisMode(42); err == nil {
		t.Error("setAxisMode(non-string) expected error, got nil")
	}
}

// TestDoCommand covers the command dispatch: the removed-flag error, the three
// axis-mode commands, a dial move, and the unknown-command fallthrough. None of
// these paths touch the arm.
func TestDoCommand(t *testing.T) {
	ctx := context.Background()

	s := newTestService(t, &Config{})
	if _, err := s.DoCommand(ctx, map[string]any{"dial_move_speed": 5}); err == nil {
		t.Error("dial_move_speed should return the removed-flag error")
	}

	res, err := s.DoCommand(ctx, map[string]any{"toggle_axis_mode": true})
	if err != nil || res["axis_mode"] != axisModeRotation {
		t.Errorf("toggle_axis_mode = (%v, %v), want rotation", res, err)
	}

	res, err = s.DoCommand(ctx, map[string]any{"set_axis_mode": axisModeTranslation})
	if err != nil || res["axis_mode"] != axisModeTranslation {
		t.Errorf("set_axis_mode = (%v, %v), want translation", res, err)
	}

	res, err = s.DoCommand(ctx, map[string]any{"get_axis_mode": true})
	if err != nil || res["axis_mode"] != axisModeTranslation {
		t.Errorf("get_axis_mode = (%v, %v), want translation", res, err)
	}

	res, err = s.DoCommand(ctx, map[string]any{"dial_move_x": 50.0})
	if err != nil || res["status"] != "dial_initialized" {
		t.Errorf("dial_move_x = (%v, %v), want dial_initialized", res, err)
	}

	if _, err := s.DoCommand(ctx, map[string]any{"nonsense": 1}); err == nil {
		t.Error("unknown command should return an error")
	}
}

// TestFlushMoves_Translation checks that accumulated translation deltas are
// scaled by their per-axis multiplier and applied to the arm's current point.
// The "orientation" axis moves along the current orientation vector (+Z for an
// identity pose).
func TestFlushMoves_Translation(t *testing.T) {
	s := newTestService(t, &Config{})
	start := spatialmath.NewPose(r3.Vector{X: 10, Y: 20, Z: 30}, &spatialmath.R4AA{Theta: 0, RX: 1})

	var captured spatialmath.Pose
	s.arm = &inject.Arm{
		EndPositionFunc: func(context.Context, map[string]any) (spatialmath.Pose, error) {
			return start, nil
		},
		MoveToPositionFunc: func(_ context.Context, to spatialmath.Pose, _ map[string]any) error {
			captured = to
			return nil
		},
	}

	// x: +2 × 3 = +6; orientation: +5 along +Z; y untouched.
	err := s.flushMoves(
		map[string]float64{"x": 2, "orientation": 5},
		map[string]float64{"x": 3},
	)
	if err != nil {
		t.Fatalf("flushMoves error: %v", err)
	}
	if captured == nil {
		t.Fatal("MoveToPosition was not called")
	}
	pt := captured.Point()
	if !almostEqual(pt.X, 16, 1e-6) {
		t.Errorf("x = %v, want 16 (10 + 2×3)", pt.X)
	}
	if !almostEqual(pt.Y, 20, 1e-6) {
		t.Errorf("y = %v, want 20 (unchanged)", pt.Y)
	}
	if !almostEqual(pt.Z, 35, 1e-6) {
		t.Errorf("z = %v, want 35 (30 + orientation move of 5 along +Z)", pt.Z)
	}
}

// TestFlushMoves_Rotation checks that a rotation-axis delta is composed onto the
// orientation while the position pivots in place.
func TestFlushMoves_Rotation(t *testing.T) {
	s := newTestService(t, &Config{})
	start := spatialmath.NewPose(r3.Vector{X: 1, Y: 2, Z: 3}, &spatialmath.R4AA{Theta: 0, RX: 1})

	var captured spatialmath.Pose
	s.arm = &inject.Arm{
		EndPositionFunc: func(context.Context, map[string]any) (spatialmath.Pose, error) {
			return start, nil
		},
		MoveToPositionFunc: func(_ context.Context, to spatialmath.Pose, _ map[string]any) error {
			captured = to
			return nil
		},
	}

	if err := s.flushMoves(map[string]float64{"rx": 90}, nil); err != nil {
		t.Fatalf("flushMoves error: %v", err)
	}
	if captured == nil {
		t.Fatal("MoveToPosition was not called")
	}
	if d := captured.Point().Sub(start.Point()).Norm(); d > 1e-6 {
		t.Errorf("position moved by %.6f mm, want 0 (rotation pivots in place)", d)
	}
	if aa := captured.Orientation().AxisAngles(); !almostEqual(aa.Theta, math.Pi/2, 1e-6) {
		t.Errorf("orientation angle = %.6f rad, want %.6f (90°)", aa.Theta, math.Pi/2)
	}
}

func TestFlushMoves_EndPositionError(t *testing.T) {
	s := newTestService(t, &Config{})
	s.arm = &inject.Arm{
		EndPositionFunc: func(context.Context, map[string]any) (spatialmath.Pose, error) {
			return nil, errors.New("arm unreachable")
		},
	}
	if err := s.flushMoves(map[string]float64{"x": 1}, nil); err == nil {
		t.Error("flushMoves should propagate an EndPosition error, got nil")
	}
}

// TestLifecycle covers the resource-contract methods: Name returns the stored
// name, Status is an empty map, and Close cancels the context the drain loop
// selects on.
func TestLifecycle(t *testing.T) {
	s := newTestService(t, &Config{})
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelCtx = ctx
	s.cancelFunc = cancel

	if s.Name() != s.name {
		t.Errorf("Name() = %v, want %v", s.Name(), s.name)
	}
	if st, err := s.Status(context.Background()); err != nil || st == nil {
		t.Errorf("Status() = (%v, %v), want (empty map, nil)", st, err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Error("Close() did not cancel cancelCtx")
	}
}
