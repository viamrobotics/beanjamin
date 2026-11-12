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
	// The arm is at zero in these inputs. That leaves the frames on it where the
	// arm's joints would not put them, but rigid to one another, so a transform
	// that stays on one side of the arm's end is right as it is.
	in := p.fullInputs(fs).ToLinearInputs()
	srcOnArm, dstOnArm := onArm(fs, p.arm, tf.Parent()), onArm(fs, p.arm, dst)
	if srcOnArm == dstOnArm {
		return fs.Transform(in, tf, dst)
	}
	goalTF, err := fs.Transform(in, p.goal, referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("locate the goal for %q: %w", p.goalFrame, err)
	}
	goalWorld := goalTF.(*referenceframe.PoseInFrame).Pose()

	if srcOnArm {
		rel, err := fs.Transform(in, tf, p.goalFrame)
		if err != nil {
			return nil, err
		}
		return fs.Transform(in, inFrame(moved(rel, goalWorld), referenceframe.World), dst)
	}
	// Geometries can't be handed to fs.Transform from a frame on the arm: it
	// would skip that frame's own transform. Move them into dst by hand.
	world, err := fs.Transform(in, tf, referenceframe.World)
	if err != nil {
		return nil, err
	}
	dstRel, err := fs.Transform(in, referenceframe.NewPoseInFrame(dst, spatialmath.NewZeroPose()), p.goalFrame)
	if err != nil {
		return nil, err
	}
	dstWorld := spatialmath.Compose(goalWorld, dstRel.(*referenceframe.PoseInFrame).Pose())
	return inFrame(moved(world, spatialmath.PoseInverse(dstWorld)), dst), nil
}

// inFrame returns tf, unchanged, as expressed in frame.
func inFrame(tf referenceframe.Transformable, frame string) referenceframe.Transformable {
	switch t := tf.(type) {
	case *referenceframe.PoseInFrame:
		return referenceframe.NewPoseInFrame(frame, t.Pose())
	case *referenceframe.GeometriesInFrame:
		return referenceframe.NewGeometriesInFrame(frame, t.Geometries())
	}
	return tf
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
