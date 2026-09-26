package coffee

// The watched ice dispense: hold the pin open and close it when the ice surface
// passes the stop row, instead of counting seconds. Gated by
// ice_vision_enabled; unset, the dispense is the fixed dwell it has always been.

import (
	"context"
	"fmt"
	"time"

	"beanjamin/coffee/icevision"
)

const (
	// defaultIceDispenseMaxSec is the ceiling on a watched dispense. Ice first
	// became visible 11.5 s into an observed run and the glass filled around
	// 17-20 s, so this has to clear that by a margin or every drink times out.
	defaultIceDispenseMaxSec = 60.0
	// defaultIceDispenseMinSec holds the pin open before any reading is taken.
	defaultIceDispenseMinSec = 2.0
	// defaultIceAfterFirstSeenMaxSec caps the dispense from the first sighting
	// rather than from the pin opening. The absolute ceiling has to clear a whole
	// fill or every drink times out, which leaves it ~10 s above a full glass on
	// a run whose surface is never confirmed past the stop row — ice all the way.
	// The step can only name rows ice_stop_row_px..ice_roi_y1-window, 58 of the
	// band's 153 at the defaults, and the observed run crossed them in ~3 s, so
	// measured from the first sighting a much tighter cap still clears a normal
	// fill by a wide margin.
	defaultIceAfterFirstSeenMaxSec = 30.0
	// defaultIceCheckIntervalSec paces the poll. A frame costs ~8 ms to fetch
	// and measure, so this is chosen for how fast the surface moves, not cost.
	defaultIceCheckIntervalSec = 0.5
	// iceConfirmations is how many consecutive frames must agree before either
	// edge — first sighting or disappearance — is acted on. One dropped frame
	// reads like a disappearance and one glare frame reads like a sighting.
	iceConfirmations = 2
	// iceVisionMaxErrors is how many consecutive camera failures fall back to a
	// fixed dwell. Vision being down must not cost a drink.
	iceVisionMaxErrors = 3
)

func (s *beanjaminCoffee) iceDispenseMaxSec() float64 {
	return orDefault(s.cfg.IceDispenseMaxSec, defaultIceDispenseMaxSec)
}

func (s *beanjaminCoffee) iceDispenseMinSec() float64 {
	return orDefault(s.cfg.IceDispenseMinSec, defaultIceDispenseMinSec)
}

func (s *beanjaminCoffee) iceCheckIntervalSec() float64 {
	return orDefault(s.cfg.IceCheckIntervalSec, defaultIceCheckIntervalSec)
}

func (s *beanjaminCoffee) iceAfterFirstSeenMaxSec() float64 {
	return orDefault(s.cfg.IceAfterFirstSeenMaxSec, defaultIceAfterFirstSeenMaxSec)
}

// iceVisionParams carries the measurement's tunables from the config.
func iceVisionParams(cfg *Config) icevision.Params {
	return icevision.Params{
		StopRowPx:        cfg.IceStopRowPx,
		ContrastWindow:   cfg.IceContrastWindow,
		MinContrast:      cfg.IceMinContrast,
		ROIX0:            cfg.IceROIX0,
		ROIX1:            cfg.IceROIX1,
		ROIY1:            cfg.IceROIY1,
		BrightnessThresh: cfg.IceBrightnessThresh,
		BrightRun:        cfg.IceBrightRun,
		SaveDir:          cfg.SaveMotionRequestsDir,
	}
}

