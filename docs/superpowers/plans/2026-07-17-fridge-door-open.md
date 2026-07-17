# Fridge-Door Open Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an `open_door` primitive that grips a passive fridge door by its handle and pulls it open along its hinge arc, keeping the static door obstacle collision-honest by re-placing it in the frame system at each swept angle.

**Architecture:** A plan-per-θ-step loop. The door is a static obstacle whose root frame origin sits on the hinge; the handle ball is a child frame. For each θ from closed→open we re-place the door frame at `Compose(baseDoorPose, Rz(θ))` (the ball rides along), read the ball's swept world pose, move the gripper to track it (rigid grasp offset), and plan that short move with `{gripper:claws, handle-ball}` collisions allowed and the door sitting at its true angle. On finish/cancel the frame system is rebuilt, so the mutation can't leak.

**Tech Stack:** Go, Viam RDK (`go.viam.com/rdk/referenceframe`, `spatialmath`, `armplanning`, `motionplan`), the `coffee` package.

## Global Constraints

- Frame rotation reuses the `lockFilterFrame` (`coffee/motion.go:189`) remove/re-add + `collectDescendants` mechanism — do NOT invent a new mutation path.
- The door is a **static** obstacle this slice; θ is a software sweep variable, never read from hardware.
- The primitive must NOT touch held-item machinery (`appendHeldItemCollisions`, held-item geometry) — gripping the handle is not a held cup.
- Cleanup is via `defer resetFrameSystem` (`coffee/motion.go:290`); a normal finish or a mid-swing cancel both restore the closed door.
- Config getters follow the `orDefault`/const-default pattern (`coffee/config.go:197`).
- Run `make lint` (runs `gofmt -s -w .`) before every commit; run `go test ./coffee/...` for the package.
- Frame/pose names are read from `Config`, never hard-coded, EXCEPT the literal gripper sub-frame `gripper:claws` (matches existing collision sets in `coffee/collisions.go`).

---

### Task 0: Fetch and verify the live frame system

Read-only investigation that unblocks every later task's config values. No code change; produces recorded findings appended to this plan and to the spec's "Step 0" notes.

**Files:**
- Modify (append findings): `docs/superpowers/plans/2026-07-17-fridge-door-open.md` (this file, "Task 0 Findings" section at the bottom)

- [ ] **Step 1: Dump the live frame system**

Try the CLI first (no script needed):

```bash
viam robot part get-robot-config --part <PART_ID>   # or the frame-system dump command available on this machine
# If the CLI cannot emit geometries, write a ~30-line Go SDK program that calls
# robotClient.FrameSystemConfig(ctx) and prints each part's Frame name, parent,
# translation, orientation, and geometry.
```

- [ ] **Step 2: Record and verify the three load-bearing facts**

Confirm and write down in "Task 0 Findings":
1. The **door frame name** and that its **origin is on the hinge axis** (pivot == origin). If not, STOP — fix the machine config first.
2. The **handle-ball frame name** and that it is a **child of the door frame**. If not, STOP.
3. Whether the door **geometry lives on the door frame itself or a `<door>_origin` companion frame** (RDK part convention — see `coffee/motion.go:217`), plus exact translations/orientations.
4. Which claws frame physically contacts the ball (`gripper:claws` and/or `coffee-claws-middle`).
5. The **hinge axis direction** — confirm it is the door frame's local Z. If the hinge is not the local Z, note which local axis it is (Task 3 rotates about local Z; a different axis changes the `Rz` construction).

- [ ] **Step 3: Physically verify the arc (calibration)**

```bash
viam robot part motion get-pose --part <PART_ID> ...   # read the real handle pose at ~0° and ~90°
```

