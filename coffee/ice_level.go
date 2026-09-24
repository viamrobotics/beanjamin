package coffee

// Visual ice-level measurement. Ice dispensed into the held glass is bright
// against the dark ice machine behind it, so its surface is a horizontal
// brightness edge: average each row of a strip through the glass and the
// surface is the largest step in that profile.
//
// The step is measured rather than absolute brightness because a fixed cutoff
// is fitted to one afternoon's light — brighter ambient lifts an empty glass
// past it and reports "full".

import (
	"context"
	"fmt"
	"image"
	"math"

	"go.viam.com/rdk/components/camera"
)

const (
	// defaultIceStopRowPx is the image row the ice surface has to reach. The
	// camera rides the gripper, so a row is a fixed height above the jaws —
	// which is what makes a bare pixel row meaningful rather than pose-dependent.
	//
	// Provisional: it rests on a single observed fetch_glass seating (46% full at
	// rim row 277, 70% at rim 379). Chosen for the worst seating, because
	// overflow is the expensive direction.
	defaultIceStopRowPx = 595
	// defaultIceContrastWindow is the half-width, in rows, that the step is
	// averaged over. The surface is a gradual edge spanning tens of rows: at a
	// window of 8 the gap between an empty glass's worst false step and ice's
	// weakest true step is 3, at 48 it is 39. Averaging is also what stops
	// glare — a single bright row — from presenting as a step.
	defaultIceContrastWindow = 48
	// defaultIceMinContrast is the smallest step counted as a surface, from a
	// synthetic 0.5x-1.8x luminance sweep: worst false positive 22, weakest true
	// positive 29. Step magnitudes shrink at both ends of that range, since
	// saturated ice clips just as dim light gives less contrast.
	defaultIceMinContrast = 25.0

	// The strip is inset from the glass walls, whose bright vertical edges would
	// drag the row means around, and stops above the ice machine's ledge, which
	// occludes the base of the glass.
	defaultIceROIX0 = 700
	defaultIceROIX1 = 910
	defaultIceROIY1 = 670
)

// iceBand is the pixel rectangle the row-brightness profile is averaged over,
// with the parameters the step is judged by.
//
// Every field is a raw pixel coordinate at the camera's configured resolution,
// valid only at ice_machine_dispense with a glass gripped.
type iceBand struct {
	x0, x1      int
	y0, y1      int
	window      int
	minContrast float64
}

// iceBandFromConfig derives the scan band from the configured stop row. The top
// is stopRow-window and never a configured value of its own: the topmost row the
// scan can nominate is then the stop row itself, which is the whole defense
// against locking onto the glass rim. A rim above the band is never scanned,
// and one below the stop row reads "not yet".
//
// A free function so Validate checks the same band the measurement runs on.
func iceBandFromConfig(cfg *Config) iceBand {
	window := orDefault(cfg.IceContrastWindow, defaultIceContrastWindow)
	top, bottom, _, _ := iceScanBand(orDefault(cfg.IceStopRowPx, defaultIceStopRowPx), window, orDefault(cfg.IceROIY1, defaultIceROIY1))
	return iceBand{
		x0:          orDefault(cfg.IceROIX0, defaultIceROIX0),
		x1:          orDefault(cfg.IceROIX1, defaultIceROIX1),
		y0:          top,
		y1:          bottom,
		window:      window,
		minContrast: orDefault(cfg.IceMinContrast, defaultIceMinContrast),
	}
}

// iceScanBand returns the rows a stop row implies: the band the profile is
// averaged over (top..bottom) and the first and last rows a surface can be
// reported at, which contrastStep's window indexing pulls in by a window at each
// end. firstRow is the stop row itself — that identity is the whole defense
// against locking onto the glass rim, and it holds only as long as the band top
// is derived from the stop row.
//
// One derivation, used by the band, the loop's log line and the drawing alike:
// a picture of a band the service does not scan is worse than no picture.
func iceScanBand(stopRow, window, roiY1 int) (top, bottom, firstRow, lastRow int) {
	top, bottom = stopRow-window, roiY1
	return top, bottom, top + window, bottom - window
}

func (s *beanjaminCoffee) iceBand() iceBand {
	return iceBandFromConfig(s.cfg)
}

