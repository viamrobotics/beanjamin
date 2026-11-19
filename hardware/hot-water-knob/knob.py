"""Hot water dial lever for the Breville BES920.

A two-piece split collar that clamps over the machine's smooth hot water dial
(like a shaft collar on a smooth shaft), with a lever on one half that the
claws grip end-on, the way they hold the portafilter handle. The lever's
copies the portafilter handle's profile and peg dimples, and it is tilted 30
degrees out from the wall.

Coordinates: Z is the dial axis, Z=0 is the collar's open end (nearest the
machine wall), +Z points out of the wall. The collar splits on the YZ plane;
half A (x > 0) carries the lever, pointing along +X and tilted toward +Z, half B (x < 0) is the plain half.
Two M3 screws, one each side (+Y and -Y), pull the halves together.

    python knob.py   # writes out/*.stl and out/knob_assembly.step

All dimensions in mm. Change the parameters below and re-run.
"""

from pathlib import Path

from build123d import (
    Align,
    Axis,
    Box,
    BuildPart,
    BuildSketch,
    Compound,
    Cone,
    Cylinder,
    Location,
    Locations,
    SlotOverall,
    Mode,
    Part,
    Pos,
    Rot,
    add,
    Plane,
    RegularPolygon,
    export_step,
    export_stl,
    extrude,
    fillet,
)

# --- The machine's dial (measured) ---
# The dial may taper. Set its diameter at the face and right at the chrome
# ring's lip (DIAL_PROTRUSION back from the face); the bore follows the line
# between them. Equal values give a straight bore.
DIAL_DIAMETER_FACE = 43.0  # calipers at the front edge: 42.95-43.0
DIAL_DIAMETER_BACK = 44.24  # calipers at the chrome ring: 44.16-44.24
DIAL_PROTRUSION = 27.0  # how far the dial stands out of the chrome ring (measured 26-28)

# --- Collar ---
# Clearance on the diameter. Too tight to slide on: raise by 0.2. If the
# screws close the gap before it grips, lower it or add a rubber liner.
BORE_CLEARANCE = 0.4
WALL = 4.0
ENGAGEMENT = 18.0  # how far the collar slides over the dial (leaves 10 mm to the wall)
FACE_THICKNESS = 3.0  # half-plate on half A that seats against the dial's front face
HUB_HEIGHT = 18.0  # solid half-hub on half A in front of the dial, that the lever grows out of
SPLIT_GAP = 1.0  # gap between the halves, so the screws can squeeze them

# --- Clamp screws (M3 through both lugs, nut on half A) ---
LUG_THICKNESS = 7.0  # each half's lug, along the screw
LUG_REACH = 9.0  # how far the lugs stand out from the collar
SCREW_CLEARANCE = 3.4
NUT_ACROSS_FLATS = 5.7  # M3 nut (5.5) + 0.2
NUT_DEPTH = 2.6
HEAD_DIAMETER = 6.2
HEAD_DEPTH = 2.5

# --- Lever (what the claws grip, end-on) ---
# The lever is tilted LEVER_TILT out from the wall: a straight-out lever puts
# the gripper in line with the arm's forearm mid-turn (a wrist singularity),
# and the arm can't turn it smoothly through that. Its centreline runs from a
# point on the dial axis (LEVER_APEX_Z) outward and away from the wall.
LEVER_TILT = 30.0  # degrees out of the dial's face plane
LEVER_APEX_Z = 7.5  # where the centreline meets the dial axis
LEVER_START = 20.0  # along the centreline from the apex; buried in the collar
# The claws grip the lever the way they grip the portafilter handle: the same
# profile (rounded ends, flat sides) with a conical dimple in each rounded end
# for the cone-shaped peg on each finger. Sizes are from the handle's STL.
# The rounded ends face the turn (±Y), so the fingers push the lever around
# through the pegs rather than by friction.
LEVER_WIDTH = 36.7  # across the rounded ends, the way the claws close
LEVER_THICKNESS = 24.0  # across the flat sides
DIMPLE_TOP = 13.0  # dimple diameter at the surface
DIMPLE_BOTTOM = 7.0
DIMPLE_DEPTH = 7.9
# Where the claws sit along the lever, from the apex. The `grip-point` frame is
# 54 mm ahead of the pegs along the tool, and the dry run put it 95 mm along
# the lever, so the pegs land at 149 mm. The fingertips reach 72 mm past the
# pegs, to 77 mm, well clear of the collar.
GRIP_DISTANCE = 95.0
PEG_BEHIND_GRIP = 54.0
DIMPLE_DISTANCE = GRIP_DISTANCE + PEG_BEHIND_GRIP
# Past the dimples, the finger channels need about 18 mm on the palm side; the
# palm itself is 43 mm past the pegs.
LEVER_END = DIMPLE_DISTANCE + 30.0
LEVER_END_FILLET = 3.0

OUT = Path(__file__).parent / "out"



def dial_r(z: float) -> float:
    """Bore radius at collar height z. The collar's top (z = ENGAGEMENT) sits at
    the dial face; z = 0 is ENGAGEMENT back from it."""
    back_from_face = ENGAGEMENT - z
    d = DIAL_DIAMETER_FACE + (DIAL_DIAMETER_BACK - DIAL_DIAMETER_FACE) * back_from_face / DIAL_PROTRUSION
    return (d + BORE_CLEARANCE) / 2


