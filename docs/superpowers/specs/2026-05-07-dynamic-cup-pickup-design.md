# Dynamic Cup Pickup — Design

**Date:** 2026-05-07
**Branch:** `dynamic-cup-pickup`
**Module:** `viam:beanjamin:coffee` (Go)

## Problem

The `setCupForCoffee` step of the brew cycle currently picks up the cup from a single hardcoded pose (`empty_cup` in the claws pose switch). The cup must be placed at exactly that location every time. This design replaces the pickup phase with a vision-driven dynamic pickup: the arm observes the workspace, finds cups via a vision service, picks the closest one within a configurable bound, and executes a side-grab. The placement phase that follows (`cup_under_machine_approach` → `cup_ready_for_coffee`) is unchanged.

The full `setCupForCoffee` lifecycle keeps both code paths via a `dynamic_cup_pickup` config flag so machines without vision configured continue to work.

## Out of scope

- `giveFullCupToCustomer` is not modified. After brewing, the cup is currently returned to the static `empty_cup` pose; under dynamic pickup that pose no longer corresponds to the original cup location, but resolving where to deliver the cup is a separate decision left for a follow-up.
- No retry across multiple detected cups on grab failure. The closest-in-range cup is tried once; failure aborts the order.
- No structured error codes for the order-tracker UI to differentiate cup-detection vs. generic step failures.
- No ephemeral world obstacle for the detected cup during planning. The planner is unaware of the dynamic cup geometry; the side-grab approach is geometric only.

## Architecture

```
beanjamin (top-level Go package)
├── module.go        — Config: 7 new fields + Vec3Mm helper; deps wiring
├── espresso.go      — setCupForCoffee branches on DynamicCupPickup
├── motion.go        — unchanged (moveToRawPose is the seam)
├── cup_pickup.go    — NEW: pickCupDynamic + 3 pure helpers
└── cup_pickup_test.go — NEW: unit tests for the pure helpers
```

**Why a new file rather than expanding `espresso.go`:** `espresso.go` is already ~675 lines doing brew-cycle orchestration. The new dynamic-pickup logic introduces a different concern (vision integration + frame transforms), pure helpers worth isolating for tests, and is the natural sibling to `cam_storage.go` / `greetings.go` / `maintenance_sensor.go` — files that each back one capability used by the orchestrator.

**Why bypass the `Step` abstraction for dynamic poses:** `Step` represents *configured, named* poses resolved through the pose switch. Dynamic poses computed at runtime have different semantics. Conflating them would force every reader of every `Step` construction in espresso.go to ask "is this raw or named?". `moveToRawPose` already takes raw `*poseData` — it's the natural seam, one layer below `executeStep`/`fetchPose`.

**Why store the relative poses in the claws pose switch (not in `Config`):** all configurable poses in this module already live in pose switches. Operators tune them in one place using the same fragment-var pattern. The pose-switch's `PoseConf.PoseValue` (a `commonpb.Pose`) has the right 6-DoF shape for a relative offset. We exploit naming convention: pose names with the `_relative_pose` suffix are interpreted by code as relative offsets to compose onto a runtime point, rather than as absolute world poses to plan toward. Trade-off: no compile-time signal that a pose is relative; same weak typing as the rest of this module's pose conventions.

### Component layout

| Symbol | File | Role |
|---|---|---|
| `pickCupDynamic(ctx, cancelCtx) error` | `cup_pickup.go` | Orchestrates: observe → detect → compose → approach → grab → retreat. Called from `setCupForCoffee` when the flag is on. |
| `observeCupCentroid(ctx) (r3.Vector, error)` | `cup_pickup.go` | Calls vision, retries on empty result, lifts each centroid into world frame, calls `selectCupCentroid`. |
| `selectCupCentroid(centroids []r3.Vector, target r3.Vector, maxDistMm float64) (r3.Vector, int, error)` | `cup_pickup.go` | Pure. Returns closest centroid to `target` within `maxDistMm`; returns its original index for log correlation. |
| `composeCupPose(centroidWorld r3.Vector, relative spatialmath.Pose) spatialmath.Pose` | `cup_pickup.go` | Pure. Wraps `spatialmath.Compose(NewPose(centroidWorld, ZeroOrientation), relative)`. |
| `cameraToWorld(fs, fsInputs, cameraFrame, point) (r3.Vector, error)` | `cup_pickup.go` | Lifts a camera-frame point into world via `fs.Transform`. |
| `vision  vision.Service` field on `beanjaminCoffee` | `module.go` | Optional dep, resolved when `DynamicCupPickup=true`. |
| `srcCameraFrameName string` cached on `beanjaminCoffee` | `module.go` | Validated at init: must exist in `cachedFS`. |

