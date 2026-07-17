# Fridge-Door Open — Design

**Date:** 2026-07-17
**Status:** Approved design, pre-implementation
**Package:** `coffee` (new file `coffee/door.go`)

## Summary

Add a motion primitive that opens a passive fridge door: the OK-1 arm grips the
door's handle and pulls the door open along its hinge arc. The door is modeled
as a **static obstacle** in the machine's frame system with a **handle-ball
child frame**. As the arm pulls, we sweep the door angle `θ` in software: at
each step we re-place the static door obstacle at `θ`, read the handle-ball's
new world pose out of the frame system, move the gripper to track it, and plan
that short move with the door sitting at its true angle so collision-checking
stays honest.

Open-only for this slice. Written so a reverse sweep can close the door later.

## Physical / coupling model

- **Arm pulls, door passive.** No door actuator. Door `θ` is *our* sweep
  variable, not something we read from hardware.
- **Root frame = pivot.** The door frame's origin sits on the hinge point, and
  the frame is oriented so its **local Z axis is the hinge axis**. Rotation is
  therefore always "θ about local Z"; the swing plane is implicit in the frame
  orientation — there is no separate "plane" to configure.
- **Handle ball is a child of the door frame.** Rotating the door frame carries
  the ball automatically; `fs.Transform(ball → world)` yields the handle pose at
  any `θ`.

### Load-bearing assumptions to verify before coding (Step 0)

A read-only dump of the **live machine frame system** must confirm:

1. The door frame's origin is on the hinge axis (pivot == frame origin).
2. The handle ball is a child frame of the door frame.
3. The exact door/hinge/ball transforms and geometry names.

If (1) or (2) is false, the "set θ → ball moves" model does not hold and the
frame-system config is fixed first. Prefer the `viam robot part` CLI to dump the
frame system; write a small (~30-line) Go SDK script only if the CLI cannot emit
geometries.

**Physical calibration knob:** the configured hinge axis must match the real
hinge. A few degrees of error makes the computed arc drift from the real handle
path and the gripper fights the door. Verify the axis/origin physically with
`viam robot part motion get-pose` on the real handle at 0° and ~90°, not just by
trusting config numbers.

## Architecture

### Chosen approach: plan-per-θ-step loop, static frame updated in place

A single `PlanRequest` carries a single `FrameSystem`, so it can only hold the
door at one angle. A swinging door occupies different space at every waypoint.
To keep collision-checking honest we therefore **re-plan once per θ step** with
the door re-placed, rather than issuing one static-world multi-goal plan.

This differs from the two existing motion primitives:

- `executePivot` (`coffee/motion.go`) rotates a *held* item about a *fixed
  point*; one plan, static world.
- `executeCircularMotion` (`coffee/motion.go`) traces a circle; one plan, static
  world.

The door sweep is the first primitive where the **obstacle world changes between
waypoints**.

### Rejected alternatives

- **Single plan, door rides the gripper (held-item style):** reparent the door
  onto the gripper subtree, compute arc waypoints, one plan. One planning call,
  but fiddly frame surgery and a new arc-about-remote-hinge generator, and the
  grasp offset must be exact. Deferred.
- **Single plan, door collision disabled during the swing:** simplest, but the
  door provides no collision protection while opening and its swept position is
  unmodeled. Least safe. Rejected.

## Components (new file `coffee/door.go`)

Mirrors how `motion.go` separates pure geometry (`computePivotPoses`, tested)
from I/O (`executePivot`).

| Unit | Purpose | Depends on |
|---|---|---|
| `computeDoorSweep(closedDeg, openDeg, degPerStep) []float64` | Pure: θ waypoints. Unit-tested like `pivot_test.go`. | — |
| `setDoorTheta(fs, θ)` | Re-place the static door obstacle at `θ` so `fs.Transform(ball→world)` returns the swept handle pose. | frame system |
| `graspBall(ctx)` | Approach the handle pose, close gripper on the ball, capture the constant gripper↔ball offset. | pose switch, gripper |
| `openDoor(ctx)` | Orchestrates: grasp → loop(setθ, goal, plan, execute) → release + retract → reset. | all above + motion layer |

### `setDoorTheta` mechanism (static obstacle)

Reuses the `lockFilterFrame` (`coffee/motion.go:189`) maneuver:

1. Collect the door frame's descendants (the handle ball) with their local
   transforms.
