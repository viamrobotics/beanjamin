package coffee

import (
	"context"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/testutils/inject"
)

// stagingGripZ is the grip point's world Z in stagingFS.
const stagingGripZ = 800.0

// stagingFS returns clawsStaticFS with the grip point at (0, 0, stagingGripZ),
// plus, when withTable is set, a static table whose top face is Z=600 under it.
func stagingFS(t *testing.T, withTable bool) *referenceframe.FrameSystem {
	t.Helper()
	fs := clawsStaticFS(t, spatialmath.NewPoseFromPoint(r3.Vector{Z: stagingGripZ - gripPointOffsetMm}))
	if !withTable {
		return fs
	}
	table, err := spatialmath.NewBox(spatialmath.NewPoseFromPoint(r3.Vector{Z: 590}), r3.Vector{X: 500, Y: 500, Z: 20}, "table")
	if err != nil {
		t.Fatalf("new table box: %v", err)
	}
	frame, err := referenceframe.NewStaticFrameWithGeometry("table", spatialmath.NewZeroPose(), table)
	if err != nil {
		t.Fatalf("new table frame: %v", err)
	}
	if err := fs.AddFrame(frame, fs.World()); err != nil {
		t.Fatalf("add table frame: %v", err)
	}
	return fs
}

// stagingService holds testBox centered on the grip point as the glass, with the
// same grasp cached as the vision pickup would have, and an arm with no joints.
func stagingService(t *testing.T, fs *referenceframe.FrameSystem) *beanjaminCoffee {
	t.Helper()
	s := heldGeomService(t, fs)
	s.arm = &inject.Arm{CurrentInputsFunc: func(context.Context) ([]referenceframe.Input, error) {
		return nil, nil
	}}
	pickupGrasp := testBox(t, spatialmath.NewZeroPose())
	if err := s.addHeldItemFrame(pickupGrasp); err != nil {
		t.Fatalf("addHeldItemFrame: %v", err)
	}
	s.cacheHeldGeometry(pickupLabelGlass, pickupGrasp)
	return s
}

// A glass released above the table is staged standing on it, and the re-grab
// models the grasp from where it stands rather than from the pickup.
func TestStageAndRegrabGlassSeatsOnSurface(t *testing.T) {
	fs := stagingFS(t, true)
	s := stagingService(t, fs)
	inputs := referenceframe.NewZeroInputs(fs)

	if err := s.stageHeldGlass(inputs); err != nil {
		t.Fatalf("stageHeldGlass: %v", err)
	}
	if s.heldItemAttached || fs.Frame(heldItemFrameName) != nil {
		t.Fatalf("the held-item frame must be gone once the glass is staged")
	}
	staged, err := s.stagedGlassGeometry()
	if err != nil {
		t.Fatalf("stagedGlassGeometry: %v", err)
	}
	// testBox is 80 mm tall: its center sits 40 mm above its seated base.
	seatedCenter := r3.Vector{Z: 600 + surfaceRestClearanceMm + 40}
	requireVecEqual(t, staged.Pose().Point(), seatedCenter, 1e-6)

	if err := s.regrabStagedGlass(context.Background()); err != nil {
		t.Fatalf("regrabStagedGlass: %v", err)
	}
	if s.stagedGlassPlaced || fs.Frame(stagedGlassFrameName) != nil {
		t.Fatalf("the staged-glass obstacle must be gone once re-grabbed")
	}
	requireVecEqual(t, heldItemWorldCenter(t, fs, inputs), seatedCenter, 1e-6)
	requireVecEqual(t, s.cachedHeldGeometry(pickupLabelGlass).Pose().Point(), r3.Vector{Z: seatedCenter.Z - stagingGripZ}, 1e-6)
}

// With no surface modeled beneath it, the glass is staged where it was released.
func TestStageGlassWithoutSurfaceStaysPut(t *testing.T) {
	fs := stagingFS(t, false)
	s := stagingService(t, fs)
	if err := s.stageHeldGlass(referenceframe.NewZeroInputs(fs)); err != nil {
		t.Fatalf("stageHeldGlass: %v", err)
	}
	staged, err := s.stagedGlassGeometry()
	if err != nil {
		t.Fatalf("stagedGlassGeometry: %v", err)
	}
	requireVecEqual(t, staged.Pose().Point(), r3.Vector{Z: stagingGripZ}, 1e-6)
}

// A re-grab with no glass staged restores the grasp cached at pickup.
func TestRegrabWithoutStagedGlassUsesCache(t *testing.T) {
	fs := stagingFS(t, false)
	s := stagingService(t, fs)
	s.detachHeldGeometry()
	if err := s.regrabStagedGlass(context.Background()); err != nil {
		t.Fatalf("regrabStagedGlass: %v", err)
	}
	requireVecEqual(t, heldItemWorldCenter(t, fs, referenceframe.NewZeroInputs(fs)), r3.Vector{Z: stagingGripZ}, 1e-6)
}
