# Visual ice-level confirmation — handoff

Ice is dispensed into a glass held by the arm, open-loop: `pulseIcePin`
(`coffee/iced.go:286`) drives a GPIO pin HIGH, sleeps `ice_dispense_sec`
(default 5 s), drives it LOW. A full hopper and a nearly-empty one produce very
different drinks. Watch the glass instead, and stop when it is actually full.

**Status:** the measurement is settled and validated offline against two capture
sets. Two numbers are missing, both cheap: how much the glass's seating height
varies between grabs (G9, ~5 min on the machine) and which row to stop at (G8,
bench work). No production code written; `cmd/cli` is instrumentation, not part
of the module.

---

## Orientation

`serveIced` (`coffee/iced.go:26`) → `prepIcedGlass` (fetch glass, ice it, stage
it) → `finishIced` (retrieve espresso, pour, serve). `prepIcedGlass` calls
`dispenseIce` (`coffee/iced.go:269`), which moves to the `ice_machine_dispense`
claws pose, calls `pulseIcePin`, retreats.

At that pose the arm holds a glass under the ice chute, against the dark ice
machine behind it — which is what makes ice visible. The espresso machine is a
separate unit off to the left, outside the measurement region; a pour changes
nothing the camera sees.

The camera (`src_camera_name`, frame `cam`) is arm-mounted and already a required
dependency. Camera and gripper both ride the arm, so the glass sits at a **fixed**
208 mm from the camera regardless of arm position. That drives most of what
follows — including why an image row is a meaningful thing to configure.

Existing manual actions via `DoCommand{"execute_action": ...}`: `pulse_ice_pin`
(pin only) and `dispense_ice` (approach, pulse, retreat; bumps the
`ice_dispenses` counter). `pulseIcePin` reads its duration from config every
call, so a different duration cannot be requested over `DoCommand`.

---

## What was established

Two capture sets, both untracked: `icedata/` (50 captures, `cappuccina-main`,
2026-08-28) and `dispense1/` (86 frames, 2026-09-17). Five frames from the first
are committed as `coffee/testdata/icelevel/`.

**Depth does not work here — do not retry it.** The glass sits 208 mm from the
camera and the sensor's closest return is ~250 mm, so it is inside the blind
zone. An opaque foil control returned nothing either, which is what proves it is
geometry rather than transparent ice — and the rigid coupling means no arm motion
changes the distance.

**RGB works.** Ice is bright against the dark machine, so its surface is a
horizontal brightness edge. Scanning row brightness down a column finds it in
7.6 ms (6.3 decode + 1.3 scan), with stdlib `image/jpeg` and arithmetic. No
model.

**Measure the contrast step, not absolute brightness.** A fixed cutoff is fitted
to one afternoon's light, and brighter ambient lifts an *empty* glass past it and
reports "full".

**The window matters more than the threshold.** The surface is a gradual edge
spanning tens of rows, so the step is measured over a comparable span — mean of N
rows below a candidate row minus N above. At N=8 the gap between an empty glass's
worst false step and ice's weakest true step is **3**; at N=48 it is **39**.
Use 48. Averaging over the window is also what stops glare, a single bright row,
from presenting as a step.

**`ice_min_contrast = 25`, from a synthetic 0.5×–1.8× luminance sweep.** Step
magnitudes shrink at *both* ends — dim light gives less contrast, bright light
also does because saturated ice clips. Worst false positive 22, weakest true
positive 29. (30 would read a 40%-full glass as empty at half brightness.)

**The ledge hides the bottom of the glass.** At 30% fill the camera returns
nothing, five frames of five. This bounded the old design and no longer matters
— see G0 — but it is why a low level cannot be confirmed at all.

**Ground truth for the stop row.** Hand-poured levels, at the seating those
captures were taken at (rim row 277):

| fill | surface row |
|---|---|
| 40% | 592 |
| 80% | 434 |
| 100% | 335 |

Linear through that range: `row ≈ 763 - 428 × fill`.

---

## Seating is the whole problem

