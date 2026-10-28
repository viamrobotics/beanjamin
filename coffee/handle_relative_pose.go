package coffee

// Handoff poses anchored on the locked portafilter's handle.
//
// release_filter and grab_filter work on a portafilter the bayonet holds at the
// filter switch's coffee_locked_final. The claws' filter_released and
// coffee_locked_final are not stored on the claws switch but derived from that
// filter pose: each is the grip point's pose relative to the handle frame
// (handleRelativePose), composed onto where the handle sits when the filter is
// locked. Re-teaching the lock pose then moves the whole handoff with it.

import (
	"context"
	"fmt"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// portafilterHandleFrame is the handle's frame, a descendant of the filter frame.
const portafilterHandleFrame = "portafilter-handle"

// handleRelativePoses are the claws poses resolved against the handle.
var handleRelativePoses = []string{clawPoseFilterReleased, clawPoseCoffeeLockedFinal}

// handleRelativePose returns the configured handle-relative offset for a
// handoff pose and the config field it comes from, and whether poseName is one.
func (s *beanjaminCoffee) handleRelativePose(poseName string) (*RelativePose, string, bool) {
	switch poseName {
	case clawPoseFilterReleased:
		return s.cfg.FilterReleasedGripRelativePose, "filter_released_grip_relative_pose", true
	case clawPoseCoffeeLockedFinal:
		return s.cfg.CoffeeLockedFinalGripRelativePose, "coffee_locked_final_grip_relative_pose", true
	}
	return nil, "", false
}

// isHandleRelativePose reports whether poseName on sw is resolved against the
// locked handle rather than read from the switch.
func (s *beanjaminCoffee) isHandleRelativePose(sw toggleswitch.Switch, poseName string) bool {
	_, _, ok := s.handleRelativePose(poseName)
	return ok && sw != nil && sw == s.clawsSw
}

// handleRelativeGripPoseData resolves a claws handoff pose: the filter switch's
// coffee_locked_final, moved out to the handle, then offset by the pose's
// configured offset in the handle's own axes.
//
// It anchors on the authored filter pose rather than the locked filter frame so
// it resolves the same whether or not lockFilterFrame ran (a manual grab_filter
// after reset_world, with the portafilter still in the machine).
func (s *beanjaminCoffee) handleRelativeGripPoseData(ctx context.Context, poseName string) (*poseData, error) {
	rel, field, ok := s.handleRelativePose(poseName)
	if !ok {
		return nil, fmt.Errorf("resolve %q: not a portafilter-handle-relative pose", poseName)
	}
	// Validate requires it; this only trips on a hand-built service.
	if rel == nil {
		return nil, fmt.Errorf("resolve %q: %s is not configured", poseName, field)
	}
	locked, err := s.fetchPose(ctx, s.filterSw, filterPoseCoffeeLockedFinal)
	if err != nil {
		return nil, fmt.Errorf("resolve %q against the portafilter handle: %w", poseName, err)
	}
	filterToHandle, err := s.filterToHandle()
	if err != nil {
		return nil, fmt.Errorf("resolve %q against the portafilter handle: %w", poseName, err)
	}
	handle := spatialmath.Compose(locked.pose, filterToHandle)
	return &poseData{
		pose:          spatialmath.Compose(handle, relativePoseToSpatial(rel)),
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
