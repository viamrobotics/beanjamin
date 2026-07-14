# Beanjamin Module

The `viam:beanjamin` module provides these models for arm-based automation workflows:

1. **`viam:beanjamin:coffee`** - A generic service that orchestrates a full coffee brew cycle by sequentially moving through all poses on a pose switcher.
2. **`viam:beanjamin:multi-poses-execution-switch`** - A switch component that moves an arm between predefined poses using the Motion service.
3. **`viam:beanjamin:maintenance-sensor`** - A sensor component that reports whether the system is safe for maintenance (arm idle, no orders running or queued).
4. **`viam:beanjamin:order-sensor`** - A sensor that yields one reading per completed order (start/end timestamps and outcome) when wired from the coffee service.
5. **`viam:beanjamin:dial-control-motion`** - A generic service that translates Stream Deck dial inputs into relative arm motions.
6. **`viam:beanjamin:customer-detector`** - A generic service that identifies return customers via facial recognition using the [`viam:vision:face-identification`](https://github.com/viam-modules/viam-face-identification) vision service.

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
  "slow_movement_vel_degs_per_sec": 25,
  "portafilter_shake_sec": 2.5,
  "save_motion_requests_dir": "/tmp/motion-requests",
  "order_sensor_name": "order-events",
  "cam_storage_mux_name": "video-store-mux",
  "slack_notifier_name": "slack-notifier",
  "cup_vision_service_name": "cup-vision",
  "src_camera_name": "cam",
  "camera_observe_pose_switcher_name": "camera-observe-switch",
  "cup_approach_relative_pose": { "x": -80, "y": 0, "z": 0, "o_x": 0, "o_y": 0, "o_z": 1, "theta": 0 },
  "cup_grab_relative_pose": { "x": -20, "y": 0, "z": 0, "o_x": 0, "o_y": 0, "o_z": 1, "theta": 0 },
  "serving_approach_relative_pose": { "x": -80, "y": 0, "z": 0, "o_x": 0, "o_y": 0, "o_z": 1, "theta": 0 },
  "serving_grab_relative_pose": { "x": -20, "y": 0, "z": 0, "o_x": 0, "o_y": 0, "o_z": 1, "theta": 0 },
  "input_range_override": {
    "my-arm": {
      "5": { "min_degs": -270, "max_degs": 270 }
    }
  }
}
```

Add a **`viam:beanjamin:order-sensor`** component to the machine, put it in the coffee service **depends_on**, and set `order_sensor_name` to that component’s name. When an order attempt finishes, one reading is queued with `start_time`, `end_time`, `order_ok`, `duration_ms`, and — for observability — `failed_step`, `operator_cancelled`, `trace_id`, the `decaf` path flag, and `error_message` (if applicable).

**Usage sensor.** The optional `usage_sensor_name` field points at a single sensor resource that holds several counters, one per key, updated through the brew lifecycle. Setting the field automatically registers the sensor as a dependency of the coffee service, so no manual `depends_on` entry is required. The sensor must support both the `Readings` API and a `DoCommand({"set": {<key>: <value>}})` that overwrites the named counter (and preserves the others). The coffee service updates each counter with a best-effort read-modify-write: it reads the current value via `Readings`, computes the new value, and writes it back via `DoCommand`. The keys are:

- `regular_grinds` — +1 after each regular (non-decaf) grind
- `decaf_grinds` — +1 after each decaf grind
- `usage` — +1 after a regular brew (espresso/decaf), +1.5 after a lungo brew (lungo/decaf_lungo)
- `cleanings` — +1 after each cleaning cycle
- `successful_consecutive_orders` — +1 after each successful order, reset to 0 after any failed **or** operator-cancelled order

Consumable counters increment only after their step completes successfully, so a brew that fails partway leaves the consumables it actually used counted and does not roll them back. A missing counter key is treated as 0 (so the first update lands a fresh count). All updates are best-effort: a read/write failure logs a warning and never fails the brew. When `usage_sensor_name` is unset, every update is skipped.

Configure a [`viam:video:storage`](https://github.com/viam-modules/video-store) camera on the machine. After each order attempt, the coffee service saves a clip via a `save` DoCommand issued from a background goroutine, so it never blocks the queue. Each clip includes a fixed **N seconds** of pre-roll (ring-buffer permitting) and **N seconds** of post-roll. The save is **synchronous** (`async: false`) so slice failures surface in the logs instead of being dropped silently; because a synchronous slice can only read segment files that have already closed on disk, the goroutine waits roughly one video-store segment (~30s) past the clip's end before issuing the save.

The save request includes a `tags` entry with the order UUID — this is what links clips to orders for cloud data filtering — and a minimal JSON `metadata` blob containing only `order_id` and `order_status` (`ok` or `failed`), which the video-store appends to the clip filename. Clips are saved after every attempt, including failed brews or panics. Failure detail (the error and the step it failed at) is not stored in the clip metadata; it is recorded separately on the order sensor.

**Slack notifications.** The optional `slack_notifier_name` field points at a [`viam:notifications:slack`](https://github.com/viam-modules/notifications) generic service. Setting the field automatically registers it as a dependency of the coffee service, so no manual `depends_on` entry is required. When set, the coffee service sends a best-effort Slack message on every **non-successful** order attempt — both genuine faults and operator cancels — via `DoCommand({"command": "send", "blocks": [...], "text": ...})`. The message is laid out with [Slack Block Kit](https://api.slack.com/block-kit) and mirrors the per-attempt fields the order sensor records, so it's a self-contained record: a header that distinguishes a fault (`:x: Order failed`) from an operator cancel (`:warning: Order cancelled by operator`), a fields section with the drink, customer, the step it failed (or was cancelled) at, the duration, and the `decaf` flag, the error in a code block (faults only), and a context footer with the order ID, trace ID, start time, and — when the module is cloud-connected — clickable `app.viam.com` deep-links to this machine's logs (built from the `VIAM_MACHINE_ID` / `VIAM_PRIMARY_ORG_ID` environment variables Viam injects) and, when `cam_storage_mux_name` is configured, to the order's video clip (a data page filtered by the order-ID tag, scoped to `VIAM_LOCATION_ID`). All links are omitted on a local or test machine where those environment variables are unset. Because the clip uploads asynchronously *after* the notification is sent, the clip link may show no results for the first ~15–60s. The flat `text` value is sent alongside as the notification/accessibility fallback Slack uses when blocks can't render. Sends run off the queue goroutine (so a slow Slack call never stalls the next order) and are bounded by a 10-second timeout; a send failure logs a warning and never affects brewing. The Slack channel/credentials (bot token or webhook URL) are configured on the notifier service itself. When `slack_notifier_name` is unset, no notifications are sent.

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
| `gripper_hold_min_pos`     | float  | No       | Gripper jaw position (0–850) below which the gripper is considered closed/empty. Positions in `[min, max]` mean an object (cup or glass) is held; used to verify grabs and self-heal an open gripper at brew-cycle start (default: 430).                 |
| `gripper_hold_max_pos`     | float  | No       | Gripper jaw position (0–850) above which the gripper is considered open (default: 685).                       |
| `slow_movement_vel_degs_per_sec` | float | No    | Max joint velocity (degrees/sec) used when a step has a `LinearConstraint` without explicit `MoveOptions`, as well as for pivot and circular motions. Raise carefully — precision and contact steps rely on this (default: 25). |
| `portafilter_shake_sec`    | float  | No       | Duration in seconds of a small circular shake at the `coffee_shake` pose during `unlock_portafilter`, to dislodge a stuck puck. Requires a `coffee_shake` pose in the filter pose switcher. Defaults to 0 (disabled). |
| `save_motion_requests_dir` | string | No       | Directory to save debugging payloads. Each plan writes a single request+response JSON (RDK's `WriteRequestAndResponseToFile`; readable back with `ReadRequestAndResponseFromFile`, and the response is absent when planning failed), nested under `tag=<order-id>/tag=step_<step>/tag=motion_<move\|pivot\|circular\|carry>/tag=planning_<success\|failure>/`. When this directory is a Viam data-synced capture dir, the data manager reads those `tag=` segments and tags each uploaded file, so plans are searchable on the data page by order, step, motion type, and planning outcome — and a failed order's Slack notification deep-links to that order's plan requests (which the reader can narrow to `planning_failure` or a specific step). Also writes a `<timestamp>_<cup\|glass>_framesystem.json` snapshot on each cup/glass observation (the frame system with the detected item geometries added as static world frames), which can be read back into a `referenceframe.FrameSystem` and drawn in a local motion-tools visualizer. |
| `order_sensor_name`        | string | No       | Name of a `viam:beanjamin:order-sensor` sensor to notify when each order attempt completes (must appear in **depends_on**). |
| `usage_sensor_name`        | string | No       | Name of a single sensor whose per-key counters are updated through the brew lifecycle: `regular_grinds`, `decaf_grinds`, `usage`, `cleanings`, and `successful_consecutive_orders`. See "Usage sensor" below. |
| `cam_storage_mux_name` | string | No   | Name of a [`viam:multiplexer:resource-multiplexer`](https://github.com/viam-modules/multiplexer) generic service whose dependencies are `viam:video:storage` stores; when set, saves a clip per order attempt (synchronous `save`) to all configured stores. |
| `data_dir`                 | string | No       | Directory for persistent module data. When set alongside `cam_storage_mux_name`, a pending-clip record is written under `<data_dir>/pending-clips` when each order starts and removed only once that order's clip has been saved successfully — a save that fails (or never runs because the process died first) leaves the record in place. Use with a Viam scheduled job calling `cleanup_pending_clips` to recover clips for any order whose save was interrupted or failed. |
| `slack_notifier_name`      | string | No       | Name of a [`viam:notifications:slack`](https://github.com/viam-modules/notifications) generic service. When set, the coffee service sends a best-effort Slack message on every non-successful order attempt (faults and operator cancels). See "Slack notifications" above. |
| `customer_detector_name`   | string | No       | Name of a `viam:beanjamin:customer-detector` service. When set, the coffee service credits each **successfully** completed order (when the `prepare_order` carried a `customer_email`) to that customer's order history via the detector's `record_order` DoCommand, powering "the usual". Setting the field automatically registers it as a dependency. Unset disables order-history recording. |
| `input_range_override`     | object | No       | Narrows joint limits on named frames before motion planning. Outer key is the frame name (typically the arm); inner key is either the joint name or its stringified index (e.g. `"5"` for the last joint of a 6-DoF arm). Each value is `{ "min_degs": number, "max_degs": number }`. |
| `conversational`           | bool   | No       | When true, the coffee service speaks its own greetings, almost-ready prompts, order-received lines, and rejection quips through `speech_service_name`. When false (default), the service stays silent except for the drink-ready announcement at cup handoff — leaving the rest of the talking to an external orchestrator (e.g. `viam:conversation-bundle:voice-command`). |
| `cup_vision_service_name`             | string | Yes      | Name of a `rdk:service:vision` segmenter that returns cup detections via `GetObjectPointClouds`. Cup pickup is always vision-guided — the arm detects the empty cup rather than grabbing from a fixed pose. |
| `src_camera_name`                     | string | Yes      | Source camera the vision service segments from. Must be present in the frame system. |
| `camera_observe_pose_switcher_name`   | string | Yes      | Switcher holding the camera observation vantages. Poses are swept **one at a time** and vision run at each (within-pose near-duplicates within 40 mm collapsed); at each pose that sees a cup the arm tries to grab the candidates closest-first, and the sweep stops as soon as a cup is in hand. When a pose's cups are all unreachable the sweep **continues to the remaining poses**, so a cup reachable only from a later vantage is still found before the machine gives up. Must include a pose named `cup_observe` (the home/recovery pose), and all poses must move the `cam` frame (set the switch's `component_name` to `cam`) |
| `cup_approach_relative_pose`          | object | Yes      | 6-DoF offset composed onto the detected cup centroid for the pre-grab pose. Shape `{ "x", "y", "z", "o_x", "o_y", "o_z", "theta" }`; same gripper orientation as the grab pose but translated further back from the cup. **Not** stored on the pose switch — it's an offset, not a real world-frame pose. |
| `cup_grab_relative_pose`              | object | Yes      | 6-DoF offset composed onto the detected cup centroid for the final grab pose. Same shape as `cup_approach_relative_pose`; gripper orientation for a side-grab with a small translation onto the cup. |
| `cup_photos_per_vantage`              | int    | No       | How many vision frames to capture at each observation pose. Every detection from every frame at that pose is merged before ranking. Default 1. |
| `cup_pickup_max_attempts`             | int    | No       | Cap on full observe-and-grab attempts per order. Each attempt sweeps **every** observe pose, grabbing the first reachable cup across all of them (closest-first within each pose, continuing to later poses when a pose's cups are all unreachable). When a whole sweep grabs nothing the machine re-observes and retries — asking the customer to place a cup (none seen anywhere) or to nudge the cups (some seen but none reachable) between attempts. Default 3. |
| `cup_dimensions`                      | object | No       | Predefined cup size, overriding the dimensions derived from the detection point cloud. Shape `{ "diameter_mm", "height_mm" }` (both must be > 0). When set, the held-item bounding box is built with width = depth = `diameter_mm` and height = `height_mm` (a square-footprint box approximating the round cup), centered on the **grasp centroid** (the point the gripper is sent to) rather than the point-cloud midpoint. The known `height_mm` also drives **resting-surface seating** (below): the grasp Z is derived from the surface the cup stands on instead of the noisy detected Z. Use when the point cloud under-reads or skews the box for a partially-observed cup. Unset (default) uses the point-cloud extents and the raw detected Z. |
| `max_batch_size`           | int    | No       | Cap on `prepare_order.count` — how many identical drinks one DoCommand may enqueue at once. Defaults to 10 when unset. Protects the queue against runaway voice commands or LLM hallucinations. |
| `can_serve_decaf`          | bool   | No       | Enables the `decaf` and `decaf_lungo` drinks, which grind from the decaf grinder instead of the regular one. Orders for those drinks are rejected when this is `false`. Default `false`. |
| `can_serve_iced`           | bool   | No       | Enables the `iced_coffee` drink. When `true`, after brewing the espresso the arm vision-detects a glass off the top shelf, dispenses ice into it via `ice_board_name`/`ice_pin_name`, sets the glass in a staging area, then pours the espresso over the ice. Both finished items — the empty espresso cup and the iced glass — are then placed in the serving area at the next round-robin slots (two slots are consumed per order). The glass is always vision-detected, so iced coffee requires `ice_board_name`, `ice_pin_name`, the `glass_*` vision fields below, and the iced claws poses below. A `serving-area` (or `serving-area_origin`) Box geometry must exist in the framesystem; this is checked at runtime, not at config time. Default `false`. |
| `ice_board_name`           | string | When `can_serve_iced` is enabled | Name of a `rdk:component:board` whose GPIO pin triggers the ice machine. |
| `ice_pin_name`             | string | When `can_serve_iced` is enabled | Board pin held HIGH to dispense ice. Required — there is no default pin. |
| `ice_dispense_sec`         | float  | No       | How long the ice pin is held HIGH per drink, in seconds. Defaults to 5. |
| `pour_vel_degs_per_sec`    | float  | No       | Max joint velocity (degrees/sec) for the pour tilt and return-upright pivots. Overrides the general slow-movement velocity so the pour isn't dragged out (default: 60). Lower it if the espresso splashes over the ice. |
| `pour_acc_degs_per_sec2`   | float  | No       | Max joint acceleration (degrees/sec²) for the pour pivots. The tilt is a short move that's usually acceleration-limited, so this — not velocity — is what actually snaps it faster. Default `0` leaves the arm's own acceleration in place; raise it to speed the tilt, watching for splash and joint stress. |
| `glass_vision_service_name`           | string | When `can_serve_iced` is enabled | Name of a `rdk:service:vision` segmenter that returns glass detections via `GetObjectPointClouds`. Glass pickup mirrors cup pickup but with its own vision service and observe poses (tuned for the taller iced-coffee glass); it shares the cup camera (`src_camera_name`). |
| `glass_observe_pose_switcher_name`    | string | When `can_serve_iced` is enabled | Switcher holding the glass observation vantages (swept one at a time, same as the cup observe switch). Must include a pose named `glass_observe` (home/recovery), and all poses must move the `cam` frame. |
| `glass_approach_relative_pose`        | object | When `can_serve_iced` is enabled | 6-DoF gripper offset composed onto the detected glass centroid for the pre-grab pose (same shape as `cup_approach_relative_pose`), tuned for the taller glass. |
| `glass_grab_relative_pose`            | object | When `can_serve_iced` is enabled | 6-DoF gripper offset for the final glass grab pose. |
| `glass_dimensions`                    | object | No       | Predefined glass size, overriding the dimensions derived from the detection point cloud. Same shape and behavior as `cup_dimensions` (`{ "diameter_mm", "height_mm" }`, both > 0), applied to the glass held-item geometry; its `height_mm` likewise drives resting-surface seating (below). Unset (default) uses the point-cloud extents and the raw detected Z. |
| `serving_approach_relative_pose`      | object | Yes      | 6-DoF gripper offset composed onto the serving-area slot anchor for the pre-release approach pose (same shape as `cup_approach_relative_pose`). Used for both the hot cup and the iced glass. |
| `serving_grab_relative_pose`          | object | Yes      | 6-DoF gripper offset composed onto the serving-area slot anchor for the release pose. Same shape as `serving_approach_relative_pose`; shared by cup and glass placement. |
| `track_held_geometry`                 | bool   | No       | When `true`, the vision-detected geometry of a picked-up cup/glass is attached to the gripper frame in the cached frame system as a `held-item` frame, so motion planning routes around the held item until it is set down (and is restored on each re-grab — the brewed cup from under the machine, the staged glass). The gripper-overlap collision pairs are allowed automatically on every move while an item is held; contact phases near a modeled surface (under the machine, the serving-area shelf) allow the held item against that surface too. The held-item frame is dropped when the frame system is rebuilt (`reset_world`, cancel recovery). Default `false`. |
| `fake_mode`                           | bool   | No       | Test-machine knob. When `true`, `AllowedCollision` entries that reference gripper sub-geometries (e.g. `gripper:claws`, which only exist on the real ufactory gripper) are skipped, so motion plans validate against fake hardware. Leave unset on the real bot. Default `false`. |
| `no_spill_carry`                      | bool   | No       | When `true`, the brewed cup is carried from under the machine to the serving-area shelf along a straight line broken into waypoints (one every 200 mm). Each waypoint commands the **held-item (container) frame** — the start and approach poses are converted onto it — interpolating the pose from the container's **upright** start to the approach pose, with a goal pose cloud (small tilt + wider twist + a little translational slack) that loosens the orientation so the planner has IK room while the drink stays close to level. The waypoints are planned as one multi-goal trajectory; the existing linear descent then settles the cup into the slot. It commands the held-item frame, so it requires `track_held_geometry=true`. The pose-cloud leeways are conservative constants in `coffee/motion.go` (`noSpillGoalCloud`) — which axis is safe to widen depends on the grasp, so tune them on hardware. Default `false` (the carry free-plans straight to the approach pose). |

Glass pickup reuses `cup_photos_per_vantage` and `cup_pickup_max_attempts` (item-agnostic operational knobs); there are no glass-specific versions.

**Resting-surface seating.** When container dimensions are configured (`cup_dimensions` / `glass_dimensions`), pickup resolves each detection's grasp **Z** from the surface the container stands on rather than the raw detected centroid Z (which depth noise pushes above or below the true base). It finds the highest **static** Box in the framesystem whose world footprint lies directly beneath the detection and whose top face is below the detected centroid, then seats the container's base **1 mm** above that top — so the grasp centroid becomes `surfaceTop + 1 mm + height/2`, keeping the detected X/Y. "Static" means world-anchored (it moves rigidly with the world frame), so the moving arm, gripper, camera, and any held item are never mistaken for a surface; non-box and rotated geometries are bounded by their world axis-aligned extent. No framesystem changes or extra config are required — the resting surface is auto-detected. When dimensions are unset or no surface is found beneath a detection, pickup uses the raw detected Z unchanged.

**Serving-area placement.** Every finished cup (and, for iced coffee, the iced glass) is placed on a dedicated served-drinks shelf. Slots are tiled along the shelf's long axis (120 mm spacing, 60 mm margin from each end) on the midline of the shelf top — as many slots as the shelf length allows; the placement anchor is set so the held container's bottom rests on the shelf top. With `track_held_geometry` on, the anchor is half the tracked container's height above the surface (so a taller iced glass is not driven into the shelf the way a fixed offset did); it falls back to a fixed 30 mm when no held-item geometry is tracked. The anchor is composed with `serving_grab_relative_pose` (and `serving_approach_relative_pose` for the approach) to derive the actual claws pose, mirroring how the pickup composes its offsets onto the detected cup centroid. Slots are filled **sequentially (round-robin)**: a process-local counter advances one slot per placement and wraps back to the first slot when it reaches the end, on the assumption that by the time it wraps the earliest-placed cup has been picked up. If the arm cannot plan a path to a slot (approach or descent), that slot is **skipped and the next one is tried**, continuing around the ring until one is reachable (the order fails only if every slot is unreachable). There is no vision-based occupancy check — placement is fully decoupled from pickup observation. The counter resets to the first slot on module restart/reconfigure. Requires a `serving-area` (or `serving-area_origin`) Box geometry in the framesystem; this is checked at runtime, not at config time. An optional `serving-area-shield` Box obstacle may be added to the framesystem to enclose the standing-cup zone above the shelf: it stays a hard obstacle during the lateral carry (so the arm steers clear of cups already on the shelf) but is allowed to be passed through by the gripper, claws, and held container on the linearly constrained descent into a slot and the retreat back out. Size it with clearance above the cups so the approach pose stays outside it; when the frame is absent the allowances are inert and placement behaves as before.

**Iced coffee — required poses on the claws pose switcher (`claws_pose_switcher_name`):**

When `can_serve_iced` is enabled, the claws switch must additionally hold these poses (all moving the `coffee-claws-middle` frame). Calibrate them physically on the machine via `viam robot part motion get-pose`/`set-pose`. The glass itself is vision-detected (see the glass-observe switch below), so there are no static glass-pickup poses.

| Pose name              | Description |
| ---------------------- | ----------- |
| `ice_machine_approach` | Staged in front of the ice chute. |
| `ice_machine_dispense` | Glass held under the chute while the ice pin pulses. |
| `staging_approach`     | Above the staging area where the glass rests during the pour. |
| `staging`              | Down in the staging area; the glass is set here to free the gripper for the pour, then re-grabbed and placed in the serving area. |
| `pour_approach`        | Espresso cup held upright above the staged glass. |
| `pour`                 | Espresso cup tilted to pour over the ice. |

**Cup pickup — required poses on the camera-observe pose switcher (`camera_observe_pose_switcher_name`):**

Cup pickup is always vision-guided, so the dedicated camera-observe switch must hold one or more observation poses, all moving the camera frame `cam` (the switch's `component_name`). The switch must include a pose named `cup_observe`.

| Pose name           | Type                | Description |
| ------------------- | ------------------- | ----------- |
| `cup_observe`       | Absolute world pose | Required. The primary view of the cup workspace and the home/recovery pose the arm returns to between grab attempts. |
| additional poses    | Absolute world pose | Optional extra vantages tried in turn, to recover cups occluded from the primary view or reachable only from a different angle. A pose is visited when earlier poses found no cup **or when the cups they saw were all unreachable**; the sweep stops as soon as a cup is grabbed. An unreachable pose logs a warning and is skipped. |

**Dynamic glass pickup — required poses on the glass-observe pose switcher (`glass_observe_pose_switcher_name`):**

When `can_serve_iced` is enabled, the dedicated glass-observe switch must hold one or more observation poses, all moving the `cam` frame. The switch must include a pose named `glass_observe`. Same sweep semantics as the cup observe switch.

| Pose name           | Type                | Description |
| ------------------- | ------------------- | ----------- |
| `glass_observe`     | Absolute world pose | Required. The primary view of the glass storage area and the home/recovery pose between grab attempts. |
| additional poses    | Absolute world pose | Optional extra vantages tried only when earlier poses found no glass. |

### DoCommand

**`prepare_order`** - Prepare a drink order with optional speech greetings. Supports `"espresso"` and `"lungo"`; `"decaf"`/`"decaf_lungo"` when `can_serve_decaf` is set, and `"iced_coffee"` when `can_serve_iced` is set.

```json
{
  "prepare_order": {
    "drink": "espresso",
    "customer_name": "Alice",
    "customer_email": "alice@example.com",
    "initial_greeting": "optional custom greeting",
    "completion_statement": "optional custom completion message",
    "count": 3
  }
}
```

Only `drink` is required. If `initial_greeting` is omitted, a random greeting is generated. If `customer_name` is provided, it personalizes the greeting and completion messages. If `customer_email` is provided **and** `customer_detector_name` is configured, the completed drink is credited to that customer's order history (see "the usual"). Orders are added to a queue and processed sequentially.

`count` is an optional positive integer (default 1) that enqueues N identical orders in one call — each gets its own UUID. The cap is `max_batch_size` (default 10). When `count > 1`, the response also includes `order_ids: [...]` (one per enqueued order) and `count`; existing `order_id` and `queue_position` keys still refer to the first order so existing callers keep working. To keep audio sane, the per-order "Order received…" line is replaced with a single consolidated batch announcement at submission time; the per-cup drink-ready announcement at cup handoff still fires once per order as each cup completes.

**`execute_action`** - Run a single coffee-making action by name, for manual step-by-step operation. An unknown name returns the full list of available actions in the error. Available actions:

- Brew cycle: `grind_coffee`, `grind_decaf`, `tamp_ground`, `lock_portafilter`, `unlock_portafilter`, `release_filter`, `grab_filter`, `turn_coffee_button_on`, `turn_coffee_button_off`, `brew_coffee`, `set_cup_for_coffee`, `give_full_cup_to_customer` (place the finished cup in the serving area), `clean_portafilter`, `place_held` (place the currently held vessel in the serving area).
- Iced coffee (require `can_serve_iced`): `fetch_glass`, `pulse_ice_pin`, `dispense_ice`, `stage_glass`, `grab_brewed_cup`, `pour_espresso`, `grab_staged_glass`, `serve_iced_coffee` (the full iced sequence end-to-end).

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

**`cleanup_pending_clips`** - Attempt a video save for any remaining pending-clip records under `data_dir`, then remove them. Catches clips whose live save was interrupted (process died during the post-roll wait) or failed (e.g. cam storage unavailable). Records younger than one full clip window plus a segment-flush margin are skipped, so an in-progress order is not double-saved. Intended to be invoked via a Viam scheduled job.

```json
{"cleanup_pending_clips": true}
```

Returns `{"saved": 1, "skipped": 0}`.

**`reset_world`** - Recover the service to a clean idle state from anywhere. In order: cancels any running sequence (waiting for it to actually stop), clears the queue (pending + recently completed), rebuilds the cached frame system from the framesystem service (discarding mid-cycle mutations like a portafilter frame reparented to world by `lock_portafilter`), and releases the cancel-induced queue pause. Safe to call from any state — each step is skipped when not applicable. Does not move the arm — if you want to re-home, run `execute_action` afterward.

```json
{"reset_world": true}
```

Returns `{"status": "reset", "cancelled": true, "cleared": 2, "unpaused": true}` — fields reflect which steps actually fired.

**`run_cup_flow`** - Exercise the full cup-handling path without brewing, `count` times. Each iteration sweeps the camera-observe poses grabbing the first reachable cup across them (closest-first, continuing past a pose whose cups are all unreachable), sets it under the machine, retrieves it, and places it on the next sequential served-shelf slot (round-robin). Intended for tuning the observe-pose sweep and shelf placement on hardware.

Assumes the portafilter has been **physically removed** from the claws — the flow never touches portafilter state. Honors `cancel`. The value is the iteration count (`>= 1`); `true` runs a single iteration.

```json
{"run_cup_flow": 5}
```

Returns `{"status": "complete", "iterations": 5}`.

**`action`** - Control the gripper. Supported values: `"open_gripper"`, `"close_gripper"`.

```json
{"action": "open_gripper"}
```

Returns `{"status": "opened"}` or `{"status": "closed", "grabbed": true}`.

---

## Model: `viam:beanjamin:dial-control-motion`

**API:** `rdk:service:generic`

Translates Stream Deck dial inputs into relative arm motions. Each dial tick contributes a step (mm for translations, degrees for rotations) along the chosen axis. The service tracks the absolute dial position between calls to determine direction (handling rollover at the dial range boundaries) and accumulates pending motion in a per-axis bucket. A background drain loop flushes accumulated motion to the arm at `drain_interval_ms`, applying a per-axis acceleration multiplier — single detents stay at 1× for fine control, while rapid spinning amplifies motion non-linearly.

### Configuration

```json
{
  "arm_name": "my-arm",
  "dial_move_x_mm": 5,
  "dial_move_y_mm": 5,
  "dial_move_z_mm": 5,
  "dial_move_orientation_mm": 5,
  "dial_move_rx_deg": 2,
  "dial_move_ry_deg": 2,
  "dial_move_rz_deg": 2,
  "dial_max_position": 100,
  "drain_interval_ms": 20,
  "accel_threshold_count": 1,
  "accel_max_multiplier": 10,
  "accel_exponent": 1.5,
  "accel_smoothing_alpha": 0.4
}
```

| Name                              | Type   | Required | Default        | Description                                                                                                                            |
| --------------------------------- | ------ | -------- | -------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `arm_name`                        | string | Yes      | —              | Name of the arm component to move.                                                                                                     |
| `dial_move_x_mm`                  | float  | No       | `1`            | Base millimeters per dial detent on the X axis.                                                                                        |
| `dial_move_y_mm`                  | float  | No       | `1`            | Base millimeters per dial detent on the Y axis.                                                                                        |
| `dial_move_z_mm`                  | float  | No       | `1`            | Base millimeters per dial detent on the Z axis.                                                                                        |
| `dial_move_orientation_mm`        | float  | No       | `1`            | Base millimeters per dial detent along the tool's orientation vector.                                                                  |
| `dial_move_rx_deg`                | float  | No       | `1`            | Base degrees per dial detent rotating around the body's local X.                                                                       |
| `dial_move_ry_deg`                | float  | No       | `1`            | Base degrees per dial detent rotating around the body's local Y.                                                                       |
| `dial_move_rz_deg`                | float  | No       | `1`            | Base degrees per dial detent rotating around the body's local Z.                                                                       |
| `dial_max_position`               | float  | No       | `100`          | Maximum dial position value, used for rollover detection.                                                                              |
| `drain_interval_ms`               | int    | No       | `20` (50 Hz)   | Flush cadence in milliseconds. Detents arriving within a window are summed before being applied.                                       |
| `accel_threshold_count`           | float  | No       | `1`            | Translation: smoothed-detent count at which multiplier reaches `1×`. Below this it's pinned to `1×`. Default of `1` ramps from the first detent. |
| `accel_max_multiplier`            | float  | No       | `10`           | Translation: upper bound on the acceleration multiplier at high spin rates.                                                            |
| `accel_exponent`                  | float  | No       | `1.5`          | Translation: curve shape, `1` linear, `2` quadratic. Multiplier = `clamp((smoothed/threshold)^exponent, 1, max)`.                      |
| `accel_smoothing_alpha`           | float  | No       | `0.4`          | Translation: EWMA factor in `(0, 1]` across drain windows. `1` = no smoothing (instant); smaller = smoother / laggier.                 |
| `accel_rotation_threshold_count`  | float  | No       | translation    | Rotation override for `accel_threshold_count`. Falls back to the translation value if unset.                                           |
| `accel_rotation_max_multiplier`   | float  | No       | translation    | Rotation override for `accel_max_multiplier`. Falls back to the translation value if unset.                                            |
| `accel_rotation_exponent`         | float  | No       | translation    | Rotation override for `accel_exponent`. Falls back to the translation value if unset.                                                  |
| `accel_rotation_smoothing_alpha`  | float  | No       | translation    | Rotation override for `accel_smoothing_alpha`. Falls back to the translation value if unset.                                           |

### DoCommand

**`dial_move_x`** / **`dial_move_y`** / **`dial_move_z`** - Enqueue a translation along the named axis from a Stream Deck dial value. The first call for a given axis calibrates the dial position and does not move the arm.

```json
{"dial_move_x": 50}
```

Returns `{"status": "queued", "axis": "x", "step": 5.0}` or `{"status": "dial_initialized", "axis": "x", "position": 50}` on first call.

**`dial_move_orientation`** - Enqueue a translation along the current tool orientation vector.

```json
{"dial_move_orientation": 50}
```

**`dial_move_rx`** / **`dial_move_ry`** / **`dial_move_rz`** - Enqueue a rotation around the named world axis. Step magnitude is in degrees per detent.

```json
{"dial_move_rx": 50}
```

**`toggle_axis_mode`** - Flip the dial-mode for X/Y/Z dials between translation and rotation. Bind this to a Stream Deck button to repurpose the dials live. While in rotation mode, `dial_move_x` is routed to `rx` (and similarly for y/z); `dial_move_orientation` is unaffected.

```json
{"toggle_axis_mode": true}
```

Returns `{"status": "toggled", "axis_mode": "rotation"}`.

**`set_axis_mode`** - Set the mode explicitly (idempotent). Value must be `"translation"` or `"rotation"`.

```json
{"set_axis_mode": "rotation"}
```

Returns `{"status": "set", "axis_mode": "rotation"}`.

**`get_axis_mode`** - Read the current mode without changing it.

```json
{"get_axis_mode": true}
```

Returns `{"axis_mode": "translation"}`.

> **Removed:** `dial_move_speed` no longer exists. The new acceleration model (`accel_threshold_count` / `accel_max_multiplier` / `accel_exponent`) replaces it. Stream Deck profiles bound to `dial_move_speed` will receive an error and need to be remapped.

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
  "operator_cancelled": false,
  "error_message": "",
  "failed_step": "",
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "decaf": false,
  "start_time": "2026-04-01T12:00:00.000000000Z",
  "end_time": "2026-04-01T12:02:05.000000000Z",
  "duration_ms": 125000
}
```

