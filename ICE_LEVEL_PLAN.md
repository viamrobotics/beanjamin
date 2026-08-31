# Visual ice-level confirmation — handoff

Ice is dispensed into a glass held by the arm, open-loop: `pulseIcePin`
(`coffee/iced.go:271`) drives a GPIO pin HIGH, sleeps `ice_dispense_sec`
(default 5 s), drives it LOW. A full hopper and a nearly-empty one produce very
different drinks. Watch the glass instead, and stop when it is actually full.

**Status:** measurement solved and validated on hardware. Three cheap
measurements left, one of which can still change the design. No production code
written; `cmd/cli` is throwaway instrumentation, not part of the module.

---

## Orientation

`serveIced` (`coffee/iced.go:26`) → `prepIcedGlass` (fetch glass, ice it, stage
it) → `finishIced` (retrieve espresso, pour, serve). `prepIcedGlass` calls
`dispenseIce` (`coffee/iced.go:249`), which moves to the `ice_machine_dispense`
claws pose, calls `pulseIcePin`, retreats.

At that pose the arm holds a clear glass under the ice chute. Behind it is the
**ice machine** — a dark unit, which is what makes ice visible against it. The
espresso machine is a separate unit off to the left, outside the measurement
region; a pour changes nothing the camera sees.

The camera (`src_camera_name`, frame `cam`) is arm-mounted and already a required
dependency. Camera and gripper both ride the arm, so the glass sits at a **fixed**
208 mm from the camera regardless of arm position. That drives most of what
follows.

Existing manual actions via `DoCommand{"execute_action": ...}`: `pulse_ice_pin`
(pin only) and `dispense_ice` (approach, pulse, retreat; bumps the
`ice_dispenses` counter). `pulseIcePin` reads its duration from config every
call, so a different duration cannot be requested over `DoCommand`.

---

## What was established

50 captures on `cappuccina-main`, 2026-08-28, in `icedata/` (~100 MB, untracked).

**Depth does not work here — do not retry it.** The camera is 208 mm from the
grip point; its closest return is ~250 mm, so the held glass is inside the
sensor's blind zone. Confirmed with an opaque foil control, which returned
nothing either — that is what proves it is geometry, not transparent ice. Camera
and gripper are rigidly coupled, so no arm motion changes the distance.

**RGB works.** Ice is bright, the empty glass shows the dark ice machine through
it, so the surface is a strong horizontal brightness edge. Scanning row
brightness down a column finds it: R² 0.994, mean abs error 0.3%, single-frame
noise ~3.5% of glass height, 7.6 ms per measurement (6.3 decode + 1.3 scan). No
model — stdlib `image/jpeg` and arithmetic.

**Measure the contrast step, not absolute brightness.** A fixed cutoff validated
the approach but must not ship: fitted under one afternoon's light, brighter
ambient can lift an *empty* glass past it and report "full". The step is
illumination-insensitive, slightly more accurate (R² 0.9938 vs 0.9895), and fails
toward "no ice seen", which the loop handles.

**The window matters more than the threshold.** The surface is a gradual edge
spanning tens of rows, so the step is measured over a comparable span — mean of N
rows below a candidate row minus N above:

| window | empty max step | ice min step | gap | full px |
|---|---|---|---|---|
| 8 | 19 | 22 | **3** | 424 |
| 32 | 17 | 42 | 24 | 429 |
| **48** | **10** | **49** | **39** | **426** |
| 56 | 9 | 53 | 44 | 408 — clipped |

Past ~48 the window clips the scan range and rescales the calibration, and **R²
stays at 0.994 through that** — the fit is still linear, just wrong. Guard the
slope separately. Averaging over the window is also what stops glare, a single
bright row, from presenting as a step.

**Blind below 40% fill.** The ice machine's ledge occludes the glass base. Do not
take that cutoff from the fit — it matches every level that registers and is
wrong exactly where it matters:

| level | measured | fit predicts |
|---|---|---|
| 30% | **0 px** | 34 px |
| 40% | 78 | 77 |
| 80% | 243 | 247 |
| 100% | 335 | 332 |

At 30% the fit predicts a detectable 34 px and the camera returns nothing, five
frames of five. Below 40%, zero means "cannot tell", not "empty" — a target under
the floor never confirms and rides to the ceiling on every drink.

**Survives a brightness change.** Scaling luminance 0.5×–1.8× (clipped at 255):
fill readings barely move, 236–242 px at 80%. Step magnitudes shrink at *both*
ends — dim light gives less contrast, bright light also does because saturated
ice clips. Worst false positive 22, weakest true positive 29, so
`ice_min_contrast = 25`. (30 would read a 40%-full glass as empty at half
brightness.)

**One 60% target serves both drinks**, so no per-drink targets. 20 points of
headroom over the floor, ~6× noise. It sits in the widest calibration gap
(levels are 0/30/40/80/100), but a leave-one-out check costs only 1.5% of glass
height, so interpolating there is cheap.

---

## Tooling

`go run ./cmd/cli <cmd>`, with `--address` plus `VIAM_API_KEY` /
`VIAM_API_KEY_ID` in the environment.

| command | purpose |
|---|---|
| `ice-snapshot` | Capture at the arm's current pose. `--raw-dir` saves clouds, all camera images, poses, joints, and a `session.json` with frame system and intrinsics. `--truth` records the level you poured. |
| `ice-level` | Measure from a capture dir's color images. `--contrast` derives the parameters, `--series` traces an `ice-dispense` run, `--dump-profile` prints raw row brightness for checking the ROI. |
| `ice-dispense` | Drive the pin *and* capture frames stamped against pin-open. Closes the pin on every exit path including Ctrl-C. |
| `ice-analyze`, `ice-fit` | Depth path. Historical only. |

`--raw-dir` exists because the machine is shared: it records everything and
defers every analysis choice to offline commands you can re-run freely. Don't
spend machine time tuning analysis parameters.

The ROI defaults (x 700–910, y 280–670) are fitted to *this* dispense pose. If
the pose is re-taught they are wrong, and the symptom is a flat or zero reading
rather than an error — `--dump-profile` is how you check.

Reproduce the numbers above from data already on disk:

```bash
go run ./cmd/cli ice-level --raw-dir icedata --contrast
```

---

## What is left

### G8 — does 60% ice leave room for milk and espresso?

No machine, no camera. Start here.

Fill ice to 60%, then (a) add a shot — iced coffee; (b) add the real
`milk_pour_dwell` of milk plus a shot — latte. Pass if both sit below ~90% of the
glass, checking during the pour and on the carry to the serving area. If the
latte overflows, lower the shared target and re-check it clears the 0.40 floor;
only if it cannot does a per-drink target become necessary.

### G6 — does falling ice corrupt the reading?

**The only remaining measurement that can change the design.** If ice in flight
biases the reading, no `ice_dispense_min_sec` fixes it — that guards only the
opening moments — and the loop becomes pulse → close pin → measure → repeat.

Glass in the jaws, parked at `ice_machine_dispense`:

```bash
go run ./cmd/cli ice-dispense --address $M --board <ice_board_name> --pin <ice_pin_name> \
  --pre 3 --dwell 25 --post 15 --raw-dir dispense1

go run ./cmd/cli ice-level --raw-dir dispense1 --series \
  --target-fill 0.6 --full-px 426 --intercept-px -94
```

The command drives the pin itself because a separate process cannot be
time-aligned to pin-open, which is the whole point. It compares the last 3 s of
dwell against the last 3 s of post-roll and prints a verdict. **Don't trim the
post-roll** — the settled reading is half the comparison.

