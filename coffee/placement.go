package coffee

import (
	"context"
	"errors"
	"fmt"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// placement is where the frames stand for a scene edit or a pivot's start.
//
// Usually that is inputs. Inside a chunk whose recorded motions are not planned
// yet the arm's joints are unknown, so inputs leaves the arm out and goal says
// where the last recorded motion brings goalFrame, a frame on the arm. Every
// frame on the arm is rigidly attached to its end, so that one pose places them
// all, whatever joints the planner picks to reach it.
type placement struct {
	inputs referenceframe.FrameSystemInputs

	arm       string
	goalFrame string
	goal      *referenceframe.PoseInFrame
}

// errUnplaceable means a placement cannot locate the arm's frames: the last
// recorded motion's goal does not pin down where the arm's end will be.
var errUnplaceable = errors.New("the recorded motions do not say where the arm will be")

// expectedPlacement returns where the frames will stand once every move handed
// off or recorded so far has run. Inside a chunk with unplanned motions it
// places the arm by the last one's goal; when that goal cannot place it, the
// chunk is planned and handed off first (expectedInputs).
func (s *beanjaminCoffee) expectedPlacement(ctx context.Context) (placement, error) {
	if r := s.run; r != nil && r.chunk.hasMotions() {
		if at, ok := s.goalPlacement(r.chunk.armlessStart(s.cfg.ArmName)); ok {
			return at, nil
		}
	}
	inputs, err := s.expectedInputs(ctx)
	if err != nil {
		return placement{}, err
	}
	return placement{inputs: inputs}, nil
}

// goalPlacement places the arm by the goal of the last motion recorded in the
// open chunk, with every other frame at inputs. It reports false when there is
// no such goal, or it names a frame off the arm, or is expressed relative to a
// frame on the arm.
func (s *beanjaminCoffee) goalPlacement(inputs referenceframe.FrameSystemInputs) (placement, bool) {
	if s.run == nil {
		return placement{}, false
	}
	frame, goal := s.run.chunk.lastGoal()
	if goal == nil {
		return placement{}, false
	}
	fs, arm := s.scene().fs, s.cfg.ArmName
	if !onArm(fs, arm, frame) || onArm(fs, arm, goal.Parent()) {
		return placement{}, false
	}
	return placement{inputs: inputs, arm: arm, goalFrame: frame, goal: goal}, true
}

// fullInputs returns p's inputs for every frame of fs, with the arm at zero
// when p does not know its joints. Frames off the arm do not depend on them.
func (p placement) fullInputs(fs *referenceframe.FrameSystem) referenceframe.FrameSystemInputs {
	inputs := referenceframe.NewZeroInputs(fs)
	for name, in := range p.inputs {
		inputs[name] = in
	}
	return inputs
}

// transform is fs.Transform for the frames standing at p.
func (p placement) transform(
	fs *referenceframe.FrameSystem, tf referenceframe.Transformable, dst string,
) (referenceframe.Transformable, error) {
	if p.goal == nil {
		return fs.Transform(p.inputs.ToLinearInputs(), tf, dst)
	}
	in := p.fullInputs(fs).ToLinearInputs()
	// With the arm at zero, every frame on it is off by the same rigid motion:
	// the one that takes goalFrame from where it stands to where the goal puts it.
	goalWorld, err := fs.Transform(in, p.goal, referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("locate the goal for %q: %w", p.goalFrame, err)
	}
	zeroWorld, err := fs.Transform(in, referenceframe.NewPoseInFrame(p.goalFrame, spatialmath.NewZeroPose()), referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("locate %q: %w", p.goalFrame, err)
	}
	fix := spatialmath.Compose(goalWorld.(*referenceframe.PoseInFrame).Pose(),
		spatialmath.PoseInverse(zeroWorld.(*referenceframe.PoseInFrame).Pose()))

	world, err := fs.Transform(in, tf, referenceframe.World)
	if err != nil {
		return nil, err
	}
	if onArm(fs, p.arm, tf.Parent()) {
		world = moved(world, fix)
	}
	if dst == referenceframe.World {
		return world, nil
	}
	dstWorld, err := fs.Transform(in, referenceframe.NewPoseInFrame(dst, spatialmath.NewZeroPose()), referenceframe.World)
	if err != nil {
		return nil, err
	}
	dstPose := dstWorld.(*referenceframe.PoseInFrame).Pose()
	if onArm(fs, p.arm, dst) {
		dstPose = spatialmath.Compose(fix, dstPose)
	}
	out := moved(world, spatialmath.PoseInverse(dstPose))
	switch t := out.(type) {
	case *referenceframe.PoseInFrame:
		return referenceframe.NewPoseInFrame(dst, t.Pose()), nil
	case *referenceframe.GeometriesInFrame:
		return referenceframe.NewGeometriesInFrame(dst, t.Geometries()), nil
	}
	return out, nil
}

// worldPose returns where frame stands in the world, at p.
func (p placement) worldPose(fs *referenceframe.FrameSystem, frame string) (spatialmath.Pose, error) {
	tf, err := p.transform(fs, referenceframe.NewPoseInFrame(frame, spatialmath.NewZeroPose()), referenceframe.World)
	if err != nil {
		return nil, err
	}
	return tf.(*referenceframe.PoseInFrame).Pose(), nil
}

// moved returns tf, in its own frame, moved by by.
func moved(tf referenceframe.Transformable, by spatialmath.Pose) referenceframe.Transformable {
	switch t := tf.(type) {
	case *referenceframe.PoseInFrame:
		return referenceframe.NewPoseInFrame(t.Parent(), spatialmath.Compose(by, t.Pose()))
	case *referenceframe.GeometriesInFrame:
		geos := make([]spatialmath.Geometry, 0, len(t.Geometries()))
		for _, g := range t.Geometries() {
			geos = append(geos, g.Transform(by))
		}
		return referenceframe.NewGeometriesInFrame(t.Parent(), geos)
	}
	return tf
}

// onArm reports whether frame is the arm named arm or hangs off it, so moves
// with the arm's end.
func onArm(fs *referenceframe.FrameSystem, arm, frame string) bool {
	f := fs.Frame(frame)
	for f != nil {
		if f.Name() == arm {
			return true
		}
		parent, err := fs.Parent(f)
		if err != nil {
			return false
		}
		f = parent
	}
	return false
}