2. `RemoveFrame` the door (detaches the ball).
3. Re-add the door at `Pose(hingePoint, Rz(θ) · originalOrientation)`. Because
   the pivot is the frame origin, only orientation changes — translation is
   fixed, so it is a pure rotation with no positional drift.
4. Re-attach the ball with its **unchanged local offset**, so it lands at the
   correct swept world position automatically.

### `openDoor` flow

```
graspBall(ctx)                      // approach + close on the handle, capture offset
defer resetFrameSystem(ctx)         // restore closed door on normal finish OR cancel
for θ in computeDoorSweep(closed, open, degPerStep):
    setDoorTheta(cachedFS, θ)       // door obstacle + ball child now at θ
    ballPose = cachedFS.Transform(ball → world)
    goal     = ballPose · graspOffset
    plan([goal], allow{gripper:claws, handle-ball})   // door at TRUE θ
    execute()
releaseAndRetract(ctx)              // open gripper, back off to a safe clear pose
```

### End state

**Release and retract.** After the sweep the gripper opens and the arm backs off
to a safe clear pose, leaving the door standing open. `openDoor` is a complete,
self-contained action.

### Cleanup / cancellation

`openDoor` `defer`s `resetFrameSystem` (`coffee/motion.go:290`), which rebuilds
the cached frame system from the service (door → closed). A normal finish or a
mid-swing cancel both restore the world, so the in-place mutation cannot leak.
The loop honors the shared `cancelCtx` the way `executeCircularMotion` does.

## Entry point

Build the reusable `openDoor` primitive first and expose it via a **`"open_door"`
DoCommand** registered in the existing dispatch table (`coffee/control.go`,
added in #245). Wiring it into `prepareDrink` as a brew step (e.g. fetch
milk/ice) is a later step once the primitive is proven.

## Config (`coffee/config.go`)

Follow the `BrewTimeSec` pattern — `float64`/`string` fields with `omitempty`
plus a small helper returning the configured value or a default constant near
the feature:

- `DoorOpenAngleDegs` (default 90)
- `DoorPivotDegreesPerStep`
- Frame/pose names: door frame, hinge, handle-ball frame, handle approach/grasp
  pose name(s).
- Velocity/accel via the existing `StepMoveOptions` path.

Document every new field in `README.md` (repo convention).

## Allowed collisions (`coffee/collisions.go`)

Add, in the style of `filterGrabCollisions`:

```go
var doorOpenCollisions = []AllowedCollision{
    {Frame1: "gripper:claws", Frame2: "<handle-ball-frame>"},
    // add {Frame1: "coffee-claws-middle", ...} if the middle frame contacts the ball
}
```

Passed through `buildConstraints` → `CollisionSpecification` exactly like the
existing sets.

## Explicitly out of scope

- **Held-item machinery.** Gripping the handle must NOT register a held cup;
  keep `appendHeldItemCollisions` / held-item geometry out of this path.
- **Closing the door.** Reverse sweep deferred (design supports it).
- **Approach-pose authoring** beyond what's needed: handle approach/grasp poses
  are authored in the multi-poses switch and verified physically per repo
  convention (`viam robot part motion get-pose`/`set-pose`), not guessed.

## Reuse

`executePivot` skeleton (fetch → compute → plan → execute), `executeCircularMotion`
plan-in-a-loop + cancel handling, `buildConstraints`, `fetchPose`,
`resetFrameSystem`, and the `lockFilterFrame` remove/re-add mutation pattern. The
genuinely new code is `computeDoorSweep`, `setDoorTheta`, and the re-plan-per-θ
loop.

## Testing

`coffee/door_test.go` — pure-math tests for `computeDoorSweep` (step count,
endpoints, sign) and the grasp-offset composition, with no hardware, mirroring
`pivot_test.go` / `circular_test.go`. Frame mutation (`setDoorTheta`) is
exercised against a small in-memory frame system asserting the ball's swept
world pose.

## Ripple effects / coordination

- **Frame-system config owner.** The follow-up below reshapes the machine's
  frame system; coordinate before committing config changes.
- **Motion viz** (localhost:3030) will render the door at each θ — useful for
  debugging the sweep.

## Documented follow-up (not this slice)

Model the fridge as a **component with kinematics + a revolute joint** (origin =
hinge, axis = hinge). Then `θ` becomes a plan *input* (`fsInputs[door] = {θ}`),
`setDoorTheta` and its frame surgery are **deleted**, and there is nothing to
reset because inputs are per-plan and never persisted in `cachedFS`. This is the
"right" long-term model; the static-in-place approach here is the pragmatic
first slice.
