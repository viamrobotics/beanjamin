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
// placement takes the next slot via a counter (servingSlotStore) modulo the
// number of tiles, on the assumption that by the time the counter wraps around
// the earliest-placed cup has been picked up. The counter is mirrored to
// VIAM_MODULE_DATA so a restart/reconfigure resumes after the last-used slot
// instead of dropping the next cup on top of one still on the shelf.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"

	"beanjamin/coffee/geom"
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

	servingSlotFile = "serving-area-slot"
)

// servingSlotStore holds the round-robin serving-area counter. Mirrored to
// VIAM_MODULE_DATA because the service is AlwaysRebuild: without persisting,
// every reconfigure would restart at slot 0, which likely still holds a cup.
// The raw counter is stored rather than a slot index so it stays meaningful if
// the serving area is resized (the slot is always counter modulo slot count).
type servingSlotStore struct {
	mu   sync.Mutex
	next uint64
	// path is "" when VIAM_MODULE_DATA is unset (tests, a local run): in-memory only.
	path string
}

// newServingSlotStore loads any persisted counter. Every read failure degrades
// to slot 0 — the pre-persistence behavior — rather than failing construction.
func newServingSlotStore(logger logging.Logger) *servingSlotStore {
	st := &servingSlotStore{}

	dir := os.Getenv("VIAM_MODULE_DATA")
	if dir == "" {
		logger.Warnf("serving area: VIAM_MODULE_DATA is unset — the slot counter will be kept in " +
			"memory only and restart at slot 1 after a reconfigure")
		return st
	}
	st.path = filepath.Join(dir, servingSlotFile)

	raw, err := os.ReadFile(st.path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("serving area: reading %s: %v — starting at slot 1", st.path, err)
		}
		return st
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		logger.Warnf("serving area: %s holds %q, which is not a slot counter: %v — starting at slot 1",
			st.path, string(raw), err)
		return st
	}

	st.next = n
	logger.Infof("serving area: resuming slot counter at %d (loaded from %s)", n, st.path)
	return st
}

func (st *servingSlotStore) get() uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.next
}

// set persists best-effort: a write failure leaves the in-memory value correct,
// costing only a restart at a stale slot.
func (st *servingSlotStore) set(logger logging.Logger, next uint64) {
	st.mu.Lock()
	st.next = next
	path := st.path
	st.mu.Unlock()

	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.FormatUint(next, 10)), 0o644); err != nil {
		logger.Warnf("serving area: persisting slot counter to %s: %v", path, err)
	}
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
// round-robin order via servingSlot and skips any it cannot reach.
//
// Returns an error only when the serving-area geometry is missing or too small
// to hold a single slot.
func (s *beanjaminCoffee) servingAreaSlots(ctx context.Context) ([]r3.Vector, float64, error) {
	logger := s.activeOrderLogger()
	shelfWorldPose, dimsMm, err := s.shelfTopGeometry(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("shelf placement: %w", err)
	}

	tiles := geom.ShelfTileCenters(shelfWorldPose, dimsMm, shelfTileSpacingMm, shelfTileMarginMm)
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