bore_r = max(dial_r(0), dial_r(ENGAGEMENT))
outer_r = bore_r + WALL
lug_y = outer_r + LUG_REACH / 2
RING_HEIGHT = 8.0  # the quick fit-test ring pair


def tilted_lever() -> Part:
    """The lever, placed: its centreline leaves the dial axis at LEVER_APEX_Z
    and runs along +X, tilted LEVER_TILT toward +Z (away from the wall)."""
    with BuildPart() as lever:
        # Built along +X from the apex, then tilted into place. The profile's
        # long axis (the rounded ends) runs along Y.
        with BuildSketch(Plane.YZ.offset(LEVER_START)):
            SlotOverall(LEVER_WIDTH, LEVER_THICKNESS)
        extrude(amount=LEVER_END - LEVER_START)
        fillet(lever.faces().sort_by(Axis.X)[-1].edges(), LEVER_END_FILLET)
        # A conical dimple in each rounded end, for the fingers' pegs.
        for sy in (1, -1):
            with Locations(Location((DIMPLE_DISTANCE, sy * (LEVER_WIDTH / 2 - DIMPLE_DEPTH / 2), 0), (sy * -90, 0, 0))):
                Cone(DIMPLE_BOTTOM / 2, DIMPLE_TOP / 2, DIMPLE_DEPTH + 0.02, mode=Mode.SUBTRACT)
    return Pos(0, 0, LEVER_APEX_Z) * Rot(0, -LEVER_TILT, 0) * lever.part


def half(side: int, lever: bool, height: float = ENGAGEMENT, face: bool = True) -> BuildPart:
    """One collar half. side=+1 is half A (x > 0, nut traps), -1 is half B.

    height is how far the collar covers the dial; face adds half A's plate over
    the dial's front face. The fit-test ring is a short pair with no face.
    """
    g = SPLIT_GAP / 2
    screw_z = height / 2
    # The ring test covers the last `height` mm of the dial, next to its face.
    z0 = ENGAGEMENT - height
    with BuildPart() as part:
        body_h = height + (FACE_THICKNESS + (HUB_HEIGHT if lever else 0) if side > 0 and face else 0)
        Cylinder(outer_r, body_h, align=(Align.CENTER, Align.CENTER, Align.MIN))
        for sy in (1, -1):
            with Locations((0, sy * (outer_r + LUG_REACH / 2), height / 2)):
                Box(2 * (LUG_THICKNESS + g), LUG_REACH + 2, height)
        if lever:
            add(tilted_lever())  # noqa: F405
        # Keep only this side of the split, less half the gap, and nothing
        # behind the collar's open end.
        keep = 400
        with Locations((side * (g + keep / 2), 0, keep / 2)):
            Box(keep, keep, keep, mode=Mode.INTERSECT)
        Cone(dial_r(z0), dial_r(ENGAGEMENT), height, align=(Align.CENTER, Align.CENTER, Align.MIN), mode=Mode.SUBTRACT) \
            if dial_r(z0) != dial_r(ENGAGEMENT) else \
            Cylinder(dial_r(z0), height, align=(Align.CENTER, Align.CENTER, Align.MIN), mode=Mode.SUBTRACT)
        outer_x = side * (LUG_THICKNESS + g)
        for sy in (1, -1):
            with Locations((side * (g + LUG_THICKNESS / 2), sy * lug_y, screw_z)):
                Cylinder(SCREW_CLEARANCE / 2, LUG_THICKNESS + 2, rotation=(0, 90, 0), mode=Mode.SUBTRACT)
            if side > 0:
                # Hex nut trap in the outer face of half A's lug.
                with BuildSketch(Plane.YZ.offset(outer_x - NUT_DEPTH)):
                    with Locations((sy * lug_y, screw_z)):
                        RegularPolygon(NUT_ACROSS_FLATS / 3**0.5, 6)
                extrude(amount=NUT_DEPTH + 0.01, mode=Mode.SUBTRACT)
            else:
                # Counterbore for the screw head in half B's lug.
                with Locations((outer_x + HEAD_DEPTH / 2, sy * lug_y, screw_z)):
                    Cylinder(HEAD_DIAMETER / 2, HEAD_DEPTH + 0.01, rotation=(0, 90, 0), mode=Mode.SUBTRACT)
    return part


if __name__ == "__main__":
    OUT.mkdir(exist_ok=True)
    for old in list(OUT.glob("*.stl")) + list(OUT.glob("*.step")):
        old.unlink()
    a = half(+1, lever=True).part
    b = half(-1, lever=False).part
    test_a = half(+1, lever=False).part
    ring_a = half(+1, lever=False, height=RING_HEIGHT, face=False).part
    ring_b = half(-1, lever=False, height=RING_HEIGHT, face=False).part
    export_stl(a, str(OUT / "half_a_lever.stl"))
    export_stl(b, str(OUT / "half_b.stl"))
    export_stl(test_a, str(OUT / "half_a_test.stl"))
    export_stl(ring_a, str(OUT / "ring_a.stl"))
    export_stl(ring_b, str(OUT / "ring_b.stl"))
    export_step(Compound([a, b]), str(OUT / "knob_assembly.step"))
    for name, p in (("half_a_lever", a), ("half_b", b), ("half_a_test", test_a), ("ring_a", ring_a), ("ring_b", ring_b)):
        bb = p.bounding_box()
        print(f"{name}: {bb.size.X:.1f} x {bb.size.Y:.1f} x {bb.size.Z:.1f} mm, {p.volume / 1000:.1f} cm3, valid={p.is_valid}")
