package coffee

import (
	"context"
	"testing"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/testutils/inject"
)

// stageHeldMugAt publishes a scene with the held item staged as the mug with
// the frames standing at fsInputs.
func stageHeldMugAt(s *beanjaminCoffee, fsInputs referenceframe.FrameSystemInputs) error {
	return s.editScene(func(sc *sceneState) error {
		_, err := sc.stageHeldItem(placement{inputs: fsInputs}, stagedMug)
		return err
	})
}

func TestResetClearsStagedMug(t *testing.T) {
	fs := stagingFS(t, true)
	s := stagingService(t, fs)
	if err := stageHeldMugAt(s, referenceframe.NewZeroInputs(fs)); err != nil {
		t.Fatalf("stage mug: %v", err)
	}
	if !s.scene().stagedMugPlaced || s.scene().fs.Frame(stagedMugFrameName) == nil {
		t.Fatal("the mug should be staged before the reset")
	}
	// A staged mug is in-flight state: the clean-world refresh must leave it.
	if err := s.refreshFrameSystemIfClean(context.Background()); err != nil {
		t.Fatalf("refreshFrameSystemIfClean: %v", err)
	}
	if !s.scene().stagedMugPlaced {
		t.Fatal("refreshFrameSystemIfClean rebuilt the scene over a staged mug")
	}
	fsSvc := inject.NewFrameSystemService("fs")
	fsSvc.FrameSystemConfigFunc = func(context.Context) (*framesystem.Config, error) {
		return &framesystem.Config{}, nil
	}
	s.fsSvc = fsSvc
	if err := s.resetFrameSystem(context.Background()); err != nil {
		t.Fatalf("resetFrameSystem: %v", err)
	}
	if s.scene().stagedMugPlaced || s.scene().fs.Frame(stagedMugFrameName) != nil {
		t.Error("reset must clear the staged mug and its obstacle frame")
	}
}

func TestStagedMugGrabCollisions(t *testing.T) {
	fs := stagingFS(t, true)
	s := stagingService(t, fs)
	if got := s.scene().stagedGrabCollisions(stagedMug); got != nil {
		t.Errorf("no mug staged: collisions = %v, want none", got)
	}
	if err := stageHeldMugAt(s, referenceframe.NewZeroInputs(fs)); err != nil {
		t.Fatalf("stage mug: %v", err)
	}
	if s.scene().stagedGlassPlaced || s.scene().stagedGlassGrabCollisions() != nil {
		t.Error("staging the mug must not mark a glass staged")
	}
	got := s.scene().stagedGrabCollisions(stagedMug)
	if len(got) == 0 {
		t.Fatal("staged mug: want jaw collisions allowed")
	}
	for _, c := range got {
		if c.Frame1 != stagedMugFrameName {
			t.Errorf("collision %v should name %s", c, stagedMugFrameName)
		}
	}
}

func TestDispenseHotWaterStubIsNoop(t *testing.T) {
	s, _ := newTestCoffee(t, nil)
	if err := s.dispenseHotWater(context.Background(), context.Background(), "americano"); err != nil {
		t.Errorf("dispenseHotWater = %v, want nil", err)
	}
}

var mugActions = []string{
	"pick_mug", "set_mug_for_coffee", "grab_brewed_mug", "slide_mug_to_water", "set_mug_at_water",
	"dispense_hot_water", "regrab_mug_at_water", "place_mug_at_water", "serve_americano", "serve_hot_water",
}

func TestMugActionsRegisteredWithMugDrinks(t *testing.T) {
	mug := (&beanjaminCoffee{cfg: &Config{CanServeMugDrinks: true}}).actionFuncs()
	plain := (&beanjaminCoffee{cfg: &Config{}}).actionFuncs()
	for _, name := range mugActions {
		if _, ok := mug[name]; !ok {
			t.Errorf("%s is not registered on a machine that serves mug drinks", name)
		}
		if _, ok := plain[name]; ok {
			t.Errorf("%s is registered on a machine that cannot serve mug drinks", name)
		}
	}
}
