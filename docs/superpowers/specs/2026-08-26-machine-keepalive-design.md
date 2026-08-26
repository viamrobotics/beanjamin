# Keeping the espresso machine at brew temperature

Status: design approved 2026-08-26. Implementation not started.

## Problem

`prepareDrink` (coffee/espresso.go) pokes the brew button and then waits out
`brew_time_sec` without ever checking whether the machine responded. On a
machine that has gone to sleep, that poke is refused with three beeps, the arm
waits against a dry group head, and `placeFullCupOnShelf` serves an empty cup.
The order sensor, the usage counters, the leaderboard, and the kiosk's "Ready!"
card all record it as a success.

Two ways to fix that: detect the condition and recover from it, or prevent the
machine from ever leaving brew temperature during hours when someone might
order. This design takes the second. The first needs a sensor; the second needs
a timer and a button press, and the machine's own Auto Start feature covers the
hardest part of it for free.

## Machine behavior (Breville BES920)

| State | Temperature | Entered by | Left by |
|---|---|---|---|
| STANDBY (ready) | 200 F / 93 C | warm-up completing; after every shot | 1 hour idle |
| POWER SAVE | ~140 F / 60 C | 1 hour idle | any button except POWER; steam lever; hot water dial |
| OFF | ambient | 4 continuous hours unused; a POWER press | a POWER press; Auto Start |

Three facts shape the design.

**The 1-hour auto-sleep cannot be disabled.** `A OF` in Advanced Features
governs only the 4-hour full shutdown. The manual corroborates this by
omission: its complete Advanced Features list (`rSEt`, `Hrd`, `dESC`, `SEt`,
`StEA`, `A OF`, `Snd`, `vOL`, pre-infusion) contains exactly one power setting.
There is no settings-only fix. Something physical has to touch the machine each
hour.

**The purge is a documented Breville procedure**, both with and without the
portafilter in the group head. Manual p20: *"press and hold the 1 CUP button.
Allow water to flow for 5 seconds, then release."* Manual p24: *"Place an empty
filter basket into the portafilter... press and hold the 2 CUP button. Release
button after 20 seconds."*

**We never press POWER.** *"Pressing the POWER button during POWER SAVE will
turn the machine off"* (p11). Every design that reads panel state has to reason
about that trap; this one never creates the opportunity.

## Approach

### Half one: Auto Start, no code

Program the machine once, `MENU -> AUTO START -> ON -> 07:45`. This covers the
cold weekday morning in hardware. It ships as an operator step in `README.md`,
not as software.

Auto Start has no day-of-week setting, so it also fires at weekends: the
machine heats, sits unused, and its own 4-hour Auto Off shuts it down. That
costs roughly four hours of idle boiler per weekend day. Buying it back would
mean the arm owning the morning wake, which is a scheduler, a timezone, and a
DST bug for eight hours of boiler time. Accepted as-is.

### Half two: the keep-alive purge

During configured windows, if nothing has run water through the machine in
~40 minutes, the arm holds the 1 CUP button briefly. The machine's idle timer
resets and it never reaches the 1-hour threshold, so it is always at
temperature when an order arrives.

## The purge sequence

Three steps on the **filter** switch, with the arm holding the portafilter
throughout.

```go
Step{PoseName: filterPosePurgeApproach, PoseSwitch: s.filterSw}
Step{PoseName: filterPosePurgePress,    PoseSwitch: s.filterSw, Pause: holdDuration,
     LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterCoffeeButtonCollisions}
Step{PoseName: filterPosePurgeApproach, PoseSwitch: s.filterSw,
     LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterCoffeeButtonCollisions}
```

### Why the filter switch, not the claws switch

The two switches hold poses of two different commanded frames. Every
`filterSw` pose (`grinder_approach`, `tamper_activate`, `coffee_in`) is a pose
of the `filter` frame. Every `clawsSw` pose (`coffee_button_on`,
`filter_released`) is a pose of `coffee-claws-middle`, authored for when the
claws are empty — which is why the existing brew-button poses live there, since
they are only ever used after `releaseFilter`.

Pressing the button while still holding the portafilter therefore has to be a
filter-frame pose. The claws are not the thing being positioned.

### New poses

`purge_approach` and `purge_press`, on the filter switch. Named for intent
rather than reusing `espresso_button_*`, which already exists on the claws
switch with entirely different geometry.

### New collision set

Mirrors `clawCoffeeButtonCollisions` with the filter frames added:

```go
var filterCoffeeButtonCollisions = []AllowedCollision{
	{Frame1: componentFilter,      Frame2: "coffee-machine-buffer-front"},
	{Frame1: "portafilter-handle", Frame2: "coffee-machine-buffer-front"},
	{Frame1: componentClaws,       Frame2: "coffee-machine-buffer-front"},
	{Frame1: "gripper:claws",      Frame2: "coffee-machine-buffer-front"},
}
```

