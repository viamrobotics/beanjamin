// Package beanjamin: served-drinks shelf placement.
//
// Helpers for placing finished cups on a dedicated shelf instead of returning
// them to the empty-cup pickup spot. The shelf is modeled as a Box geometry
// in the framesystem under shelfTopFrameName (with a fallback to
// shelfTopOriginFrameName for the RDK tail-geometry frame). Tile centers are
// laid out along the shelf's long axis at fixed spacing on the midline; the
// claws are commanded to land shelfDropZOffsetMm above the shelf top surface.
//
// Free-tile selection re-uses the existing cup vision service (already
// required by DynamicCupPickup): the world-frame cup geometries returned by
// the vision service are checked for collision against each tile (modeled as
// a point geometry) with a shelfOccupancyBufferMm clearance buffer. The first
// tile that does not collide with any detected cup is chosen. This
// computation piggybacks on the pickup observation at cup_observe so no
// extra arm motion is needed before placement.
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
	// shelfOccupancyBufferMm is the collision clearance buffer used to decide
	// whether a tile is free: a tile is occupied iff at least one detected
	// cup geometry is within this many millimeters of the tile-center point.
	// Sized so a placed cup of typical diameter has comfortable clearance
	// from neighbors at shelfTileSpacingMm spacing.
	shelfOccupancyBufferMm = 50.0
)

// servedShelfTilePick records the world-frame target tile chosen at
// observation time and the world-frame Z of the shelf top surface (used to
// compose the drop pose). zero value (ok=false) means no tile was selected.
type servedShelfTilePick struct {
	tileWorld r3.Vector
	shelfTopZ float64
	ok        bool
}

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

// firstFreeTile returns the index, world position, and ok=true of the first
// tile whose tile-center point does not collide (within bufferMm) with any
// of the supplied cup geometries. Returns (-1, zero, false, nil) when every
// tile collides with at least one cup. Returns an error if a collision check
// itself fails.
func firstFreeTile(tiles []r3.Vector, cupGeometries []spatialmath.Geometry, bufferMm float64) (int, r3.Vector, bool, error) {
	for i, t := range tiles {
		tileGeom := spatialmath.NewPoint(t, fmt.Sprintf("shelf_tile_%d", i))
		occupied := false
		for _, cup := range cupGeometries {
			collides, _, err := cup.CollidesWith(tileGeom, bufferMm)
			if err != nil {
				return -1, r3.Vector{}, false, fmt.Errorf("collision check tile %d vs %q: %w", i, cup.Label(), err)
			}
			if collides {
				occupied = true
				break
			}
		}
		if !occupied {
			return i, t, true, nil
		}
	}
	return -1, r3.Vector{}, false, nil
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

// selectShelfTile computes the shelf-top tile layout, picks the first tile
// whose tile-center point does not collide with any of the supplied
// already-on-shelf cup geometries (within shelfOccupancyBufferMm), and
// stashes the result on s.servedShelfTile for later use by
// placeFullCupOnShelf.
//
// onShelfCups is the set of detections that the caller has already
// classified as belonging to the shelf (currently: detections whose world Z
// is above shelfTopZ — see findCupCandidates). Pose+dims are passed in to
// avoid a second framesystem lookup.
//
// Returns an error when the shelf is full — bubbled up to abort the order
// before any pickup motion.
func (s *beanjaminCoffee) selectShelfTile(shelfWorldPose spatialmath.Pose, dimsMm r3.Vector, onShelfCups []spatialmath.Geometry) error {
	tiles := computeShelfTileCenters(shelfWorldPose, dimsMm, shelfTileSpacingMm, shelfTileMarginMm)
	if len(tiles) == 0 {
		return fmt.Errorf("shelf placement: shelf-top dimensions %v leave no room for tiles (margin=%.0fmm, spacing=%.0fmm)",
			dimsMm, shelfTileMarginMm, shelfTileSpacingMm)
	}
	shelfCenter := shelfWorldPose.Point()
	s.logger.Infof("shelf placement: shelf top at %v, dims %v, %d tile candidate(s); %d on-shelf cup(s)",
		shelfCenter, dimsMm, len(tiles), len(onShelfCups))

	idx, tileWorld, ok, err := firstFreeTile(tiles, onShelfCups, shelfOccupancyBufferMm)
	if err != nil {
		return fmt.Errorf("shelf placement: %w", err)
	}
	if !ok {
		return fmt.Errorf("shelf placement: shelf full — every tile collides with a detected cup (buffer=%.0fmm)", shelfOccupancyBufferMm)
	}

	shelfTopZ := shelfCenter.Z + dimsMm.Z/2
	s.servedShelfTile.Store(servedShelfTilePick{
		tileWorld: tileWorld,
		shelfTopZ: shelfTopZ,
		ok:        true,
	})
	s.logger.Infof("shelf placement: chose tile %d/%d at world %v (shelf top Z=%.1fmm)",
		idx+1, len(tiles), tileWorld, shelfTopZ)
	return nil
}