`start_time` and `end_time` are UTC RFC3339Nano timestamps: wall clock from when queue processing begins for that order through when the attempt finishes (greeting, drink prep, completion speech). `duration_ms` matches `end_time − start_time`. On failure, `order_ok` is `false` and `error_message` is set; panics use a `panic: ...` message. When successful, `error_message` is an empty string.

The remaining fields exist to support observability (per-step error rates and failure investigation):

- **`failed_step`** — the step label the order errored at (e.g. `"Brewing"`, `"Grinding"`), matching the `setStep` labels surfaced through `get_queue`. Empty on success. Count readings by `failed_step` to see where orders die.
- **`operator_cancelled`** — `true` when the failure was an operator `cancel` (a `context.Canceled` interruption), not a genuine fault. **Exclude these from step error-rate metrics** so intentional cancellations don't inflate failure counts. `failed_step` is still populated (it marks where the cancel interrupted).
- **`trace_id`** — the OpenTelemetry trace ID for the order. Use it to jump from a failed reading to the order's full distributed trace (every motion plan and step span). Empty if no trace context was present.
- **`decaf`** — whether the order took the decaf grinder branch, so you can tell why a given step ran (or didn't) without cross-referencing the coffee service config. Derived from the drink.

A per-step error rate is then `count(failed_step == X AND NOT operator_cancelled) / count(all orders)`.

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

**`record_order`** — Append a completed drink to a customer's order history (the data behind "the usual"). The coffee service calls this automatically after a successful brew when `customer_detector_name` is configured and the order carried a `customer_email`; you can also call it directly. An unknown email is a no-op (not an error), so it's safe to call for anonymous walk-ups.

```json
{"record_order": {"email": "alice@example.com", "drink": "espresso"}}
```

Returns:

```json
{"recorded": true, "email": "alice@example.com", "drink": "espresso"}
```

**`get_usual`** — Return a customer's usual drink, derived from their recorded order history. Returns `{"has_usual": false}` when the customer is unknown or has no history.

```json
{"get_usual": "alice@example.com"}
```

Returns:

```json
{"has_usual": true, "drink": "espresso", "count": 7}
```

**`get_info`** — Return static service info. Currently `{"camera_name": <short name>}` for the camera the detector is wired to.

```json
{"get_info": true}
```

### `Status()`

`Status()` reports the customer currently in front of the camera and their usual, so a poller (notably `viam:conversation-bundle:voice-command`'s `command_status`) can greet them by name and offer their usual. It runs a best-effort identification and folds in `get_usual`; results are cached briefly so per-turn polling doesn't re-run the vision model on every call.

When a registered customer is recognized:

```json
{
  "recognized": true,
  "name": "Alice Smith",
  "email": "alice@example.com",
  "confidence": 0.87,
  "usual_drink": "espresso",
  "usual_count": 7
}
```

Otherwise: `{"recognized": false}`.

### Storage

Customer records (name, email, image directory, order history) are persisted to `<data_dir>/customers.json`. Order history is capped at the most recent 50 entries per customer. Face images are stored under `<data_dir>/known_faces/<email>/` — one subdirectory per customer, which is the directory structure the face-identification vision service expects. Registering the same customer multiple times adds additional face samples, improving recognition accuracy.

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