Under 5% of glass height: poll continuously, set `ice_dispense_min_sec` just past
the reported first-ice time, ship as planned. Over 5%: decide from how bad the
bias is. Pulse-pause-check is materially more work and slower per drink; staying
open-loop with the measurement as a diagnostic is the alternative. `ice-dispense`
drives the pin directly, so prototyping burst-and-pause is cheap.

### G10b — real lighting, not synthetic gain (~5 min, same trip)

The brightness sweep was synthetic; it cannot reproduce moving specular
highlights, colour temperature, or a shadow crossing the glass — those change the
profile's *shape*, not its scale.

Capture 0% / 45% / 60%, three frames each, under normal light; then kill the room
lights or add a lamp and repeat into the same dir, then
`ice-level --raw-dir lighting --contrast`. Pass if 0% still reads zero, the others
still register, and steps land on the same side of 25. 45% is nearest the floor
where margin is thinnest; 60% is the target and the only level inside the
calibration gap, so it also retires the interpolation assumption.

### Reaching the dispense pose

A standalone move to `ice_machine_dispense` fails planning on the phantom
portafilter. Use `lock_portafilter` + `release_filter` (reparents `filter` to
world rather than deleting it), or disable the filter frame in machine config by
hand, which is what has been done so far.

A `detach_filter` action that drops the subtree is **parked** — motion goals are
keyed by frame name (`coffee/motion.go:610`), so removing `filter` stops every
`filterSw` step from planning, and the keepalive purge would fail on a timer
until someone ran `reset_world`. Not part of this work.

---

## Implementation

**`ice_level.go`** — `glassFillFraction(ctx) (float64, error)` fetches with
`s.srcCamera.Images(ctx, []string{"color"}, nil)`; filtering to `"color"` keeps
the 1.8 MB depth payload off the wire, which is a latency decision. It delegates
to a pure `iceFillFromImage(img image.Image) float64` so the measurement
unit-tests against saved fixtures.

Scan row brightness in the ROI; the largest step is the surface. Below
`ice_min_contrast`, **return fraction 0 directly** — do not run zero pixels
through the linear conversion, which with a -94 px intercept yields 0.22, a
fifth-full reading for a glass the camera cannot see into. Otherwise convert
pixels above the ROI bottom with the fitted constants, clamped to [0,1]. In-process, not a vision service — no model
is needed and a CNN behind a DoCommand would not reliably fit the check interval.

```go
const (
	defaultIceFillTargetFraction = 0.6  // one target, both drinks
	defaultIceMinFillFraction    = 0.40 // measured floor, NOT the fit's 22%
	defaultIceContrastWindow     = 48
	defaultIceMinContrast        = 25.0 // survives the brightness sweep
	defaultIceFullGlassPx        = 426.0
	defaultIceInterceptPx        = -94.0
	defaultIceDispenseMaxSec     = 30.0 // a ceiling, not a measurement
	defaultIceDispenseMinSec     = 2.0  // set from G6
	defaultIceCheckIntervalSec   = 0.5
	iceConfirmations             = 2 // single-frame noise is ~3.5%
	iceVisionMaxErrors           = 3
)
```

**The loop** — `dwellUntilFull(ctx, cancelCtx, target, measure)` takes `measure`
as a parameter so it tests without a camera. Wait `ice_dispense_min_sec`, tick at
`ice_check_interval_sec` to a deadline of `ice_dispense_max_sec`. Readings at or
above target increment a run counter, below resets it, `iceConfirmations`
consecutive confirms — ice settles between frames, so one frame is not a
measurement. After `iceVisionMaxErrors` consecutive failures, warn and fall back
to a fixed dwell; vision being down must not cost a drink. On the deadline: warn,
`s.say(...)`, bump `ice_dispense_timeouts`, **return nil** — the espresso is
already brewed, a light glass beats a discarded drink, and a rising counter is how
an empty hopper should surface. **G6 decides whether this structure survives.**

