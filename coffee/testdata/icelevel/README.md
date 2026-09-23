# Ice-level test fixtures

Two seatings of the same glass at `ice_machine_dispense` on `cappuccina-main`,
exercised by `coffee/ice_level_test.go`. Two, because where the glass hangs in
the jaws is the failure mode the measurement is designed around, and a set
captured at one seating cannot catch a seating bug.

**`fill_*.jpg` — rim row 277** (2026-08-28). Hand-poured levels; the filename is
the true fill percentage. The glass was placed once and levels poured into it,
so all five agree on the rim to the pixel.

**`rim379_*.jpg` — rim row 379** (2026-09-17). Frames from one real
`fetch_glass` + ice-dispense run, which gripped the glass 102 px — about
23 mm — lower. Nothing bounds that difference: `fetch_glass` grabs at whatever
centroid segmentation returned.

With the shipping band (window 48, `ice_min_contrast` 25, `ice_stop_row_px` 565,
so rows 517-670):

| fixture | surface | note |
|---|---|---|
| `fill_0.jpg` | none | empty |
| `fill_30.jpg` | none | the ledge occludes the glass base |
| `fill_40.jpg` | row 595, step 58 | below the stop row: not yet |
| `fill_80.jpg` | none | risen past the stop row (real surface 434) |
| `fill_100.jpg` | none | risen past the stop row (real surface 335) |
| `rim379_empty.jpg` | none | **the important one**: the rim sits at 379 with a step of 56, stronger than any real ice surface, and is excluded by position alone |
| `rim379_rising.jpg` | row 567, step 49 | +13.5s into the run, just below the stop row |
| `rim379_passed.jpg` | none | +16.5s, risen past it — the frame the loop stops on |

Single-frame values; the plan quotes 5-frame means for the `fill_*` set, so small
differences there are per-frame noise.

Note what the empty glass and the full one have in common: **no surface in the
band**. The measurement cannot tell them apart and does not try — the loop's
`sawSurface` latch does. That is why a false sighting is more dangerous than a
missed one: a single glare frame among the ~20 empty ticks before ice arrives
would arm the latch and let the next empty reading stop a dispense on an empty
glass.

These fixtures pin the contrast step, not a fill fraction: the measurement
answers "has the surface passed the stop row", not "how full is the glass".
Beyond the table the tests scale fixture luminance 0.5x-1.8x and confirm an empty glass
still reports no surface: an absolute-brightness threshold reads a brightly-lit
empty glass as full, and the contrast step exists to prevent exactly that.

The full 148 MB capture set these were cut from (five frames per level, point
clouds, depth frames, poses, intrinsics) is not in the repo, and the CLI that
produced it has been removed. These eight frames are the record: a change to the
band or to either measurement has to be justified against them, and new fixtures
now come off a machine via `check_ice_level` and the annotated frame each
watched dispense saves.
