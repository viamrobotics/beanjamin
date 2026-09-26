package coffee

// Arm steps: the Step description each brew phase is built from, and
// executeStep, which turns one into a direct move, pivot or circular motion.
// Also the step labels a running sequence publishes through setStep.

import (
	"context"
	"fmt"
	"time"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/module/trace"
)

const (
	shortPause   = 100 * time.Millisecond
	gripperPause = 500 * time.Millisecond
	pourPause    = 3 * time.Second
)

// StepLinearConstraint keeps a step's motion close to the straight line between
// its start and goal poses, within the given translation and orientation
// tolerances.
type StepLinearConstraint struct {
	LineToleranceMm          float64
	OrientationToleranceDegs float64
}

// AllowedCollision is a pair of frames permitted to touch during a step, for
// contact phases such as locking the portafilter or pressing a button.
type AllowedCollision struct {
	Frame1 string
	Frame2 string
}

// StepMoveOptions caps the arm's joint velocity and acceleration for a step.
type StepMoveOptions struct {
	MaxVelDegsPerSec  float64
	MaxAccDegsPerSec2 float64
}

// Step is one arm motion in a brew phase, run by executeStep: a move to
// PoseName (read from PoseSwitch), optionally shaped as a pivot, a circular
// motion, or a level no-spill carry, followed by Pause.
type Step struct {
	PoseName            string
	Pause               time.Duration
	LinearConstraint    *StepLinearConstraint
	MoveOptions         *StepMoveOptions
	AllowedCollisions   []AllowedCollision
	PivotFromPose       string
	PivotDegreesPerStep float64

	// PivotExtraDegrees rotates past PoseName by this much along the same axis,
	// then unwinds back onto it — all in one planned trajectory. It compensates
	// for the gripper slipping on the portafilter handle while the bayonet is
	// under load: the arm must over-rotate for the filter to seat, and the
	// unwind re-zeroes the grip, since a seated filter out-holds the claws so
	// the handle slides back through them.
	PivotExtraDegrees float64

	// NoSpill routes this step's move through the level carry (carryHeldLevel)
	// rather than a direct plan.
	NoSpill bool

	// PoseSwitch is the switch this step's pose is read from (fetchPose).
	PoseSwitch toggleswitch.Switch

	// Circular motion: move in small circles around PoseName to distribute
	// material (e.g. coffee grounds) evenly. The motion continues until
	// CircularDurationSec is exceeded.
	CircularRadiusMm     float64
	CircularDurationSec  float64
	CircularPointsPerRev int
}

// runSteps executes each step in order, wrapping the first failure with label
// (e.g. "tamp_ground") so the caller's error identifies the failed phase.
func (s *beanjaminCoffee) runSteps(ctx, cancelCtx context.Context, label string, steps ...Step) error {
	for _, step := range steps {
		if err := s.executeStep(ctx, cancelCtx, step); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

func (s *beanjaminCoffee) executeStep(ctx, cancelCtx context.Context, step Step) error {
	logger := s.activeOrderLogger()
	ctx, span := trace.StartSpan(ctx, "beanjamin::executeStep::"+step.PoseName)
	defer span.End()

	select {
	case <-ctx.Done():
		return fmt.Errorf("cancelled before %q: %w", step.PoseName, ctx.Err())
	case <-cancelCtx.Done():
		return fmt.Errorf("cancelled before %q", step.PoseName)
	default:
	}

	if step.PivotFromPose != "" {
		logger.Infof("pivoting from %q to %q", step.PivotFromPose, step.PoseName)
		if err := s.executePivot(ctx, cancelCtx, step); err != nil {
			return err
		}
	} else if step.CircularRadiusMm > 0 {
		logger.Infof("circular motion around %q", step.PoseName)
		if err := s.executeCircularMotion(ctx, cancelCtx, step); err != nil {
			return err
		}
	} else {
		logger.Infof("moving to %q", step.PoseName)
		if err := s.moveToPose(ctx, cancelCtx, step); err != nil {
			return err
		}
	}

	if step.Pause > 0 {
		logger.Infof("pausing %s after %q", step.Pause, step.PoseName)
		select {
		case <-time.After(step.Pause):
		case <-ctx.Done():
			return fmt.Errorf("cancelled during pause after %q: %w", step.PoseName, ctx.Err())
		case <-cancelCtx.Done():
			return fmt.Errorf("cancelled during pause after %q", step.PoseName)
		}
	}
	return nil
}

// Step labels surfaced through setStep -> get_queue, the order sensor's
// failed_step, and the web tracker. Constants so the brew sequence
// (prepare.go) and rewind recovery reference the same strings.
const (
	stepGrinding             = "Grinding"
	stepTamping              = "Tamping"
	stepLockingPortafilter   = "Locking portafilter"
	stepReleasingFilter      = "Releasing filter"
	stepPlacingCup           = "Placing cup"
	stepBrewing              = "Brewing"
	stepServing              = "Serving"
	stepGrabbingFilter       = "Grabbing filter"
	stepUnlockingPortafilter = "Unlocking portafilter"
	stepCleaning             = "Cleaning"
	stepAddingMilk           = "Adding milk"
	stepFinishingUp          = "Finishing up"
	stepRecoveringFilter     = "Recovering filter"
	// stepKeepAlive is published while a keep-alive purge runs. No order is
	// active, so it surfaces through Status/get_queue only, never on an order.
	stepKeepAlive = "Keep-alive purge"
)

func (s *beanjaminCoffee) setStep(step string) {
	s.currentStep.Store(step)
	// No-op when nothing is on the arm, which is what a keep-alive purge wants:
	// its step is service-global and belongs to no order.
	s.queue.SetCurrentStep(step)
}
