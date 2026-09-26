package icevision

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
	"go.viam.com/rdk/logging"
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

// Band is the pixel rectangle the row-brightness profile is averaged over,
// with the parameters the step is judged by.
//
// Every field is a raw pixel coordinate at the camera's configured resolution,
// valid only at ice_machine_dispense with a glass gripped.
type Band struct {
	X0, X1      int
	Y0, Y1      int
	Window      int
	MinContrast float64
}

// Band derives the scan band from the configured stop row. The top is
// stopRow-window and never a configured value of its own: the topmost row the
// scan can nominate is then the stop row itself, which is the whole defense
// against locking onto the glass rim. A rim above the band is never scanned,
// and one below the stop row reads "not yet".
//
// On Params rather than the Detector so Validate checks the same band the
// measurement runs on.
func (p Params) Band() Band {
	window := orDefault(p.ContrastWindow, defaultIceContrastWindow)
	top, bottom, _, _ := ScanBand(orDefault(p.StopRowPx, defaultIceStopRowPx), window, orDefault(p.ROIY1, defaultIceROIY1))
	return Band{
		X0:          orDefault(p.ROIX0, defaultIceROIX0),
		X1:          orDefault(p.ROIX1, defaultIceROIX1),
		Y0:          top,
		Y1:          bottom,
		Window:      window,
		MinContrast: orDefault(p.MinContrast, defaultIceMinContrast),
	}
}

// ScanBand returns the rows a stop row implies: the band the profile is
// averaged over (top..bottom) and the first and last rows a surface can be
// reported at, which contrastStep's window indexing pulls in by a window at each
// end. firstRow is the stop row itself — that identity is the whole defense
// against locking onto the glass rim, and it holds only as long as the band top
// is derived from the stop row.
//
// One derivation, used by the band, the loop's log line and the drawing alike:
// a picture of a band the service does not scan is worse than no picture.
func ScanBand(stopRow, window, roiY1 int) (top, bottom, firstRow, lastRow int) {
	top, bottom = stopRow-window, roiY1
	return top, bottom, top + window, bottom - window
}

// StopRow returns the configured or default stop row. The band's topmost
// candidate row sits a full window below its top, so this is also what the
// scan's effective ceiling works out to.
func (p Params) StopRow() int {
	return orDefault(p.StopRowPx, defaultIceStopRowPx)
}

// Reading is one frame's measurement. Row is meaningful only when Found, and
// Step carries whatever number the method judged by — the brightness step for
// the contrast method, the row's mean brightness for the shadow.
type Reading struct {
	Row   int
	Step  float64
	Found bool
}

// Measurement is one frame read by both methods. Contrast decides the
// dispense; Brightness is the shadow and is only populated when Shadow is set.
// Frame is retained so the deciding moment can be drawn and saved.
type Measurement struct {
	Contrast   Reading
	Brightness Reading
	Shadow     bool
	Frame      image.Image
}

// surfaceRow finds the ice surface in the band: the largest brightness step
// above minContrast, reported as an absolute image row. Pure, so it tests
// against the committed fixtures.
func surfaceRow(img image.Image, b Band) (Reading, error) {
	rows, err := rowBrightness(img, b)
	if err != nil {
		return Reading{}, err
	}
	return contrastReading(rows, b), nil
}

// contrastReading is surfaceRow over a profile already in hand, so the
// shadow reads the same rows rather than fetching a second frame.
func contrastReading(rows []float64, b Band) Reading {
	step, idx := contrastStep(rows, b.Window)
	if idx < 0 || step < b.MinContrast {
		return Reading{}
	}
	return Reading{Row: b.Y0 + idx, Step: step, Found: true}
}

// strongestStepAboveBand scans the region the measurement deliberately skips.
// On an empty glass that step is the rim — dark ice-machine interior above,
// bright glass wall below — which is the one number that shows how the seating
// drifted between grabs, so check_ice_level logs it.
//
// It is the rim only while the glass is empty. Once ice rises past the band the
// strongest step up there is the ice surface itself (an 80%-full fixture reports
// 434, its real surface, not its rim at 277), so read it on the pre-roll.
func strongestStepAboveBand(img image.Image, b Band) (Reading, error) {
	above := b
	above.Y0, above.Y1 = 0, b.Y0
	return surfaceRow(img, above)
}