`dispense1/` was meant to answer G6 and instead measured nothing: the pre-roll
sat dead flat at 291 px ±1 for three seconds *before the pin opened*, on an empty
glass, and an overflowing glass 25 s later read 317 px. Twenty-six pixels of
range across the entire fill.

The scan had locked onto the **glass rim**. Above the rim is dark ice-machine
interior; below it is the bright glass wall, and that step wins every frame.

The arm was not at fault — `grip_point` matches the calibration run to 0.2 mm —
and neither is the glass a different one: its width is 320 px in the fixtures and
330 px in `dispense1`, so it is the same glass at the same distance. What moved
is purely how high it hangs in the jaws:

| | rim row |
|---|---|
| all five `coffee/testdata/icelevel/fill_*.jpg` | 277 |
| `dispense1`, empty glass | 379 |

`fetch_glass` grabs at the detected geometry's centroid Z (`graspZFromGeom`,
`coffee/cup_pickup.go:519`), so how far down the glass the jaws close is whatever
segmentation returned that attempt. 102 px at 208 mm and fy 905 is **~23 mm**,
and nothing authored bounds it.

The old ROI survived only because `y0 = 280` sat **3 px** below the rim at 277.
That margin, not the contrast step, is what kept the rim out of the scan. All
five fixtures agree on 277 to the pixel — the glass was placed once and levels
poured into it — so that set contains zero seating variance and could never have
exposed this.

**The failure ran toward "full".** A rim-locked 291 px read ~88%, which would
clear any sane target on the first frame and serve a drink with a second of ice.

---

## Tooling

`go run ./cmd/cli <cmd>`, with `--address` plus `VIAM_API_KEY` /
`VIAM_API_KEY_ID` in the environment.

| command | purpose |
|---|---|
| `ice-snapshot` | Capture at the arm's current pose. `--raw-dir` saves all camera images, poses, joints, and a `session.json` with frame system and intrinsics; `--cloud` adds point clouds, which nothing needs any more. `--truth` records the level you poured. |
| `ice-level` | Measure from a capture dir's color images. `--y0` sets the band top, `--series` traces an `ice-dispense` run, `--dump-profile` prints raw row brightness, `--contrast` derives parameters from labelled levels. |
| `ice-dispense` | Drive the pin *and* capture frames stamped against pin-open. Closes the pin on every exit path including Ctrl-C. |

`--raw-dir` exists because the machine is shared: it records everything and
defers every analysis choice to offline commands you can re-run freely. Don't
spend machine time tuning analysis parameters.

`--series` refuses to print a falling-ice verdict when the pre-roll reads
non-zero. An empty glass reading zero before the pin opens is the control for
every run, and the run that skipped it is the one that wasted a trip.

---

## What is left

### G0 — stop row, not fill fraction — **done, validated offline**

Drop the fill fraction and the calibration behind it. The camera is bolted to the
gripper, so an image row *is* a fixed height above the jaws: pick the row the ice
surface has to reach and stop there. One number replaces the whole fit.

The rim is kept out by where the band starts, and the band starts at the stop
row: scan `ice_stop_row_px - ice_contrast_window .. ice_roi_y1`, so the topmost
row the scan can nominate is `ice_stop_row_px` itself. Ice rises through the
band, and **the stop signal is the surface leaving it**. One parameter, no second
bound.

That ordering is what makes the rim harmless, both ways:

- Rim above the band (every seating observed): never scanned.
- Rim below the stop row: found, but reported *below* where ice is wanted, so it
  reads "not yet" and the loop keeps dispensing. Safe.

The dangerous case — rim reported *above* the stop row, i.e. "already full" — is
the one the band top structurally excludes. It has to be positional, because
magnitude cannot do it: through a 48-row window the rim step is **54.7** and ice
surfaces run **33–52**. They overlap, so a "too strong to be ice" cap would
misfire on real ice.

Re-analysing `dispense1` with `--y0 500` turns the dead run into a clean ramp:

