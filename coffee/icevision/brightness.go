package icevision

// The brightness shadow: a second, independent read of every dispense frame,
// logged beside the contrast step and never allowed to decide anything.
//
// The contrast step ships because it is illumination-invariant and an absolute
// cutoff is not. But the threshold method sees rows the step structurally
// cannot — it needs no window, so it has no dead band at either end of the scan
// — and on the committed fixtures it separates the same glasses correctly. That
// is worth measuring on real dispenses rather than arguing about from fixtures.
// Set ice_brightness_thresh and every dispense reports what the other method
// would have done, and when.

import (
	"fmt"
	"time"

	"go.viam.com/rdk/logging"
)

// defaultIceBrightRun is how many consecutive rows must clear the threshold.
// One bright row is a glint off the glass; ice holds its brightness over many
// rows. This is the threshold method's whole glare defense — the job the
// contrast window does for the step.
const defaultIceBrightRun = 8

// ShadowOn reports whether the shadow runs. An unset threshold is the off
// switch, and deliberately has no default: an absolute brightness cutoff is the
// one number that does not survive a change in lighting, so it has to be read
// off the machine it will run on (check_ice_level prints it).
func (p Params) ShadowOn() bool {
	return p.BrightnessThresh > 0
}

// ShadowRun returns how many consecutive rows must clear the threshold,
// configured or default.
func (p Params) ShadowRun() int {
	return orDefault(p.BrightRun, defaultIceBrightRun)
}

// brightnessSurfaceRow finds the ice surface by absolute brightness: the
// topmost run of `run` consecutive rows at or above thresh, as an absolute
// image row. Pure, so it tests against the same fixtures the step does.
//
// Two differences from contrastStep are what the shadow exists to measure. It
// takes the topmost hit rather than the strongest edge anywhere in the band,
// and it needs no rows above the candidate, so it can name rows the step cannot
// — the whole band instead of the band inset by a window at each end. A full
// glass therefore reports a row near the band top rather than reporting
// nothing, which is why the shadow's stop test is a row comparison and not the
// absence the step's latch is built around.
func brightnessSurfaceRow(rows []float64, b Band, thresh float64, run int) Reading {
	if run < 1 {
		run = 1
	}
	for i := 0; i+run <= len(rows); i++ {
		bright := true
		for j := 0; j < run; j++ {
			if rows[i+j] < thresh {
				bright = false
				break
			}
		}
		if bright {
			return Reading{Row: b.Y0 + i, Step: rows[i], Found: true}
		}
	}
	return Reading{}
}

// Shadow follows the shadow through one dispense so the comparison
// logs as a few edges rather than one line per poll. A 30-second dispense is
// ~60 polls; logging each one buries the lines that matter and trips the
// logger's own repeat suppression.
type Shadow struct {
	on      bool
	stopRow int

	sawSurface bool
	firstSeen  time.Duration
	firstRow   int

	wouldStop   bool
	wouldStopAt time.Duration
	stoppedRow  int
	// stoppedOnFirstSighting means the very first surface it ever saw was
	// already at or above the stop row. A real fill is watched rising: seen low
	// in the band, reaching the stop row polls later. Arriving already there
	// means it latched onto something that was there all along — the glass rim,
	// or a reflection — which on a live run is a pin closed over an empty glass.
	stoppedOnFirstSighting bool

	// closestRow is the nearest the shadow ever came to the stop row, which is
	// the only useful thing to report about a run where it never fired.
	closestRow int
}

// NewShadow starts following one dispense.
func (p Params) NewShadow() *Shadow {
	return &Shadow{
		on:         p.ShadowOn(),
		stopRow:    p.StopRow(),
		closestRow: -1,
	}
}

// On reports whether the shadow runs on this dispense.
func (b *Shadow) On() bool {
	return b.on
}

// Observe folds one frame's shadow reading in, logging only the two edges:
// first sighting, and the first frame its own stop test would have fired on.
// contrast is a short description of what the deciding method saw on the same
// frame, so a disagreement is readable without cross-referencing timestamps.
func (b *Shadow) Observe(r Reading, elapsed time.Duration, contrast string, logger logging.Logger) {
	if !b.on || !r.Found {
		return
	}
	if b.closestRow < 0 || r.Row < b.closestRow {
		b.closestRow = r.Row
	}
	firstSighting := !b.sawSurface
	if firstSighting {
		b.sawSurface, b.firstSeen, b.firstRow = true, elapsed, r.Row
		logger.Infof("dispensing ice: brightness shadow first saw a surface at row %d (brightness %.0f) after %s — contrast %s",
			r.Row, r.Step, elapsed.Round(time.Millisecond), contrast)
	}
	if !b.wouldStop && r.Row <= b.stopRow {
		b.wouldStop, b.wouldStopAt, b.stoppedRow = true, elapsed, r.Row
		b.stoppedOnFirstSighting = firstSighting
		logger.Infof("dispensing ice: brightness shadow would close the pin now — row %d is at or above stop row %d, %s in — contrast %s",
			r.Row, b.stopRow, elapsed.Round(time.Millisecond), contrast)
	}
}

// Verdict compares the finished run against what the deciding method did and
// returns the summary line plus whether the two disagreed.
//
// A timing difference is not a disagreement. The shadow sees rows the step
// structurally cannot, so it is *expected* to run ahead — that gap is the
// number this whole thing exists to gather, including where it runs ahead of
// the step's first sighting. A disagreement is a difference in kind: never
// seeing ice, seeing it but never reaching the stop row, or reaching the stop
// row on the first frame it saw anything at all. The last is the one that would
// disqualify the swap.
func (b *Shadow) Verdict(contrastStopped bool, contrastAt, contrastFirstSeen time.Duration) (string, bool) {
	if !b.on {
		return "", false
	}
	outcome := fmt.Sprintf("contrast rode to its ceiling at %s", contrastAt.Round(time.Millisecond))
	if contrastStopped {
		outcome = fmt.Sprintf("contrast closed the pin at %s", contrastAt.Round(time.Millisecond))
	}
	switch {
	case !b.sawSurface:
		return fmt.Sprintf("%s; brightness shadow never saw ice at all — its threshold is too high for this lighting", outcome), true
	case !b.wouldStop:
		return fmt.Sprintf("%s; brightness shadow saw ice at row %d but never reached stop row %d (closest %d)",
			outcome, b.firstRow, b.stopRow, b.closestRow), true
	case b.stoppedOnFirstSighting:
		return fmt.Sprintf("%s; brightness shadow would have closed it at %s, on the first surface it ever saw (row %d) — it watched nothing rise, so that is the rim or a reflection, not ice",
			outcome, b.wouldStopAt.Round(time.Millisecond), b.stoppedRow), true
	default:
		return fmt.Sprintf("%s; brightness shadow saw ice at %s (row %d) and would have closed it at %s (%s %s, row %d); contrast first saw ice at %s",
			outcome, b.firstSeen.Round(time.Millisecond), b.firstRow,
			b.wouldStopAt.Round(time.Millisecond), (contrastAt - b.wouldStopAt).Abs().Round(time.Millisecond),
			earlierOrLater(b.wouldStopAt, contrastAt), b.stoppedRow,
			contrastFirstSeen.Round(time.Millisecond)), false
	}
}

func earlierOrLater(shadow, deciding time.Duration) string {
	if shadow < deciding {
		return "earlier"
	}
	return "later"
}
