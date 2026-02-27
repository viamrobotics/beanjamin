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

Runs named sequences of poses on a `multi-poses-execution-switch` component. Supports multiple sequences, forward/reverse execution, and optional position enforcement.

### Configuration

```json
{
  // string (required) — name of the multi-poses-execution-switch component
  "pose_switcher_name": "multi-pose-execution-switch",

  // string (optional) — name of a viam-labs speech service for spoken greetings
  "speech_service_name": "speech-1",

  // map[string][]Step (required) — named sequences of steps
  // each step has a pose_name and optional pause_secs
  "sequences": {
    "brew": [
      {"pose_name": "grinder_approach"},
      {"pose_name": "grinder_activate", "pause_secs": 10},
      {"pose_name": "grinder_approach", "pause_secs": 5},
      {"pose_name": "tamper_approach"},
      {"pose_name": "tamper_activate", "pause_secs": 3},
      {"pose_name": "coffee_approach"},
      {"pose_name": "coffee_in"},
      {"pose_name": "coffee_locked_mid"},
      {"pose_name": "coffee_locked_final", "pause_secs": 25}
    ],
    "clean": [
      {"pose_name": "grinder_approach"},
      {"pose_name": "tamper_approach"}
    ]
  }
}
```

### DoCommand

**`run`** - Run a named sequence forward. Only one sequence can run at a time. Optionally pass `enforce_start` to check that the switch is at the first step before running.

```json
{"run": "brew"}
{"run": "brew", "enforce_start": true}
```

**`rewind`** - Run a named sequence in reverse. Only allowed when the switch is at the last step of that sequence.

```json
{"rewind": "brew"}
```

**`prepare_order`** - Prepare a drink order with optional speech greetings. Currently only supports espresso.

```json
{
  "prepare_order": {
    "drink": "espresso",
    "customer_name": "Alice",
    "initial_greeting": "optional custom greeting",
    "completion_statement": "optional custom completion message"
  }
}
```

Only `drink` is required. If `initial_greeting` is omitted, a random greeting is generated. If `customer_name` is provided, it personalizes the greeting and completion messages. Runs the full espresso sequence: grind, tamp, and lock porta filter.

**`execute_action`** - Run a single coffee-making action by name. Available actions: `grind_coffee`, `tamp_ground`, `lock_porta_filter`, `unlock_porta_filter`.

```json
{"execute_action": "grind_coffee"}
```

**`cancel`** - Cancel whatever sequence or action is currently running.

```json
{"cancel": true}
```

All commands return `{"status": "complete"}` on success or `{"status": "cancelled"}` for cancel.

### Behavior

- The `sequences` map defines named sequences of poses. Each can be run or rewound independently.
- Poses can be repeated with different pauses at each occurrence.
- `enforce_start` on `run` checks the switch is at the first step. `rewind` always checks the switch is at the last step.
- All execution is cancellation-aware — cancel stops the sequence between steps.

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

