# Beanjamin Module

The `viam:beanjamin` module provides two models for arm-based automation workflows:

1. **`viam:beanjamin:coffee`** - A generic service that orchestrates a full coffee brew cycle by sequentially moving through all poses on a pose switcher.
2. **`viam:beanjamin:multi-poses-execution-switch`** - A switch component that moves an arm between predefined poses using the Motion service.

---

## Model: `viam:beanjamin:multi-poses-execution-switch`

**API:** `rdk:component:switch`

Moves an arm (or any movable component) between a list of named poses via the Motion service. Each "position" of the switch corresponds to a pose. Only one movement can execute at a time.

### Configuration

```json
{
  "component_name": "<string>",
  "motion": "<string>",
  "reference_frame": "<string>",
  "poses": [
    {
      "pose_name": "<string>",
      "x": <float>, "y": <float>, "z": <float>,
      "o_x": <float>, "o_y": <float>, "o_z": <float>,
      "theta_degrees": <float>
    }
  ]
}
```

| Name              | Type   | Required | Description                                                                             |
| ----------------- | ------ | -------- | --------------------------------------------------------------------------------------- |
| `component_name`  | string | Yes      | Name of the arm component to move.                                                      |
| `motion`          | string | Yes      | Name of the motion service (typically `"builtin"`).                                     |
| `reference_frame` | string | No       | Reference frame for poses. Defaults to `"world"`.                                       |
| `poses`           | array  | Yes      | One or more named poses. Each pose needs a `pose_name` and position/orientation fields. |

**Pose fields:** `x`, `y`, `z` are in millimeters. `o_x`, `o_y`, `o_z` define the orientation axis, `theta_degrees` is the rotation angle in degrees.

### Example Configuration

```json
{
  "component_name": "my-arm",
  "motion": "builtin",
  "reference_frame": "world",
  "poses": [
    {
      "pose_name": "home",
      "x": 0, "y": 0, "z": 500,
      "o_x": 0, "o_y": 0, "o_z": 1,
      "theta_degrees": 0
    },
    {
      "pose_name": "pour",
      "x": 200, "y": 100, "z": 350,
      "o_x": 0, "o_y": 1, "o_z": 0,
      "theta_degrees": 90
    }
  ]
}
```

### Switch Interface

| Method                 | Description                                        |
| ---------------------- | -------------------------------------------------- |
| `GetNumberOfPositions` | Returns the total number of poses and their names. |
| `GetPosition`          | Returns the index of the current pose (0-based).   |
| `SetPosition(index)`   | Moves the arm to the pose at the given index.      |

### DoCommand

**`set_position_by_name`** - Move to a pose by name.

```json
{ "set_position_by_name": "home" }
```

**`get_current_position_name`** - Get the name of the current pose.

```json
{ "get_current_position_name": true }
```

Returns:

```json
{ "position_name": "home" }
```

---

## Model: `viam:beanjamin:coffee`

**API:** `rdk:service:generic`

Orchestrates a full coffee brew cycle by moving through a configurable sequence of poses on a `multi-poses-execution-switch` component. A single `DoCommand` triggers the entire sequence — no manual button presses needed.

### Configuration

```json
{
  // string (required) — name of the multi-poses-execution-switch component
  "pose_switcher_name": "multi-pose-execution-switch",

  // []Step (required) — ordered list of steps to execute
  // each step has a pose name and an optional pause (in seconds) after it completes
  // poses can be repeated with different pauses at each occurrence
  "sequence": [
    {"pose_name": "grinder_approach"},
    {"pose_name": "grinder_activate", "pause_secs": 10},
    {"pose_name": "grinder_approach", "pause_secs": 5},
    {"pose_name": "tamper_approach"},
    {"pose_name": "tamper_activate", "pause_secs": 3},
    {"pose_name": "coffee_approach"},
    {"pose_name": "coffee_in"},
    {"pose_name": "coffee_locked_mid"},
    {"pose_name": "coffee_locked_final", "pause_secs": 25}
  ]
}
```

### DoCommand

**`brew`** - Run the full brew cycle. Moves through the configured sequence of poses in order. Only one brew can run at a time.

```json
{ "brew": true }
```

Returns on success:

```json
{ "status": "complete" }
```

Returns an error if a brew is already in progress, a motion step fails, or the request is cancelled.

**`unbrew`** - Run the sequence in reverse. Only allowed when the switch is at the last position in the sequence (e.g., `coffee_locked_final`). Skips the current position and works backwards through every prior step.

```json
{ "unbrew": true }
```

Returns on success:

```json
{ "status": "complete" }
```

Returns an error if the switch is not at the last position, a cycle is already running, or a motion step fails.

**`cancel`** - Cancel a brew or unbrew cycle in progress. The cycle stops after the current motion completes.

```json
{ "cancel": true }
```

Returns:

```json
{ "status": "cancelled" }
```

### Behavior

- The `sequence` field defines the exact order of poses to execute. Poses can be repeated and reordered as needed.
- When `{"brew": true}` is received, it iterates through the sequence, calling `set_position_by_name` on the switcher for each step.
- Each step can optionally include `pause_secs` to wait after the pose completes. Steps without `pause_secs` (or set to 0) have no pause.
- The brew cycle is cancellation-aware — cancelling the request or stopping the service will halt the cycle between steps.

---

## Development

When iterating on poses, we recommend using the built-in `viam` CLI motion commands to query and test arm positions on a running machine.

Note: `--organization` , `--location`, and `--machine` will be infered from the part ID

### Print motion service status

```bash
viam robot part motion print-status \
  --organization <org> \
  --location <location> \
  --machine <machine> \
  --part <part>
```

### Get the current pose of a component

```bash
viam robot part motion get-pose \
  --organization <org> \
  --location <location> \
  --machine <machine> \
  --part <part> \
  --component <component-name>
```

### Move a component to a pose

```bash
viam robot part motion set-pose \
  --organization <org> \
  --location <location> \
  --machine <machine> \
  --part <part> \
  --component <component-name> \
  -x <mm> -y <mm> -z <mm> \
  --ox <float> --oy <float> --oz <float> --theta <degrees>
```

Note: Only the pose values specified will be modified. Example if you only set `-x 100`, it will move the component by just changing the X value of its current pose

Once you've found the right poses, add them to your `multi-poses-execution-switch` configuration.

