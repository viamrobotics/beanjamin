package coffee

// The re-grab pose anchored on the locked portafilter's handle.
//
// grab_filter closes the claws on a portafilter the bayonet holds at the filter
// switch's coffee_locked_final. The claws' coffee_locked_final is not stored on
// the claws switch but derived from that filter pose: the grip point's pose
// relative to the handle frame (coffee_locked_final_grip_relative_pose),
// composed onto where the handle sits when the filter is locked. Re-teaching
// the lock pose then moves the re-grab with it.

import (
	"context"
	"fmt"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// portafilterHandleFrame is the handle's frame, a descendant of the filter frame.
const portafilterHandleFrame = "portafilter-handle"

// isHandleRelativePose reports whether poseName on sw is resolved against the
// locked handle rather than read from the switch.
func (s *beanjaminCoffee) isHandleRelativePose(sw toggleswitch.Switch, poseName string) bool {
	return poseName == clawPoseCoffeeLockedFinal && sw != nil && sw == s.clawsSw
}

// handleRelativeGripPoseData resolves the claws' coffee_locked_final: the
// filter switch's coffee_locked_final, moved out to the handle, then offset by
// coffee_locked_final_grip_relative_pose in the handle's own axes.
//
// It anchors on the authored filter pose rather than the locked filter frame so
// it resolves the same whether or not lockFilterFrame ran (a manual grab_filter
// after reset_world, with the portafilter still in the machine).
func (s *beanjaminCoffee) handleRelativeGripPoseData(ctx context.Context) (*poseData, error) {
	// Validate requires it; this only trips on a hand-built service.
	if s.cfg.CoffeeLockedFinalGripRelativePose == nil {
		return nil, fmt.Errorf("resolve %q: coffee_locked_final_grip_relative_pose is not configured", clawPoseCoffeeLockedFinal)
	}
	locked, err := s.fetchPose(ctx, s.filterSw, filterPoseCoffeeLockedFinal)
	if err != nil {
		return nil, fmt.Errorf("resolve %q against the portafilter handle: %w", clawPoseCoffeeLockedFinal, err)
	}
	filterToHandle, err := s.filterToHandle()
	if err != nil {
		return nil, fmt.Errorf("resolve %q against the portafilter handle: %w", clawPoseCoffeeLockedFinal, err)
	}
	handle := spatialmath.Compose(locked.pose, filterToHandle)
	return &poseData{
		pose:          spatialmath.Compose(handle, relativePoseToSpatial(s.cfg.CoffeeLockedFinalGripRelativePose)),
		refFrame:      locked.refFrame,
		componentName: gripPoint,
	}, nil
}

// filterToHandle returns the handle frame's pose in the filter frame. The two
// are rigidly linked, so the transform holds wherever the filter is parented.
func (s *beanjaminCoffee) filterToHandle() (spatialmath.Pose, error) {
	if s.cachedFS == nil {
		return nil, fmt.Errorf("no frame system")
	}
	tf, err := s.cachedFS.Transform(
		referenceframe.NewZeroInputs(s.cachedFS).ToLinearInputs(),
		referenceframe.NewPoseInFrame(portafilterHandleFrame, spatialmath.NewZeroPose()),
		componentFilter,
	)
	if err != nil {
		return nil, fmt.Errorf("transform %q into %q: %w", portafilterHandleFrame, componentFilter, err)
	}
	return tf.(*referenceframe.PoseInFrame).Pose(), nil
}
