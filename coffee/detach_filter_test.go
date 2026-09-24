package coffee

import (
	"strings"
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

func TestDropFilterRestores(t *testing.T) {
	s := heldGeomService(t, filterOnArmFS(t))

	restore, err := s.dropFilter()
	if err != nil {
		t.Fatalf("dropFilter: %v", err)
	}
	if s.cachedFS.Frame(componentFilter) != nil {
		t.Error("filter still present after the drop")
	}
	restore()
	if s.cachedFS.Frame(componentFilter) == nil {
		t.Error("filter not restored")
	}
}

func TestDropFilterLeavesLockedFilterAlone(t *testing.T) {
	// The iced sequence runs with the portafilter locked in the machine: there is
	// nothing on the claws to drop, and dropping it would plan through the real one.
	s := heldGeomService(t, filterOnArmFS(t))
	s.filterFrameLocked = true

	restore, err := s.dropFilter()
	if err != nil {
		t.Fatalf("dropFilter with a locked filter: %v", err)
	}
	if s.cachedFS.Frame(componentFilter) == nil {
		t.Error("locked filter was removed from the frame system")
	}
	restore()
}

func TestWithGlassAllowlist(t *testing.T) {
	// The pickup actions are the reason the allowlist exists: they start with
	// empty jaws, so a stand-in glass lands in the free approach plan.
	for _, name := range []string{"move_to_ice_dispense", "dispense_ice", "stage_glass", "pulse_ice_pin"} {
		if err := checkWithGlassAllowed(name); err != nil {
			t.Errorf("checkWithGlassAllowed(%q) = %v, want allowed", name, err)
		}
	}
	for _, name := range []string{"fetch_glass", "grab_staged_glass", "grab_brewed_cup", "fetch_milk", "grind_coffee", "lock_portafilter", "serve_iced_coffee"} {
		err := checkWithGlassAllowed(name)
		if err == nil {
			t.Errorf("checkWithGlassAllowed(%q) = nil, want refused", name)
			continue
		}
		// The operator has to learn what they can pass it to from the refusal.
		if !strings.Contains(err.Error(), "move_to_ice_dispense") {
			t.Errorf("refusal for %q does not name the permitted actions: %v", name, err)
		}
	}
}

// Every allowlisted name must be a real action, or the flag is accepted on
// something that then fails as unknown.
func TestWithGlassAllowlistNamesRealActions(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{CanServeIced: true}}
	actions := s.actionFuncs()
	for name := range withGlassActions {
		if _, ok := actions[name]; !ok {
			t.Errorf("withGlassActions names %q, which is not a registered action", name)
		}
	}
}