Start with all four pairs and trim whatever the planner does not need.
`FakeMode` already strips the `gripper:*` entries.

### The approach step must not carry the allowance

This lesson is already paid for in `brewButtonSteps`' comment and applies
identically here. An `AllowedCollision` is a property of the **entire plan**,
not of the neighbourhood of its goal — `buildConstraints` hands the planner a
`CollisionSpecification` covering the whole trajectory.

The approach is free-planned from wherever the arm happens to be, which is
home. Allowing `coffee-machine-buffer-front` across that whole traverse lets
the planner route the portafilter straight through the machine's front face,
and the filter hits the machine. Only the two short linear moves get the
allowance.

### What this sequence deliberately does not do

Compared with parking the filter in the group head to free the claws: no
`lockPortaFilter`, `releaseFilter`, `grabFilter`, `unlockPortaFilter`, or
`cleanPortafilter`.

- `portafilterInMachine` and `portafilterHasGrounds` are never mutated, so
  nothing interacts with `rewind`'s recovery state machine.
- Nothing gets wet, so the p30 wet-lugs hazard (*"wet surfaces reduce the
  friction required to hold the portafilter in place whilst under pressure"*)
  and the repeated "wipe the basket dry before dosing" instructions do not
  apply.
- A failed purge leaves the arm holding the filter somewhere harmless.
  Recovery is a move to home.

## The loop

New file `coffee/keepalive.go`. Goroutine started in `NewCoffee` when
`cfg.KeepAlive != nil`, running under `s.cancelCtx`, ticking every
`check_interval_min`:

```
in window && since(lastActivity) >= after_min   -> else sleep
!running && !paused && queue empty              -> else sleep

CAS running false->true, defer release
  setStep(stepKeepAlive); defer clear
  purge()
    ok   -> lastActivity = now; persist; drip_tray_brews++
    fail -> log Error; Slack; best-effort move home; back off to next tick
```

### No special case for the window opening

Nothing runs outside the window, so `since(lastActivity)` at window open is
necessarily hours, which always exceeds `after_min`. The elapsed check fires a
purge at window open on its own; a `wasOutside` flag could never be the reason
a purge happens, so there is no such flag.

The one behavior this changes: two windows less than `after_min` apart will not
purge at the second one's open. That is correct — the machine was kept alive
until the first window closed.

### Concurrency

Taking the same `running` CAS that `prepareDrink` uses is the entire
concurrency story. To the queue a purge is indistinguishable from an order, so
an order arriving mid-purge waits for it (~15 seconds).

The `defer` release is load-bearing: a purge wedged while holding that flag
would stall the queue permanently. The purge also runs under its own timeout
for the same reason.

`setStep(stepKeepAlive)` publishes the purge through `Status` / `get_queue` for
observability, and is cleared afterwards so a stale label does not linger.
`setStep` only writes to the queue when an order ID is set, so a purge cannot
corrupt order state.

## Config

```go
KeepAlive *KeepAlive `json:"keepalive,omitempty"`   // nil disables everything
```

| field | default | notes |
|---|---|---|
| `auto_start` | required | e.g. `"07:45"`; mirrors what is programmed on the machine, and is the window open |
| `end` | required | e.g. `"17:00"`; the window is half-open, `[auto_start, end)` |
| `days` | mon-fri | |
| `timezone` | required | IANA name, resolved with `time.LoadLocation` |
| `after_min` | 40 | idle minutes before a purge is due |
| `check_interval_min` | 5 | tick period |
| `hold_sec` | 1.0 | button dwell; sets the water volume |

Window open is tied to the Auto Start time rather than being its own
independent number, because two configured times that must agree will drift.
Auto Start at 07:45 with a window opening at 09:00 means the machine wakes,
idles, and sleeps at 08:45 before the window even opens.

`hold_sec` defaults short and deliberately below Breville's documented 5
seconds. The purge only has to make the pump run so the firmware counts it as
use; it is not trying to stabilize group-head temperature. Tune on the machine.

### Validation

`Validate` must:

- parse `auto_start` and `end` as `HH:MM` and require `auto_start < end`
- resolve `timezone` via `time.LoadLocation`
- check every entry in `days` against a weekday set
- require `has_separate_brew_buttons` — holding the single-toggle machine's
  switch for a second is a pour of unknown dose
- enforce `after_min + 2*check_interval_min < 55`

That last margin has to absorb the check interval, a tick skipped because an
order was running, and the purge's own duration. `after_min: 50` with
`check_interval_min: 10` lands exactly on the 60-minute cliff with nothing to
spare.

Every number stays a knob. The 1-hour threshold is a vendor claim nobody here
has measured, and owners report real sleep onset anywhere from 45 to 60
minutes.

### First tick of the day

The first tick fires at `auto_start`, during the ~10-minute warm-up, so that
press is refused: three beeps, no water, nothing wetted. Roughly 15 seconds of
wasted motion once a morning, and it self-corrects on the next tick. Not worth
a margin parameter.

## Persistence

`VIAM_MODULE_DATA`, confirmed in RDK at `module/modmanager/manager.go:1086`,
pointing at `~/.viam/module-data/<robot-id>/<module-name>/`. The module manager
creates the directory, and it survives restarts, reconfigurations, and module
version upgrades. No `data_dir`, no operator config, namespaced per machine and
per module.

One RFC3339 timestamp in `machine-activity`, holding **last machine activity**,
not last brew. The purge is itself activity: a file tracking only brews would
re-fire every tick until someone ordered a coffee.

Written on a successful purge, and when `brew()` returns successfully rather
than when the order does — a shot that poured and then failed at serving still
reset the machine's idle timer, so the brew is the accurate signal.

An unset env var (tests, local runs) degrades to in-memory with a startup
warning, mirroring how `pending-clips` degrades without `data_dir`. Worst case
on a lost file is one extra purge.

## Out of scope, and the risk that leaves

No state sensor, no camera, no smart plug. The loop is open-loop: it assumes
the machine is on and hot and never verifies it.

Precisely what survives: **pressing the 1 CUP button on a fully off machine is
a no-op.** The purge rescues a machine that drifted into POWER SAVE, but
nothing here rescues one that is actually off. If Auto Start does not fire (a
power outage resetting the clock to 12:00AM, someone changing the setting) or a
human presses POWER off, the arm purges at a dead machine all day and every
order serves an empty cup, logged as a success.

Disabling `A OF` so the machine can never reach fully-off would make this
design airtight. The manual rules it out, p2: *"If the appliance is to be left
unattended... Always switch off the appliance by pressing the POWER button to
off and unplug from the power outlet."*

So the hole stays open by choice. Closing it is what a state sensor is for, and
when that day comes a **smart plug with energy monitoring is the better sensor
than a webcam**: ~2W off, sustained ~1400W heating, small periodic cycles at
temperature, a visible pump spike during a shot. One number, no ROI
calibration, no lighting sensitivity, and it confirms the shot actually pulled.
A webcam's only remaining edge is reading `FILL TANK` / `CLEAN ME!` off the
LCD.

## Ripple effects

- **`requiredPoses()`** gains the two purge poses, gated on
  `cfg.KeepAlive != nil`. Necessary: ungated, the other machine fails
  construction on upgrade for poses it will never use.
- **No web-app change.** The purge is not an order and never enters the queue.
  The tracker renders `raw_step` as free text and only `READY_RAW_STEPS` is a
  fixed set.
- **No change to `brewButtonSteps`.** The purge builds its own step list on the
  filter switch, matching how `grind`, `tampGround`, and `cleanPortafilter`
  each build theirs.
- **`drip_tray_brews`** increments per purge — at most about 13 per day in a
  fully idle 9-hour window (fewer when real orders reset the timer), so the
  tray-emptying interval stays honest.
- **`README.md`**: the `keepalive` config block, the two new poses for the
  switch fragment, and the Auto Start operator step.
- **`data_dir` now looks vestigial.** `pending-clips` does the same job through
  an operator-configured path and could move to `VIAM_MODULE_DATA`. Separate
  cleanup, not this change.

## Testing

Testable without hardware:

- `inKeepAliveWindow(now, cfg)` as a pure function, with timezone and DST cases
- the due / not-due decision table: in window, outside window, elapsed just
  under and just over `after_min`
- the interlock predicates against scripted `running` / `paused` / queue states
- `Validate` rejecting bad times, a missing timezone, `auto_start >= end`, the
  single-toggle machine, and a too-tight margin
- persistence round-trip with `t.Setenv`, plus the missing-file fallback
- the three-step sequence against a fake arm: pose order, the dwell, and that
  the approach step carries no allowed collisions

## Remaining unknowns, to settle on the machine

1. **`hold_sec` and the real drip-tray load.** Default is deliberately short;
   measure the water a single hold actually produces.
2. **Actual sleep onset on this unit** — sets `after_min`. Owners report 45 to
   60 minutes.
3. **Does a bare button press reset the idle timer?** Neither the manual nor
   Home-Barista, CoffeeSnobs, nor CoffeeForums addresses it. If an UP-arrow tap
   works, the purge becomes zero-water and only the pose and the dwell change.
   One hour of wall-clock to find out. Not worth blocking on: an extraction is
   unambiguously *use* under any reading of "idle."

## Sources

- [BES920 instruction book](https://assets.breville.com/BES920/BES920_USCM_IB_Y23_LR.pdf)
- [Does BES920XL have auto shut off? — Home-Barista](https://www.home-barista.com/espresso-machines/does-breville-bes920xl-have-auto-shut-off-t52311.html)
- [BES920 auto shut off — CoffeeSnobs](https://coffeesnobs.com.au/forum/equipment/brewing-equipment-midrange-500-1500/53140-breville-bes920-auto-shut-off)
- [Viam module developer reference](https://docs.viam.com/operate/reference/module-configuration)
