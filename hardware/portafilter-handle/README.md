# Portafilter handle

The printed handle the arm grips the portafilter by. The claws (`../gripper-claws/`) close on its grip and their pegs seat in its two dimples, so the handle stays put in the jaws while the arm grinds, tamps, and locks the portafilter into the group head.

| File | Extent (mm) | Volume |
| --- | --- | --- |
| `barista-handle-ss.stl` | 37 × 24 × 133 | 69 cm³ |

The STL is exported from SolidWorks in millimetres. There is no STEP source, so changes mean remodelling rather than editing this file.

## What depends on it

- **`portafilter-handle` frame:** the handle's obstacle, under the `filter` frame. `coffee_locked_final_grip_relative_pose` and `filter_released_grip_relative_pose` are taught relative to it, so a reprint with a different grip position means re-teaching them (see the coffee service section of the top-level README).
- **`filter` frame:** sits 95 mm ahead of `grip-point`, along the tool. A handle that puts the portafilter at a different distance from the grip moves it.
