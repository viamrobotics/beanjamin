package beanjamin

import (
	"fmt"
	"math"
	"strconv"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
)

// JointLimitDegs specifies a joint range in degrees.
type JointLimitDegs struct {
	MinDegs float64 `json:"min_degs"`
	MaxDegs float64 `json:"max_degs"`
}

// applyJointLimits replaces the frames named in overrides with copies whose
// joint limits are narrowed. Keys in the inner map may be either the moveable
// frame's name or its stringified index (e.g. "5" for the last joint of a
// 6-DoF arm). Limits are provided in degrees and converted to radians.
//
// Adapted from viamrobotics/sanding's lib/framesys.ApplyJointLimits.
func applyJointLimits(logger logging.Logger, fs *referenceframe.FrameSystem, overrides map[string]map[string]JointLimitDegs) error {
	for fName, mods := range overrides {
		f := fs.Frame(fName)
		if f == nil {
			return fmt.Errorf("frame %q in input_range_override doesn't exist", fName)
		}

		sm, ok := f.(*referenceframe.SimpleModel)
		if !ok {
			return fmt.Errorf("can only override joints for SimpleModel, not %T (frame %q)", f, fName)
		}

		resolved := make(map[string]referenceframe.Limit, len(mods))
		moveableNames := sm.MoveableFrameNames()
		for key, limit := range mods {
			if limit.MinDegs >= limit.MaxDegs {
				return fmt.Errorf("invalid joint limit for %q joint %q: min_degs (%v) must be < max_degs (%v)", fName, key, limit.MinDegs, limit.MaxDegs)
			}
			matched := false
			for i, name := range moveableNames {
				if key == name || key == strconv.Itoa(i) {
					resolved[name] = referenceframe.Limit{
						Min: limit.MinDegs * math.Pi / 180.0,
						Max: limit.MaxDegs * math.Pi / 180.0,
					}
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("frame %q has no joint matching %q (moveable joints: %v)", fName, key, moveableNames)
			}
		}

		newModel, err := referenceframe.NewModelWithLimitOverrides(sm, resolved)
		if err != nil {
			return fmt.Errorf("override limits on %q: %w", fName, err)
		}
		if err := fs.ReplaceFrame(newModel); err != nil {
			return fmt.Errorf("replace frame %q: %w", fName, err)
		}
		logger.Infof("applied joint limit overrides to frame %q: %v", fName, mods)
	}
	return nil
}