## Configuration

Added to `Config` in `module.go`:

```go
DynamicCupPickup bool `json:"dynamic_cup_pickup,omitempty"`

CupVisionServiceName string  `json:"cup_vision_service_name,omitempty"`
SrcCameraName        string  `json:"src_camera_name,omitempty"`

// World-frame heuristic point. Closest detection wins; ties broken by detection order.
ExpectedCupPositionMm *Vec3Mm `json:"expected_cup_position_mm,omitempty"`

// Hard cutoff. Detections beyond this distance from ExpectedCupPositionMm
// are dropped. Default 300mm.
CupMaxDistanceFromTargetMm float64 `json:"cup_max_distance_from_target_mm,omitempty"`

// Retry behavior when the vision service returns 0 cups.
CupDetectionRetries      int `json:"cup_detection_retries,omitempty"`        // default 0
CupDetectionRetrySleepMs int `json:"cup_detection_retry_sleep_ms,omitempty"` // default 250
```

Helper type, defined in `module.go`:

```go
type Vec3Mm struct {
    X float64 `json:"x"`
    Y float64 `json:"y"`
    Z float64 `json:"z"`
}
```

Three named poses live in the **claws pose switch** by convention. Component name is `coffee-claws-middle`.

| Pose name | Interpretation | Reference frame |
|---|---|---|
| `cup_observe_pose` | Absolute world pose. Arm moves here to observe. | world |
| `cup_approach_relative_pose` | Relative offset from cup centroid for approach. Translation + side-grab orientation. | (ignored by code) |
| `cup_grab_relative_pose` | Relative offset from cup centroid for grab. Translation + side-grab orientation. | (ignored by code) |

### Validation (`Config.Validate`)

When `DynamicCupPickup=true`:
- Require `CupVisionServiceName`, `SrcCameraName`, `ExpectedCupPositionMm`.
- Append `vision.Named(CupVisionServiceName).String()` and `camera.Named(SrcCameraName).String()` to required deps.
- If `CupMaxDistanceFromTargetMm == 0`, default it to `300`.
- If `CupDetectionRetries < 0`, return an error.

When `DynamicCupPickup=false`, none of the cup_* fields are required and any present values are ignored.

### Runtime validation (in `NewCoffee`)

When `DynamicCupPickup=true`, after building `cachedFS`:
- Verify `cachedFS.Frame(SrcCameraName) != nil`. Fail the constructor with `"src_camera_name %q not found in frame system — add the camera to the frame system fragment"` if missing.
- Resolve the vision service and camera deps from `deps`. Failure to resolve fails the constructor (same shape as the existing `OrderSensorName` / `CamStorageMuxName` resolution at module.go:236–278).

The three pose-switch poses are not validated at startup; they are looked up at first call. Failure surfaces as `"get pose %q: %w"` from `fetchPose` (motion.go:73), which propagates up out of `prepareDrink` as a normal step error.

## Data flow

When `setCupForCoffee` is called with `DynamicCupPickup=true`:

```
[Brew cycle: prepareDrink, step 5/9 "Placing cup"]
└─> setCupForCoffee(ctx, cancelCtx)
      │
      if !s.cfg.DynamicCupPickup:
        └─> existing static pickup (empty_cup_approach → empty_cup → grab → retreat)
      else:
        └─> pickCupDynamic(ctx, cancelCtx)
            │
            ├─ 1. executeStep("cup_observe_pose", coffee-claws-middle, shortPause)
            │
            ├─ 2. observeCupCentroid(ctx) -> r3.Vector
            │      ├─ for attempt := 0..CupDetectionRetries:
            │      │    objects, _ := vision.GetObjectPointClouds(ctx, SrcCameraName, nil)
            │      │    if len(objects) > 0: break
            │      │    sleep(CupDetectionRetrySleepMs)
            │      ├─ if still empty: return error
            │      ├─ TRANSFORM #1 (camera → world):
            │      │    for each obj:
            │      │      centroidLocal := obj.Geometry.Pose().Point()
            │      │      centroidWorld := cameraToWorld(cachedFS, fsInputs,
            │      │                                     SrcCameraName, centroidLocal)
            │      ├─ selectCupCentroid(worldCentroids,
            │      │                    ExpectedCupPositionMm,
            │      │                    CupMaxDistanceFromTargetMm)
            │      │    → closest in-range centroid, or "no cup within range" error
            │      └─ return centroidWorld
            │
            ├─ 3. fetch the two relative poses:
            │      approachRel := s.fetchPose(ctx, "coffee-claws-middle",
            │                                 "cup_approach_relative_pose")
            │      grabRel     := s.fetchPose(ctx, "coffee-claws-middle",
            │                                 "cup_grab_relative_pose")
            │
            ├─ 4. compose into world-frame *poseData:
            │      centroidPose := spatialmath.NewPose(centroidWorld,
            │                                          spatialmath.NewZeroOrientation())
            │      approachPose := spatialmath.Compose(centroidPose, approachRel.pose)
            │      grabPose     := spatialmath.Compose(centroidPose, grabRel.pose)
            │      approachPD := &poseData{pose: approachPose, refFrame: "world",
            │                              componentName: "coffee-claws-middle"}
            │      grabPD     := &poseData{pose: grabPose,     refFrame: "world",
            │                              componentName: "coffee-claws-middle"}
            │
            ├─ 5. moveToRawPose(ctx, approachPD, nil, nil, nil)
            │
            ├─ 6. gripper.Open(ctx, nil); sleep(gripperPause)
            │
            ├─ 7. moveToRawPose(ctx, grabPD, defaultApproachConstraint, nil, nil)
            │
            ├─ 8. gripper.Grab(ctx, nil); sleep(gripperPause)
            │
            └─ 9. moveToRawPose(ctx, approachPD, defaultApproachConstraint, nil, nil)

[after pickCupDynamic returns, setCupForCoffee continues unchanged:]
    ├─ executeStep("cup_under_machine_approach", ...)
    └─ executeStep("cup_ready_for_coffee", ...) → release → exit → close gripper
```

