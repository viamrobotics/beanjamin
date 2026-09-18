package coffee

import (
	"context"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// filterOnArmFS mirrors how the RDK builds a part: the model frame parented
// under its "_origin", which carries the collision geometry. Both hang off the
// claws, as the portafilter does while the arm holds it.
func filterOnArmFS(t *testing.T) *referenceframe.FrameSystem {
	t.Helper()
	fs := clawsStaticFS(t, spatialmath.NewZeroPose())

	geom, err := spatialmath.NewBox(spatialmath.NewZeroPose(), r3.Vector{X: 60, Y: 60, Z: 30}, componentFilter)
	if err != nil {
		t.Fatalf("new filter geometry: %v", err)
	}
	origin, err := referenceframe.NewStaticFrameWithGeometry(
		componentFilter+"_origin", spatialmath.NewPoseFromPoint(r3.Vector{Z: -50}), geom)
	if err != nil {
		t.Fatalf("new filter origin: %v", err)
	}
	if err := fs.AddFrame(origin, fs.Frame(componentClaws)); err != nil {
		t.Fatalf("add filter origin: %v", err)
	}
	filter, err := referenceframe.NewStaticFrame(componentFilter, spatialmath.NewZeroPose())
	if err != nil {
		t.Fatalf("new filter frame: %v", err)
	}
	if err := fs.AddFrame(filter, origin); err != nil {
		t.Fatalf("add filter frame: %v", err)
	}
	handle, err := referenceframe.NewStaticFrame("portafilter-handle", spatialmath.NewPoseFromPoint(r3.Vector{X: 80}))
	if err != nil {
		t.Fatalf("new handle frame: %v", err)
	}
	if err := fs.AddFrame(handle, filter); err != nil {
		t.Fatalf("add handle frame: %v", err)
	}
	return fs
}

func TestDetachFilterFrame(t *testing.T) {
	s := heldGeomService(t, filterOnArmFS(t))

	if _, err := s.detachFilterFrame(); err != nil {
		t.Fatalf("detachFilterFrame: %v", err)
	}
	// A handle left behind would still block every plan.
	for _, name := range []string{componentFilter, componentFilter + "_origin", "portafilter-handle"} {
		if s.cachedFS.Frame(name) != nil {
			t.Errorf("frame %q still present after detach", name)
		}
	}
	if s.cachedFS.Frame(componentClaws) == nil {
		t.Error("claws frame was removed; only the portafilter should go")
	}
}

func TestDetachFilterFrameIsIdempotent(t *testing.T) {
	// Two actions run back to back with the override: the second must not fail
	// because the first already took the filter out.
	s := heldGeomService(t, filterOnArmFS(t))

	if _, err := s.detachFilterFrame(); err != nil {
		t.Fatalf("first detach: %v", err)
	}
	if _, err := s.detachFilterFrame(); err != nil {
		t.Fatalf("second detach: %v", err)
	}
}

func TestDetachFilterFrameRefusesWhenLocked(t *testing.T) {
	// A locked filter is the real portafilter sitting in the espresso machine.
	// Dropping it would let the arm plan through it.
	s := heldGeomService(t, filterOnArmFS(t))
	s.filterFrameLocked = true

	if _, err := s.detachFilterFrame(); err == nil {
		t.Fatal("detachFilterFrame succeeded with the filter frame locked, want an error")
	}
	if s.cachedFS.Frame(componentFilter) == nil {
		t.Error("filter frame was removed despite the error")
	}
}

func TestReattachFilterFrameRestoresSubtree(t *testing.T) {
	s := heldGeomService(t, filterOnArmFS(t))

	parents := map[string]string{}
	for _, name := range []string{componentFilter, componentFilter + "_origin", "portafilter-handle"} {
		p, err := s.cachedFS.Parent(s.cachedFS.Frame(name))
		if err != nil {
			t.Fatalf("parent of %q: %v", name, err)
		}
		parents[name] = p.Name()
	}

	detached, err := s.detachFilterFrame()
	if err != nil {
		t.Fatalf("detachFilterFrame: %v", err)
	}
	// A held item is the case refreshFrameSystemIfClean declines to rebuild, so
	// the reattach has to preserve it rather than reset around it.
	held, err := referenceframe.NewStaticFrame(heldItemFrameName, spatialmath.NewZeroPose())
	if err != nil {
		t.Fatalf("new held frame: %v", err)
	}
	if err := s.cachedFS.AddFrame(held, s.cachedFS.Frame(componentClaws)); err != nil {
		t.Fatalf("add held frame: %v", err)
	}

	if err := s.reattachFilterFrame(detached); err != nil {
		t.Fatalf("reattachFilterFrame: %v", err)
	}
	for name, wantParent := range parents {
		f := s.cachedFS.Frame(name)
		if f == nil {
			t.Fatalf("frame %q was not restored", name)
		}
		p, err := s.cachedFS.Parent(f)
		if err != nil {
			t.Fatalf("parent of restored %q: %v", name, err)
		}
		if p.Name() != wantParent {
			t.Errorf("frame %q restored under %q, want %q", name, p.Name(), wantParent)
		}
	}
	if s.cachedFS.Frame(heldItemFrameName) == nil {
		t.Error("held item frame was lost by the reattach")
	}
}

// A failing action must still put the portafilter back. heldItemAttached is set
// so the frame-system refreshes are skipped on both sides, which is also the
// case the restore exists for.
func TestExecuteActionReattachesFilterOnFailure(t *testing.T) {
	s := heldGeomService(t, filterOnArmFS(t))
	s.heldItemAttached = true
	s.cancelCtx = context.Background()

	// pulse_ice_pin fails immediately on the nil board, before touching the arm.
	if _, err := s.executeAction(context.Background(), "pulse_ice_pin", true); err == nil {
		t.Fatal("executeAction succeeded with no ice board, want an error")
	}
	for _, name := range []string{componentFilter, componentFilter + "_origin", "portafilter-handle"} {
		if s.cachedFS.Frame(name) == nil {
			t.Errorf("frame %q was not restored after the action failed", name)
		}
	}
	if s.running.Load() {
		t.Error("running flag left set after the action returned")
	}
}
