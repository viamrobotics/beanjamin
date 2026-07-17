package coffee

// Fridge-door open: the arm grips the passive handle and pulls the door open
// along its hinge arc. The door is a static obstacle whose root frame origin
// sits on the hinge (verified via the live frame system — see the door-open
// design/plan docs), so rotating that frame about its local Z pivots the panel
// about the hinge. The handle chain (fridge-handle-top → -lower-bar → -ball)
// hangs off the door subtree and rides the rotation. θ is swept in software;
// at each step we re-place the static door obstacle at θ, read the handle
// ball's new world pose, and move the gripper to track it.

import (
	"fmt"
	"math"

	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

const (
	// frameFridgeDoor is the static door obstacle whose origin is the hinge.
	frameFridgeDoor = "fridge-door"
	// frameFridgeHandleBall is the grasp-target frame at the end of the handle
	// chain (a point frame; it has no geometry in the current machine config).
	frameFridgeHandleBall = "fridge-handle-ball"
)

// computeDoorSweep returns inclusive absolute-angle waypoints (degrees) from
// closedDeg to openDeg, one every ~degPerStep. The first waypoint is closedDeg
// and the last is exactly openDeg. Direction follows the sign of travel, so it
// also works when openDeg < closedDeg (a future close sweep). Mirrors the
// step-count logic of computePivotPoses (motion.go).
func computeDoorSweep(closedDeg, openDeg, degPerStep float64) []float64 {
	total := math.Abs(openDeg - closedDeg)
	numSteps := max(1, int(math.Round(total/degPerStep)))
	out := make([]float64, numSteps+1)
	for i := 0; i <= numSteps; i++ {
		t := float64(i) / float64(numSteps)
		out[i] = closedDeg + (openDeg-closedDeg)*t
	}
	return out
}

// setDoorTheta re-places the static door obstacle at thetaDeg about its own
// origin (the hinge). It composes Rz(θ) onto the door's original closed
// transform, then reuses the lockFilterFrame maneuver (motion.go): capture the
// door's descendants, remove the door, re-add it rotated, and re-attach the
// descendants (the handle chain) with their local transforms unchanged so they
// ride the swing. The door frame's own geometry (the panel, offset from the
// hinge) is preserved across the swap.
//
// baseDoorPose MUST be the door's original closed parent-relative transform,
// captured once by the caller and passed on every call, so repeated calls stay
// absolute rather than accumulating rotation.
func setDoorTheta(fs *referenceframe.FrameSystem, doorFrameName string, baseDoorPose spatialmath.Pose, thetaDeg float64) error {
	door := fs.Frame(doorFrameName)
	if door == nil {
		return fmt.Errorf("door frame %q not found", doorFrameName)
	}
	parent, err := fs.Parent(door)
	if err != nil {
		return fmt.Errorf("door parent: %w", err)
	}

	// Rotation about the door frame's local Z, applied at the origin (the hinge).
	rz := spatialmath.NewPoseFromOrientation(&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: thetaDeg})
	rotated := spatialmath.Compose(baseDoorPose, rz)

	// Preserve the door frame's own geometry (the panel), if any.
	var geom spatialmath.Geometry
	if geos, gerr := door.Geometries([]referenceframe.Input{}); gerr == nil && geos != nil && len(geos.Geometries()) > 0 {
		geom = geos.Geometries()[0]
	}

	descendants := collectDescendants(fs, doorFrameName)
	fs.RemoveFrame(door)

	var newDoor referenceframe.Frame
	if geom != nil {
		newDoor, err = referenceframe.NewStaticFrameWithGeometry(doorFrameName, rotated, geom)
	} else {
		newDoor, err = referenceframe.NewStaticFrame(doorFrameName, rotated)
	}
	if err != nil {
		return fmt.Errorf("build rotated door frame: %w", err)
	}
	if err := fs.AddFrame(newDoor, parent); err != nil {
		return fmt.Errorf("re-add door frame: %w", err)
	}
	for _, d := range descendants {
		p := fs.Frame(d.parentName)
		if err := fs.AddFrame(d.frame, p); err != nil {
			return fmt.Errorf("re-attach descendant %q under %q: %w", d.frame.Name(), d.parentName, err)
		}
	}
	return nil
}