// iceStopRowPx returns the configured or default stop row. The band's topmost
// candidate row sits a full window below its top, so this is also what the
// scan's effective ceiling works out to.
func (s *beanjaminCoffee) iceStopRowPx() int {
	return orDefault(s.cfg.IceStopRowPx, defaultIceStopRowPx)
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

	b := iceBandFromConfig(cfg)
	if b.x0 >= b.x1 {
		return fmt.Errorf(
			"%s: ice_roi_x0 (%d) must be left of ice_roi_x1 (%d) — a band with no width scans nothing, never sees a surface, and every dispense runs to ice_dispense_max_sec",
			path,
			b.x0,
			b.x1,
		)
	}
	if b.y0 < 0 {
		return fmt.Errorf(
			"%s: ice_stop_row_px (%d) must be at least ice_contrast_window (%d) — the band opens a full window above the stop row",
			path,
			b.y0+b.window,
			b.window,
		)
	}
	// The topmost candidate row needs a full window of rows on each side, so the
	// stop row itself is only reachable when the band extends a window below it.
	if b.y0+2*b.window >= b.y1 {
		return fmt.Errorf(
			"%s: ice_roi_y1 (%d) must be more than one contrast window (%d) below ice_stop_row_px (%d) — otherwise the ice surface can never be found at the stop row",
			path,
			b.y1,
			b.window,
			b.y0+b.window,
		)
	}
	if b.minContrast <= 0 {
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
		if run := orDefault(cfg.IceBrightRun, defaultIceBrightRun); run > b.y1-b.y0 {
			return fmt.Errorf("%s: ice_bright_run (%d) must fit inside the %d-row scan band", path, run, b.y1-b.y0)
		}
	}
	return nil
}

// iceReading is one frame's measurement. row is meaningful only when found,
// and step carries whatever number the method judged by — the brightness step
// for the contrast method, the row's mean brightness for the shadow.
type iceReading struct {
	row   int
	step  float64
	found bool
}

// iceMeasurement is one frame read by both methods. contrast decides the
// dispense; brightness is the shadow and is only populated when shadow is set.
// frame is retained so the deciding moment can be drawn and saved.
type iceMeasurement struct {
	contrast   iceReading
	brightness iceReading
	shadow     bool
	frame      image.Image
}

// iceSurfaceRow finds the ice surface in the band: the largest brightness step
// above minContrast, reported as an absolute image row. Pure, so it tests
// against the committed fixtures.
func iceSurfaceRow(img image.Image, b iceBand) (iceReading, error) {
	rows, err := rowBrightness(img, b)
	if err != nil {
		return iceReading{}, err
	}
	return contrastReading(rows, b), nil
}

// contrastReading is iceSurfaceRow over a profile already in hand, so the
// shadow reads the same rows rather than fetching a second frame.
func contrastReading(rows []float64, b iceBand) iceReading {
	step, idx := contrastStep(rows, b.window)
	if idx < 0 || step < b.minContrast {
		return iceReading{}
	}
	return iceReading{row: b.y0 + idx, step: step, found: true}
}

// strongestStepAboveBand scans the region the measurement deliberately skips.
// On an empty glass that step is the rim — dark ice-machine interior above,
// bright glass wall below — which is the one number that shows how the seating
// drifted between grabs, so check_ice_level logs it.
//
// It is the rim only while the glass is empty. Once ice rises past the band the
// strongest step up there is the ice surface itself (an 80%-full fixture reports
// 434, its real surface, not its rim at 277), so read it on the pre-roll.
func strongestStepAboveBand(img image.Image, b iceBand) (iceReading, error) {
	above := b
	above.y0, above.y1 = 0, b.y0
	return iceSurfaceRow(img, above)
}

// rowBrightness returns the mean brightness of each row of the band, top to
// bottom — the profile both measurements read.
//
// Every coordinate in the band is a raw pixel, so a camera reconfigured to a
// different frame size invalidates all of them silently and in an unknown
// direction. That is why a band reaching outside the frame is an error rather
// than a clamp: a clamped band measures a different part of the glass and says
// nothing about it.
func rowBrightness(img image.Image, b iceBand) ([]float64, error) {
	if b.x0 >= b.x1 || b.y0 >= b.y1 || b.y0 < 0 {
		return nil, fmt.Errorf("empty ice band: x %d..%d, y %d..%d", b.x0, b.x1, b.y0, b.y1)
	}
	bounds := img.Bounds()
	if b.x1 > bounds.Max.X || b.y1 > bounds.Max.Y {
		return nil, fmt.Errorf(
			"ice band x %d..%d y %d..%d is outside the %dx%d frame — the ice_roi_* and ice_stop_row_px values are pixels at the resolution they were measured at",
			b.x0,
			b.x1,
			b.y0,
			b.y1,
			bounds.Max.X,
			bounds.Max.Y,
		)
	}
	rows := make([]float64, 0, b.y1-b.y0)
	width := float64(b.x1 - b.x0)
	for y := b.y0; y < b.y1; y++ {
		var sum float64
		for x := b.x0; x < b.x1; x++ {
			red, green, blue, _ := img.At(x, y).RGBA()
			sum += float64(red>>8+green>>8+blue>>8) / 3
		}
		rows = append(rows, sum/width)
	}
	return rows, nil
}