// validateIceVision rejects ice geometry that would fail silently rather than
// loudly. At ice_min_contrast 0 every frame reports a surface, so the first
// tick after the first sighting ends the dispense; a transposed x pair scans
// nothing, never sees a surface, and rides to the ceiling. Both look like
// working config from the outside.
//
// Negative values are rejected rather than left to fall back to their defaults,
// which is how a typo would otherwise disappear.
func validateIceVision(cfg *Config, path string) error {
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"ice_stop_row_px", float64(cfg.IceStopRowPx)},
		{"ice_contrast_window", float64(cfg.IceContrastWindow)},
		{"ice_min_contrast", cfg.IceMinContrast},
		{"ice_roi_x0", float64(cfg.IceROIX0)},
		{"ice_roi_x1", float64(cfg.IceROIX1)},
		{"ice_roi_y1", float64(cfg.IceROIY1)},
		{"ice_dispense_max_sec", cfg.IceDispenseMaxSec},
		{"ice_dispense_min_sec", cfg.IceDispenseMinSec},
		{"ice_after_first_seen_max_sec", cfg.IceAfterFirstSeenMaxSec},
		{"ice_check_interval_sec", cfg.IceCheckIntervalSec},
		{"ice_brightness_thresh", cfg.IceBrightnessThresh},
		{"ice_bright_run", float64(cfg.IceBrightRun)},
	} {
		if f.v < 0 {
			return fmt.Errorf("%s: %s must not be negative, got %v", path, f.name, f.v)
		}
	}

	p := iceVisionParams(cfg)
	b := p.Band()
	if b.X0 >= b.X1 {
		return fmt.Errorf(
			"%s: ice_roi_x0 (%d) must be left of ice_roi_x1 (%d) — a band with no width scans nothing, never sees a surface, and every dispense runs to ice_dispense_max_sec",
			path,
			b.X0,
			b.X1,
		)
	}
	if b.Y0 < 0 {
		return fmt.Errorf(
			"%s: ice_stop_row_px (%d) must be at least ice_contrast_window (%d) — the band opens a full window above the stop row",
			path,
			b.Y0+b.Window,
			b.Window,
		)
	}
	// The topmost candidate row needs a full window of rows on each side, so the
	// stop row itself is only reachable when the band extends a window below it.
	if b.Y0+2*b.Window >= b.Y1 {
		return fmt.Errorf(
			"%s: ice_roi_y1 (%d) must be more than one contrast window (%d) below ice_stop_row_px (%d) — otherwise the ice surface can never be found at the stop row",
			path,
			b.Y1,
			b.Window,
			b.Y0+b.Window,
		)
	}
	if b.MinContrast <= 0 {
		return fmt.Errorf(
			"%s: ice_min_contrast must be greater than 0 — at 0 every frame reports an ice surface and the first check ends the dispense",
			path,
		)
	}
	if maxSec, minSec := orDefault(cfg.IceDispenseMaxSec, defaultIceDispenseMaxSec), orDefault(cfg.IceDispenseMinSec, defaultIceDispenseMinSec); minSec >= maxSec {
		return fmt.Errorf("%s: ice_dispense_min_sec (%v) must be less than ice_dispense_max_sec (%v)", path, minSec, maxSec)
	}
	// The cap runs from the first sighting, which itself takes iceConfirmations
	// polls to latch. At or below one interval it fires on the poll right after,
	// ending every dispense the moment ice becomes visible.
	if capSec, interval := orDefault(cfg.IceAfterFirstSeenMaxSec, defaultIceAfterFirstSeenMaxSec), orDefault(cfg.IceCheckIntervalSec, defaultIceCheckIntervalSec); capSec <= interval {
		return fmt.Errorf(
			"%s: ice_after_first_seen_max_sec (%v) must be more than ice_check_interval_sec (%v) — otherwise the dispense ends on the first poll after ice becomes visible",
			path,
			capSec,
			interval,
		)
	}
	// The shadow decides nothing, so its settings are only rejected where they
	// would make it silently useless rather than visibly wrong: a cutoff no
	// 8-bit row mean can reach, or a run longer than the band it scans, both
	// report "no ice" on every frame for the life of the machine.
	if cfg.IceBrightnessThresh > 0 {
		if cfg.IceBrightnessThresh >= 255 {
			return fmt.Errorf(
				"%s: ice_brightness_thresh (%v) must be below 255 — row brightness is an 8-bit mean, so nothing ever reaches it and the shadow reports no ice on every frame",
				path,
				cfg.IceBrightnessThresh,
			)
		}
		if run := p.ShadowRun(); run > b.Y1-b.Y0 {
			return fmt.Errorf("%s: ice_bright_run (%d) must fit inside the %d-row scan band", path, run, b.Y1-b.Y0)
		}
	}
	return nil
}

