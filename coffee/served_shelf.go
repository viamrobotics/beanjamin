package coffee

// Served-drinks shelf placement.
//
// Helpers for placing finished cups on a dedicated served-drinks shelf. The
// shelf is modeled as a Box geometry
// in the framesystem under servingAreaFrameName (with a fallback to
// servingAreaOriginFrameName for the RDK tail-geometry frame). Tile centers are
// laid out along the shelf's long axis at fixed spacing on the midline — as
// many slots as the serving-area length allows; the claws are commanded to land
// shelfDropZOffsetMm above the serving area top surface.
//
// Slot selection is a simple sequential round-robin (no vision): each
// placement takes the next slot via an in-memory counter (servingAreaSlotCounter)
// modulo the number of tiles, on the assumption that by the time the counter
// wraps around the earliest-placed cup has been picked up. The counter is
// process-local and resets to slot 0 on module restart/reconfigure.

import (
	"context"
	"fmt"
	"math"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

const (
	servingAreaFrameName       = "serving-area"
	servingAreaOriginFrameName = "serving-area_origin"

	// servingAreaShieldFrameName is an obstacle defined in the machine frame
	// system (like serving-area / shelf-top) enclosing the standing-cup zone
	// above the serving area. It stays a hard obstacle on the lateral carry so
	// the arm steers clear of cups already on the shelf; only the linearly
	// constrained descent into a slot and the retreat back out are allowed to
	// pass through it (servingAreaShieldCollisions).
	servingAreaShieldFrameName = "serving-area-shield"

	shelfTileSpacingMm = 120.0
	shelfTileMarginMm  = 60.0
	// shelfDropZOffsetMm is the fallback Z height of the placement anchor above
	// the shelf top surface, used only when no held-item geometry is tracked.
	// With a cup/glass geometry attached, servingAreaDropZOffset derives the
	// offset from the container's own height instead, so taller containers (e.g.
	// the iced glass) are not driven into the shelf. The anchor plays the same
	// role as the detected cup centroid at pickup: it is composed with
	// CupGrabRelativePose to derive the world-frame grip-point pose for the drop.
	shelfDropZOffsetMm = 30.0
)

// computeShelfTileCenters returns world-frame tile centers spaced spacingMm
// apart along the shelf's long axis (the larger of dimsMm.X / dimsMm.Y in
// shelf-local frame), centered on the midline, at the top face
// (Z = +dimsMm.Z/2 in shelf-local frame).
//
// Tiles are returned in ascending order along the long axis. Returns nil
// when the shelf is shorter than 2*marginMm along its long axis.
func computeShelfTileCenters(shelfWorldPose spatialmath.Pose, dimsMm r3.Vector, spacingMm, marginMm float64) []r3.Vector {
	xLong := dimsMm.X >= dimsMm.Y
	longDim := dimsMm.X
	if !xLong {
		longDim = dimsMm.Y
	}

	usable := longDim - 2*marginMm
	if usable < 0 {
		return nil
	}

	n := int(math.Floor(usable/spacingMm)) + 1
	span := float64(n-1) * spacingMm
	startOffset := -span / 2
	topZ := dimsMm.Z / 2

	out := make([]r3.Vector, n)
	for i := range n {
		offset := startOffset + float64(i)*spacingMm
		var local r3.Vector
		if xLong {
			local = r3.Vector{X: offset, Y: 0, Z: topZ}
		} else {
			local = r3.Vector{X: 0, Y: offset, Z: topZ}
		}
		world := spatialmath.Compose(shelfWorldPose, spatialmath.NewPoseFromPoint(local))
		out[i] = world.Point()
	}
	return out
}

// slotIndex maps a monotonically increasing placement counter onto a tile
// index in [0, n) by wrapping (round-robin). Panics-free for n <= 0 by
// returning 0, though callers guard against an empty tile set first.
func slotIndex(counter uint64, n int) int {
	if n <= 0 {
		return 0
	}
	return int(counter % uint64(n))
}

// shelfTopGeometry returns the world-frame center pose and box dimensions of
// the served-drinks serving-area obstacle. Looks for a Box geometry under
// servingAreaFrameName first, then under servingAreaOriginFrameName (RDK creates
// both frames for any part with a collision body — only one of them
// typically carries the geometry, so we try each). Non-Box geometries return
// an error.
func (s *beanjaminCoffee) shelfTopGeometry(ctx context.Context) (spatialmath.Pose, r3.Vector, error) {
	logger := s.activeOrderLogger()
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return nil, r3.Vector{}, err
	}

	var (
		frameName string
		geos      []spatialmath.Geometry
		anyFound  bool
	)
	for _, name := range []string{servingAreaFrameName, servingAreaOriginFrameName} {
		frame := fs.Frame(name)
		if frame == nil {
			continue
		}
		anyFound = true
		gif, gErr := frame.Geometries([]referenceframe.Input{})
		if gErr != nil {
			logger.Debugf("shelf placement: frame %q has no geometries (%v); trying next", name, gErr)
			continue
		}
		if g := gif.Geometries(); len(g) > 0 {
			frameName = name
			geos = g
			break
		}
		logger.Debugf("shelf placement: frame %q exists but carries no geometry; trying next", name)
	}
	if !anyFound {
		return nil, r3.Vector{}, fmt.Errorf("serving-area frame %q (or %q) not found in framesystem", servingAreaFrameName, servingAreaOriginFrameName)
	}
	if len(geos) == 0 {
		return nil, r3.Vector{}, fmt.Errorf("neither frame %q nor %q carries a geometry", servingAreaFrameName, servingAreaOriginFrameName)
	}

	// Transform geometry to world coordinates via the framesystem so the
	// parent-to-world transform is applied correctly. Mirrors the pattern in
	// lockFilterFrame — composing the geometry's local pose on top of
	// frame.Pose() would double-count the origin offset for tail-geometry
	// frames.
	worldGifTF, err := fs.Transform(
		fsInputs.ToLinearInputs(),
		referenceframe.NewGeometriesInFrame(frameName, geos),
		referenceframe.World,
	)
	if err != nil {
		return nil, r3.Vector{}, fmt.Errorf("transform %q geometry to world: %w", frameName, err)
	}
	worldGeos := worldGifTF.(*referenceframe.GeometriesInFrame).Geometries()
	if len(worldGeos) == 0 {
		return nil, r3.Vector{}, fmt.Errorf("no world geometry after transform of %q", frameName)
	}
	worldGeom := worldGeos[0]

	proto := worldGeom.ToProtobuf()
	box := proto.GetBox()
	if box == nil || box.DimsMm == nil {
		return nil, r3.Vector{}, fmt.Errorf("frame %q geometry is not a Box", frameName)
	}
	dims := r3.Vector{X: box.DimsMm.X, Y: box.DimsMm.Y, Z: box.DimsMm.Z}
	return worldGeom.Pose(), dims, nil
}