```
  -3000..-500ms  pre     0     empty glass reads zero — the control the old ROI failed
      +11500ms  dwell   74     first ice enters the band
      +13500ms  dwell  103
      +16500ms  dwell  122     pinned at the top of the band
      +17000ms  dwell    0     surface has left the band — stop
```

Same 86 frames that previously reported a full glass before the pin opened.
Nothing was wrong with the contrast step; `ice_roi_y0 = 280` was.

The reading pins at 122 px rather than the band's full 170 because a candidate
row needs a full window on each side — so **the effective stop row is the band
top plus the window** (500 + 48 = 548 in that run). Configure the row you mean
and derive the band from it, never the reverse.

This deletes the calibration and makes the blind floor irrelevant — it limited
measuring *low* fills, and a stop row well above the ledge is always visible. It
costs volume consistency: row 565 is 46% full at rim 277 and 70% at rim 379.

### G9 — rim spread (machine, ~5 min) — **do this before G8**

Three or four `fetch_glass` grabs, recording the rim row each time
(`ice-snapshot`, then `--dump-profile`). This decides the *design*, not just a
number, so tuning a stop row first risks tuning one that cannot work.

Only two rim samples exist and they are not a spread: 277 from a single placement
and 379 from a single `fetch_glass`.

- **Spread under ~40 px** → one row works. Take it from G8 and stop thinking
  about seating.
- **Spread near the observed 102 px** → a single row cannot hold; write the
  rim-relative fallback under Implementation.

### G8 — pick the stop row (bench: a glass, ice, milk, an espresso — no arm)

Was "does 60% ice leave room for milk and espresso?". Same test, but it now
*produces* `ice_stop_row_px` rather than checking a target that came from a fit.
From the ground-truth table:

| stop row | fill at rim 277 | fill at rim 379 |
|---|---|---|
| 520 | 57% | 81% |
| **565** | **46%** | **70%** |
| 608 | 36% | 60% |

**Provisional: `ice_stop_row_px = 565`** (band top 517). It caps the worst
observed seating at ~70%, leaving 30% for milk and espresso, while a well-seated
glass still gets 46%. Provisional because that column rests on one `fetch_glass`
sample.

**Choose for the worst seating, not the average.** A row that leaves headroom on
a high-seated glass overflows a low-seated one, and `fetch_glass` picks the
seating. Fill to the candidate row at the *lowest* seating G9 found, then (a) add
a shot — iced coffee; (b) add the real `milk_pour_dwell` of milk plus a shot —
latte. Pass if both sit below ~90% of the glass, checked during the pour and on
the carry.

**Don't judge by eye without correcting for the ledge.** It hides the bottom
third, so a glass that looks 45% full at rim 379 holds ~70%. Read the fill off
the row.

### G6 — falling ice — **answered from `dispense1/`, no re-run**

Whether ice in flight biases the reading enough to force pulse → close pin →
measure → repeat instead of continuous polling. It does not.

The band-bounded re-analysis gives a 10-second ramp with the pin open throughout.
Drawing each reported surface row back onto its own frame, it sits on top of the
pile at every point from +11.5 s (row 596) through +21.5 s (row 448), including
frames where the chute stream is plainly visible above the glass. Dips in the
trace are ~5 px, about 1.2% of glass height — noise, not bias. The only systematic deviation is a slight under-read once the pile
mounds unevenly, which delays the stop rather than shortening it.

So: **poll continuously with the pin open.** This is stronger evidence than the
planned test, which compared the algorithm against itself; it was impossible here
anyway, since the overflowing glass left the surface above the band for the whole
post-roll.

**`ice_dispense_sec = 5` is badly short.** Ice first became visible at +11.5 s
and the glass was full around +17–20 s. Whatever the stop row, the ceiling has to
clear ~20 s and the fixed-dwell fallback needs re-timing.

### G10b — real lighting — **dropped**

The lights are always on, so the synthetic sweep covers the variation that
occurs. It does not cover a change to the fixture itself — a bulb dying, a lamp
moved — but that failure is loud: every iced drink times out and
`ice_dispense_timeouts` climbs. `check_ice_level` is the diagnostic.

