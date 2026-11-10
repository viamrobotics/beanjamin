package coffee

import (
	"fmt"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// sceneState is the cell as one point in a run leaves it: the frame system plans
// are checked against, where the arm stands, and what the frame system cannot
// hold on its own. A scene is never changed once it is published (editScene):
// the next one is built from a copy, so the execution goroutine can hold one
// scene while planning moves on to the next.
type sceneState struct {
	// fs is never changed in place once published. Scenes that differ only in
	// inputs share it.
	fs *referenceframe.FrameSystem
	// inputs is where the frames with degrees of freedom stand, or nil when the
	// run has not planned it and the arm has to be read.
	inputs referenceframe.FrameSystemInputs

	// heldCupGeom, heldGlassGeom and heldMilkGeom are the gripper-local geometry
	// of the cup and glass (detected at pickup) and of the milk bottle (modeled
	// from milk_bottle_dimensions), kept so a re-grab can restore them.
	heldCupGeom   spatialmath.Geometry
	heldGlassGeom spatialmath.Geometry
	heldMilkGeom  spatialmath.Geometry
	// heldItemAttached is whether the held-item frame is in fs.
	heldItemAttached bool

	// heldMilk names the milk bottle out of the fridge, set by fetchMilkBottle and
	// read by returnMilkBottle to put it back at its own spot (milk.go). Empty
	// when no bottle is out.
	heldMilk string

	// filterFrameLocked is whether lockFilterFrame has re-parented the filter
	// frame to the world in fs.
	filterFrameLocked bool

	// stagedGlassPlaced is whether stageGlassAsObstacle has added the released
	// glass to the world in fs.
	stagedGlassPlaced bool

	// doorOpenDegs is how far the fridge door physically stands open, in degrees
	// about its hinge (0 = shut). The frame system always rebuilds with the door
	// at its authored shut transform, so this is the only record that survives a
	// rebuild — resetFrameSystem re-applies it, keeping the modeled panel where
	// the real one actually is instead of snapping it closed behind the arm's
	// back. Cleared only by the two commands in which an operator asserts the
	// world is as configured — reset_world and proceed — and never by a rebuild
	// on its own.
	doorOpenDegs float64
}

// clone returns a copy of sc whose frame system can be edited without touching
// sc's.
func (sc *sceneState) clone() (*sceneState, error) {
	fs, err := copyFrameSystem(sc.fs)
	if err != nil {
		return nil, err
	}
	next := *sc
	next.fs = fs
	return &next, nil
}

// withInputs returns sc with the frames standing at inputs.
func (sc *sceneState) withInputs(inputs referenceframe.FrameSystemInputs) *sceneState {
	next := *sc
	next.inputs = inputs
	return &next
}

// withArm returns sc with the arm named arm standing at joints and every other
// frame where sc has it, or at zero when sc has not planned where it stands.
func (sc *sceneState) withArm(arm string, joints []referenceframe.Input) *sceneState {
	inputs := referenceframe.NewZeroInputs(sc.fs)
	for name, in := range sc.inputs {
		inputs[name] = in
	}
	inputs[arm] = joints
	return sc.withInputs(inputs)
}

// armInputs returns where sc has the arm named arm standing, or nil when the
// arm has to be read.
func (sc *sceneState) armInputs(arm string) []referenceframe.Input {
	if sc.inputs == nil {
		return nil
	}
	return sc.inputs[arm]
}

// copyFrameSystem returns a frame system with fs's structure. The frames
// themselves are shared: a scene's frame system only ever has frames added,
// removed and replaced, never changed in place, so the copy is unaffected by
// what is done to fs afterwards and the other way round.
func copyFrameSystem(fs *referenceframe.FrameSystem) (*referenceframe.FrameSystem, error) {
	out := referenceframe.NewEmptyFrameSystem(fs.Name())
	if err := out.MergeFrameSystem(fs, out.World()); err != nil {
		return nil, fmt.Errorf("copy frame system: %w", err)
	}
	return out, nil
}

// scene returns the scene planning works against: the planned scene of the run
// holding the arm, or the settled scene between runs. Callers must not change
// it; editScene publishes a new one.
func (s *beanjaminCoffee) scene() *sceneState {
	if s.run != nil {
		return s.run.planned
	}
	return s.settledScene
}

// editScene applies edit to a copy of the scene and publishes the copy. When
// edit fails nothing is published.
func (s *beanjaminCoffee) editScene(edit func(sc *sceneState) error) error {
	next, err := s.scene().clone()
	if err != nil {
		return err
	}
	if err := edit(next); err != nil {
		return err
	}
	s.publishScene(next)
	return nil
}

// publishScene makes next the scene planning works against. In a run it also
// becomes the executed scene once every move handed off before it has run, so
// a failure in one of those moves leaves the executed scene without it.
func (s *beanjaminCoffee) publishScene(next *sceneState) {
	r := s.run
	if r == nil {
		s.settledScene = next
		return
	}
	r.planned = next
	if r.moves != nil {
		r.moves <- move{name: "scene", scene: next}
		return
	}
	r.executed.Store(next)
}