// dwellUntilFull holds the pin open — the caller owns it — until the ice
// surface has risen past the stop row, then returns so the caller can close it.
// timedOut reports that the ceiling ran out instead; sawSurface, whether ice was
// ever seen, which is what tells an empty hopper from a stop row that no longer
// matches the seating. Neither is an error: the caller acts on them once the pin
// is shut.
//
// The stop signal is an *absence*: "no surface in the band" means "not enough
// ice yet" before ice arrives and "risen past the stop row" after. Same reading,
// opposite responses, and only history separates them, so a disappearance
// counts as done only once a surface has been seen. Both edges need
// iceConfirmations in a row — one dropped frame reads like a disappearance and
// one glare frame reads like a sighting. Confirming only the disappearance is
// the dangerous asymmetry: ice takes ~11.5 s to appear, so a run has ~20 empty
// ticks before the real signal, and a single false sighting among them would
// arm the latch and let the next empty reading stop a dispense on an empty glass.
//
// Alongside all of that, the brightness shadow reads the same frames and logs
// what the other method would have done (coffee/icevision/brightness.go). It
// decides nothing here; it is the evidence for whether it ever should.
//
// measure is a parameter so the loop tests without a camera.
func (s *beanjaminCoffee) dwellUntilFull(
	ctx, cancelCtx context.Context, measure func(context.Context) (icevision.Measurement, error),
) (iceDwellResult, error) {
	logger := s.activeOrderLogger()
	params := s.iceVision.Params()
	stopRow := params.StopRow()
	start := time.Now()
	deadline := start.Add(secondsToDuration(s.iceDispenseMaxSec()))
	shadow := params.NewShadow()
	if shadow.On() {
		b := params.Band()
		_, _, first, last := icevision.ScanBand(stopRow, b.Window, b.Y1)
		logger.Infof(
			"dispensing ice: brightness shadow on (threshold %.0f over %d rows, scanning rows %d-%d against the step's %d-%d) — the contrast step still decides",
			s.cfg.IceBrightnessThresh,
			params.ShadowRun(),
			b.Y0,
			b.Y1-params.ShadowRun(),
			first,
			last,
		)
	}
	var sawSurface bool
	var firstSeen time.Duration
	// The most recent frame that measured cleanly, and when. A run that ends on
	// a camera failure or a cancel still leaves the last thing the loop actually
	// saw — captioned with its age, because a frame from before the failure is
	// evidence about the run, not a picture of how it ended.
	var last icevision.Measurement
	var lastAt time.Duration

	// Reporting is handed back on every exit rather than done here: holdIcePin's
	// deferred pin close does not run until this function has returned, so a
	// sensor RPC or a JPEG encode from inside the loop is more ice in the glass.
	result := func(outcome string, at time.Duration, note string) iceDwellResult {
		return iceDwellResult{
			sawSurface: sawSurface,
			shadow:     shadow,
			stopped:    outcome == "stopped",
			elapsed:    at,
			firstSeen:  firstSeen,
			frame:      last,
			outcome:    outcome,
			note:       note,
		}
	}

	// Ice takes seconds to reach the glass; nothing read before that can be a
	// surface, and a reading taken as the pin opens is only a chance to be wrong.
	if err := s.waitOrCancel(ctx, cancelCtx, secondsToDuration(s.iceDispenseMinSec())); err != nil {
		return result("cancelled", time.Since(start), ""), err
	}

	var sightings, misses, visionErrors int
	interval := secondsToDuration(s.iceCheckIntervalSec())
	afterFirstSeen := secondsToDuration(s.iceAfterFirstSeenMaxSec())
	for {
		measurement, err := measure(ctx)
		reading := measurement.Contrast
		if err == nil {
			last, lastAt = measurement, time.Since(start)
			shadow.Observe(measurement.Brightness, lastAt, describeContrast(reading, sawSurface, stopRow), logger)
		}
		switch {
		case err != nil:
			visionErrors++
			logger.Warnf("dispensing ice: level check failed (%d of %d before falling back to a fixed dwell): %v",
				visionErrors, iceVisionMaxErrors, err)
			// Vision being down must not cost a drink: finish the dispense the
			// open-loop way rather than abandoning it or running to the ceiling.
			if visionErrors >= iceVisionMaxErrors {
				elapsed := time.Since(start)
				remaining := secondsToDuration(s.iceDispenseSec()) - elapsed
				logger.Warnf("dispensing ice: level checks are failing; falling back to a fixed %s dispense (%s of it already elapsed)",
					secondsToDuration(s.iceDispenseSec()), elapsed.Round(time.Millisecond))
				return result("vision_fallback", elapsed, staleness(lastAt, elapsed)),
					s.waitOrCancel(ctx, cancelCtx, remaining)
			}
		case reading.Found:
			visionErrors, misses = 0, 0
			sightings++
			if !sawSurface && sightings >= iceConfirmations {
				sawSurface, firstSeen = true, time.Since(start)
				logger.Infof("dispensing ice: surface visible at row %d (step %.0f), stop row %d — %s after the pin opened",
					reading.Row, reading.Step, stopRow, firstSeen.Round(time.Millisecond))
			}
		default:
			visionErrors, sightings = 0, 0
			misses++
			if sawSurface && misses >= iceConfirmations {
				elapsed := time.Since(start)
				logger.Infof("dispensing ice: the surface has risen past row %d after %s — closing the pin",
					stopRow, elapsed.Round(time.Millisecond))
				res := result("stopped", elapsed, "")
				res.compare = true
				return res, nil
			}
		}

		// Two ceilings, both serving the glass as it is. The absolute one is the
		// backstop for a hopper that never delivers; the cap since the first
		// sighting is what bounds the overflow when ice is flowing but the surface
		// is never confirmed past the stop row.
		elapsed := time.Since(start)
		if !time.Now().Before(deadline) {
			res := result("timeout", elapsed, "")
			res.timedOut, res.compare = true, true
			return res, nil
		}
		if sawSurface && elapsed-firstSeen >= afterFirstSeen {
			logger.Warnf("dispensing ice: %s since the surface first appeared without it passing row %d — closing the pin",
				afterFirstSeen, stopRow)
			res := result("surface_cap", elapsed, "")
			res.timedOut, res.compare, res.cappedAfterFirstSeen = true, true, true
			return res, nil
		}
		if err := s.waitOrCancel(ctx, cancelCtx, interval); err != nil {
			elapsed = time.Since(start)
			return result("cancelled", elapsed, staleness(lastAt, elapsed)), err
		}
	}
}