### Reaching the dispense pose

Don't edit the machine config. `prepareDrink` locks the portafilter (step 3) and
releases it (step 4) long before it reaches ice at step 6, so the state the ice
dispense runs in is simply the normal one. Two `execute_action` calls reproduce
it from rest:

1. `lock_portafilter` — twists the filter into the bayonet, then `lockFilterFrame`
   (`coffee/motion.go:344`) re-parents `filter` from the arm subtree to world.
2. `release_filter` — opens the jaws, backs off, closes on nothing.

The arm rests holding the portafilter — there is no "pick the filter off a dock"
step — which is exactly why a cold standalone move plans into a phantom. After
these two the jaws are empty and `filter` is a static world obstacle. Reverses
with `grab_filter` + `unlock_portafilter`. Then `fetch_glass`.

**Nothing parks at the pose.** `dispenseIce` is approach → dispense → pulse →
retreat, and the pulse disqualifies it: `ice-dispense` has to own the pin or its
timing against pin-open is meaningless. Split `dispenseIce` at the pulse and
register the first half as `move_to_ice_dispense`; production is unchanged.
`check_ice_level` needs it too.

**Or take the portafilter off by hand.** `execute_action` accepts
`without_portafilter: true`, which drops `filter` and its subtree from the frame
system for that call (`detachFilterFrame`, `coffee/motion.go:509`). Physically
lift the portafilter off at rest, then run `fetch_glass` and
`move_to_ice_dispense` with the flag. It is refused while the filter frame is
locked, where `filter` models the real portafilter in the bayonet rather than one
on the arm. Every `filterSw` step stops planning while it is gone — motion goals
are keyed by frame name (`coffee/motion.go:135`) — so it is for claw-only runs.
The filter goes back when the action ends, so the flag belongs on every call, not
just the first.

`viam robot part motion set-pose` is the repo convention for pose work but plans
outside the service's world state — no held-glass geometry, no area shields. Fine
with empty jaws, not while carrying a glass past the ice machine.

---

## Implementation

**`ice_level.go`** — `iceSurfaceSeen(ctx) (bool, error)` fetches with
`s.srcCamera.Images(ctx, []string{"color"}, nil)`; filtering to `"color"` keeps
the 1.8 MB depth payload off the wire, a latency decision. It delegates to a pure
`iceSurfaceRow(img image.Image) (int, bool)` so the measurement unit-tests
against the committed fixtures.

Scan row brightness down the band `ice_stop_row_px - ice_contrast_window ..
ice_roi_y1`; the largest step above `ice_min_contrast` is the ice surface. Return
whether one was found — the loop needs the transition, not a level. In-process,
not a vision service: no model is needed and a CNN behind a DoCommand would not
reliably fit the check interval.

```go
const (
	defaultIceStopRowPx        = 565.0 // provisional, from G8; 46% at rim 277, 70% at rim 379
	defaultIceContrastWindow   = 48
	defaultIceMinContrast      = 25.0 // survives the brightness sweep
	defaultIceDispenseMaxSec   = 30.0 // must clear the ~20 s real fill time
	defaultIceDispenseMinSec   = 2.0
	defaultIceCheckIntervalSec = 0.5
	iceConfirmations           = 2
	iceVisionMaxErrors         = 3
)
```

**The stop signal is an absence, so it needs the sighting first.** "No surface in
the band" means "not enough ice yet" before ice arrives and "risen past the stop
row" after. Same reading, opposite responses, and only history separates them:
count consecutive sightings, and treat a disappearance as *done* only once a
surface has been seen. Both edges need `iceConfirmations` in a row — a single
dropped frame reads like a disappearance, and a single glare frame reads like a
sighting. Confirming only the disappearance is the dangerous asymmetry: ice takes
~11.5 s to appear, so a run has ~20 zero ticks before the real signal, and one
false sighting anywhere in them arms the latch and the next zeros stop the
dispense on an empty glass.

