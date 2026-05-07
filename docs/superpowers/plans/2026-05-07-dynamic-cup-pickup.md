# Dynamic Cup Pickup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the hardcoded `empty_cup` pickup phase of `setCupForCoffee` with a vision-driven dynamic pickup gated behind a `dynamic_cup_pickup` config flag.

**Architecture:** New file `cup_pickup.go` adds three pure helpers (`selectCupCentroid`, `composeCupPose`, `cameraToWorld`) plus an orchestrator `pickCupDynamic` that calls a vision service, lifts centroids into world frame, composes configured relative poses (read from the claws pose switch), and feeds the resulting world poses to the existing `moveToRawPose`. `setCupForCoffee` branches on the new flag; the placement phase that follows is unchanged.

**Tech Stack:** Go, Viam RDK (`go.viam.com/rdk`), packages `services/vision`, `components/camera`, `referenceframe`, `spatialmath`. Tests use the standard library `testing` package, matching `circular_test.go` / `pivot_test.go` / `queue_test.go` style.

**Spec:** `docs/superpowers/specs/2026-05-07-dynamic-cup-pickup-design.md`

---

## File map

| File | Action | Responsibility |
|---|---|---|
| `module.go` | Modify | Add 7 Config fields + `Vec3Mm` type; extend `Validate`; resolve vision/camera deps in `NewCoffee`; verify camera in frame system; add struct fields |
| `module_test.go` | Create | Unit tests for the new validation paths |
| `cup_pickup.go` | Create | `pickCupDynamic`, `observeCupCentroid`, `selectCupCentroid`, `composeCupPose`, `cameraToWorld` |
| `cup_pickup_test.go` | Create | Unit tests for the three pure helpers |
| `espresso.go` | Modify | Branch in `setCupForCoffee` (line 406) on `s.cfg.DynamicCupPickup` |
| `README.md` | Modify | Document new config fields + claws-switch pose convention |

---

## Task 1: Add Config fields and `Vec3Mm` type

Structural change only — no behavior, no tests yet. Just makes the new fields available so subsequent tasks can read them.

**Files:**
- Modify: `module.go` (add `Vec3Mm` near `JointLimitDegs`; add fields to `Config`)

- [ ] **Step 1: Add fields to the `Config` struct in `module.go`**

Locate the `Config` struct (currently lines 73–101). Add the new fields immediately above the `InputRangeOverride` field. The `Vec3Mm` type goes right after the `Config` struct definition.

Insert these fields inside `Config`:

```go
	// Dynamic cup pickup. When true, setCupForCoffee uses vision-driven
	// detection to find the cup; when false, the existing static pickup
	// (empty_cup_approach -> empty_cup) is used.
	DynamicCupPickup           bool     `json:"dynamic_cup_pickup,omitempty"`
	CupVisionServiceName       string   `json:"cup_vision_service_name,omitempty"`
	SrcCameraName              string   `json:"src_camera_name,omitempty"`
	ExpectedCupPositionMm      *Vec3Mm  `json:"expected_cup_position_mm,omitempty"`
	CupMaxDistanceFromTargetMm float64  `json:"cup_max_distance_from_target_mm,omitempty"`
	CupDetectionRetries        int      `json:"cup_detection_retries,omitempty"`
	CupDetectionRetrySleepMs   int      `json:"cup_detection_retry_sleep_ms,omitempty"`
```

Add the `Vec3Mm` type right after the closing `}` of `Config`:

```go
// Vec3Mm is a 3D point in millimeters used for world-frame configuration.
type Vec3Mm struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}
```

- [ ] **Step 2: Verify the package still compiles**

Run: `go build ./...`
Expected: clean build, no errors.

- [ ] **Step 3: Verify existing tests still pass**

Run: `go test ./...`
Expected: all existing tests pass; no new tests yet.

- [ ] **Step 4: Commit**

```bash
git add module.go
git commit -m "$(cat <<'EOF'
Add Config fields for dynamic cup pickup

Structural change only. Adds DynamicCupPickup flag, vision/camera
service-name fields, target-position heuristic, max-distance cutoff,
and retry knobs. No behavior yet — Validate and consumers come in
follow-up commits.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Extend `Config.Validate` for the new fields (TDD)

**Files:**
- Create: `module_test.go`
- Modify: `module.go` (`Validate` function, currently lines 103–130)

- [ ] **Step 1: Write the failing test in `module_test.go`**

Create the file with these tests. There is no existing `module_test.go` in the top-level package — this is the first.

```go
package beanjamin

import (
	"strings"
	"testing"
)

func validBaseConfig() *Config {
	return &Config{
		PoseSwitcherName:      "filter-switch",
		ClawsPoseSwitcherName: "claws-switch",
		ArmName:               "arm",
		GripperName:           "gripper",
	}
}

