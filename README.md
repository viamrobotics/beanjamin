# Beanjamin Module

The `viam:beanjamin` module provides these models for arm-based automation workflows:

1. **`viam:beanjamin:coffee`** - A generic service that orchestrates a full coffee brew cycle by sequentially moving through all poses on a pose switcher.
2. **`viam:beanjamin:multi-poses-execution-switch`** - A switch component that moves an arm between predefined poses using the Motion service.
3. **`viam:beanjamin:text-to-speech`** *(deprecated — migrate to [`viam:conversation-bundle:text-to-speech`](https://app.viam.com/module/viam/conversation-bundle))* - A generic service that synthesises speech via Google Cloud Text-to-Speech and plays it through an audioout service.
4. **`viam:beanjamin:maintenance-sensor`** - A sensor component that reports whether the system is safe for maintenance (arm idle, no orders running or queued).
5. **`viam:beanjamin:order-sensor`** - A sensor that yields one reading per completed order (start/end timestamps and outcome) when wired from the coffee service.
6. **`viam:beanjamin:dial-control-motion`** - A generic service that translates Stream Deck dial inputs into relative arm motions.
7. **`viam:beanjamin:customer-detector`** - A generic service that identifies return customers via facial recognition using the [`viam:vision:face-identification`](https://github.com/viam-modules/viam-face-identification) vision service.

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
| `translation` | object | No                  | Position offset added to the baseline. Fields: `x`, `y`, `z` (millimeters along world axes, default `0`), and `along_orientation` (millimeters along the baseline's normalized orientation vector, default `0`). |
| `orientation` | object | No                  | Orientation that **replaces** the baseline orientation. Fields: `o_x`, `o_y`, `o_z`, `theta`.    |

The `along_orientation` component is projected onto the **baseline's** orientation vector, not onto any `orientation` override set on the same pose — translation is applied before the orientation replace. If the baseline's orientation vector has zero norm, the `along_orientation` offset is silently skipped.

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
      "pose_name": "backed-off-home",
      "baseline": "home",
      "translation": { "along_orientation": -50 }
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
- **backed-off-home** inherits home's pose and translates `-50` mm along home's orientation vector `(0, 0, 1)` → final position `(0, 0, 450)`.
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

Orchestrates a full coffee brew cycle using a `multi-poses-execution-switch` component. Supports preparing espresso and lungo orders, executing individual actions, and cancellation.

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
  "lungo_brew_time_sec": 40,
  "grind_time_sec": 7.5,
  "place_cup": true,
  "clean_after_use": true,
  "portafilter_shake_sec": 2.5,
  "save_motion_requests_dir": "/tmp/motion-requests",
  "order_sensor_name": "order-events",
  "zoo_cam_storage_name": "video-store",
  "input_range_override": {
    "my-arm": {
      "5": { "min_degs": -270, "max_degs": 270 }
    }
  }
}
```

Add a **`viam:beanjamin:order-sensor`** component to the machine, put it in the coffee service **depends_on**, and set `order_sensor_name` to that component’s name. When an order attempt finishes, one reading is queued with `start_time`, `end_time`, `order_ok`, `duration_ms`, and `error_message` (if applicable).

Configure a [`viam:video:storage`](https://github.com/viam-modules/video-store) camera on the machine. After each order attempt, the coffee service issues an async `save` DoCommand. Each clip includes a fixed **N seconds** of pre-roll (ring-buffer permitting) and **N seconds** of post-roll; the short post-roll wait means the next queued order starts slightly after the prior one fully finishes.

The save request includes a `tags` entry with the order UUID (for cloud data filtering) and JSON `metadata` with order and customer fields. Clips are queued after every attempt, including failed brews or panics; failures set `order_status` to `failed` and include an `error` string.

**Top-level fields:**

| Name                       | Type   | Required | Description                                                                                                   |
| -------------------------- | ------ | -------- | ------------------------------------------------------------------------------------------------------------- |
| `pose_switcher_name`       | string | Yes      | Name of the multi-poses-execution-switch component.                                                           |
| `claws_pose_switcher_name` | string | Yes      | Name of the claws pose switcher component.                                                                    |
| `arm_name`                 | string | Yes      | Name of the arm component used for motion planning and execution.                                             |
| `gripper_name`             | string | Yes      | Name of the gripper component.                                                                                |
| `speech_service_name`      | string | No       | Name of a text-to-speech generic service for spoken greetings.                                                |
| `viz_url`                  | string | No       | URL of a [motion-tools](https://github.com/viam-labs/motion-tools) viz server. When set, the frame system is drawn before each motion plan, useful for debugging collisions and frame placement. |
| `brew_time_sec`            | float  | No       | Espresso brew duration in seconds (default: 8).                                                               |
| `lungo_brew_time_sec`      | float  | No       | Lungo brew duration in seconds (default: 15).                                                                 |
| `grind_time_sec`           | float  | No       | Bean grinding duration in seconds, applied to both regular and decaf grinders (default: 7.5).                 |
| `place_cup`                | bool   | No       | Enable cup placement step in the brew cycle.                                                                  |
| `clean_after_use`          | bool   | No       | Enable cleaning step after each brew.                                                                         |
| `portafilter_shake_sec`    | float  | No       | Duration in seconds of a small circular shake at the `coffee_shake` pose during `unlock_portafilter`, to dislodge a stuck puck. Requires a `coffee_shake` pose in the filter pose switcher. Defaults to 0 (disabled). |
| `save_motion_requests_dir` | string | No       | Directory to save motion request payloads for debugging.                                                      |
| `order_sensor_name`        | string | No       | Name of a `viam:beanjamin:order-sensor` sensor to notify when each order attempt completes (must appear in **depends_on**). |
| `zoo_cam_storage_name`     | string | No       | Name of the “zoo” camera storage ([`viam:video:storage`](https://github.com/viam-modules/video-store) or compatible); when set, uploads a clip per order attempt (async `save`), with fixed 5s pre-roll and 5s post-roll. |
| `input_range_override`     | object | No       | Narrows joint limits on named frames before motion planning. Outer key is the frame name (typically the arm); inner key is either the joint name or its stringified index (e.g. `"5"` for the last joint of a 6-DoF arm). Each value is `{ "min_degs": number, "max_degs": number }`. |

### DoCommand

**`prepare_order`** - Prepare a drink order with optional speech greetings. Supports `"espresso"` and `"lungo"`.

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
{"count": 2, "orders": ["Alice", "Bob"], "is_paused": false, "is_busy": true}
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

**`reset_world`** - Rebuild the cached frame system from the framesystem service, discarding any mid-cycle mutations (e.g. a portafilter frame that was reparented to world by `lock_portafilter`). Useful after a `cancel` leaves the world in a state where the portafilter is "stuck" attached to the world at wherever the arm abandoned it. Only callable when nothing is running AND the queue is paused (i.e. after `cancel`, or during an inter-order cleanup pause when `clean_after_use` is false). Does not move the arm — if you want to re-home, run `execute_action` afterward.

```json
{"reset_world": true}
```

Returns `{"status": "reset"}`.

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

> **⚠️ Deprecated.** This model is deprecated and will be removed in a future release. Migrate to [`viam:conversation-bundle:text-to-speech`](https://app.viam.com/module/viam/conversation-bundle), which offers the same functionality and is actively maintained. Existing configurations continue to work, but you should plan to move off this model.

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

**`say_async`** — Queue text for playback and return immediately without waiting for synthesis or playback to finish. A background worker drains the queue and plays items sequentially. Audio is only sent to the speaker when no other speech (sync or async) is currently playing, so queued messages will never overlap with an in-flight `say` call. Returns an error if the async queue is full (capacity 64).

```json
{"say_async": "Hello, your espresso is ready!"}
```

Returns:

```json
{"queued": "Hello, your espresso is ready!"}
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

## Model: `viam:beanjamin:order-sensor`

**API:** `rdk:component:sensor`

Receives a summary of each order attempt from the `viam:beanjamin:coffee` service. Configure the coffee service with `order_sensor_name` set to this component’s name, and add this sensor under the coffee resource’s **depends_on**.

**Each reading is returned at most once** from `Readings`. When there is no queued reading, `Readings` returns [`data.ErrNoCaptureToStore`](https://pkg.go.dev/go.viam.com/rdk/data#pkg-variables) (and a nil readings map), which Data Management treats as “nothing to store” until the next order completes.

### Configuration

```json
{}
```

No attributes. Wire the sensor through the coffee service as described above.

### Readings

With nothing queued, `Readings` returns **`ErrNoCaptureToStore`** and no readings map (clients should use `data.IsNoCaptureToStoreError` in Go).

After each order attempt completes (success, failure, or panic), the **next** `Readings` call returns something like:

```json
{
  "order_id": "<uuid>",
  "drink": "espresso",
  "customer_name": "Alice",
  "order_ok": true,
  "error_message": "",
  "start_time": "2026-04-01T12:00:00.000000000Z",
  "end_time": "2026-04-01T12:02:05.000000000Z",
  "duration_ms": 125000
}
```

`start_time` and `end_time` are UTC RFC3339Nano timestamps: wall clock from when queue processing begins for that order through when the attempt finishes (greeting, drink prep, completion speech). `duration_ms` matches `end_time − start_time`. On failure, `order_ok` is `false` and `error_message` is set; panics use a `panic: ...` message. When successful, `error_message` is an empty string.

---

## Model: `viam:beanjamin:customer-detector`

**API:** `rdk:service:generic`

Identifies return customers using facial recognition. Wraps the [`viam:vision:face-identification`](https://github.com/viam-modules/viam-face-identification) vision service to register customer faces (associated with a name and email) and later identify them when they return.

### Prerequisites

- A configured camera component.
- The [`viam:vision:face-identification`](https://app.viam.com/module/viam/face-identification) module added as a vision service, with its `picture_directory` pointing to `<data_dir>/known_faces`.

### Configuration

```json
{
  "camera_name": "<string>",
  "vision_service_name": "<string>",
  "data_dir": "<string>",
  "confidence_threshold": <float>,
  "min_face_area_fraction": <float>
}
```

| Name                     | Type   | Required | Description                                                                                     |
| ------------------------ | ------ | -------- | ----------------------------------------------------------------------------------------------- |
| `camera_name`            | string | Yes      | Name of the camera component used to capture customer photos.                                   |
| `vision_service_name`    | string | Yes      | Name of the `face-identification` vision service dependency.                                    |
| `data_dir`               | string | Yes      | Directory for storing known face images and customer records. Must match the vision service's `picture_directory` parent (i.e. the vision service's `picture_directory` should be `<data_dir>/known_faces`). |
| `confidence_threshold`   | float  | No       | Minimum confidence score to consider a face match. Defaults to `0.5`.                           |
| `min_face_area_fraction` | float  | No       | Minimum fraction of the (center-cropped) image area a detected face bounding box must cover to be considered for identification. Defaults to `0.08` (face spans ~28% of the frame linearly). |

### Example Configuration

```json
{
  "camera_name": "customer-cam",
  "vision_service_name": "face-detector",
  "data_dir": "/data/customers",
  "confidence_threshold": 0.6,
  "min_face_area_fraction": 0.08
}
```

The face-identification vision service should be configured with `picture_directory` set to `/data/customers/known_faces` (matching the `data_dir` above). Both modules must share this path so the customer-detector can write face images that the vision service reads.

### DoCommand

**`register_customer`** — Capture a single photo from the camera, save it as a known face, and associate it with the customer's name and email. Call this multiple times during a registration session to capture different angles (front, left, right, etc.). Does **not** trigger embedding recomputation — call `finish_registration` when done.

```json
{
  "register_customer": {
    "name": "Alice Smith",
    "email": "alice@example.com"
  }
}
```

Returns:

```json
{
  "registered": "alice@example.com",
  "name": "Alice Smith",
  "image_path": "/data/customers/known_faces/alice@example.com/face_1.jpeg"
}
```

**`finish_registration`** — Call after capturing all face images for a customer. Triggers the vision service to recompute its embeddings so the new faces become recognisable.

```json
{"finish_registration": "alice@example.com"}
```

Returns:

```json
{"email": "alice@example.com", "name": "Alice Smith", "face_images": 5}
```

**`identify_customer`** — Capture a photo and attempt to match the face against registered customers.

```json
{"identify_customer": true}
```

Returns (match found):

```json
{
  "identified": true,
  "name": "Alice Smith",
  "email": "alice@example.com",
  "confidence": 0.87,
  "is_registered": true
}
```

Returns (no match):

```json
{
  "identified": false,
  "message": "no known customer detected",
  "num_detections": 0
}
```

**`list_customers`** — List all registered customer emails.

```json
{"list_customers": true}
```

Returns:

```json
{"customers": ["alice@example.com", "bob@example.com"], "count": 2}
```

**`remove_customer`** — Remove a customer and their face images.

```json
{"remove_customer": "alice@example.com"}
```

Returns:

```json
{"removed": "alice@example.com"}
```

### Storage

Customer records (name, email, image directory) are persisted to `<data_dir>/customers.json`. Face images are stored under `<data_dir>/known_faces/<email>/` — one subdirectory per customer, which is the directory structure the face-identification vision service expects. Registering the same customer multiple times adds additional face samples, improving recognition accuracy.

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