### Frame transforms

There are two transforms in this flow, intentionally split:

1. **Camera → world** for the centroid (in `cameraToWorld`). Vision returns object geometry in the camera's local frame. We lift to world ourselves before composing.
2. **World → world** for the goal (inside `moveToRawPose`). Identity transform for `refFrame: "world"`, but goes through the same code path as every other goal — no new planner code.

### Cancellation

Every call to `executeStep`, `moveToRawPose`, `vision.GetObjectPointClouds`, and `gripper.Open` / `gripper.Grab` already takes `ctx`. The retry-sleep loop in `observeCupCentroid` honors `ctx.Done()` and `cancelCtx.Done()` via a `select` with `time.After`. Cancellation behavior matches every other step in the brew cycle — no new wiring.

### Tracing

Wrap `pickCupDynamic` in `trace.StartSpan(ctx, "beanjamin::dynamic_cup_pickup")` and the vision call in a child span `beanjamin::dynamic_cup_pickup::detect`, mirroring the existing pattern at espresso.go:127, 146, 157.

### UI / step labels

`setStep("Placing cup")` is set once before this whole flow. The dynamic pickup phase does not introduce finer-grained step labels — the order-tracker UI sees the same "Placing cup" it sees today. This keeps the UI stable.

## Error handling

Three error surfaces. Behavior matches the rest of the brew cycle: fail fast, surface a clear error, let the operator decide via `cancel` / `proceed`.

### Configuration / startup errors (non-recoverable)

- Missing required dep when `DynamicCupPickup=true` → `Validate` returns `NewConfigValidationFieldRequiredError` for the missing field.
- Vision/camera dep not in `deps` map at construction → `NewCoffee` returns `fmt.Errorf("cup vision service %q: %w", ...)` (or analogous for camera).
- `cachedFS.Frame(SrcCameraName) == nil` at startup → `NewCoffee` fails with the camera-not-in-frame-system error described above.

### Detection errors (per-order)

- `vision.GetObjectPointClouds` transport error → `fmt.Errorf("dynamic_cup_pickup: detect: %w", err)`.
- 0 objects after `CupDetectionRetries+1` attempts → `fmt.Errorf("dynamic_cup_pickup: no cups detected after %d attempts", retries+1)`.
- All detections beyond `CupMaxDistanceFromTargetMm` → `fmt.Errorf("dynamic_cup_pickup: %d detection(s) found but none within %.0fmm of expected position", n, maxDist)`. Distinct from the zero-detection error so operators can triage camera vs. cup-placement issues separately.
- `cameraToWorld` transform fails → wrap and return.

### Motion errors (per-order)

- `moveToRawPose` for approach / grab / retreat → `fmt.Errorf("dynamic_cup_pickup: <phase>: %w", err)` where `<phase>` is `approach`, `grab`, or `retreat`.
- `gripper.Open` / `gripper.Grab` → `fmt.Errorf("dynamic_cup_pickup: <op> gripper: %w", err)`.

All errors propagate up out of `prepareDrink`, the order is marked failed, and the cycle's existing cancel handling applies. **No silent fallback** to the static pickup if dynamic fails — that would mask vision regressions.

### Logging

