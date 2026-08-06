package main

// lockpose proposes replacement `coffee_locked_final` orientations that carry the
// portafilter further around the bayonet than the current filter-switch config does.
//
// lock_portafilter pivots from `coffee_in` to `coffee_locked_final` about a fixed
// point — executePivot rejects a pair whose translations differ by more than
// 0.5mm — slerping the orientation in PivotDegreesPerStep increments. So "lock it
// more" means continuing along that same rotation past its endpoint: take the
// axis-angle of the coffee_in -> coffee_locked_final delta and re-apply it about
// the same world-frame axis with a larger angle. The axis is printed so it can be
// sanity-checked against the group head's bore before anything is driven.

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/spatialmath"

	"beanjamin/multiposesexecutionswitch"
)

// Defaults mirror the filter pose switch as configured today, so the command is
// runnable with no flags. Pass -from/-to once the config moves on.
const (
	defaultLockFrom = "0.66033,-0.75038,0.02999,-180" // coffee_in
	defaultLockTo   = "0.98559,-0.16646,0.02998,-179" // coffee_locked_final
	defaultLockAdd  = "2,5,10,15"
)

func runLockPose(args []string) error {
	flagSet := flag.NewFlagSet("lockpose", flag.ExitOnError)
	fromFlag := flagSet.String("from", defaultLockFrom, "coffee_in orientation as \"ox,oy,oz,theta\" (theta in degrees)")
	toFlag := flagSet.String("to", defaultLockTo, "coffee_locked_final orientation as \"ox,oy,oz,theta\"")
	addFlag := flagSet.String("add", defaultLockAdd, "comma-separated extra degrees of lock to propose")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	from, err := parseOVD(*fromFlag, "-from")
	if err != nil {
		return err
	}
	to, err := parseOVD(*toFlag, "-to")
	if err != nil {
		return err
	}
	extras, err := parseDegreeList(*addFlag)
	if err != nil {
		return err
	}

	axis, lockDegrees := lockAxisAngle(from, to)

	fmt.Printf("coffee_in            o_x=% .5f  o_y=% .5f  o_z=% .5f  theta=% .2f\n", from.OX, from.OY, from.OZ, from.Theta)
	fmt.Printf("coffee_locked_final  o_x=% .5f  o_y=% .5f  o_z=% .5f  theta=% .2f\n", to.OX, to.OY, to.OZ, to.Theta)
	fmt.Printf("\ncurrent lock sweep: %.2f° about world axis (%.5f, %.5f, %.5f)\n", lockDegrees, axis.X, axis.Y, axis.Z)

	// Re-deriving the existing endpoint from the extracted axis must reproduce
	// coffee_locked_final exactly. If the axis or the compose order were wrong,
	// every candidate below would be wrong the same way and look plausible — this
	// is the one check that catches it before the arm moves.
	if check := rotateAbout(from, axis, lockDegrees); !spatialmath.OrientationAlmostEqual(check, to) {
		got := check.OrientationVectorDegrees()
		return fmt.Errorf("self-check failed: rotating coffee_in %.2f° about the derived axis gives "+
			"(%.5f, %.5f, %.5f, %.2f), want coffee_locked_final", lockDegrees, got.OX, got.OY, got.OZ, got.Theta)
	}
	fmt.Println("self-check: derived axis reproduces coffee_locked_final")

	for _, extra := range extras {
		total := lockDegrees + extra
		ovd := rotateAbout(from, axis, total).OrientationVectorDegrees()
		block, err := lockedFinalBlock(ovd)
		if err != nil {
			return err
		}
		fmt.Printf("\n--- +%.1f°  (total %.2f° from coffee_in) ---\n%s\n", extra, total, block)
	}
	return nil
}

// lockAxisAngle returns the world-frame rotation axis carrying `from` to `to`,
// and the sweep in degrees. The axis is flipped when the extracted angle is
// negative so the returned angle is always positive — callers add to it to
// rotate further in the same direction, which only reads correctly one way
// round. Mirrors computePivotPoses, which takes the magnitude for the same reason.
func lockAxisAngle(from, to spatialmath.Orientation) (r3.Vector, float64) {
	aa := spatialmath.OrientationBetween(from, to).AxisAngles()
	axis := r3.Vector{X: aa.RX, Y: aa.RY, Z: aa.RZ}
	degrees := aa.Theta * 180 / math.Pi
	if degrees < 0 {
		return axis.Mul(-1), -degrees
	}
	return axis, degrees
}

// rotateAbout rotates `from` through `degrees` about the world-frame `axis`.
// Composing zero-translation poses composes their rotations, and the delta is
// applied on the left to match OrientationBetween's q2 * conj(q1) convention.
func rotateAbout(from spatialmath.Orientation, axis r3.Vector, degrees float64) spatialmath.Orientation {
	delta := &spatialmath.R4AA{Theta: degrees * math.Pi / 180, RX: axis.X, RY: axis.Y, RZ: axis.Z}
	return spatialmath.Compose(
		spatialmath.NewPoseFromOrientation(delta),
		spatialmath.NewPoseFromOrientation(from),
	).Orientation()
}

// lockedFinalBlock renders the pose entry to paste over coffee_locked_final. It
// marshals the switch's own PoseConf so the emitted keys cannot drift from what
// the switch actually parses.
func lockedFinalBlock(ovd *spatialmath.OrientationVectorDegrees) (string, error) {
	conf := multiposesexecutionswitch.PoseConf{
		PoseName: "coffee_locked_final",
		Baseline: "coffee_in",
		Orientation: &multiposesexecutionswitch.Orientation{
			OX:    round5(ovd.OX),
			OY:    round5(ovd.OY),
			OZ:    round5(ovd.OZ),
			Theta: round5(ovd.Theta),
		},
	}
	out, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal pose block: %w", err)
	}
	return string(out), nil
}

// round5 trims to 5 decimal places, matching the precision the existing poses are
// written at — the switch config is hand-edited and full float64 noise is unreadable.
func round5(f float64) float64 {
	return math.Round(f*1e5) / 1e5
}

func parseOVD(s, flagName string) (*spatialmath.OrientationVectorDegrees, error) {
	fields, err := parseFloats(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flagName, err)
	}
	if len(fields) != 4 {
		return nil, fmt.Errorf("%s: want \"ox,oy,oz,theta\" (4 values), got %d in %q", flagName, len(fields), s)
	}
	return &spatialmath.OrientationVectorDegrees{OX: fields[0], OY: fields[1], OZ: fields[2], Theta: fields[3]}, nil
}

func parseDegreeList(s string) ([]float64, error) {
	fields, err := parseFloats(s)
	if err != nil {
		return nil, fmt.Errorf("-add: %w", err)
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("-add: no degree values given")
	}
	return fields, nil
}

func parseFloats(s string) ([]float64, error) {
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", p)
		}
		out = append(out, f)
	}
	return out, nil
}