// contrastStep finds the largest brightness step in the profile: below the
// surface is bright ice, above it is the dark machine seen through empty glass.
// Returns the step magnitude and its index into rows.
//
// This is illumination-invariant where an absolute threshold is not — brighter
// ambient raises both sides of the step and leaves its size alone.
func contrastStep(rows []float64, w int) (step float64, idx int) {
	if w < 1 {
		w = 1
	}
	if len(rows) < 2*w+1 {
		return 0, -1
	}
	best, bestIdx := 0.0, -1
	for i := w; i+w <= len(rows); i++ {
		var above, below float64
		for j := 0; j < w; j++ {
			above += rows[i-1-j] // higher in the image: empty glass
			below += rows[i+j]   // lower in the image: ice
		}
		if d := (below - above) / float64(w); d > best {
			best, bestIdx = d, i
		}
	}
	return best, bestIdx
}

// measureIceSurface grabs a frame and measures it with both methods.
func (s *beanjaminCoffee) measureIceSurface(ctx context.Context) (iceMeasurement, error) {
	img, err := s.iceFrame(ctx)
	if err != nil {
		return iceMeasurement{}, err
	}
	return s.measureIceFrame(img)
}

// measureIceFrame runs both methods over one frame.
func (s *beanjaminCoffee) measureIceFrame(img image.Image) (iceMeasurement, error) {
	b := s.iceBand()
	rows, err := rowBrightness(img, b)
	if err != nil {
		return iceMeasurement{}, err
	}
	return s.measureIceRows(rows, b, img), nil
}

// measureIceRows runs both methods over a profile already in hand. One profile,
// not two: readings taken a poll apart would compare the ice machine against
// itself rather than the two methods against each other. The shadow is pure
// over rows the deciding method has already read, so it has no failure of its
// own and can never contribute to the camera-error count that falls back to a
// fixed dwell.
func (s *beanjaminCoffee) measureIceRows(rows []float64, b iceBand, img image.Image) iceMeasurement {
	m := iceMeasurement{contrast: contrastReading(rows, b), frame: img}
	if s.iceBrightnessShadowOn() {
		m.shadow = true
		m.brightness = brightnessSurfaceRow(rows, b, s.cfg.IceBrightnessThresh, s.iceBrightRun())
	}
	return m
}

// iceFrame grabs one frame from the arm-mounted camera. Filtering to the color
// source keeps the depth payload — 1.8 MB that nothing here reads — off the
// wire, which is a latency decision: the check has to fit inside the poll
// interval.
func (s *beanjaminCoffee) iceFrame(ctx context.Context) (image.Image, error) {
	if s.srcCamera == nil {
		return nil, fmt.Errorf("no source camera")
	}
	images, _, err := s.srcCamera.Images(ctx, []string{iceColorSourceName}, nil)
	if err != nil {
		return nil, fmt.Errorf("getting camera images: %w", err)
	}
	return firstDecodableImage(ctx, images)
}