// iceDwellResult is everything a watched dispense produced. timedOut and
// sawSurface are what the caller acts on; the rest is reporting, which the
// caller must do only once the pin is shut.
type iceDwellResult struct {
	timedOut   bool
	sawSurface bool
	// cappedAfterFirstSeen distinguishes the two ceilings, which differ in what
	// they say about the machine: the absolute one on an empty hopper, this one
	// on ice that flowed without ever being confirmed past the stop row.
	cappedAfterFirstSeen bool

	shadow    *icevision.Shadow
	compare   bool // log the shadow comparison — only where a comparison means anything
	stopped   bool
	elapsed   time.Duration
	firstSeen time.Duration

	frame   icevision.Measurement
	outcome string // tag slug for the saved frame
	note    string // what the frame cannot be trusted on by itself
}

// reportIceDispense logs the shadow comparison and saves the annotated frame.
// Called by pulseIcePin with the pin already shut, never from inside the loop:
// between them these are two sensor RPCs and a JPEG encode, and every one of
// them would otherwise be more ice in the glass.
//
// A cancelled dispense still saves its frame, so the context is rebuilt rather
// than reused: a cancelled context is the wrong thing to hand a write that has
// to finish.
func (s *beanjaminCoffee) reportIceDispense(ctx context.Context, res iceDwellResult) {
	// A fixed dwell, or a pin that never opened, produces no result to report.
	if res.outcome == "" {
		return
	}
	if res.compare {
		s.reportShadow(ctx, res.shadow, res.stopped, res.elapsed, res.firstSeen)
	}
	s.iceVision.SaveFrame(s.iceFrameSavingCtx(ctx), s.activeOrderLogger(), s.queue.CurrentID(), res.frame, res.outcome, res.note, res.elapsed)
}

// staleness notes how far before the end of the run a frame was taken, for the
// exits where the last good frame is not a picture of how the run ended.
func staleness(takenAt, endedAt time.Duration) string {
	if takenAt <= 0 {
		return ""
	}
	return fmt.Sprintf("last clean frame, %s before the run ended", (endedAt - takenAt).Round(time.Millisecond))
}