// servingAreaSlots resolves the serving-area geometry and returns the slot
// layout (as many slots as the serving area length allows at
// shelfTileSpacingMm) ordered along the serving area's long axis, plus the
// world-frame Z of the serving area top surface. The caller picks a slot in
// round-robin order via servingAreaSlotCounter and skips any it cannot reach.
//
// Returns an error only when the serving-area geometry is missing or too small
// to hold a single slot.
func (s *beanjaminCoffee) servingAreaSlots(ctx context.Context) ([]r3.Vector, float64, error) {
	logger := s.activeOrderLogger()
	shelfWorldPose, dimsMm, err := s.shelfTopGeometry(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("shelf placement: %w", err)
	}

	tiles := computeShelfTileCenters(shelfWorldPose, dimsMm, shelfTileSpacingMm, shelfTileMarginMm)
	if len(tiles) == 0 {
		return nil, 0, fmt.Errorf("shelf placement: serving-area dimensions %v leave no room for slots (margin=%.0fmm, spacing=%.0fmm)",
			dimsMm, shelfTileMarginMm, shelfTileSpacingMm)
	}

	shelfTopZ := shelfWorldPose.Point().Z + dimsMm.Z/2
	logger.Infof("shelf placement: serving area at %v, dims %v, %d slot(s) (shelf top Z=%.1fmm)",
		shelfWorldPose.Point(), dimsMm, len(tiles), shelfTopZ)
	return tiles, shelfTopZ, nil
}

// servingAreaDropZOffset returns the Z height of the placement anchor above the
// serving-area top surface. With a held-item geometry tracked it is half the
// container's height: the held-item box is modeled upright and centered on the
// grasp anchor, so its bottom sits halfHeight below the anchor — placing the
// container's bottom on the shelf regardless of its height (so a taller iced
// glass is not driven into the shelf the way the fixed offset did). Falls back
// to shelfDropZOffsetMm (tuned for the cup) when nothing is tracked.
func (s *beanjaminCoffee) servingAreaDropZOffset() float64 {
	if half, ok := s.heldItemHalfHeightMm(); ok {
		return half
	}
	return shelfDropZOffsetMm
}
