// Package beanjamin: served-drinks shelf placement.
//
// Helpers for placing finished cups on a dedicated shelf instead of returning
// them to the empty-cup pickup spot. The shelf is modeled as a Box geometry
// in the framesystem under shelfTopFrameName (with a fallback to
// shelfTopOriginFrameName for the RDK tail-geometry frame). Tile centers are
// laid out along the shelf's long axis at fixed spacing on the midline — as
// many slots as the shelf-top length allows; the claws are commanded to land
// shelfDropZOffsetMm above the shelf top surface.
//
// Slot selection is a simple sequential round-robin (no vision): each
// placement takes the next slot via an in-memory counter (shelfSlotCounter)
// modulo the number of tiles, on the assumption that by the time the counter
// wraps around the earliest-placed cup has been picked up. The counter is
// process-local and resets to slot 0 on module restart/reconfigure.
package beanjamin

import (
	"context"
	"fmt"
	"math"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

const (
	shelfTopFrameName       = "shelf-top"
	shelfTopOriginFrameName = "shelf-top_origin"

	shelfTileSpacingMm = 120.0
	shelfTileMarginMm  = 60.0
	// shelfDropZOffsetMm is the Z height of the placement anchor above the
	// shelf top surface. The anchor plays the same role as the detected cup
	// centroid at pickup: it is composed with CupGrabRelativePose to derive
	// the world-frame claws pose used for the drop.
	shelfDropZOffsetMm    = 30.0
	shelfApproachZExtraMm = 80.0
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
// the served-drinks shelf-top obstacle. Looks for a Box geometry under
// shelfTopFrameName first, then under shelfTopOriginFrameName (RDK creates
// both frames for any part with a collision body — only one of them
// typically carries the geometry, so we try each). Non-Box geometries return
// an error.
func (s *beanjaminCoffee) shelfTopGeometry(ctx context.Context) (spatialmath.Pose, r3.Vector, error) {
	fs, fsInputs, err := s.currentInputs(ctx)
	if err != nil {
		return nil, r3.Vector{}, err
	}

	var (
		frameName string
		geos      []spatialmath.Geometry
		anyFound  bool
	)
	for _, name := range []string{shelfTopFrameName, shelfTopOriginFrameName} {
		frame := fs.Frame(name)
		if frame == nil {
			continue
		}
		anyFound = true
		gif, gErr := frame.Geometries([]referenceframe.Input{})
		if gErr != nil {
			s.logger.Debugf("shelf placement: frame %q has no geometries (%v); trying next", name, gErr)
			continue
		}
		if g := gif.Geometries(); len(g) > 0 {
			frameName = name
			geos = g
			break
		}
		s.logger.Debugf("shelf placement: frame %q exists but carries no geometry; trying next", name)
	}
	if !anyFound {
		return nil, r3.Vector{}, fmt.Errorf("shelf-top frame %q (or %q) not found in framesystem", shelfTopFrameName, shelfTopOriginFrameName)
	}
	if len(geos) == 0 {
		return nil, r3.Vector{}, fmt.Errorf("neither frame %q nor %q carries a geometry", shelfTopFrameName, shelfTopOriginFrameName)
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

// nextShelfTile resolves the shelf-top geometry, computes the tile layout
// (as many slots as the shelf length allows at shelfTileSpacingMm), and
// returns the next slot in round-robin order along with the world-frame Z of
// the shelf top surface. The slot is chosen by an in-memory counter that
// increments once per placement and wraps modulo the tile count, on the
// assumption that the earliest-placed cup has been picked up by the time the
// counter wraps. The counter is process-local and resets on restart.
//
// Returns an error only when the shelf-top geometry is missing or too small
// to hold a single slot.
func (s *beanjaminCoffee) nextShelfTile(ctx context.Context) (r3.Vector, float64, error) {
	shelfWorldPose, dimsMm, err := s.shelfTopGeometry(ctx)
	if err != nil {
		return r3.Vector{}, 0, fmt.Errorf("shelf placement: %w", err)
	}

	tiles := computeShelfTileCenters(shelfWorldPose, dimsMm, shelfTileSpacingMm, shelfTileMarginMm)
	if len(tiles) == 0 {
		return r3.Vector{}, 0, fmt.Errorf("shelf placement: shelf-top dimensions %v leave no room for tiles (margin=%.0fmm, spacing=%.0fmm)",
			dimsMm, shelfTileMarginMm, shelfTileSpacingMm)
	}

	// Add returns the post-increment value; subtract 1 so the first placement
	// after a (re)start uses slot 0.
	counter := s.shelfSlotCounter.Add(1) - 1
	idx := slotIndex(counter, len(tiles))
	tileWorld := tiles[idx]
	shelfTopZ := shelfWorldPose.Point().Z + dimsMm.Z/2

	s.logger.Infof("shelf placement: shelf top at %v, dims %v, %d slot(s); placement #%d -> slot %d/%d at world %v (shelf top Z=%.1fmm)",
		shelfWorldPose.Point(), dimsMm, len(tiles), counter+1, idx+1, len(tiles), tileWorld, shelfTopZ)
	return tileWorld, shelfTopZ, nil
}