func TestValidate_DynamicCupPickup_OffLeavesUnsetFieldsAlone(t *testing.T) {
	cfg := validBaseConfig()
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_RequiresVisionServiceName(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_vision_service_name") {
		t.Fatalf("expected cup_vision_service_name required error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_RequiresSrcCameraName(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "src_camera_name") {
		t.Fatalf("expected src_camera_name required error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_RequiresExpectedCupPosition(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "expected_cup_position_mm") {
		t.Fatalf("expected expected_cup_position_mm required error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_DefaultsMaxDistance(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.CupMaxDistanceFromTargetMm != 300 {
		t.Fatalf("expected default 300mm, got %f", cfg.CupMaxDistanceFromTargetMm)
	}
}

func TestValidate_DynamicCupPickup_PreservesExplicitMaxDistance(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	cfg.CupMaxDistanceFromTargetMm = 500
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.CupMaxDistanceFromTargetMm != 500 {
		t.Fatalf("expected 500mm preserved, got %f", cfg.CupMaxDistanceFromTargetMm)
	}
}

func TestValidate_DynamicCupPickup_RejectsNegativeRetries(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	cfg.CupDetectionRetries = -1
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_detection_retries") {
		t.Fatalf("expected cup_detection_retries error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_AppendsDeps(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	req, _, err := cfg.Validate("")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	var sawVision, sawCamera bool
	for _, d := range req {
		if strings.Contains(d, "vis") {
			sawVision = true
		}
		if strings.Contains(d, "cam") {
			sawCamera = true
		}
	}
	if !sawVision {
		t.Fatalf("expected vision dep in required deps, got %v", req)
	}
	if !sawCamera {
		t.Fatalf("expected camera dep in required deps, got %v", req)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

Run: `go test -run "TestValidate_DynamicCupPickup" ./...`
Expected: tests fail — `Validate` does not yet enforce the new fields.

- [ ] **Step 3: Extend `Validate` in `module.go`**

The existing `Validate` (lines 103–130) stops at `return reqDeps, optDeps, nil`. Add the new validation block immediately before that return.

You will need to import these packages at the top of `module.go` (alongside the existing imports):

```go
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/services/vision"
```

Replace the body of `Validate` so it ends like this (the lines before the new block remain unchanged — only the trailing `return` and the new block are shown):

```go
	if cfg.DynamicCupPickup {
		if cfg.CupVisionServiceName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "cup_vision_service_name")
		}
		if cfg.SrcCameraName == "" {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "src_camera_name")
		}
		if cfg.ExpectedCupPositionMm == nil {
			return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "expected_cup_position_mm")
		}
		if cfg.CupDetectionRetries < 0 {
			return nil, nil, fmt.Errorf("%s: cup_detection_retries must be >= 0", path)
		}
		if cfg.CupMaxDistanceFromTargetMm == 0 {
			cfg.CupMaxDistanceFromTargetMm = 300
		}
		reqDeps = append(reqDeps,
			vision.Named(cfg.CupVisionServiceName).String(),
			camera.Named(cfg.SrcCameraName).String(),
		)
	}

	return reqDeps, optDeps, nil
```

- [ ] **Step 4: Run tests to confirm they pass**

Run: `go test -run "TestValidate_DynamicCupPickup" -v ./...`
Expected: all eight test cases pass.

Run: `go test ./...`
Expected: no regressions in existing tests.

- [ ] **Step 5: Commit**

```bash
git add module.go module_test.go
git commit -m "$(cat <<'EOF'
Validate dynamic cup pickup config

Enforces required fields when dynamic_cup_pickup=true, defaults
cup_max_distance_from_target_mm to 300, rejects negative retries, and
appends vision/camera service deps to the required-deps list.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Implement `selectCupCentroid` (TDD)

Pure function. Returns the closest centroid to the configured target within `maxDistMm`, plus its original index for log correlation.

**Files:**
- Create: `cup_pickup.go`
- Create: `cup_pickup_test.go`

- [ ] **Step 1: Write the failing tests in `cup_pickup_test.go`**

```go
package beanjamin

import (
	"strings"
	"testing"

	"github.com/golang/geo/r3"
)

func TestSelectCupCentroid_Empty(t *testing.T) {
	_, _, err := selectCupCentroid(nil, r3.Vector{}, 100)
	if err == nil {
		t.Fatalf("expected error on empty input")
	}
}

func TestSelectCupCentroid_SingleInRange(t *testing.T) {
	c := []r3.Vector{{X: 110, Y: 0, Z: 0}}
	got, idx, err := selectCupCentroid(c, r3.Vector{X: 100, Y: 0, Z: 0}, 50)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 0 {
		t.Fatalf("expected index 0, got %d", idx)
	}
	if got != c[0] {
		t.Fatalf("expected centroid %v, got %v", c[0], got)
	}
}

func TestSelectCupCentroid_SingleOutOfRange(t *testing.T) {
	c := []r3.Vector{{X: 1000, Y: 0, Z: 0}}
	_, _, err := selectCupCentroid(c, r3.Vector{}, 100)
	if err == nil || !strings.Contains(err.Error(), "within") {
		t.Fatalf("expected 'within' error, got %v", err)
	}
}

func TestSelectCupCentroid_PicksClosest(t *testing.T) {
	c := []r3.Vector{
		{X: 200, Y: 0, Z: 0}, // 100mm from target — farther
		{X: 110, Y: 0, Z: 0}, // 10mm from target — closer
		{X: 150, Y: 0, Z: 0}, // 50mm from target
	}
	target := r3.Vector{X: 100, Y: 0, Z: 0}
	got, idx, err := selectCupCentroid(c, target, 300)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	if got != c[1] {
		t.Fatalf("expected centroid %v, got %v", c[1], got)
	}
}

func TestSelectCupCentroid_AllOutOfRange(t *testing.T) {
	c := []r3.Vector{
		{X: 1000, Y: 0, Z: 0},
		{X: 2000, Y: 0, Z: 0},
	}
	_, _, err := selectCupCentroid(c, r3.Vector{}, 100)
	if err == nil || !strings.Contains(err.Error(), "within") {
		t.Fatalf("expected 'within' error, got %v", err)
	}
}

func TestSelectCupCentroid_ZeroMaxMeansNoCutoff(t *testing.T) {
	c := []r3.Vector{
		{X: 1e6, Y: 0, Z: 0},
		{X: 100, Y: 0, Z: 0},
	}
	got, idx, err := selectCupCentroid(c, r3.Vector{}, 0)
	if err != nil {
		t.Fatalf("expected no error with maxDistMm=0, got %v", err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	if got != c[1] {
		t.Fatalf("expected closest centroid, got %v", got)
	}
}

func TestSelectCupCentroid_TieBreaksFirst(t *testing.T) {
	c := []r3.Vector{
		{X: 110, Y: 0, Z: 0},
		{X: 90, Y: 0, Z: 0}, // both 10mm from target
	}
	target := r3.Vector{X: 100, Y: 0, Z: 0}
	got, idx, err := selectCupCentroid(c, target, 50)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 0 {
		t.Fatalf("expected first-wins (index 0), got %d", idx)
	}
	if got != c[0] {
		t.Fatalf("expected first centroid, got %v", got)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

Run: `go test -run "TestSelectCupCentroid" ./...`
Expected: tests fail — `selectCupCentroid` does not yet exist.

- [ ] **Step 3: Create `cup_pickup.go` with the minimal implementation**

```go
// Package beanjamin: dynamic cup pickup.
//
// pickCupDynamic replaces the static empty_cup grab in setCupForCoffee
// when dynamic_cup_pickup is enabled. It moves the arm to a configured
// observe pose, calls a vision service for cup detections, lifts each
// centroid into world frame, picks the closest detection within range,
// composes the configured approach/grab relative poses (read from the
// claws pose switch by name), and feeds the resulting world poses to
// moveToRawPose.
package beanjamin

import (
	"fmt"

	"github.com/golang/geo/r3"
)

// selectCupCentroid returns the centroid in `centroids` closest to `target`
// (Euclidean distance), provided it is within maxDistMm. maxDistMm == 0
// disables the cutoff. Returns the chosen centroid and its original index
// (for log correlation), or an error if the input is empty or no centroid
// is within range.
func selectCupCentroid(centroids []r3.Vector, target r3.Vector, maxDistMm float64) (r3.Vector, int, error) {
	if len(centroids) == 0 {
		return r3.Vector{}, -1, fmt.Errorf("no centroids")
	}
	bestIdx := -1
	bestDist := 0.0
	for i, c := range centroids {
		d := c.Sub(target).Norm()
		if maxDistMm > 0 && d > maxDistMm {
			continue
		}
		if bestIdx == -1 || d < bestDist {
			bestIdx = i
			bestDist = d
		}
	}
	if bestIdx == -1 {
		return r3.Vector{}, -1, fmt.Errorf("no centroid within %.0fmm of target", maxDistMm)
	}
	return centroids[bestIdx], bestIdx, nil
}
```

- [ ] **Step 4: Run tests to confirm they pass**

Run: `go test -run "TestSelectCupCentroid" -v ./...`
Expected: all seven test cases pass.

- [ ] **Step 5: Commit**

```bash
git add cup_pickup.go cup_pickup_test.go
git commit -m "$(cat <<'EOF'
Add selectCupCentroid helper for dynamic cup pickup

Pure function: given world-frame centroids, a target heuristic point,
and a max-distance cutoff, returns the closest in-range centroid and
its original index. maxDistMm=0 disables the cutoff. Tests cover
empty input, in/out of range, multi-cup picking, ties, and the no-cutoff
escape hatch.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Implement `composeCupPose` (TDD)

Pure function. Composes a configured relative pose (translation + orientation) onto a world-frame centroid point.

**Files:**
- Modify: `cup_pickup.go`
- Modify: `cup_pickup_test.go`

- [ ] **Step 1: Append failing tests to `cup_pickup_test.go`**

Add these tests to the existing file. You may need to add `"go.viam.com/rdk/spatialmath"` to the imports if not already there.

```go
import (
	// ... existing imports ...
	"go.viam.com/rdk/spatialmath"
)

func TestComposeCupPose_IdentityRelative(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 300}
	relative := spatialmath.NewZeroPose()
	got := composeCupPose(centroid, relative)
	if got.Point() != centroid {
		t.Fatalf("expected centroid preserved %v, got %v", centroid, got.Point())
	}
	if !spatialmath.OrientationAlmostEqual(got.Orientation(), spatialmath.NewZeroOrientation()) {
		t.Fatalf("expected zero orientation, got %v", got.Orientation())
	}
}

func TestComposeCupPose_PureTranslation(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 300}
	relative := spatialmath.NewPoseFromPoint(r3.Vector{X: 10, Y: 0, Z: 0})
	got := composeCupPose(centroid, relative)
	want := r3.Vector{X: 110, Y: 200, Z: 300}
	if got.Point() != want {
		t.Fatalf("expected %v, got %v", want, got.Point())
	}
}

func TestComposeCupPose_PureRotation(t *testing.T) {
	centroid := r3.Vector{X: 100, Y: 200, Z: 300}
	orient := &spatialmath.OrientationVectorDegrees{OX: 1, OY: 0, OZ: 0, Theta: 90}
	relative := spatialmath.NewPose(r3.Vector{}, orient)
	got := composeCupPose(centroid, relative)
	if got.Point() != centroid {
		t.Fatalf("expected centroid preserved %v, got %v", centroid, got.Point())
	}
	if !spatialmath.OrientationAlmostEqual(got.Orientation(), orient) {
		t.Fatalf("expected %v, got %v", orient, got.Orientation())
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

Run: `go test -run "TestComposeCupPose" ./...`
Expected: tests fail — `composeCupPose` not defined.

- [ ] **Step 3: Append the implementation to `cup_pickup.go`**

Add `"go.viam.com/rdk/spatialmath"` to the imports.

```go
// composeCupPose builds a world-frame target pose by composing a relative
// pose (translation + orientation) onto a centroid point with identity
// orientation. The relative pose is read from the claws pose switch under
// names like cup_grab_relative_pose / cup_approach_relative_pose; its
// reference_frame field is intentionally ignored — these poses are
// interpreted as offsets from the runtime centroid, not absolute poses.
func composeCupPose(centroidWorld r3.Vector, relative spatialmath.Pose) spatialmath.Pose {
	centroid := spatialmath.NewPoseFromPoint(centroidWorld)
	return spatialmath.Compose(centroid, relative)
}
```

- [ ] **Step 4: Run tests to confirm they pass**

Run: `go test -run "TestComposeCupPose" -v ./...`
Expected: all three tests pass.

- [ ] **Step 5: Commit**

```bash
git add cup_pickup.go cup_pickup_test.go
git commit -m "$(cat <<'EOF'
Add composeCupPose helper for dynamic cup pickup

Pure function: composes a relative pose (translation + orientation)
onto a world-frame centroid point. Used to build approach and grab
target poses from the configured cup_*_relative_pose entries in the
claws pose switch.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Implement `cameraToWorld` (TDD)

Lifts a point in the camera's local frame into world coordinates via the cached frame system.

**Files:**
- Modify: `cup_pickup.go`
- Modify: `cup_pickup_test.go`

- [ ] **Step 1: Append failing tests to `cup_pickup_test.go`**

Add `"go.viam.com/rdk/referenceframe"` to the imports.

```go
func cameraToWorldTestFS(t *testing.T, camPose spatialmath.Pose) *referenceframe.FrameSystem {
	t.Helper()
	fs := referenceframe.NewEmptyFrameSystem("test")
	camFrame, err := referenceframe.NewStaticFrame("camera", camPose)
	if err != nil {
		t.Fatalf("create camera frame: %v", err)
	}
	if err := fs.AddFrame(camFrame, fs.World()); err != nil {
		t.Fatalf("add camera frame: %v", err)
	}
	return fs
}

func TestCameraToWorld_Identity(t *testing.T) {
	fs := cameraToWorldTestFS(t, spatialmath.NewZeroPose())
	fsInputs := referenceframe.NewZeroInputs(fs)
	point := r3.Vector{X: 50, Y: 60, Z: 70}
	got, err := cameraToWorld(fs, fsInputs, "camera", point)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != point {
		t.Fatalf("expected %v unchanged, got %v", point, got)
	}
}

func TestCameraToWorld_Translated(t *testing.T) {
	camPose := spatialmath.NewPose(r3.Vector{X: 100, Y: 0, Z: 0}, spatialmath.NewZeroOrientation())
	fs := cameraToWorldTestFS(t, camPose)
	fsInputs := referenceframe.NewZeroInputs(fs)
	got, err := cameraToWorld(fs, fsInputs, "camera", r3.Vector{X: 10, Y: 0, Z: 0})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	want := r3.Vector{X: 110, Y: 0, Z: 0}
	if got != want {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestCameraToWorld_MissingFrame(t *testing.T) {
	fs := referenceframe.NewEmptyFrameSystem("test")
	fsInputs := referenceframe.NewZeroInputs(fs)
	_, err := cameraToWorld(fs, fsInputs, "no-such-camera", r3.Vector{})
	if err == nil {
		t.Fatalf("expected error for missing camera frame")
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

Run: `go test -run "TestCameraToWorld" ./...`
Expected: tests fail — `cameraToWorld` not defined.

- [ ] **Step 3: Append the implementation to `cup_pickup.go`**

Add `"go.viam.com/rdk/referenceframe"` to the imports.

```go
// cameraToWorld lifts a point given in the camera's local frame into the
// world frame. The vision service returns object geometry in the camera
// frame; this function uses the frame system Transform to convert.
func cameraToWorld(
	fs *referenceframe.FrameSystem,
	fsInputs referenceframe.FrameSystemInputs,
	cameraFrame string,
	point r3.Vector,
) (r3.Vector, error) {
	pif := referenceframe.NewPoseInFrame(cameraFrame, spatialmath.NewPoseFromPoint(point))
	tf, err := fs.Transform(fsInputs.ToLinearInputs(), pif, referenceframe.World)
	if err != nil {
		return r3.Vector{}, fmt.Errorf("transform %q to world: %w", cameraFrame, err)
	}
	worldPose, ok := tf.(*referenceframe.PoseInFrame)
	if !ok {
		return r3.Vector{}, fmt.Errorf("unexpected transform result type %T", tf)
	}
	return worldPose.Pose().Point(), nil
}
```

- [ ] **Step 4: Run tests to confirm they pass**

Run: `go test -run "TestCameraToWorld" -v ./...`
Expected: all three tests pass.

- [ ] **Step 5: Commit**

```bash
git add cup_pickup.go cup_pickup_test.go
git commit -m "$(cat <<'EOF'
Add cameraToWorld helper for dynamic cup pickup

Lifts a camera-frame point into world via fs.Transform. Vision
services return centroids in the camera frame; this is the seam
that makes the result usable as a planning goal.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Wire vision/camera deps and runtime checks in `NewCoffee`

Adds struct fields on `beanjaminCoffee`, resolves the new optional deps, and verifies the camera is registered in the frame system at module init.

**Files:**
- Modify: `module.go` (struct definition lines 132–159; `NewCoffee` constructor lines 169–302)

- [ ] **Step 1: Add fields to the `beanjaminCoffee` struct**

Inside the struct definition (around line 158, after `orderSensorSink orderSensorSink`), add:

```go
	cupVision     vision.Service // optional; nil when DynamicCupPickup=false
	cupCameraName string         // SrcCameraName, validated to exist in cachedFS
```

- [ ] **Step 2: Resolve deps and validate the camera frame in `NewCoffee`**

After the existing optional-dep resolution block (around line 278, after the order sensor block) and **before** the cached-frame-system creation block — wait, the camera-frame check needs `cachedFS` to exist. Place it AFTER the `cachedFS` is built (line 216) and AFTER `applyJointLimits` (line 218–221) and BEFORE the speech/camStorage blocks. Cleanest spot: directly after `applyJointLimits` succeeds.

Insert this block after line 221 (`}` closing the `applyJointLimits` if-err):

```go
	var cupVision vision.Service
	var cupCameraName string
	if conf.DynamicCupPickup {
		visRes, err := vision.FromProvider(deps, conf.CupVisionServiceName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("cup vision service %q: %w", conf.CupVisionServiceName, err)
		}
		cupVision = visRes
		if cachedFS.Frame(conf.SrcCameraName) == nil {
			cancelFunc()
			return nil, fmt.Errorf("src_camera_name %q not found in frame system — add the camera to the frame system fragment", conf.SrcCameraName)
		}
		cupCameraName = conf.SrcCameraName
		logger.Infof("dynamic cup pickup enabled (vision=%q, camera=%q)", conf.CupVisionServiceName, conf.SrcCameraName)
	}
```

- [ ] **Step 3: Plumb the new fields into the struct literal**

In the `s := &beanjaminCoffee{...}` initializer (currently lines 280–299), append:

```go
		cupVision:        cupVision,
		cupCameraName:    cupCameraName,
```

(Order doesn't matter; place them after `orderSensorSink: sink,` for readability.)

- [ ] **Step 4: Verify the build is clean**

Run: `go build ./...`
Expected: clean build. `vision` is already imported via the `Validate` change in Task 2; no new imports needed here.

Run: `go test ./...`
Expected: all existing tests still pass.

- [ ] **Step 5: Commit**

```bash
git add module.go
git commit -m "$(cat <<'EOF'
Resolve vision and camera deps for dynamic cup pickup

When dynamic_cup_pickup=true, NewCoffee now resolves the vision
service from deps and verifies the configured src_camera_name is
present in the cached frame system before any orders are taken.
Failure surfaces immediately at module init rather than mid-brew.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Implement `observeCupCentroid`

Combines the three pure helpers with the vision service and retry/sleep loop. No unit test (per spec scope — tested via the helpers and on hardware).

**Files:**
- Modify: `cup_pickup.go`

- [ ] **Step 1: Append `observeCupCentroid` to `cup_pickup.go`**

Add these imports to `cup_pickup.go` (alongside existing imports):

```go
	"context"
	"time"

	viz "go.viam.com/rdk/vision"
```

Then append the function:

```go
// observeCupCentroid calls the vision service for cup detections, retries
// on empty results per the configured retry policy, lifts each detection's
// centroid into world coordinates, and returns the closest in-range
// centroid (per selectCupCentroid). Returns an error if no cups are found
// after all retries, or if all detections fall outside the configured
// max distance.
func (s *beanjaminCoffee) observeCupCentroid(ctx context.Context) (r3.Vector, error) {
	maxAttempts := s.cfg.CupDetectionRetries + 1
	sleep := time.Duration(s.cfg.CupDetectionRetrySleepMs) * time.Millisecond
	if sleep <= 0 {
		sleep = 250 * time.Millisecond
	}

	var objects []*viz.Object
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		objs, err := s.cupVision.GetObjectPointClouds(ctx, s.cupCameraName, nil)
		if err != nil {
			return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: detect: %w", err)
		}
		s.logger.Infof("dynamic cup pickup: attempt %d/%d, found %d detections", attempt, maxAttempts, len(objs))
		if len(objs) > 0 {
			objects = objs
			break
		}
		if attempt < maxAttempts {
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: cancelled during retry: %w", ctx.Err())
			}
		}
	}
	if len(objects) == 0 {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: no cups detected after %d attempts", maxAttempts)
	}

	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %w", err)
	}

	centroids := make([]r3.Vector, 0, len(objects))
	for _, obj := range objects {
		if obj.Geometry == nil {
			continue
		}
		local := obj.Geometry.Pose().Point()
		world, err := cameraToWorld(fs, fsInputs, s.cupCameraName, local)
		if err != nil {
			return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %w", err)
		}
		s.logger.Debugf("dynamic cup pickup: detection at camera-local %v -> world %v", local, world)
		centroids = append(centroids, world)
	}
	if len(centroids) == 0 {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %d detection(s) had no usable geometry", len(objects))
	}

	target := r3.Vector{X: s.cfg.ExpectedCupPositionMm.X, Y: s.cfg.ExpectedCupPositionMm.Y, Z: s.cfg.ExpectedCupPositionMm.Z}
	chosen, idx, err := selectCupCentroid(centroids, target, s.cfg.CupMaxDistanceFromTargetMm)
	if err != nil {
		return r3.Vector{}, fmt.Errorf("dynamic_cup_pickup: %d detection(s) found but %w", len(centroids), err)
	}
	dist := chosen.Sub(target).Norm()
	s.logger.Infof("dynamic cup pickup: chose centroid %d at (x=%.1f, y=%.1f, z=%.1f) — %.1fmm from target",
		idx, chosen.X, chosen.Y, chosen.Z, dist)
	return chosen, nil
}
```

- [ ] **Step 2: Verify the build is clean**

Run: `go build ./...`
Expected: clean build. The function is not yet called, so no behavior tests run, but compile errors would surface here.

Run: `go test ./...`
Expected: all existing tests still pass.

- [ ] **Step 3: Commit**

```bash
git add cup_pickup.go
git commit -m "$(cat <<'EOF'
Add observeCupCentroid for dynamic cup pickup

Calls the vision service with the configured retry/sleep policy,
lifts each detection's centroid into world frame, and selects the
closest in-range centroid via selectCupCentroid. Returns distinct
errors for transport failure, all-empty detections, and out-of-range
detections so operators can triage camera vs. cup-placement issues.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Implement `pickCupDynamic` orchestrator

Top-level orchestrator: observe pose → centroid → fetch relative poses → compose → approach/grab/retreat.

**Files:**
- Modify: `cup_pickup.go`

- [ ] **Step 1: Append `pickCupDynamic` to `cup_pickup.go`**

Add `"go.viam.com/rdk/module/trace"` to the imports.

```go
// pickCupDynamic moves the arm to the configured cup_observe_pose, observes
// the closest cup via the vision service, and executes a side-grab using
// the cup_approach_relative_pose / cup_grab_relative_pose entries from the
// claws pose switch composed onto the detected centroid. Called by
// setCupForCoffee when DynamicCupPickup=true.
func (s *beanjaminCoffee) pickCupDynamic(ctx, cancelCtx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "beanjamin::dynamic_cup_pickup")
	defer span.End()

	if s.gripper == nil {
		return fmt.Errorf("dynamic_cup_pickup: no gripper configured")
	}

	// 1. Move to observe pose.
	observeStep := Step{PoseName: "cup_observe_pose", Component: "coffee-claws-middle", Pause: shortPause}
	if err := s.executeStep(ctx, cancelCtx, observeStep); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: observe: %w", err)
	}

	// 2. Observe.
	detectCtx, detectSpan := trace.StartSpan(ctx, "beanjamin::dynamic_cup_pickup::detect")
	centroidWorld, err := s.observeCupCentroid(detectCtx)
	detectSpan.End()
	if err != nil {
		return err
	}

	// 3. Fetch relative poses from the claws pose switch.
	approachRel, err := s.fetchPose(ctx, "coffee-claws-middle", "cup_approach_relative_pose")
	if err != nil {
		return fmt.Errorf("dynamic_cup_pickup: %w", err)
	}
	grabRel, err := s.fetchPose(ctx, "coffee-claws-middle", "cup_grab_relative_pose")
	if err != nil {
		return fmt.Errorf("dynamic_cup_pickup: %w", err)
	}

	// 4. Compose into world-frame *poseData for moveToRawPose.
	approachPD := &poseData{
		pose:          composeCupPose(centroidWorld, approachRel.pose),
		refFrame:      referenceframe.World,
		componentName: "coffee-claws-middle",
	}
	grabPD := &poseData{
		pose:          composeCupPose(centroidWorld, grabRel.pose),
		refFrame:      referenceframe.World,
		componentName: "coffee-claws-middle",
	}

	// 5. Approach (free planning).
	if err := s.moveToRawPose(ctx, approachPD, nil, nil, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: approach: %w", err)
	}

	// 6. Open the gripper before descending.
	if err := s.gripper.Open(ctx, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: open gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// 7. Linear move to grab pose.
	if err := s.moveToRawPose(ctx, grabPD, defaultApproachConstraint, nil, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: grab: %w", err)
	}

	// 8. Close the gripper.
	if _, err := s.gripper.Grab(ctx, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: grab gripper: %w", err)
	}
	time.Sleep(gripperPause)

	// 9. Linear retreat back to the approach pose.
	if err := s.moveToRawPose(ctx, approachPD, defaultApproachConstraint, nil, nil); err != nil {
		return fmt.Errorf("dynamic_cup_pickup: retreat: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Verify the build is clean**

Run: `go build ./...`
Expected: clean build.

Run: `go test ./...`
Expected: all existing tests still pass. `pickCupDynamic` isn't called yet, so no behavior change.

- [ ] **Step 3: Commit**

```bash
git add cup_pickup.go
git commit -m "$(cat <<'EOF'
Add pickCupDynamic orchestrator

Moves the arm to cup_observe_pose, calls observeCupCentroid, fetches
the configured cup_approach_relative_pose and cup_grab_relative_pose
from the claws pose switch, composes them onto the detected centroid,
and runs approach -> open -> grab -> close -> retreat via moveToRawPose.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Branch `setCupForCoffee` on the new flag

Wire the new orchestrator into the brew cycle without touching the placement phase.

**Files:**
- Modify: `espresso.go` (`setCupForCoffee`, currently lines 406–467)

- [ ] **Step 1: Modify `setCupForCoffee` to branch at the start**

The static pickup phase runs from line 411 (`approachStep := Step{PoseName: "empty_cup_approach", ...}`) through line 438 (the second `executeStep` for the retreat). The placement phase (`cup_under_machine_approach` → `cup_ready_for_coffee` → release → exit → close gripper) starts at line 439 and stays as-is.

Replace lines 406–438 (function signature through end of static retreat) with:

```go
func (s *beanjaminCoffee) setCupForCoffee(ctx, cancelCtx context.Context) error {
	if s.gripper == nil {
		return fmt.Errorf("set_cup_for_coffee: no gripper configured")
	}

	if s.cfg.DynamicCupPickup {
		if err := s.pickCupDynamic(ctx, cancelCtx); err != nil {
			return fmt.Errorf("set_cup_for_coffee: %w", err)
		}
	} else {
		// Static pickup: approach -> open gripper -> grab -> retreat.
		approachStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, approachStep); err != nil {
			return fmt.Errorf("set_cup_for_coffee: %w", err)
		}

		if err := s.gripper.Open(ctx, nil); err != nil {
			return fmt.Errorf("set_cup_for_coffee: open gripper: %w", err)
		}
		time.Sleep(gripperPause)

		grabStep := Step{PoseName: "empty_cup", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, grabStep); err != nil {
			return fmt.Errorf("set_cup_for_coffee: %w", err)
		}
		if _, err := s.gripper.Grab(ctx, nil); err != nil {
			return fmt.Errorf("set_cup_for_coffee: grab gripper: %w", err)
		}
		time.Sleep(gripperPause)

		retreatStep := Step{PoseName: "empty_cup_approach", Component: "coffee-claws-middle", LinearConstraint: defaultApproachConstraint, AllowedCollisions: cupGrabCollisions, Pause: shortPause}
		if err := s.executeStep(ctx, cancelCtx, retreatStep); err != nil {
			return fmt.Errorf("set_cup_for_coffee: %w", err)
		}
	}

	// Placement phase (unchanged): under-machine approach -> ready -> release -> exit -> close.
```

The existing code from line 439 onwards (`cupPlacementApproach := Step{PoseName: "cup_under_machine_approach", ...}` through the closing `return nil` at line 466) stays exactly as it is — only the leading static pickup phase is now wrapped in the `else` branch.

- [ ] **Step 2: Verify the build is clean**

Run: `go build ./...`
Expected: clean build.

- [ ] **Step 3: Verify all tests still pass**

Run: `go test ./...`
Expected: all existing + new tests pass. The static path is unchanged behaviorally; the dynamic path is exercised by helper-level tests.

- [ ] **Step 4: Commit**

```bash
git add espresso.go
git commit -m "$(cat <<'EOF'
Branch setCupForCoffee on DynamicCupPickup

When the flag is true, the pickup phase runs through pickCupDynamic
(vision-driven side-grab); otherwise the existing static pickup
(empty_cup_approach -> empty_cup) runs unchanged. The placement phase
that follows is identical in both branches.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Update `README.md`

Per CLAUDE.md "Conventions": "Config docs live in README.md."

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Locate the existing `viam:beanjamin:coffee` configuration section**

Run: `grep -n "place_cup\|coffee:" README.md | head -20`
Find the section that documents `Config` fields for the coffee service. New fields go at the end of that section.

- [ ] **Step 2: Add documentation for the new fields**

Append (preserving the existing markdown style) a subsection for dynamic cup pickup. Sample wording:

```markdown
### Dynamic cup pickup

When `dynamic_cup_pickup: true`, the arm uses a vision service to detect cups in the workspace rather than picking from the static `empty_cup` pose. Configure these additional fields:

| Field | Type | Required | Description |
|---|---|---|---|
| `dynamic_cup_pickup` | bool | no | Enables dynamic pickup. Default `false`. |
| `cup_vision_service_name` | string | when enabled | Name of a `rdk:service:vision` segmenter that returns cup detections via `GetObjectPointClouds`. |
| `src_camera_name` | string | when enabled | Source camera the vision service segments from. Must be present in the frame system. |
| `expected_cup_position_mm` | `{x, y, z}` | when enabled | World-frame heuristic point. Detection closest to this point wins. |
| `cup_max_distance_from_target_mm` | float | no | Hard cutoff. Detections beyond this distance from `expected_cup_position_mm` are dropped. Default 300mm. |
| `cup_detection_retries` | int | no | Number of additional vision calls if the first returns 0 detections. Default 0. |
| `cup_detection_retry_sleep_ms` | int | no | Sleep between retries in milliseconds. Default 250. |

In addition, three named poses must exist on the **claws pose switch** (`claws_pose_switcher_name`):

| Pose name | Type | Description |
|---|---|---|
| `cup_observe_pose` | absolute world pose | Arm moves here to observe the cup. |
| `cup_approach_relative_pose` | offset (composed onto centroid) | Pre-grab pose; same orientation as the grab, larger translation behind the cup. |
| `cup_grab_relative_pose` | offset (composed onto centroid) | Final grab pose; gripper orientation for a side-grab, small translation onto the cup. |

The `_relative_pose` entries are interpreted by the dynamic-pickup code as offsets to compose onto the runtime-detected cup centroid; their `reference_frame` field is ignored.
```

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "$(cat <<'EOF'
Document dynamic cup pickup in README

Adds operator-facing documentation for the seven new Config fields
and the three claws-switch pose-name conventions
(cup_observe_pose, cup_approach_relative_pose, cup_grab_relative_pose).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Lint, full test pass, and final verification

**Files:** none modified directly

- [ ] **Step 1: Run gofmt and golangci-lint**

Run: `make lint`
Expected: clean exit. If `gofmt -s -w .` rewrites anything, re-stage and re-commit (or amend). If `golangci-lint run` flags issues in the new files, fix them.

- [ ] **Step 2: Run the full test suite**

Run: `make test`
Expected: all tests pass, including the new ones (`TestValidate_DynamicCupPickup_*`, `TestSelectCupCentroid_*`, `TestComposeCupPose_*`, `TestCameraToWorld_*`).

- [ ] **Step 3: Run a default build to verify the binary still compiles**

Run: `make`
Expected: `bin/beanjamin` produced without errors.

- [ ] **Step 4: If lint or build required follow-up edits, commit them**

```bash
git status
# If there are leftover edits from gofmt/golangci-lint:
git add <files>
git commit -m "$(cat <<'EOF'
Lint fixes for dynamic cup pickup

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

If there's nothing to commit, this step is a no-op.

- [ ] **Step 5: Inspect the final branch state**

Run: `git log --oneline main..HEAD`
Expected: 10–11 commits on the `dynamic-cup-pickup` branch covering: gitignore (already on main), Config fields, Validate, the three pure helpers, NewCoffee wiring, observeCupCentroid, pickCupDynamic, setCupForCoffee branch, README, optional lint fixes.

---

## Manual / hardware verification (post-implementation)

These do not block the plan but should be in the PR description for the operator to run before merge:

- Configure a fake-mode machine with a vision-service stub returning one centroid. Verify `pickCupDynamic` moves to observe pose, plans approach + grab, and moves correctly.
- Configure two cups, one in-range, one outside `cup_max_distance_from_target_mm`. Verify the in-range cup is chosen and the log shows the rejected cup's distance.
- Configure with zero detections. Verify retries and the final error message. Verify `cancel` / `proceed` recovers.
- On real hardware: tune `cup_observe_pose`, `cup_approach_relative_pose`, `cup_grab_relative_pose` using `viam robot part motion get-pose` (per CLAUDE.md "Pose work" convention), then verify a real cup is picked up cleanly.

---

## Coordination points (for the PR)

- **Robot fragment owners:** add the three new claws-switch poses and wire a vision service + realsense camera into the frame system before merging.
- **Order-tracker UI:** no changes for v1 — `setStep("Placing cup")` remains the only label observed during this phase.
- **Future work tracked in the spec:** dynamic delivery pose for `giveFullCupToCustomer`, ephemeral world obstacle for the detected cup, structured error codes for the UI, multi-cup retry on grab failure, compile-time validation of `_relative_pose` naming.
