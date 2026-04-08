# Beanjamin Module

The `viam:beanjamin` module provides five models for arm-based automation workflows:

1. **`viam:beanjamin:coffee`** - A generic service that orchestrates a full coffee brew cycle by sequentially moving through all poses on a pose switcher.
2. **`viam:beanjamin:multi-poses-execution-switch`** - A switch component that moves an arm between predefined poses using the Motion service.
3. **`viam:beanjamin:text-to-speech`** - A generic service that synthesises speech via Google Cloud Text-to-Speech and plays it through an audioout service.
4. **`viam:beanjamin:maintenance-sensor`** - A sensor component that reports whether the system is safe for maintenance (arm idle, no orders running or queued).
5. **`viam:beanjamin:dial-control-motion`** - A generic service that translates Stream Deck dial inputs into relative arm motions.

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
      "pose_value": { ... }
    }
  ]
}
```

| Name              | Type   | Required | Description                                                                             |
| ----------------- | ------ | -------- | --------------------------------------------------------------------------------------- |
| `component_name`  | string | Yes      | Name of the arm component to move.                                                      |
| `motion`          | string | Yes      | Name of the motion service (typically `"builtin"`).                                     |
| `reference_frame` | string | No       | Reference frame for poses. Defaults to `"world"`.                                       |
| `poses`           | array  | Yes      | One or more named poses. Pose names must be unique.                                     |

### Defining poses

Each pose in the `poses` array must have a `pose_name` and **exactly one** of two definition styles:

#### Absolute pose (`pose_value`)

Define the pose directly with position and orientation coordinates:

```json
{
  "pose_name": "home",
  "pose_value": {
    "x": 0, "y": 0, "z": 500,
    "o_x": 0, "o_y": 0, "o_z": 1,
    "theta": 0
  }
}
```

**Pose value fields:** `x`, `y`, `z` are in millimeters. `o_x`, `o_y`, `o_z` define the orientation axis, `theta` is the rotation angle in degrees.

#### Relative pose (`baseline`)

Define a pose relative to another pose in the same `poses` array. Optionally add a `translation` (offset added to the baseline position) and/or an `orientation` (replaces the baseline orientation entirely). The baseline can appear anywhere in the array — before or after the pose that references it.

```json
{
  "pose_name": "left-of-home",
  "baseline": "home",
  "translation": { "x": -100 }
}
```

| Field         | Type   | Required            | Description                                                                                      |
| ------------- | ------ | ------------------- | ------------------------------------------------------------------------------------------------ |
| `baseline`    | string | Yes (instead of `pose_value`) | Name of another pose in the `poses` array.                                            |
| `translation` | object | No                  | Position offset added to the baseline. Fields: `x`, `y`, `z` (millimeters, default `0`).         |
| `orientation` | object | No                  | Orientation that **replaces** the baseline orientation. Fields: `o_x`, `o_y`, `o_z`, `theta`.    |

Baselines can be chained — a relative pose can itself be used as a baseline for another pose. Multiple poses can share the same baseline.

**Validation rules:**
- A pose must have either `pose_value` or `baseline`, not both.
- `translation` and `orientation` are only allowed with `baseline`.
- The `baseline` must reference an existing `pose_name` in the `poses` array.
- Circular baseline references are not allowed (e.g. A → B → A).

### Example Configuration

```json
{
  "component_name": "my-arm",
  "motion": "builtin",
  "reference_frame": "world",
  "poses": [
    {
      "pose_name": "home",
      "pose_value": {
        "x": 0, "y": 0, "z": 500,
        "o_x": 0, "o_y": 0, "o_z": 1,
        "theta": 0
      }
    },
    {
      "pose_name": "above-home",
      "baseline": "home",
      "translation": { "z": 100 }
    },
    {
      "pose_name": "pour",
      "baseline": "home",
      "translation": { "x": 200, "y": 100, "z": -150 },
      "orientation": { "o_x": 0, "o_y": 1, "o_z": 0, "theta": 90 }
    }
  ]
}
```

In this example:
- **home** is defined absolutely at `(0, 0, 500)` with orientation `(0, 0, 1, 0°)`.
- **above-home** inherits home's position and orientation, then adds `z: +100` → final position `(0, 0, 600)`.
- **pour** inherits home's position, adds a translation → `(200, 100, 350)`, and overrides the orientation to `(0, 1, 0, 90°)`.

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

**`get_pose_by_name`** - Get the pose coordinates, reference frame, and component name for a named pose.

```json
{ "get_pose_by_name": "home" }
```

Returns:

```json
{
  "x": 0, "y": 0, "z": 500,
  "o_x": 0, "o_y": 0, "o_z": 1,
  "theta": 0,
  "reference_frame": "world",
  "component_name": "my-arm"
}
```

---

## Model: `viam:beanjamin:coffee`

**API:** `rdk:service:generic`

Orchestrates a full coffee brew cycle using a `multi-poses-execution-switch` component. Supports preparing espresso orders, executing individual actions, and cancellation.

### Configuration

```json
{
  "pose_switcher_name": "multi-pose-execution-switch",
  "claws_pose_switcher_name": "claws-switch",
  "arm_name": "my-arm",
  "gripper_name": "my-gripper",
  "speech_service_name": "speech",
  "viz_url": "http://localhost:8080",
  "brew_time_sec": 25,
  "place_cup": true,
  "clean_after_use": true,
  "save_motion_requests_dir": "/tmp/motion-requests"
}
```

**Top-level fields:**

| Name                       | Type   | Required | Description                                                                                                   |
| -------------------------- | ------ | -------- | ------------------------------------------------------------------------------------------------------------- |
| `pose_switcher_name`       | string | Yes      | Name of the multi-poses-execution-switch component.                                                           |
| `claws_pose_switcher_name` | string | Yes      | Name of the claws pose switcher component.                                                                    |
| `arm_name`                 | string | Yes      | Name of the arm component used for motion planning and execution.                                             |
| `gripper_name`             | string | Yes      | Name of the gripper component.                                                                                |
| `speech_service_name`      | string | No       | Name of a text-to-speech generic service for spoken greetings.                                                |
| `viz_url`                  | string | No       | URL of a [motion-tools](https://github.com/viam-labs/motion-tools) viz server. When set, the frame system is drawn before each motion plan, useful for debugging collisions and frame placement. |
| `brew_time_sec`            | float  | No       | Brew duration in seconds.                                                                                     |
| `place_cup`                | bool   | No       | Enable cup placement step in the brew cycle.                                                                  |
| `clean_after_use`          | bool   | No       | Enable cleaning step after each brew.                                                                         |
| `save_motion_requests_dir` | string | No       | Directory to save motion request payloads for debugging.                                                      |

### DoCommand

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

Only `drink` is required. If `initial_greeting` is omitted, a random greeting is generated. If `customer_name` is provided, it personalizes the greeting and completion messages. Orders are added to a queue and processed sequentially.

**`execute_action`** - Run a single coffee-making action by name. Available actions: `grind_coffee`, `tamp_ground`, `lock_portafilter`, `unlock_portafilter`.

```json
{"execute_action": "grind_coffee"}
```

**`cancel`** - Cancel whatever action is currently running.

```json
{"cancel": true}
```

**`get_queue`** - Get the current order queue status.

```json
{"get_queue": true}
```

Returns:

```json
{"count": 2, "orders": ["Alice", "Bob"], "is_paused": false, "is_running": true}
```

**`proceed`** - Resume queue processing after a pause between orders.

```json
{"proceed": true}
```

Returns `{"status": "resumed"}`.

**`clear_queue`** - Remove all pending orders from the queue.

```json
{"clear_queue": true}
```

Returns `{"status": "cleared", "removed": 2}`.

**`action`** - Control the gripper. Supported values: `"open_gripper"`, `"close_gripper"`.

```json
{"action": "open_gripper"}
```

Returns `{"status": "opened"}` or `{"status": "closed", "grabbed": true}`.

---

## Model: `viam:beanjamin:dial-control-motion`

**API:** `rdk:service:generic`

Translates Stream Deck dial inputs into relative arm motions. Each dial tick moves the arm by a configurable number of millimeters along the X, Y, or Z axis, or along the tool's orientation vector. The service tracks the absolute dial position between calls to determine direction (handling rollover at the dial range boundaries).

### Configuration

```json
{
  "arm_name": "my-arm",
  "dial_move_x_mm": 5,
  "dial_move_y_mm": 5,
  "dial_move_z_mm": 5,
  "dial_move_orientation_mm": 5,
  "dial_max_position": 100
}
```

| Name                       | Type   | Required | Description                                                                                    |
| -------------------------- | ------ | -------- | ---------------------------------------------------------------------------------------------- |
| `arm_name`                 | string | Yes      | Name of the arm component to move.                                                             |
| `dial_move_x_mm`           | float  | No       | Millimeters to move per dial tick on the X axis. Defaults to `1`.                              |
| `dial_move_y_mm`           | float  | No       | Millimeters to move per dial tick on the Y axis. Defaults to `1`.                              |
| `dial_move_z_mm`           | float  | No       | Millimeters to move per dial tick on the Z axis. Defaults to `1`.                              |
| `dial_move_orientation_mm` | float  | No       | Millimeters to move per dial tick along the tool's orientation vector. Defaults to `1`.        |
| `dial_max_position`        | float  | No       | Maximum dial position value, used for rollover detection. Defaults to `100`.                   |

### DoCommand

**`dial_move_x`** / **`dial_move_y`** / **`dial_move_z`** - Move the arm along an axis using a Stream Deck dial value. The first call for a given axis calibrates the dial position and does not move the arm.

```json
{"dial_move_x": 50}
```

Returns `{"status": "moved", "axis": "x", "mm": 5.0}` or `{"status": "dial_initialized", "axis": "x", "position": 50}` on first call (calibration).

**`dial_move_orientation`** - Move the arm along its current tool orientation vector.

```json
{"dial_move_orientation": 50}
```

**`dial_move_speed`** - Adjust dial movement speed. Updates `dial_move_x_mm`, `dial_move_y_mm`, `dial_move_z_mm`, and `dial_move_orientation_mm` based on dial input.

```json
{"dial_move_speed": 75}
```

Returns `{"status": "speed_updated", "dial_move_x_mm": 7.5, "dial_move_y_mm": 7.5, "dial_move_z_mm": 7.5, "dial_move_orientation_mm": 7.5}`.

---

## Model: `viam:beanjamin:text-to-speech`

**API:** `rdk:service:generic`

Synthesises speech using the [Google Cloud Text-to-Speech API](https://cloud.google.com/text-to-speech) and plays the resulting audio through an `rdk:component:audio_out` component. Can be used standalone or as the speech backend for the `coffee` service (via `speech_service_name`).

### Prerequisites

- A Google Cloud project with the Text-to-Speech API enabled.
- A service account key (JSON) with access to the API.
- A configured `audio_out` component on the same machine.

### Configuration

```json
{
  "audio_out": "<string>",
  "google_credentials_json": { ... },
  "language_code": "<string>",
  "voice_name": "<string>"
}
```

| Name                      | Type   | Required | Description                                                                                                 |
| ------------------------- | ------ | -------- | ----------------------------------------------------------------------------------------------------------- |
| `audio_out`               | string | Yes      | Name of the `audio_out` component dependency used for playback.                                             |
| `google_credentials_json` | object | Yes      | Google Cloud service account credentials as a JSON object (not a string).                                   |
| `language_code`           | string | No       | BCP-47 language code. Defaults to `"en-US"`.                                                                |
| `voice_name`              | string | No       | Specific Google voice name (e.g. `"en-US-Neural2-F"`). If omitted, Google picks a default for the language. |

### Example Configuration

```json
{
  "audio_out": "ao",
  "google_credentials_json": {
    "type": "service_account",
    "project_id": "my-project",
    "private_key_id": "abc123",
    "private_key": "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n",
    "client_email": "tts@my-project.iam.gserviceaccount.com",
    "client_id": "123456789",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token"
  },
  "language_code": "en-US",
  "voice_name": "en-US-Neural2-F"
}
```

### DoCommand

**`say`** — Synthesise and play text. The call blocks until playback completes.

```json
{"say": "Hello, your espresso is ready!"}
```

Returns:

```json
{"text": "Hello, your espresso is ready!"}
```

---

## Model: `viam:beanjamin:maintenance-sensor`

**API:** `rdk:component:sensor`

Reports whether the system is safe for maintenance. Returns `is_safe: true` only when the arm is not moving, no order is running, and the queue is empty. Useful for gating maintenance workflows or triggering alerts.

### Configuration

```json
{
  "coffee_service_name": "coffee",
  "arm_name": "my-arm"
}
```

| Name                 | Type   | Required | Description                              |
| -------------------- | ------ | -------- | ---------------------------------------- |
| `coffee_service_name`| string | Yes      | Name of the `viam:beanjamin:coffee` service to query for queue/running state. |
| `arm_name`           | string | Yes      | Name of the arm component to check for physical movement. |

### Readings

Returns a single reading:

```json
{"is_safe": true}
```

`is_safe` is `false` when any of the following are true:
- The arm is physically moving
- An order is currently running
- There are orders in the queue

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