**The loop** — `dwellUntilFull(ctx, cancelCtx, stopRow, measure)` takes `measure`
as a parameter so it tests without a camera. Wait `ice_dispense_min_sec`, tick at
`ice_check_interval_sec` to a deadline of `ice_dispense_max_sec`. After
`iceVisionMaxErrors` consecutive failures, warn and fall back to a fixed dwell;
vision being down must not cost a drink. On the deadline: warn, `s.say(...)`,
bump `ice_dispense_timeouts`, **return nil** — the espresso is already brewed, a
light glass beats a discarded drink, and a rising counter is how an empty hopper
should surface.

**Config** — the constants above as `ice_*` fields plus `ice_vision_enabled` and
the `ice_roi_*` bounds, all optional, inside the existing `if cfg.CanServeIced`
block so nothing changes for machines that don't set them. Validate `x0 < x1`,
non-negative, `ice_min_contrast > 0`, `ice_stop_row_px - window >= 0`, and
**`ice_stop_row_px + window < ice_roi_y1`** — the band needs a full window each
side of its topmost candidate. Check the ROI against `img.Bounds()` per frame
too (the CLI's `rowProfile` already does): every row here is a raw pixel at one
resolution, so a camera reconfigured to a different frame size invalidates all
of them silently and in an unknown direction.

These guard *silent* failures: at `ice_min_contrast = 0` every frame reports a
surface, so the first tick after the first sighting stops the dispense; a
transposed x pair scans nothing, never sees a surface, and rides to the ceiling.
Both look like working config.

The rows are pixel coordinates, valid only at `ice_machine_dispense` with a glass
gripped. Because the camera rides the gripper, a row is a fixed height above the
jaws — which is what makes a bare row meaningful rather than pose-dependent. What
it is *not* invariant to is where the glass sits in those jaws.

**If G9 finds a wide spread — the rim-relative fallback.** Anchor the row to the
rim instead of the frame:

```go
stopRow := s.iceStopRowPx()
if rim, ok := rimRow(img); ok {
	stopRow = rim + s.iceStopOffsetPx() // 288 for row 565 at rim 277
}
```

The rim is the topmost step above `ice_rim_min_contrast`, scanned from the top of
the image down to the band — the one scan the main path deliberately skips. It
falls back to the fixed row when no rim is found, so a bad frame costs nothing.
**Only write it if G9 says so.**

**Wiring** — `module.go` needs `srcCamera camera.Camera` on the struct; the
camera is already a required dependency, the service just never kept the handle,
only `cupCameraName` (`coffee/module.go:119`). `pulseIcePin` keeps its `stop()`
closure and `context.Background()` write verbatim — that invariant is what the
whole feature hangs off — and gains a **named-return `defer`** replacing today's
four explicit `stop()` calls, because the loop has more exit paths and "left the
ice machine running" is the one bug class that matters. `Close`
(`coffee/module.go:434`) drives the pin LOW too — it cancels `cancelCtx` but
never touches the pin, and the window in which a crash or a rebuild strands the
ice machine running goes from 5 s to the full ceiling.

**`check_ice_level`** — registered conditionally in `actionFuncs`
(`coffee/espresso.go:305`) like the brew-button actions. Logs the detected
surface row against the stop row, **and the topmost strong step in the whole
frame** — that is the rim, and it is the one number that shows seating drift.
Free here, and the reason no rim guard is needed in the loop.

**Tests** — loop tests with a stub `measure` and an `inject.Board` recording pin
`Set` calls (`inject.NewArm` in `control_test.go` is the nearest pattern;
`inject.Board` is not yet used anywhere in `coffee/`): confirms, does-not-confirm
on one sighting then one miss, ceiling, cancel mid-poll, error fallback, vision
disabled. Measurement tests against `coffee/testdata/icelevel/` — including the
one that pins the design: **`fill_0.jpg` scaled brighter still reports no
surface** rather than confirming full. Commit two `dispense1` frames at rim 379
alongside them — all five existing fixtures sit at rim 277, so as it stands the
regression set is exactly the single-seating set Trap 2 says cannot catch a
seating bug.

**Docs** — new config fields, `ice_dispense_timeouts` in the usage-counter lists,
`check_ice_level` and `move_to_ice_dispense` in the manual-step list. Put the
stop row on the **`ice_machine_dispense` row of the pose table**, not only in the
ice section: whoever re-teaches that pose won't be reading the ice fields and is
exactly who needs to know.

---

## Sequencing

G0 and G6 are done; no further capture is needed for the measurement itself.

1. **G9 — rim spread** (machine, ~5 min). Decides whether a fixed stop row can
   work at all, so it comes before the number that depends on it.
2. **G8 — the stop row** (bench, no arm). Provisionally 565; G9 says whether that
   is a row or an offset from the rim.
3. Config and wiring → the loop and tests → docs with the measured values.

The measurement and `move_to_ice_dispense` need neither gate and can be written
now.

## Decisions already made

- **A stop row, not a fill fraction.** The camera rides the gripper, so a row is
  a height above the jaws. This deletes the 426/-94 fit, the interpolation, the
  blind floor and `ice_min_fill_fraction`. Rim-relative measurement is more
  accurate and was not worth the machinery — unless G9 says otherwise.
- **The rim is excluded by where the band starts, not by detecting it.** A rim
  above the band is never scanned; one below the stop row reads "not yet". Safe
  both ways. `ice_roi_y0` used to do this by accident, with 3 px of margin.
- **No rim guard, and no magnitude test for one.** Rim step 54.7 against ice
  33–52 — overlapping, so only position discriminates. A rim landing within one
  window below the band top would still fool it, but that needs roughly twice the
  worst seating error yet observed, by which point the glass is barely under the
  chute at all.
- **The stop row is chosen for the worst seating**, so a well-seated glass gets
  less ice than it could. Overflow is the expensive direction.
- **Contrast step, not a brightness threshold.** A threshold fails toward "full"
  on an empty glass. Don't simplify back.
- **Poll continuously, pin open.** Settled by G6 against the photographs.
- **Timeout serves a light glass** and bumps a counter — doesn't fail the order
  or pause the queue.
- **No seating correction at the pose.** Lifting the glass ~23 mm to a canonical
  height was designed when the reading was a fill fraction and needed one.
- **`check_ice_level` logs only.** No response contract, no web-app work.
- **`lock_portafilter` + `release_filter` to reach the pose**, never a config
  edit — it is the state production dispenses ice in.
- **The portafilter can be taken off instead**, via `without_portafilter` on
  `execute_action`. The two-call route above needs no hands but leaves the filter
  in the machine; this one is a lift and a flag.
- **No real-lighting check.** The lights are always on.

## Traps

1. **The rim looks exactly like ice.** 54.7 through the working window against
   33–52 for real ice. Only its position gives it away, so never widen the band
   upward "just to be safe" — that is the bug, not the fix.
2. **A calibration set at one seating cannot reveal a seating bug.** All five
   fixtures put the rim at 277 to the pixel. Vary the grab before trusting any
   number that depends on where the glass hangs.
3. **A flat trace looks like a clean signal.** 291 px ±1 for three seconds reads
   as a stable measurement; it was the rim. Check the pre-roll on an empty glass
   reads 0 before believing anything after it.
4. **Zero means two opposite things.** No surface in the band is "not yet" before
   ice arrives and "done" after it has passed the stop row. Only `sawSurface`
   separates them, and it is the whole stop condition.
5. **The visible fill is not the fill.** The ledge hides the bottom third, so a
   glass that looks 45% full at rim 379 holds ~70%. Judge from the row.
6. **The effective stop row is the band top plus the window.** A band opened at
   500 tops out at 548. Configure the row you mean, derive the band from it.
7. **An 8-row contrast window looks reasonable and doesn't work.** Gap of 3.
8. **"In frame" is not "measurable."** That conflation sent the first round down
   the depth path. Verify per modality, and check camera-to-glass distance
   against the sensor's minimum range.
9. **`icedata/` and `dispense1/` are large and untracked.** The five committed
   fixtures are the regression set; don't commit the directories.
