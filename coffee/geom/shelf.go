package geom

// Served-drinks shelf slot layout: tile centers along the shelf's long axis and
// the round-robin mapping of a placement counter onto them.

import (
	"math"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/spatialmath"
)

// ShelfTileCenters returns world-frame tile centers spaced spacingMm
// apart along the shelf's long axis (the larger of dimsMm.X / dimsMm.Y in
// shelf-local frame), centered on the midline, at the top face
// (Z = +dimsMm.Z/2 in shelf-local frame).
//
// Tiles are returned in ascending order along the long axis. Returns nil
// when the shelf is shorter than 2*marginMm along its long axis.
func ShelfTileCenters(shelfWorldPose spatialmath.Pose, dimsMm r3.Vector, spacingMm, marginMm float64) []r3.Vector {
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

// SlotIndex maps a monotonically increasing placement counter onto a tile
// index in [0, n) by wrapping (round-robin). Panics-free for n <= 0 by
// returning 0, though callers guard against an empty tile set first.
func SlotIndex(counter uint64, n int) int {
	if n <= 0 {
		return 0
	}
	return int(counter % uint64(n))
}
