# Beanjamin Module

The `viam:beanjamin` module provides two models for arm-based automation workflows:

1. **`viam:beanjamin:coffee`** - A generic service placeholder for coffee machine control (not yet implemented).
2. **`viam:beanjamin:multi-poses-execution-switch`** - A switch component that moves an arm between predefined poses using the Motion service.

It also ships a CLI (`beanjamin-cli`) for ad-hoc arm pose queries and movements.

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

| Name              | Type     | Required | Description |
|-------------------|----------|----------|-------------|
| `component_name`  | string   | Yes      | Name of the arm component to move. |
| `motion`          | string   | Yes      | Name of the motion service (typically `"builtin"`). |
| `reference_frame` | string   | No       | Reference frame for poses. Defaults to `"world"`. |
| `poses`           | array    | Yes      | One or more named poses. Each pose needs a `pose_name` and position/orientation fields. |

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

| Method                 | Description |
|------------------------|-------------|
| `GetNumberOfPositions` | Returns the total number of poses and their names. |
| `GetPosition`          | Returns the index of the current pose (0-based). |
| `SetPosition(index)`   | Moves the arm to the pose at the given index. |

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

Placeholder service for future coffee machine control. Currently has no configuration attributes and `DoCommand` is not implemented.

---

## CLI: `beanjamin-cli`

A standalone tool for querying and commanding arm poses on a running Viam machine.

Authentication can be provided via flags or environment variables (`VIAM_API_KEY`, `VIAM_API_KEY_ID`).

### `get-pose`

Get the current pose of a component in the world frame.

```bash
beanjamin-cli get-pose \
  --address <machine-address> \
  --api-key <key> --api-key-id <key-id> \
  --component-name arm
```

Output:

```
Component: arm
Frame:     world
Position:  x=100.00  y=200.00  z=300.00 (mm)
Orientation: ox=0.0000  oy=0.0000  oz=1.0000  theta=0.00 (deg)
```

### `move-to-pose`

Move an arm to a specified pose via the Motion service. Automatically builds a world state from the robot's frame system for obstacle avoidance.

```bash
beanjamin-cli move-to-pose \
  --address <machine-address> \
  --api-key <key> --api-key-id <key-id> \
  --component-name arm \
  --x 100 --y 200 --z 300 \
  --ox 0 --oy 0 --oz 1 --theta 0 \
  --frame world
```

| Flag               | Default   | Description |
|--------------------|-----------|-------------|
| `--address`        | (required)| Machine gRPC address. |
| `--api-key`        | env var   | API key for auth. |
| `--api-key-id`     | env var   | API key ID for auth. |
| `--component-name` | `arm`     | Name of the component to move/query. |
| `--x/y/z`          | `0`       | Position in mm. |
| `--ox/oy/oz`       | `0,0,1`   | Orientation axis vector. |
| `--theta`          | `0`       | Rotation angle in degrees. |
| `--frame`          | `world`   | Reference frame for the destination. |
