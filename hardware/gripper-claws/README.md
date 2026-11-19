# Gripper claws

The printed finger fitted to the arm's uFactory gripper. The claw is two of these, so print the STL twice. It holds the portafilter by its printed handle (`../portafilter-handle/`): the claw's pegs seat in the handle's two dimples, which keeps the grip from slipping while the arm locks and unlocks the portafilter.

| File | Extent (mm) | Volume | Print |
| --- | --- | --- | --- |
| `barista-gripper.stl` | 31 × 32 × 115 | 51 cm³ | ×2, one per jaw |

The STL is exported from SolidWorks in millimetres. There is no STEP source, so changes mean remodelling rather than editing this file.

## What depends on it

The coffee service's motion planning models the claws, not this mesh. If a reprint changes the finger's shape or length, update these to match:

- **`gripper:claws` geometry:** reported by the gripper component.
- **`coffee-claws-middle` frame:** the claws' obstacle.
- **`grip-point` frame:** the tool point every claw pose and relative pose is taught against. A longer or shorter claw moves it, and every taught pose with it.

Any other part the claws need to hold should copy the portafilter handle's grip profile and peg dimples, so the claws close on it the same way.
