package coffee

// Pour poses anchored on the staged glass.
//
// The four pour poses (pour_approach/pour for the espresso cup,
// milk_pour_approach/milk_pour for the milk bottle) are not stored on the claws
// switch: they are configured as the pose of the held container (the held-item
// frame: on the grip point, +Z along the container's axis) relative to the
// center of the staged glass's rim, and the rim is read off the staged-glass
// obstacle in the frame system (its position and its height). The pours
// therefore follow wherever the glass was actually set down, and moving the
// staging area only means re-teaching staging.

import (
	"context"
	"fmt"

	"github.com/golang/geo/r3"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// glassRelativePoseField maps each pour pose to the config field holding its
// rim-relative offset. A claws pose absent from this map is read from the switch.
var glassRelativePoseField = map[string]string{
	clawPosePourApproach:     "pour_approach_relative_pose",
	clawPosePour:             "pour_relative_pose",
	clawPoseMilkPourApproach: "milk_pour_approach_relative_pose",
	clawPoseMilkPour:         "milk_pour_relative_pose",
}

// glassRelativePose returns the configured rim-relative offset for a pour pose.
func (s *beanjaminCoffee) glassRelativePose(poseName string) *RelativePose {
	switch poseName {
	case clawPosePourApproach:
		return s.cfg.PourApproachRelativePose
	case clawPosePour:
		return s.cfg.PourRelativePose
	case clawPoseMilkPourApproach:
		return s.cfg.MilkPourApproachRelativePose
	case clawPoseMilkPour:
		return s.cfg.MilkPourRelativePose
	}
	return nil
}

// resolvePose returns the goal for a named pose: a pour pose is resolved
// against the staged glass, anything else is read from its switch. Every motion
// kind resolves its poses here so the pours work for direct moves, no-spill
// carries and pivots alike.
func (s *beanjaminCoffee) resolvePose(ctx context.Context, sw toggleswitch.Switch, poseName string) (*poseData, error) {
	if field, ok := glassRelativePoseField[poseName]; ok && sw != nil && sw == s.clawsSw {
		rel := s.glassRelativePose(poseName)
		// Validate requires the pair wherever its pour can run; this only trips
		// on a manually-run action whose feature flag is off.
		if rel == nil {
			return nil, fmt.Errorf("resolve %q: %s is not configured", poseName, field)
		}
		return s.glassRelativePoseData(poseName, rel)
	}
	return s.fetchPose(ctx, sw, poseName)
}

// glassRelativePoseData resolves a rim-relative pour pose against the staged
// glass. It fails when no glass is staged, or no container is modeled in the
// gripper: pouring blind would empty the cup onto the table.
func (s *beanjaminCoffee) glassRelativePoseData(poseName string, rel *RelativePose) (*poseData, error) {
	if !s.heldItemAttached {
		return nil, fmt.Errorf("resolve %q: no container is modeled in the gripper to pour from", poseName)
	}
	glass, err := s.stagedGlassGeometry()
	if err != nil {
		return nil, fmt.Errorf("resolve %q against the staged glass: %w", poseName, err)
	}
	// The staged glass is always a box (containerBox), whose Z extent is the
	// glass height: the box is built upright and keeps its dims when lifted into
	// the world at staging.
	box := glass.ToProtobuf().GetBox()
	if box == nil || box.DimsMm == nil {
		return nil, fmt.Errorf("resolve %q against the staged glass: %q geometry is not a box", poseName, stagedGlassFrameName)
	}
	rim := glassRimCenter(glass.Pose(), box.DimsMm.Z)
	s.activeOrderLogger().Infof("resolving %q against the staged glass rim at %v", poseName, rim)
	return &poseData{
		pose:          composeCupPose(rim, relativePoseToSpatial(rel)),
		refFrame:      referenceframe.World,
		componentName: heldItemFrameName,
	}, nil
}

// stagedGlassGeometry returns the staged glass's world-frame geometry, as
// stageGlassAsObstacle recorded it. The frame is World-parented with an
// identity transform, so the geometry's own pose is already in world
// coordinates.
func (s *beanjaminCoffee) stagedGlassGeometry() (spatialmath.Geometry, error) {
	if !s.stagedGlassPlaced || s.cachedFS == nil {
		return nil, fmt.Errorf("no glass is staged in the frame system")
	}
	frame := s.cachedFS.Frame(stagedGlassFrameName)
	if frame == nil {
		return nil, fmt.Errorf("no %q frame in the frame system", stagedGlassFrameName)
	}
	gif, err := frame.Geometries([]referenceframe.Input{})
	if err != nil {
		return nil, fmt.Errorf("get %q geometry: %w", stagedGlassFrameName, err)
	}
	geos := gif.Geometries()
	if len(geos) == 0 {
		return nil, fmt.Errorf("%q frame carries no geometry", stagedGlassFrameName)
	}
	return geos[0], nil
}

// glassRimCenter returns the world position of the center of the rim of a glass
// of the given height whose centroid is at glass: half the height above the
// centroid along the glass's own axis.
func glassRimCenter(glass spatialmath.Pose, heightMm float64) r3.Vector {
	return spatialmath.Compose(glass, spatialmath.NewPoseFromPoint(r3.Vector{Z: heightMm / 2})).Point()
}