// iceFrameSavingCtx carries the intent to save onto a fresh context, so a frame
// still lands when the dispense was cancelled.
func (s *beanjaminCoffee) iceFrameSavingCtx(ctx context.Context) context.Context {
	if !icevision.FramesWanted(ctx, s.queue.CurrentID()) {
		return ctx
	}
	return icevision.WithFrameSaving(context.Background())
}

// describeContrast renders what the deciding method saw on one frame, so a
// shadow log line stands on its own instead of having to be lined up against
// the step's own lines by timestamp.
func describeContrast(r icevision.Reading, sawSurface bool, stopRow int) string {
	switch {
	case r.Found && r.Row > stopRow:
		return fmt.Sprintf("has it at row %d, %d px short", r.Row, r.Row-stopRow)
	case r.Found:
		return fmt.Sprintf("has it at row %d", r.Row)
	case sawSurface:
		return "sees nothing and has already seen ice, so it is about to close the pin"
	default:
		return "sees nothing yet"
	}
}

// reportShadow logs the one-line comparison at the end of a dispense and counts
// the runs where the two methods differed in kind. The counter is the number
// worth watching: timing gaps are expected and are what is being gathered, a
// disagreement is a reason not to swap.
func (s *beanjaminCoffee) reportShadow(ctx context.Context, shadow *icevision.Shadow, stopped bool, elapsed, firstSeen time.Duration) {
	line, disagreed := shadow.Verdict(stopped, elapsed, firstSeen)
	if line == "" {
		return
	}
	s.activeOrderLogger().Infof("dispensing ice: %s", line)
	if disagreed {
		s.incrementSensorReading(ctx, s.usageSensor, "ice machine", "ice_shadow_disagreements", 1)
	}
}

// iceDispenseTimedOut reports a dispense the measurement never stopped. The
// glass is served anyway and the order does not fail: the espresso is already
// brewed, so a light drink beats a discarded one, and the counter is how an
// empty hopper — or a stop row no longer matching the seating — surfaces.
//
// Called by pulseIcePin once the pin is shut, never from inside the loop.
func (s *beanjaminCoffee) iceDispenseTimedOut(ctx context.Context, res iceDwellResult) {
	logger := s.activeOrderLogger()
	reason := "no ice was ever visible — check the hopper"
	if res.sawSurface {
		reason = "ice was visible but never rose past the stop row"
	}
	limit := fmt.Sprintf("the %v ceiling", secondsToDuration(s.iceDispenseMaxSec()))
	if res.cappedAfterFirstSeen {
		limit = fmt.Sprintf("the %v cap on the time since ice first appeared", secondsToDuration(s.iceAfterFirstSeenMaxSec()))
	}
	logger.Warnf("dispensing ice: hit %s: %s. Serving the glass as it is.", limit, reason)
	if err := s.speaker.Say(ctx, "I couldn't tell when the glass was full, so this one might be light on ice."); err != nil {
		logger.Warnf("dispensing ice: announcing the timeout failed: %v", err)
	}
	s.incrementSensorReading(ctx, s.usageSensor, "ice machine", "ice_dispense_timeouts", 1)
}

// waitOrCancel sleeps, returning early with an error if either context ends.
// A non-positive duration returns immediately.
func (s *beanjaminCoffee) waitOrCancel(ctx, cancelCtx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return fmt.Errorf("cancelled during ice dispense: %w", ctx.Err())
	case <-cancelCtx.Done():
		return fmt.Errorf("cancelled during ice dispense")
	}
	return nil
}

func secondsToDuration(sec float64) time.Duration {
	return time.Duration(sec * float64(time.Second))
}

// closeIcePin drives the ice pin LOW, best effort and independently of whatever
// a dispense is doing, for shutdown. Setting an already-low pin low is a no-op,
// so it is safe whether or not ice is running.
func (s *beanjaminCoffee) closeIcePin() {
	if s.iceBoard == nil {
		return
	}
	pin, err := s.iceBoard.GPIOPinByName(s.icePinName())
	if err != nil {
		s.logger.Warnf("closing the ice pin: get pin %q: %v", s.icePinName(), err)
		return
	}
	if err := pin.Set(context.Background(), false, nil); err != nil {
		s.logger.Warnf("closing the ice pin %q: %v", s.icePinName(), err)
	}
}