**Config** — the constants above as `ice_*` fields plus `ice_vision_enabled` and
four `ice_roi_*` bounds, all optional, inside the existing `if cfg.CanServeIced`
block so nothing changes for machines that don't set them. Validate `x0 < x1`,
`y0 < y1`, non-negative, `ice_full_glass_px > 0`, `ice_min_contrast > 0`,
`2*window < y1-y0`, and target above `ice_min_fill_fraction`.

Two of those guard *silent* failures, which is why they're worth writing: at
`ice_min_contrast = 0` an empty glass reports a surface near the rim, i.e. nearly
full; a transposed ROI pair scans nothing, reads zero, and rides to the ceiling.
Both look like working config.

The ROI is pixel coordinates, valid only at `ice_machine_dispense` with a glass
gripped. That's fine — it's the only pose it runs at, and the pose is authored on
the claws switcher rather than drifting. Deriving it by projection would work but
solves a problem this machine doesn't have.

**Wiring** — `module.go` needs `srcCamera camera.Camera` on the struct; the
camera is already a required dependency, the service just never kept the handle,
only `cupCameraName` (`coffee/module.go:108`). `pulseIcePin` keeps its `stop()`
closure and its `context.Background()` write verbatim — that invariant is what
the whole feature hangs off — and gains a **named-return `defer`** that replaces
today's four explicit `stop()` calls. The loop has more exit paths than the
current code, and "left the ice machine running" is the one bug class that
matters. No other signatures change: one target means nothing needs to know which
drink is being made.

**`check_ice_level`** — registered conditionally in `executeAction`
(`coffee/espresso.go:318`) like the brew-button actions. Logs the fill against
the target and nothing more. This is the ROI/pose agreement check: run it on a
gripped glass after any recalibration of that pose.

**Tests** — loop tests with a stub `measure` and an `inject.Board` recording pin
`Set` calls (construction pattern in `cam_storage_test.go`, injection in
`control_test.go`): confirms, does-not-confirm on one high then one low, ceiling,
cancel mid-poll, error fallback, vision disabled. Measurement tests against
fixtures copied from `icedata/` — and the one that pins the whole design: **a
fixture scaled brighter still reads 0** rather than confirming full.

**Docs** — new config fields, `ice_dispense_timeouts` in the usage-counter lists,
`check_ice_level` in the manual-step list. Put the ROI on the
**`ice_machine_dispense` row of the pose table**, not only in the ice section:
whoever re-teaches that pose won't be reading the ice fields and is exactly who
needs to know. State plainly that a target below `ice_min_fill_fraction` never
confirms.

---

## Sequencing

G8 → unblock the pose → G6 → G10b → config and wiring and measurement → the loop
(shape decided by G6) and tests → docs, recording measured values.

## Decisions already made

- **Contrast step, not a brightness threshold.** A threshold fails toward "full"
  on an empty glass; the step fails toward "no ice seen". Don't simplify back.
- **Timeout serves a light glass** and bumps a counter — doesn't fail the order
  or pause the queue.
- **One 60% target for both drinks.** An earlier design threaded `withMilk` down
  to `dispenseIce`; its absence is what keeps two targets from drifting apart.
- **`check_ice_level` logs only.** No response contract, no web-app work.
- **ROI configured, not derived.**
- **`ice_min_fill_fraction` is 0.40**, from what registered — not the fit's 22%.
- **`detach_filter` parked.**

## Traps

1. **Don't fit the blind floor.** The fit is confidently wrong through the
   occlusion boundary — 22% versus a real 40%.
2. **R² doesn't catch a rescaled calibration.** A too-wide window shifts
   full-glass px 5% while R² holds at 0.994.
3. **An 8-row contrast window looks reasonable and doesn't work.** Gap of 3.
4. **`icedata/` is ~100 MB and untracked.** Copy a few frames to `testdata/`;
   don't commit the directory.
5. **"In frame" is not "measurable."** That conflation sent the first round down
   the depth path. Verify per modality, and check camera-to-glass distance
   against the sensor's minimum range.