- `Infof` per-attempt detection counts: `"dynamic cup pickup: attempt 1/3, found 2 detections"`.
- `Infof` chosen centroid in world coords: `"dynamic cup pickup: chose centroid (x=120.4, y=-30.1, z=85.2) — 12.3mm from target"`.
- `Debugf` all detection centroids and their distances. Useful during commissioning, off in production by default.

## Testing

Three tiers, matching the project's existing test style (`circular_test.go`, `pivot_test.go`, `queue_test.go`, `joints_test.go` are all pure-function unit tests at the package root).

### 1. Pure-function unit tests in `cup_pickup_test.go`

The three pure helpers carry the testable math.

**`selectCupCentroid`**
- empty input → error
- single in-range → returned with index 0
- single out-of-range → "no cup within range" error
- multiple, closest within range → closest returned with correct index
- all out of range → error
- `maxDistMm == 0` interpreted as "no cutoff" → closest always wins
- tie-breaking: equal distances → first wins (deterministic)

**`composeCupPose`**
- identity relative → returns centroid pose with zero orientation
- pure-translation relative → adds offset, no rotation
- pure-rotation relative → keeps centroid translation, applies orientation
- (commented assertion that we always pass zero centroid orientation; non-zero centroid orientation is not a tested case)

**`cameraToWorld`**
- camera frame at world origin, identity orientation → point unchanged
- camera frame translated → point translated correspondingly
- camera frame missing from frame system → error
- Tests build a hand-rolled mini frame system inline, the same way `joints_test.go` does.

### 2. Validation tests in `module_test.go` (new file)

There is no existing `module_test.go` in the top-level package. This work adds one and covers only the new fields; covering existing fields is scope creep tracked as a follow-up.

Cases:
- `DynamicCupPickup=true`, missing `CupVisionServiceName` → required-field error.
- `DynamicCupPickup=true`, missing `SrcCameraName` → required-field error.
- `DynamicCupPickup=true`, missing `ExpectedCupPositionMm` → required-field error.
- `DynamicCupPickup=true`, `CupMaxDistanceFromTargetMm == 0` → defaults to 300 after `Validate`.
- `DynamicCupPickup=true`, `CupDetectionRetries == -1` → error.
- `DynamicCupPickup=false`, all cup_* fields empty → no error.

### 3. Integration tests — explicitly NOT in v1

The full `pickCupDynamic` flow needs a vision service, a camera, a frame system, and an arm. The other "integration"-shaped methods in this codebase (`setCupForCoffee`, `lockPortaFilter`) have no Go-side integration tests either — they're verified on real hardware and via `make module.tar.gz` deploy. Same standard.

### Manual / hardware verification (PR description checklist)

- Configure a fake-mode machine with a vision service stub returning one centroid. Verify pickCupDynamic moves to observe pose, plans approach + grab, moves correctly.
- Configure two cups, one in-range, one outside `CupMaxDistanceFromTargetMm`. Verify the in-range cup is chosen; log shows the rejected cup's distance.
- Configure with zero detections. Verify retries + final error. Verify `cancel` / `proceed` recovers.
- On real hardware: tune `cup_observe_pose`, `cup_approach_relative_pose`, `cup_grab_relative_pose` using `viam robot part motion get-pose` (per CLAUDE.md "Pose work" convention), then verify a real cup is picked up cleanly.

### Lint

`make lint` (`gofmt -s -w .` + `golangci-lint run`) per CLAUDE.md — non-negotiable before commit.

## Coordination points

- **Robot fragment owners.** This change requires three new poses in the claws pose switch (`cup_observe_pose`, `cup_approach_relative_pose`, `cup_grab_relative_pose`) and a vision service + realsense camera wired into the frame system. Whoever maintains the fragment for the target machine needs to coordinate before this rolls out.
- **README.md.** Add a section describing the new `Config` fields and the pose-name convention. Required by the project's "Config docs live in README.md" rule (CLAUDE.md "Conventions").
- **Order-tracker UI / web app.** No changes required for v1 — `setStep("Placing cup")` is unchanged. Future work to expose distinct dynamic-pickup error states in the UI is a separate ticket.

## Future work

- Replace `giveFullCupToCustomer`'s static `empty_cup` return pose with a corresponding dynamic delivery pose. Out of scope here.
- Insert an ephemeral world obstacle for the detected cup so the planner is aware of cup geometry during the grab.
- Try the next-closest detection on grab failure (currently aborts after first failure).
- Surface structured error codes so the order-tracker UI can differentiate detection vs. motion failures.
- Compile-time / config-time validation that pose names with the `_relative_pose` suffix are only used by code that composes them with a runtime point.