// rowBrightness returns the mean brightness of each row of the band, top to
// bottom — the profile both measurements read.
//
// Every coordinate in the band is a raw pixel, so a camera reconfigured to a
// different frame size invalidates all of them silently and in an unknown
// direction. That is why a band reaching outside the frame is an error rather
// than a clamp: a clamped band measures a different part of the glass and says
// nothing about it.
func rowBrightness(img image.Image, b Band) ([]float64, error) {
	if b.X0 >= b.X1 || b.Y0 >= b.Y1 || b.Y0 < 0 {
		return nil, fmt.Errorf("empty ice band: x %d..%d, y %d..%d", b.X0, b.X1, b.Y0, b.Y1)
	}
	bounds := img.Bounds()
	if b.X1 > bounds.Max.X || b.Y1 > bounds.Max.Y {
		return nil, fmt.Errorf(
			"ice band x %d..%d y %d..%d is outside the %dx%d frame — the ice_roi_* and ice_stop_row_px values are pixels at the resolution they were measured at",
			b.X0,
			b.X1,
			b.Y0,
			b.Y1,
			bounds.Max.X,
			bounds.Max.Y,
		)
	}
	rows := make([]float64, 0, b.Y1-b.Y0)
	width := float64(b.X1 - b.X0)
	for y := b.Y0; y < b.Y1; y++ {
		var sum float64
		for x := b.X0; x < b.X1; x++ {
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

// Measure grabs a frame and measures it with both methods.
func (d *Detector) Measure(ctx context.Context) (Measurement, error) {
	img, err := d.frame(ctx)
	if err != nil {
		return Measurement{}, err
	}
	return d.measureFrame(img)
}

// measureFrame runs both methods over one frame.
func (d *Detector) measureFrame(img image.Image) (Measurement, error) {
	b := d.p.Band()
	rows, err := rowBrightness(img, b)
	if err != nil {
		return Measurement{}, err
	}
	return d.measureRows(rows, b, img), nil
}

// measureRows runs both methods over a profile already in hand. One profile,
// not two: readings taken a poll apart would compare the ice machine against
// itself rather than the two methods against each other. The shadow is pure
// over rows the deciding method has already read, so it has no failure of its
// own and can never contribute to the camera-error count that falls back to a
// fixed dwell.
func (d *Detector) measureRows(rows []float64, b Band, img image.Image) Measurement {
	m := Measurement{Contrast: contrastReading(rows, b), Frame: img}
	if d.p.ShadowOn() {
		m.Shadow = true
		m.Brightness = brightnessSurfaceRow(rows, b, d.p.BrightnessThresh, d.p.ShadowRun())
	}
	return m
}

// frame grabs one frame from the arm-mounted camera. Filtering to the color
// source keeps the depth payload — 1.8 MB that nothing here reads — off the
// wire, which is a latency decision: the check has to fit inside the poll
// interval.
func (d *Detector) frame(ctx context.Context) (image.Image, error) {
	if d.cam == nil {
		return nil, fmt.Errorf("no source camera")
	}
	images, _, err := d.cam.Images(ctx, []string{colorSourceName}, nil)
	if err != nil {
		return nil, fmt.Errorf("getting camera images: %w", err)
	}
	return firstDecodableImage(ctx, images)
}

// CheckLevel measures one frame at the arm's current pose and logs it. Logs
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
//
// orderID is the order in progress, if any: the saved frame is tagged with it,
// and an order's frame is saved without "annotate".
func (d *Detector) CheckLevel(ctx context.Context, logger logging.Logger, orderID string) error {
	img, err := d.frame(ctx)
	if err != nil {
		return fmt.Errorf("check_ice_level: %w", err)
	}
	b := d.p.Band()
	rows, err := rowBrightness(img, b)
	if err != nil {
		return fmt.Errorf("check_ice_level: %w", err)
	}
	measurement := d.measureRows(rows, b, img)
	surface := measurement.Contrast
	d.SaveFrame(ctx, logger, orderID, measurement, "check", "", 0)

	bounds := img.Bounds()
	stopRow := d.p.StopRow()
	logger.Infof("check_ice_level: frame %dx%d, band x %d..%d y %d..%d, window %d, min contrast %.0f, stop row %d",
		bounds.Max.X, bounds.Max.Y, b.X0, b.X1, b.Y0, b.Y1, b.Window, b.MinContrast, stopRow)
	d.reportCheckBrightness(logger, rows, measurement.Brightness, stopRow)
	if !surface.Found {
		logger.Infof("check_ice_level: no ice surface in the band — either not enough ice yet, or risen past row %d", stopRow)
	} else {
		logger.Infof("check_ice_level: ice surface at row %d (step %.0f), %d px below the stop row %d",
			surface.Row, surface.Step, surface.Row-stopRow, stopRow)
	}
	rim, err := strongestStepAboveBand(img, b)
	if err != nil {
		return fmt.Errorf("check_ice_level: %w", err)
	}
	if !rim.Found {
		logger.Infof("check_ice_level: no strong step above the band — on an empty glass that means the rim was not found where one is expected")
		return nil
	}
	logger.Infof(
		"check_ice_level: strongest step above the band at row %d (step %.0f) — on an empty glass that is the glass rim, and how the seating is checked; the fixtures ice_stop_row_px was chosen against sit at 277 and 379",
		rim.Row,
		rim.Step,
	)
	return nil
}

// reportCheckBrightness prints the brightness shadow's read of the same frame,
// and — whether or not the shadow is configured — the range of row means it
// found. That range is how ice_brightness_thresh gets chosen in the first
// place: an empty glass has to sit below it and ice above it, at this machine's
// lighting rather than the one the fixtures were shot under.
func (d *Detector) reportCheckBrightness(logger logging.Logger, rows []float64, shadow Reading, stopRow int) {
	lo, hi := rows[0], rows[0]
	for _, v := range rows {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	logger.Infof(
		"check_ice_level: row brightness in the band runs %.0f to %.0f — ice_brightness_thresh has to sit above an empty glass's highest row and below ice's lowest",
		lo,
		hi,
	)
	if !d.p.ShadowOn() {
		logger.Infof(
			"check_ice_level: brightness shadow off (set ice_brightness_thresh to log what an absolute cutoff would have decided on every dispense)",
		)
		return
	}
	if !shadow.Found {
		logger.Infof("check_ice_level: brightness shadow (threshold %.0f over %d rows) found no surface",
			d.p.BrightnessThresh, d.p.ShadowRun())
		return
	}
	logger.Infof("check_ice_level: brightness shadow (threshold %.0f over %d rows) has the surface at row %d (%.0f) — %s",
		d.p.BrightnessThresh, d.p.ShadowRun(), shadow.Row, shadow.Step, rowRelativeToStop(shadow.Row, stopRow))
}

// rowRelativeToStop says which side of the stop row a row falls on, in
// words, because the sign of the difference is the thing a bare row number
// hides.
func rowRelativeToStop(row, stopRow int) string {
	switch {
	case row > stopRow:
		return fmt.Sprintf("%d px below the stop row — not yet", row-stopRow)
	case row < stopRow:
		return fmt.Sprintf("%d px above the stop row", stopRow-row)
	default:
		return "level with the stop row"
	}
}

// colorSourceName is the camera source the measurement reads.
const colorSourceName = "color"

// firstDecodableImage decodes the first image in the response. The filter is a
// request, not a guarantee — a camera that ignores filterSourceNames returns
// everything — so the color image is preferred by name when several come back.
func firstDecodableImage(ctx context.Context, images []camera.NamedImage) (image.Image, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("camera returned no images")
	}
	pick, named := &images[0], false
	for i := range images {
		if images[i].SourceName == colorSourceName {
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
			len(images), colorSourceName, names)
	}
	img, err := pick.Image(ctx)
	if err != nil {
		return nil, fmt.Errorf("decoding %q image: %w", pick.SourceName, err)
	}
	return img, nil
}