// checkIceLevel measures one frame at the arm's current pose and logs it. Logs
// only: it is the operator's window onto the measurement, run at
// ice_machine_dispense with a glass in the jaws.
//
// It reports the strongest step above the band as well as the surface. On an
// empty glass that is the rim, and the rim row is the one number that shows the
// seating drifting between grabs — the thing the stop row cannot correct for,
// and the reason to re-check a machine's ice_stop_row_px.
//
// With "annotate": true on the DoCommand it also saves the frame drawn, which
// is the whole loop for tuning a stop row: hold a glass at the dispense pose and
// fire this as often as you like, without spending any ice.
func (s *beanjaminCoffee) checkIceLevel(ctx, _ context.Context) error {
	logger := s.activeOrderLogger()
	img, err := s.iceFrame(ctx)
	if err != nil {
		return fmt.Errorf("check_ice_level: %w", err)
	}
	b := s.iceBand()
	rows, err := rowBrightness(img, b)
	if err != nil {
		return fmt.Errorf("check_ice_level: %w", err)
	}
	measurement := s.measureIceRows(rows, b, img)
	surface := measurement.contrast
	s.saveIceDispenseFrame(ctx, measurement, "check", "", 0)

	bounds := img.Bounds()
	stopRow := s.iceStopRowPx()
	logger.Infof("check_ice_level: frame %dx%d, band x %d..%d y %d..%d, window %d, min contrast %.0f, stop row %d",
		bounds.Max.X, bounds.Max.Y, b.x0, b.x1, b.y0, b.y1, b.window, b.minContrast, stopRow)
	s.reportCheckBrightness(rows, measurement.brightness, stopRow)
	if !surface.found {
		logger.Infof("check_ice_level: no ice surface in the band — either not enough ice yet, or risen past row %d", stopRow)
	} else {
		logger.Infof("check_ice_level: ice surface at row %d (step %.0f), %d px below the stop row %d",
			surface.row, surface.step, surface.row-stopRow, stopRow)
	}
	rim, err := strongestStepAboveBand(img, b)
	if err != nil {
		return fmt.Errorf("check_ice_level: %w", err)
	}
	if !rim.found {
		logger.Infof("check_ice_level: no strong step above the band — on an empty glass that means the rim was not found where one is expected")
		return nil
	}
	logger.Infof(
		"check_ice_level: strongest step above the band at row %d (step %.0f) — on an empty glass that is the glass rim, and how the seating is checked; the fixtures ice_stop_row_px was chosen against sit at 277 and 379",
		rim.row,
		rim.step,
	)
	return nil
}

// reportCheckBrightness prints the brightness shadow's read of the same frame,
// and — whether or not the shadow is configured — the range of row means it
// found. That range is how ice_brightness_thresh gets chosen in the first
// place: an empty glass has to sit below it and ice above it, at this machine's
// lighting rather than the one the fixtures were shot under.
func (s *beanjaminCoffee) reportCheckBrightness(rows []float64, shadow iceReading, stopRow int) {
	logger := s.activeOrderLogger()
	lo, hi := rows[0], rows[0]
	for _, v := range rows {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	logger.Infof(
		"check_ice_level: row brightness in the band runs %.0f to %.0f — ice_brightness_thresh has to sit above an empty glass's highest row and below ice's lowest",
		lo,
		hi,
	)
	if !s.iceBrightnessShadowOn() {
		logger.Infof(
			"check_ice_level: brightness shadow off (set ice_brightness_thresh to log what an absolute cutoff would have decided on every dispense)",
		)
		return
	}
	if !shadow.found {
		logger.Infof("check_ice_level: brightness shadow (threshold %.0f over %d rows) found no surface",
			s.cfg.IceBrightnessThresh, s.iceBrightRun())
		return
	}
	logger.Infof("check_ice_level: brightness shadow (threshold %.0f over %d rows) has the surface at row %d (%.0f) — %s",
		s.cfg.IceBrightnessThresh, s.iceBrightRun(), shadow.row, shadow.step, iceRowRelativeToStop(shadow.row, stopRow))
}

// iceColorSourceName is the camera source the measurement reads.
const iceColorSourceName = "color"

// firstDecodableImage decodes the first image in the response. The filter is a
// request, not a guarantee — a camera that ignores filterSourceNames returns
// everything — so the color image is preferred by name when several come back.
func firstDecodableImage(ctx context.Context, images []camera.NamedImage) (image.Image, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("camera returned no images")
	}
	pick, named := &images[0], false
	for i := range images {
		if images[i].SourceName == iceColorSourceName {
			pick, named = &images[i], true
			break
		}
	}
	// Falling back to the first of several images would measure row brightness
	// off whatever the camera happened to list first — a depth frame reads as a
	// perfectly good brightness profile and yields a stop row from nothing. One
	// image is the ordinary case for a camera that serves a single stream, named
	// or not, so only an ambiguous response is refused.
	if !named && len(images) > 1 {
		names := make([]string, 0, len(images))
		for i := range images {
			names = append(names, images[i].SourceName)
		}
		return nil, fmt.Errorf("camera returned %d images and none is the %q source (%v) — the ice measurement reads color rows, not depth",
			len(images), iceColorSourceName, names)
	}
	img, err := pick.Image(ctx)
	if err != nil {
		return nil, fmt.Errorf("decoding %q image: %w", pick.SourceName, err)
	}
	return img, nil
}
