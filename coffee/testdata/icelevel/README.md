# Ice-level test fixtures

One color frame per hand-filled level, captured at `ice_machine_dispense` on
`cappuccina-main` (2026-08-28). The filename is the true fill percentage.

Expected results with the shipping measurement — contrast step, ROI x 700-910 /
y 280-670, window 48, min-contrast 25, full glass 426 px, intercept -94 px:

| fixture | step | ice px | fill fraction | note |
|---|---|---|---|---|
| `fill_0.jpg` | 10 | 0 | 0 | empty; step is below min-contrast |
| `fill_30.jpg` | 8 | 0 | 0 | **below the blind floor** — reads empty despite being filled |
| `fill_40.jpg` | 58 | 75 | 0.40 | lowest level that registers |
| `fill_80.jpg` | 78 | 236 | 0.77 | |
| `fill_100.jpg` | 90 | 335 | 1.00 | |

These are single-frame values. The plan quotes 5-frame means for the same levels
(78 / 243 px at 40% / 80%), so small differences are per-frame noise, not a bug.

**Zero pixels must map to fill 0, not through the linear formula.** With
intercept -94, running px=0 through `(px - intercept) / fullPx` yields 0.22 — a
fifth-full reading for a glass the camera cannot see into. Short-circuit it: no
surface found means fraction 0.

`fill_30.jpg` is the important one: it is genuinely 30% full and the camera
cannot see it, because the ice machine's ledge occludes the glass base. Any
change that makes it report nonzero has broken the floor, not fixed it.

The other test worth having is synthetic: scale `fill_0.jpg`'s luminance up and
confirm it still reads 0. An absolute-brightness threshold reports a
brightly-lit empty glass as FULL, and the contrast step exists to prevent that.

The full 148 MB capture set (five frames per level, point clouds, depth frames,
poses, intrinsics) is not in the repo. Regenerate with `cmd/cli`'s
`ice-snapshot`; see ICE_LEVEL_PLAN.md.