Confirm the computed arc (radius = |ball − hinge|, swept in the door frame's XY plane) matches the real handle path within a few mm. Record the measured open angle the door physically allows.

- [ ] **Step 4: Commit findings**

```bash
git add docs/superpowers/plans/2026-07-17-fridge-door-open.md
git commit -m "docs: record fridge frame-system findings"
```

---

### Task 1: Config fields, defaults, and getters

**Files:**
- Modify: `coffee/config.go` (add fields to `Config`, add getters near the other numeric getters ~line 210)
- Test: `coffee/config_test.go`
- Modify: `README.md` (coffee-service Config section)

**Interfaces:**
- Produces:
  - `Config.DoorOpenAngleDegs float64` (json `door_open_angle_degs,omitempty`)
  - `Config.DoorPivotDegreesPerStep float64` (json `door_pivot_degrees_per_step,omitempty`)
  - `func (s *beanjaminCoffee) doorOpenAngleDegs() float64` → configured or `defaultDoorOpenAngleDegs` (90)
  - `func (s *beanjaminCoffee) doorPivotDegreesPerStep() float64` → configured or `defaultDoorPivotDegreesPerStep` (10)

Frame names (`fridge-door`, `fridge-handle-ball`) and pose names are **constants** (Task 0), not config — see Task 3/Task 4.

- [ ] **Step 1: Write the failing test**

Add to `coffee/config_test.go`:

```go
func TestDoorGetters_Defaults(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	if got := s.doorOpenAngleDegs(); got != 90 {
		t.Errorf("doorOpenAngleDegs default = %v, want 90", got)
	}
	if got := s.doorPivotDegreesPerStep(); got != 10 {
		t.Errorf("doorPivotDegreesPerStep default = %v, want 10", got)
	}
}

func TestDoorGetters_Configured(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{DoorOpenAngleDegs: 75, DoorPivotDegreesPerStep: 5}}
	if got := s.doorOpenAngleDegs(); got != 75 {
		t.Errorf("doorOpenAngleDegs = %v, want 75", got)
	}
	if got := s.doorPivotDegreesPerStep(); got != 5 {
		t.Errorf("doorPivotDegreesPerStep = %v, want 5", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./coffee/ -run TestDoorGetters -v`
Expected: FAIL — `s.doorOpenAngleDegs undefined`.

- [ ] **Step 3: Add fields and getters**

In `coffee/config.go`, add to the `Config` struct (near the other tunables):

```go
	// Fridge-door open (coffee/door.go): swing angle and per-step θ increment.
	// Frame and pose names are fixed constants in door.go, not config.
	DoorOpenAngleDegs       float64 `json:"door_open_angle_degs,omitempty"`
	DoorPivotDegreesPerStep float64 `json:"door_pivot_degrees_per_step,omitempty"`
```

Add near the other getters (~line 220):

```go
// defaultDoorOpenAngleDegs is the door swing angle when DoorOpenAngleDegs is unset.
const defaultDoorOpenAngleDegs = 90

// defaultDoorPivotDegreesPerStep is the per-step θ increment when unset.
const defaultDoorPivotDegreesPerStep = 10

func (s *beanjaminCoffee) doorOpenAngleDegs() float64 {
	return orDefault(s.cfg.DoorOpenAngleDegs, defaultDoorOpenAngleDegs)
}

func (s *beanjaminCoffee) doorPivotDegreesPerStep() float64 {
	return orDefault(s.cfg.DoorPivotDegreesPerStep, defaultDoorPivotDegreesPerStep)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./coffee/ -run TestDoorGetters -v`
Expected: PASS.

- [ ] **Step 5: Document the fields in README.md**

Under the coffee-service Config section, add a row/paragraph for each new field (matching the existing field-doc style), noting: frame/pose names are required to enable `open_door`; `door_open_angle_degs` defaults to 90; `door_pivot_degrees_per_step` defaults to 10.

- [ ] **Step 6: Commit**

```bash
make lint && go test ./coffee/ -run TestDoorGetters
git add coffee/config.go coffee/config_test.go README.md
git commit -m "feat: add fridge-door open config fields and getters"
```

---

### Task 2: `computeDoorSweep` — pure θ waypoint generator

**Files:**
- Create: `coffee/door.go`
- Test: `coffee/door_test.go`

**Interfaces:**
- Produces: `func computeDoorSweep(closedDeg, openDeg, degPerStep float64) []float64` — inclusive absolute-angle waypoints from `closedDeg` to `openDeg`. First element == `closedDeg`, last == `openDeg`. Step count = `max(1, round(|openDeg-closedDeg|/degPerStep))`. Sign follows the direction of travel (works when `openDeg < closedDeg`). Mirrors `computePivotPoses` (`coffee/motion.go:956`).

- [ ] **Step 1: Write the failing test**

Create `coffee/door_test.go`:

```go
package coffee

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

func TestComputeDoorSweep_StepCountAndEndpoints(t *testing.T) {
	got := computeDoorSweep(0, 90, 10) // ceil-round(90/10)=9 steps -> 10 waypoints
	if len(got) != 10 {
		t.Fatalf("len = %d, want 10", len(got))
	}
	if got[0] != 0 {
		t.Errorf("first = %v, want 0", got[0])
	}
	if math.Abs(got[len(got)-1]-90) > 1e-9 {
		t.Errorf("last = %v, want 90", got[len(got)-1])
	}
}

func TestComputeDoorSweep_Monotonic(t *testing.T) {
	got := computeDoorSweep(0, 90, 15)
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("not increasing at %d: %v then %v", i, got[i-1], got[i])
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./coffee/ -run TestComputeDoorSweep -v`
Expected: FAIL — `computeDoorSweep undefined`.

- [ ] **Step 3: Implement**

In `coffee/door.go`:

```go
package coffee

import (
	"context"
	"fmt"
	"math"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// computeDoorSweep returns inclusive absolute-angle waypoints (degrees) from
// closedDeg to openDeg, one every ~degPerStep. The first waypoint is closedDeg
// and the last is exactly openDeg. Direction follows the sign of the travel, so
// it also works when openDeg < closedDeg (a later close sweep).
func computeDoorSweep(closedDeg, openDeg, degPerStep float64) []float64 {
	total := math.Abs(openDeg - closedDeg)
	numSteps := max(1, int(math.Round(total/degPerStep)))
	out := make([]float64, numSteps+1)
	for i := 0; i <= numSteps; i++ {
		t := float64(i) / float64(numSteps)
		out[i] = closedDeg + (openDeg-closedDeg)*t
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./coffee/ -run TestComputeDoorSweep -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
make lint && go test ./coffee/ -run TestComputeDoorSweep
git add coffee/door.go coffee/door_test.go
git commit -m "feat: add computeDoorSweep θ waypoint generator"
```

---

### Task 3: `setDoorTheta` — re-place the static door obstacle at θ

**Files:**
- Modify: `coffee/door.go`
- Test: `coffee/door_test.go`

**Interfaces:**
- Consumes: `collectDescendants` (`coffee/motion.go:350`), `descendantEntry{frame, parentName}`.
- Produces: `func setDoorTheta(fs *referenceframe.FrameSystem, doorFrameName string, baseDoorPose spatialmath.Pose, thetaDeg float64) error` — re-places the door frame at `Compose(baseDoorPose, Rz(thetaDeg))` about its own origin, preserving the door frame's geometry (if any) and re-attaching all descendants (the `<door>_origin` geometry frame and the handle ball) with unchanged local transforms. `baseDoorPose` is the door's original (closed) parent-relative transform, captured once by the caller so repeated calls don't accumulate.

**NOTE (from Task 0):** rotation is about the door frame's local **Z**. If Task 0 found the hinge axis is a different local axis, change the `Rz` orientation vector accordingly.

- [ ] **Step 1: Write the failing test**

Add to `coffee/door_test.go`:

```go
func TestSetDoorTheta_BallSweepsArc(t *testing.T) {
	fs := referenceframe.NewEmptyFrameSystem("test")
	// Door root at (500,0,0), identity orientation, origin == hinge.
	doorPose := spatialmath.NewPoseFromPoint(r3.Vector{X: 500, Y: 0, Z: 0})
	door, err := referenceframe.NewStaticFrame("door", doorPose)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.AddFrame(door, fs.World()); err != nil {
		t.Fatal(err)
	}
	// Handle ball 300mm out along the door's -Y (a child of the door).
	ball, err := referenceframe.NewStaticFrame("ball",
		spatialmath.NewPoseFromPoint(r3.Vector{X: 0, Y: -300, Z: 0}))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.AddFrame(ball, door); err != nil {
		t.Fatal(err)
	}

	if err := setDoorTheta(fs, "door", doorPose, 90); err != nil {
		t.Fatal(err)
	}

	// Ball local (0,-300,0) rotated +90° about Z -> (300,0,0), + hinge (500,0,0).
	tf, err := fs.Transform(referenceframe.NewZeroInputs(fs).ToLinearInputs(),
		referenceframe.NewPoseInFrame("ball", spatialmath.NewZeroPose()),
		referenceframe.World)
	if err != nil {
		t.Fatal(err)
	}
	got := tf.(*referenceframe.PoseInFrame).Pose().Point()
	want := r3.Vector{X: 800, Y: 0, Z: 0}
	if got.Sub(want).Norm() > 0.5 {
		t.Errorf("ball world = %v, want ~%v", got, want)
	}

	// Hinge (door origin) must be unchanged — pure rotation, no drift.
	dtf, err := fs.Transform(referenceframe.NewZeroInputs(fs).ToLinearInputs(),
		referenceframe.NewPoseInFrame("door", spatialmath.NewZeroPose()),
		referenceframe.World)
	if err != nil {
		t.Fatal(err)
	}
	if dtf.(*referenceframe.PoseInFrame).Pose().Point().Sub(r3.Vector{X: 500}).Norm() > 0.01 {
		t.Errorf("door origin moved to %v, want (500,0,0)", dtf.(*referenceframe.PoseInFrame).Pose().Point())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./coffee/ -run TestSetDoorTheta -v`
Expected: FAIL — `setDoorTheta undefined`.

- [ ] **Step 3: Implement**

Add to `coffee/door.go`:

```go
// setDoorTheta re-places the static door obstacle at thetaDeg about its own
// origin (the hinge). It composes Rz(θ) onto the door's original closed
// transform, then reuses the lockFilterFrame maneuver: capture descendants,
// remove the door, re-add it rotated, re-attach descendants (the <door>_origin
// geometry frame and the handle ball) with their local transforms unchanged so
// they ride the swing. baseDoorPose must be the closed transform captured once
// by the caller — passing it every call keeps rotations absolute, not cumulative.
func setDoorTheta(fs *referenceframe.FrameSystem, doorFrameName string, baseDoorPose spatialmath.Pose, thetaDeg float64) error {
	door := fs.Frame(doorFrameName)
	if door == nil {
		return fmt.Errorf("door frame %q not found", doorFrameName)
	}
	parent, err := fs.Parent(door)
	if err != nil {
		return fmt.Errorf("door parent: %w", err)
	}

	// Rotation about the door frame's local Z, applied at the origin.
	rz := spatialmath.NewPoseFromOrientation(&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: thetaDeg})
	rotated := spatialmath.Compose(baseDoorPose, rz)

	// Preserve the door frame's own geometry, if it carries one.
	var geom spatialmath.Geometry
	if geos, gerr := door.Geometries([]referenceframe.Input{}); gerr == nil && geos != nil && len(geos.Geometries()) > 0 {
		geom = geos.Geometries()[0]
	}

	descendants := collectDescendants(fs, doorFrameName)
	fs.RemoveFrame(door)

	var newDoor referenceframe.Frame
	if geom != nil {
		newDoor, err = referenceframe.NewStaticFrameWithGeometry(doorFrameName, rotated, geom)
	} else {
		newDoor, err = referenceframe.NewStaticFrame(doorFrameName, rotated)
	}
	if err != nil {
		return fmt.Errorf("build rotated door frame: %w", err)
	}
	if err := fs.AddFrame(newDoor, parent); err != nil {
		return fmt.Errorf("re-add door frame: %w", err)
	}
	for _, d := range descendants {
		p := fs.Frame(d.parentName)
		if err := fs.AddFrame(d.frame, p); err != nil {
			return fmt.Errorf("re-attach descendant %q under %q: %w", d.frame.Name(), d.parentName, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./coffee/ -run TestSetDoorTheta -v`
Expected: PASS (ball at ~(800,0,0), door origin at (500,0,0)).

- [ ] **Step 5: Commit**

```bash
make lint && go test ./coffee/ -run TestSetDoorTheta
git add coffee/door.go coffee/door_test.go
git commit -m "feat: add setDoorTheta static-obstacle rotation"
```

---

### Task 4: `openDoor` orchestrator + allowed-collision set

**Files:**
- Modify: `coffee/collisions.go` (add `doorOpenCollisions`)
- Modify: `coffee/door.go` (add `openDoor`)

**Interfaces:**
- Consumes: `computeDoorSweep`, `setDoorTheta`, `s.fetchPose`, `s.moveToRawPose`, `s.currentInputs`, `s.resetFrameSystem`, `s.gripper.Grab`/`.Open`, `buildConstraints`, `armplanning.PlanMotion`, `s.arm.MoveThroughJointPositions`, `s.slowMovementMoveOptions`, `s.doorOpenAngleDegs`, `s.doorPivotDegreesPerStep`, `s.activeOrderLogger`.
- Produces: `func (s *beanjaminCoffee) openDoor(ctx context.Context) (map[string]any, error)`.

**NOTE:** This task is arm-I/O heavy and cannot be fully unit-tested without hardware. It relies on Tasks 2–3 unit tests plus a compile/lint gate. Verification is the physical run in Task 5's handoff.

- [ ] **Step 1: Add the allowed-collision set**

In `coffee/collisions.go`, using the frame name(s) confirmed in Task 0:

```go
// doorOpenCollisions permits the gripper claws to touch the fridge handle ball
// while pulling the door open. Frame2 is Config.HandleBallFrameName at runtime;
// this static form covers the default configured name — see openDoor, which
// builds the pair from config.
var doorOpenCollisions = func(handleBallFrame string) []AllowedCollision {
	return []AllowedCollision{
		{Frame1: "gripper:claws", Frame2: handleBallFrame},
		// If Task 0 found coffee-claws-middle also contacts the ball, add:
		// {Frame1: componentClaws, Frame2: handleBallFrame},
	}
}
```

- [ ] **Step 2: Implement `openDoor`**

Add to `coffee/door.go`:

```go
// openDoor grips the passive fridge handle and pulls the door open along its
// hinge arc, re-placing the static door obstacle at each swept angle so
// collision-checking stays honest. It releases and retracts when done, leaving
// the door standing open. The frame system is rebuilt on exit (normal or
// cancel) so the in-place door mutation cannot leak.
func (s *beanjaminCoffee) openDoor(ctx context.Context) (map[string]any, error) {
	logger := s.activeOrderLogger()
	if s.cfg.DoorFrameName == "" || s.cfg.HandleBallFrameName == "" || s.cfg.DoorGraspPoseName == "" {
		return nil, fmt.Errorf("open_door not configured (need door_frame_name, handle_ball_frame_name, door_grasp_pose_name)")
	}

	// Restore the closed door on any exit path.
	defer func() {
		if err := s.resetFrameSystem(ctx); err != nil {
			logger.Warnf("open_door: resetFrameSystem failed: %v", err)
		}
	}()

	// 1. Approach + grasp the handle.
	if s.cfg.DoorApproachPoseName != "" {
		if err := s.moveToPose(ctx, Step{PoseName: s.cfg.DoorApproachPoseName, PoseSwitch: s.clawsSw}); err != nil {
			return nil, fmt.Errorf("approach handle: %w", err)
		}
	}
	graspPD, err := s.fetchPose(ctx, s.clawsSw, s.cfg.DoorGraspPoseName)
	if err != nil {
		return nil, fmt.Errorf("grasp pose: %w", err)
	}
	if err := s.moveToRawPose(ctx, graspPD, nil, nil, nil); err != nil {
		return nil, fmt.Errorf("move to grasp: %w", err)
	}
	if s.gripper != nil {
		if _, err := s.gripper.Grab(ctx, nil); err != nil {
			return nil, fmt.Errorf("grab handle: %w", err)
		}
	}

	// 2. Capture the rigid gripper↔ball offset and the door's base transform.
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return nil, err
	}
	linInputs := fsInputs.ToLinearInputs()
	ballBase, err := s.ballWorldPose(fs, linInputs)
	if err != nil {
		return nil, err
	}
	// graspPD.pose is expressed in graspPD.refFrame; transform it to world.
	graspWorld, err := fs.Transform(linInputs,
		referenceframe.NewPoseInFrame(graspPD.refFrame, graspPD.pose), referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("grasp pose to world: %w", err)
	}
	gripperInBall := spatialmath.PoseBetween(ballBase, graspWorld.(*referenceframe.PoseInFrame).Pose())

	doorFrame := fs.Frame(s.cfg.DoorFrameName)
	if doorFrame == nil {
		return nil, fmt.Errorf("door frame %q not found", s.cfg.DoorFrameName)
	}
	baseDoorPose, err := doorFrame.Transform([]referenceframe.Input{})
	if err != nil {
		return nil, fmt.Errorf("door base transform: %w", err)
	}

	// 3. Sweep θ closed→open, re-planning each step with the door repositioned.
	sweep := computeDoorSweep(0, s.doorOpenAngleDegs(), s.doorPivotDegreesPerStep())
	collisions := doorOpenCollisions(s.cfg.HandleBallFrameName)
	logger.Infof("open_door: sweeping %.0f° in %d steps", s.doorOpenAngleDegs(), len(sweep)-1)

	for _, theta := range sweep[1:] { // skip 0° — already there
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelled during open_door: %w", ctx.Err())
		default:
		}
		if err := setDoorTheta(fs, s.cfg.DoorFrameName, baseDoorPose, theta); err != nil {
			return nil, err
		}
		fsNow, inNow, err := s.currentInputs(ctx)
		if err != nil {
			return nil, err
		}
		ballNow, err := s.ballWorldPose(fsNow, inNow.ToLinearInputs())
		if err != nil {
			return nil, err
		}
		goalPose := spatialmath.Compose(ballNow, gripperInBall)
		goal := armplanning.NewPlanState(referenceframe.FrameSystemPoses{
			graspPD.componentName: referenceframe.NewPoseInFrame(referenceframe.World, goalPose),
		}, nil)

		req := &armplanning.PlanRequest{
			FrameSystem: fsNow,
			Goals:       []*armplanning.PlanState{goal},
			StartState:  armplanning.NewPlanState(nil, inNow),
			Constraints: buildConstraints(nil, collisions),
		}
		plan, _, err := armplanning.PlanMotion(ctx, logger, req)
		s.savePlanRequestAndResponse(req, plan, "open_door", err)
		if err != nil {
			return nil, fmt.Errorf("plan open_door step θ=%.0f: %w", theta, err)
		}
		positions, err := plan.Trajectory().GetFrameInputs(s.cfg.ArmName)
		if err != nil {
			return nil, fmt.Errorf("frame inputs θ=%.0f: %w", theta, err)
		}
		if err := s.arm.MoveThroughJointPositions(ctx, positions, s.slowMovementMoveOptions(), nil); err != nil {
			return nil, fmt.Errorf("execute open_door step θ=%.0f: %w", theta, err)
		}
	}

	// 4. Release and retract.
	if s.gripper != nil {
		if err := s.gripper.Open(ctx, nil); err != nil {
			return nil, fmt.Errorf("release handle: %w", err)
		}
	}
	if s.cfg.DoorRetractPoseName != "" {
		if err := s.moveToPose(ctx, Step{PoseName: s.cfg.DoorRetractPoseName, PoseSwitch: s.clawsSw}); err != nil {
			return nil, fmt.Errorf("retract: %w", err)
		}
	}
	return map[string]any{"status": "door_open"}, nil
}

// ballWorldPose returns the handle-ball frame's current world pose.
func (s *beanjaminCoffee) ballWorldPose(fs *referenceframe.FrameSystem, inputs referenceframe.FrameSystemInputs) (spatialmath.Pose, error) {
	tf, err := fs.Transform(inputs,
		referenceframe.NewPoseInFrame(s.cfg.HandleBallFrameName, spatialmath.NewZeroPose()),
		referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("ball to world: %w", err)
	}
	return tf.(*referenceframe.PoseInFrame).Pose(), nil
}
```

**Verify signatures against the source while implementing:** confirm `spatialmath.PoseBetween(a, b)` returns the pose of `b` in `a`'s frame (used for the rigid offset) and `ToLinearInputs()`'s return type matches `fs.Transform`'s first arg (see `executePivot`, `coffee/motion.go:663-669`). If `currentInputs` already returns the linear-input type, drop the `.ToLinearInputs()` calls to match.

- [ ] **Step 3: Build + lint**

Run: `make lint && go build ./... && go test ./coffee/...`
Expected: compiles; existing + Task 2/3 tests pass.

- [ ] **Step 4: Commit**

```bash
git add coffee/collisions.go coffee/door.go
git commit -m "feat: add openDoor orchestrator and handle-ball allowed collision"
```

---

### Task 5: Wire the `open_door` DoCommand + docs

**Files:**
- Modify: `coffee/api.go` (dispatch table `coffeeCommands` ~line 134; error string ~line 198)
- Modify: `README.md` (coffee-service DoCommand list)

**Interfaces:**
- Consumes: `s.openDoor(ctx)` from Task 4.

- [ ] **Step 1: Register the command**

In `coffee/api.go`, add to `coffeeCommands`:

```go
	{key: "open_door", run: func(s *beanjaminCoffee, ctx context.Context, _ map[string]any) (map[string]any, error) {
		return s.openDoor(ctx)
	}},
```

And add `open_door` to the "supported commands" error string (~line 198).

- [ ] **Step 2: Build**

Run: `make lint && go build ./... && go test ./coffee/...`
Expected: compiles and passes.

- [ ] **Step 3: Document in README.md**

Add `open_door` to the coffee-service DoCommand list: "Grips the fridge handle and pulls the door open along its hinge arc, then releases and retracts. Requires `door_frame_name`, `handle_ball_frame_name`, and the door pose names."

- [ ] **Step 4: Commit**

```bash
git add coffee/api.go README.md
git commit -m "feat: expose open_door DoCommand"
```

- [ ] **Step 5: Physical verification (handoff)**

On a machine with the fridge configured, issue `DoCommand{"open_door": true}` and confirm: the arm approaches + grips the handle, the door swings ~90° with the gripper tracking it, the motion-viz door (localhost:3030) rotates in lockstep, the arm releases and retracts, and `get_queue`/state is clean afterward. Watch for the gripper fighting the door (hinge-axis calibration from Task 0).

---

## Self-Review

- **Spec coverage:** model/coupling → Tasks 3–4; root-frame-as-pivot + local-Z rotation → Task 3; static-in-place update → Task 3; plan-per-θ loop → Task 4; release+retract → Task 4; allowed collision → Task 4; entry point DoCommand → Task 5; config fields + defaults + README → Tasks 1/5; frame-system fetch + calibration → Task 0; not-held-item constraint → Global Constraints + Task 4 (no `appendHeldItemCollisions`); documented revolute follow-up → recorded in spec, intentionally not a task.
- **Placeholder scan:** frame/pose names are config-driven (no literal placeholders in code); `<PART_ID>` in Task 0 is a real CLI arg the operator fills; the two "if Task 0 found …" notes are conditional real code, not TODOs.
- **Type consistency:** `computeDoorSweep`/`setDoorTheta`/`openDoor`/`ballWorldPose`/`doorOpenCollisions` signatures match across tasks; getters `doorOpenAngleDegs`/`doorPivotDegreesPerStep` defined in Task 1 and used in Task 4.

## Task 0 Findings

From `viam machines part motion print-config` (part `5be4df6e…`) on 2026-07-17.

**Fridge subtree (world → fridge → fridge-door → handle chain):**

| Frame | Parent | Translation (mm) | Orientation | Geometry |
|---|---|---|---|---|
| `fridge` | world | (-1030, 440, 250) | identity | Box 470×470×500 |
| `fridge-door` | `fridge` | (258, 235, 0) | identity | Box 45×470×500 @ **Position (0, -235, 0)** |
| `fridge-handle-top` | `fridge-door` | (76, -459, 215) | identity | **none** |
| `fridge-handle-lower-bar` | `fridge-handle-top` | (54, 0, -50) | identity | **none** |
| `fridge-handle-ball` | `fridge-handle-lower-bar` | (0, 0, -40) | identity | **none** |

**Verdicts on the load-bearing assumptions:**

1. ✅ **Door origin == hinge.** The door panel's geometry is offset **Y −235** (half its 470mm width) from the `fridge-door` frame origin — so the origin sits on the **+Y vertical edge** of the panel. That edge is the hinge. Rotating about the frame's local **Z** pivots the panel about the hinge, exactly as designed.
2. ✅ **Handle is in the door subtree** (as a 3-level grandchild, not a direct child). `collectDescendants(fs, "fridge-door")` is BFS/recursive (`coffee/motion.go:350`), so `fridge-handle-top`, `-lower-bar`, and `-ball` all ride the rotation. No change needed.
3. **Geometry is inline on `fridge-door`** (no `<door>_origin` companion frame — these are config frames, not RDK parts). `setDoorTheta` must preserve the door frame's own geometry (with its −235 offset) across the remove/re-add. Task 3's test asserts this.
4. **Hinge axis = local Z** (all fridge frames are identity-oriented; the door is upright). Confirmed.
5. ⚠️ **The handle ball has NO geometry** — nor do `-top`/`-lower-bar`. The only geometry in the whole subtree is the **door panel** (`fridge-door`). See the collision note below.

**Frame names are fixed machine obstacles** → used as **constants** in code (matching how `coffee/collisions.go` references `"coffee-machine-actuation-area"` etc.), not new Config fields. Constants: `frameFridgeDoor = "fridge-door"`, `frameFridgeHandleBall = "fridge-handle-ball"`.

**⚠️ Collision-target correction (affects Task 4):** the spec's `{gripper, handle-ball}` allowance is a **no-op today** — `fridge-handle-ball` has no geometry to collide with. The geometry the gripper is actually pressed against while gripping the handle at the panel's outer edge is the **door panel** (`fridge-door`). So the working allowance is `{gripper:claws, fridge-door}` + `{coffee-claws-middle, fridge-door}`. Making a *ball*-vs-gripper allowance meaningful requires first adding a sphere geometry to `fridge-handle-ball` in the machine config (owned by whoever configures the fridge). **Decision pending from user** — recorded in Task 4.

**Still requires physical work before a live run (not code):**
- Author the handle **approach / grasp / retract** poses on the claws switch via `viam machines part motion set-pose` and verify them physically (repo convention). Code references them as constants `doorPoseApproach`, `doorPoseGrasp`, `doorPoseRetract`.
- Verify the **open direction** (sign of θ) and the physically-allowed open angle by driving the handle and reading `get-pose` at 0° and the open extreme.
