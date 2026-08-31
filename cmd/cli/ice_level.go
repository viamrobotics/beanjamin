package main

// ice-level: measure ice height from the color images in a capture dir.
//
// Depth is unusable at the dispense pose — the glass sits ~208mm from an
// arm-mounted camera whose minimum range is ~250mm, so nothing inside it
// returns. RGB works well: ice is bright and the empty glass shows the dark
// machine through it, so the ice surface is a strong horizontal brightness
// edge. Scanning rows down a column inside the glass finds it.
//
// The ROI defaults are for the cappuccina dispense pose. Check them against one
// image (--dump-profile) before trusting a batch from any other pose.

import (
	"flag"
	"fmt"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// roi is the column inside the glass that gets scanned, in image pixels.
type roi struct {
	x0, x1, y0, y1 int
	thresh         float64 // --threshold only: brightness above which a row counts as ice
	runLen         int     // --threshold only: consecutive bright rows required, to reject glare
	window         int     // contrast-step half-width, in rows
	minContrast    float64 // smallest step counted as a surface
	useThreshold   bool    // use the historical absolute-brightness method
}

func runIceLevel(args []string) error {
	flagSet := flag.NewFlagSet("ice-level", flag.ExitOnError)
	rawDir := flagSet.String("raw-dir", "", "Directory written by ice-snapshot or ice-dispense (required)")
	x0 := flagSet.Int("x0", 700, "ROI left edge, px (inset from the glass wall)")
	x1 := flagSet.Int("x1", 910, "ROI right edge, px")
	y0 := flagSet.Int("y0", 280, "ROI top, px (the glass rim)")
	y1 := flagSet.Int("y1", 670, "ROI bottom, px (the lowest visible part of the glass)")
	useThreshold := flagSet.Bool("threshold", false, "Measure with the absolute-brightness method instead of the contrast step. For comparison only — a fixed cutoff reads a brightly-lit empty glass as full")
	thresh := flagSet.Float64("thresh", 150, "Brightness cutoff for --threshold (0-255). Ignored otherwise")
	runLen := flagSet.Int("run", 8, "Consecutive bright rows required, to reject glare and reflections")
	minContrast := flagSet.Float64("min-contrast", 25, "Smallest brightness step counted as an ice surface. Below it, the frame reports no ice")
	window := flagSet.Int("window", 48, "Contrast-step half-width in rows. The ice surface is a gradual edge, so a narrow window measures only part of it")
	series := flagSet.Bool("series", false, "Report as a time series against the ice pin, for ice-dispense runs")
	targetFill := flagSet.Float64("target-fill", 0.7, "Fill level the loop would stop at, for time-to-target in --series")
	fullPx := flagSet.Float64("full-px", 0, "Pixels for a full glass, from a calibration run's \"full glass\" figure")
	interceptPx := flagSet.Float64("intercept-px", 0, "Calibration intercept in px, from the same run")
	contrast := flagSet.Bool("contrast", false, "Report the contrast-step distribution and recommend ice_min_contrast")
	dumpProfile := flagSet.Bool("dump-profile", false, "Print the row-brightness profile of the first frame per label and stop")
	csvPath := flagSet.String("csv", "", "Write per-frame measurements here")

	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if *rawDir == "" {
		return fmt.Errorf("--raw-dir is required")
	}
	r := roi{x0: *x0, x1: *x1, y0: *y0, y1: *y1, thresh: *thresh, runLen: *runLen,
		window: *window, minContrast: *minContrast, useThreshold: *useThreshold}
	if r.x0 >= r.x1 || r.y0 >= r.y1 {
		return fmt.Errorf("empty ROI: x %d..%d, y %d..%d", r.x0, r.x1, r.y0, r.y1)
	}

	frames, err := readManifest(*rawDir)
	if err != nil {
		return err
	}
	var withImages []rawFrame
	for _, f := range frames {
		if len(f.Images) > 0 {
			withImages = append(withImages, f)
		}
	}
	if len(withImages) == 0 {
		return fmt.Errorf("%s has no frames with images — capture with a newer ice-snapshot", *rawDir)
	}
	fmt.Printf("%d frames with images in %s\n", len(withImages), *rawDir)
	if r.useThreshold {
		fmt.Printf("ROI x %d..%d, y %d..%d — absolute threshold %.0f over %d rows (comparison only)\n\n",
			r.x0, r.x1, r.y0, r.y1, r.thresh, r.runLen)
	} else {
		fmt.Printf("ROI x %d..%d, y %d..%d — contrast step, window %d, min %.0f\n\n",
			r.x0, r.x1, r.y0, r.y1, r.window, r.minContrast)
	}

	if *dumpProfile {
		return dumpProfiles(*rawDir, withImages, r)
	}
	if *contrast {
		return reportContrast(*rawDir, withImages, r)
	}
	if *series {
		// Without a calibration the target can't be located in pixels, so the
		// trace is still printed but time-to-target is skipped rather than faked.
		targetPx, showFill := 0.0, false
		if *fullPx > 0 {
			targetPx = *targetFill**fullPx + *interceptPx
			showFill = true
			fmt.Printf("target %.0f%% fill = %.0f px (from --full-px %.0f, --intercept-px %.0f)\n\n",
				*targetFill*100, targetPx, *fullPx, *interceptPx)
		} else {
			fmt.Printf("no --full-px given, so no target line; run a calibration first\n\n")
		}
		return reportSeries(*rawDir, withImages, r, targetPx, showFill, *fullPx, *interceptPx)
	}
	return reportLevels(*rawDir, withImages, r, *csvPath)
}

// measureFrame returns the ice height in pixels above the ROI's bottom, or 0
// when no surface is found.
//
// The contrast step is the shipping method; --threshold selects the historical
// absolute-brightness one, which is kept only so the two can be compared on the
// same captures. A fixed cutoff reads a brightly-lit empty glass as FULL, which
// is why it is not the default.
func measureFrame(dir string, f rawFrame, r roi) (float64, error) {
	rows, err := rowProfile(filepath.Join(dir, colorImage(f)), r)
	if err != nil {
		return 0, err
	}
	if !r.useThreshold {
		step, idx := contrastStep(rows, r.window)
		if idx < 0 || step < r.minContrast {
			return 0, nil
		}
		return float64(len(rows) - idx), nil
	}
	// The topmost sustained bright run is the ice surface. A single bright row is
	// glare off the glass; ice holds its brightness over many rows.
	for i := 0; i+r.runLen < len(rows); i++ {
		bright := true
		for j := 0; j < r.runLen; j++ {
			if rows[i+j] < r.thresh {
				bright = false
				break
			}
		}
		if bright {
			return float64(len(rows) - i), nil
		}
	}
	return 0, nil
}

// contrastStep finds the ice surface as the largest brightness step in the row
// profile: below the surface is bright ice, above it is the dark machine seen
// through empty glass. Returns the step magnitude and the row it sits at.
//
// This is illumination-invariant where an absolute threshold is not — brighter
// ambient light raises both sides of the step and leaves its size alone, while
// it can lift a whole empty glass past a fixed cutoff.
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
		// Averaging over w rows is what stops a single glare row presenting as
		// a step.
		if d := below/float64(w) - above/float64(w); d > best {
			best, bestIdx = d, i
		}
	}
	return best, bestIdx
}

// colorImage picks the color image out of a frame's saved images, falling back
// to the first one when nothing is named like a color source.
func colorImage(f rawFrame) string {
	for _, name := range f.Images {
		if strings.Contains(name, "color") || strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".png") {
			return name
		}
	}
	return f.Images[0]
}

// rowProfile returns the mean brightness of each row in the ROI, top to bottom.
func rowProfile(path string, r roi) ([]float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // read-only
	img, err := jpeg.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	bounds := img.Bounds()
	if r.x1 > bounds.Max.X || r.y1 > bounds.Max.Y {
		return nil, fmt.Errorf("ROI x..%d y..%d is outside %s (%dx%d)",
			r.x1, r.y1, filepath.Base(path), bounds.Max.X, bounds.Max.Y)
	}
	rows := make([]float64, 0, r.y1-r.y0)
	width := float64(r.x1 - r.x0)
	for y := r.y0; y < r.y1; y++ {
		var sum float64
		for x := r.x0; x < r.x1; x++ {
			red, green, blue, _ := img.At(x, y).RGBA()
			sum += float64(red>>8+green>>8+blue>>8) / 3
		}
		rows = append(rows, sum/width)
	}
	return rows, nil
}

// reportLevels groups by label and fits measured height against the true fill,
// the same shape ice-fit reports for depth captures.
func reportLevels(dir string, frames []rawFrame, r roi, csvPath string) error {
	byLabel := map[string][]float64{}
	truthOf := map[string]float64{}
	var order []string
	for _, f := range frames {
		px, err := measureFrame(dir, f, r)
		if err != nil {
			return err
		}
		if _, seen := byLabel[f.Label]; !seen {
			order = append(order, f.Label)
			truthOf[f.Label] = f.Truth
		}
		byLabel[f.Label] = append(byLabel[f.Label], px)
	}
	sort.Slice(order, func(i, j int) bool { return truthOf[order[i]] < truthOf[order[j]] })

	fmt.Printf("  %-10s %7s %10s %8s %5s\n", "label", "truth", "ice px", "sd", "n")
	var pts []fitPoint
	for _, l := range order {
		v := byLabel[l]
		fmt.Printf("  %-10s %6.0f%% %10.0f %8.1f %5d\n", l, truthOf[l]*100, mean(v), stddev(v), len(v))
		if truthOf[l] < 0 {
			continue // a control, not a calibration level
		}
		for _, px := range v {
			pts = append(pts, fitPoint{truth: truthOf[l], z: px, label: l})
		}
	}

	// Levels reading exactly zero are below the occlusion line and carry no
	// information; fitting them drags the line toward a floor that isn't real.
	var visible []fitPoint
	for _, p := range pts {
		if p.z > 0 {
			visible = append(visible, p)
		}
	}
	if len(visible) < 2 {
		fmt.Printf("\n  too few levels show any ice to fit — check the ROI with --dump-profile\n")
		return nil
	}
	slope, intercept, r2 := leastSquares(visible)
	fmt.Printf("\n  full glass       %.0f px\n", slope)
	fmt.Printf("  R²               %.4f\n", r2)

	// The blind floor is where levels stop registering, NOT where the fit crosses
	// zero. The fit matches every level that registers and is confidently wrong
	// through the occlusion boundary, so extrapolating into it understates the
	// floor — on the reference captures, 22% against a real 40%.
	highestZero, lowestNonZero := -1.0, -1.0
	for _, l := range order {
		if truthOf[l] < 0 {
			continue
		}
		if mean(byLabel[l]) == 0 {
			highestZero = math.Max(highestZero, truthOf[l])
		} else if lowestNonZero < 0 || truthOf[l] < lowestNonZero {
			lowestNonZero = truthOf[l]
		}
	}
	if lowestNonZero >= 0 {
		fmt.Printf("  measured floor   %.0f%% — the lowest level that registered at all\n", lowestNonZero*100)
		if highestZero >= 0 {
			fmt.Printf("                   (%.0f%% read zero; set ice_min_fill_fraction to %.2f, not lower)\n",
				highestZero*100, lowestNonZero)
		}
	}
	if slope > 0 && intercept < 0 {
		fmt.Printf("  fit extrapolates %.0f%% — IGNORE THIS. The fit is wrong through the\n", -intercept/slope*100)
		fmt.Printf("                   occlusion boundary; use the measured floor above.\n")
	}

	fmt.Printf("\n  %-10s %7s %10s %8s\n", "label", "truth", "reported", "error")
	var absErr float64
	var n int
	for _, l := range order {
		if truthOf[l] < 0 {
			continue
		}
		reported := (mean(byLabel[l]) - intercept) / slope
		fmt.Printf("  %-10s %6.0f%% %9.0f%% %+7.0f%%\n", l, truthOf[l]*100, reported*100, (reported-truthOf[l])*100)
		if mean(byLabel[l]) > 0 {
			absErr += math.Abs(reported - truthOf[l])
			n++
		}
	}
	if n > 0 {
		fmt.Printf("\n  mean abs error   %.1f%% of glass height (measurable levels only)\n", absErr/float64(n)*100)
	}
	if csvPath == "" {
		return nil
	}
	return writeLevelCSV(csvPath, order, truthOf, byLabel, slope, intercept)
}

// reportSeries prints the fill trace against the ice pin. Multiple runs in one
// dir (distinguished by label) are compared: dispense rate varies run to run, so
// a single trace sets a ceiling that a slower run will blow straight through.
func reportSeries(dir string, frames []rawFrame, r roi, targetPx float64, showFill bool, fullPx, interceptPx float64) error {
	type sample struct {
		ms    int64
		phase string
		px    float64
	}
	runs := map[string][]sample{}
	var order []string
	for _, f := range frames {
		if f.TMs == nil {
			continue
		}
		px, err := measureFrame(dir, f, r)
		if err != nil {
			return err
		}
		if _, seen := runs[f.Label]; !seen {
			order = append(order, f.Label)
		}
		runs[f.Label] = append(runs[f.Label], sample{ms: *f.TMs, phase: f.Phase, px: px})
	}
	if len(runs) == 0 {
		return fmt.Errorf("no timed frames — this dir is not from ice-dispense")
	}
	for _, l := range order {
		sort.Slice(runs[l], func(i, j int) bool { return runs[l][i].ms < runs[l][j].ms })
	}

	asFill := func(px float64) string {
		if !showFill {
			return ""
		}
		return fmt.Sprintf(" (%.0f%%)", (px-interceptPx)/fullPx*100)
	}

	var firstIce, peaks, settles []float64
	var missedTarget []string
	// inFlight: readings from the last stretch of dwell, while ice is still
	// falling. settled: readings from the end of post-roll, after it stopped.
	// The gap between them is the whole point of the run.
	var inFlight, settled []float64

	for _, l := range order {
		samples := runs[l]
		peak := 0.0
		for _, s := range samples {
			peak = math.Max(peak, s.px)
		}
		fmt.Printf("\n=== %s ===\n", l)
		fmt.Printf("  %8s %6s %8s  %s\n", "t", "phase", "ice px", "trace")
		for _, s := range samples {
			bars := 0
			if peak > 0 {
				bars = int(s.px / peak * 40)
			}
			mark := " "
			if targetPx > 0 && s.px >= targetPx {
				mark = "|" // first row at or past target is where the loop would stop
			}
			fmt.Printf("  %+7dms %6s %8.0f %s%s\n", s.ms, s.phase, s.px, mark, strings.Repeat("█", bars))
		}

		var first, reach int64 = -1, -1
		var lastDwell, lastPost float64
		for _, s := range samples {
			if s.ms >= 0 && first < 0 && s.px > 0 {
				first = s.ms
			}
			if s.ms >= 0 && reach < 0 && targetPx > 0 && s.px >= targetPx {
				reach = s.ms
			}
			switch s.phase {
			case "dwell":
				lastDwell = s.px
			case "post":
				lastPost = s.px
			}
		}
		// Last 3s of dwell vs last 3s of post-roll, per run.
		var dwellEnd, postEnd int64 = math.MinInt64, math.MinInt64
		for _, sm := range samples {
			if sm.phase == "dwell" && sm.ms > dwellEnd {
				dwellEnd = sm.ms
			}
			if sm.phase == "post" && sm.ms > postEnd {
				postEnd = sm.ms
			}
		}
		for _, sm := range samples {
			if sm.phase == "dwell" && sm.ms >= dwellEnd-3000 {
				inFlight = append(inFlight, sm.px)
			}
			if sm.phase == "post" && sm.ms >= postEnd-3000 {
				settled = append(settled, sm.px)
			}
		}
		peaks = append(peaks, peak)
		fmt.Printf("  peak %.0f px%s", peak, asFill(peak))
		if first >= 0 {
			firstIce = append(firstIce, float64(first))
			fmt.Printf(", first ice +%dms", first)
		}
		if reach >= 0 {
			fmt.Printf(", target at +%dms", reach)
		} else if targetPx > 0 {
			missedTarget = append(missedTarget, l)
			fmt.Printf(", TARGET NEVER REACHED")
		}
		if lastPost != 0 {
			settles = append(settles, lastPost-lastDwell)
			fmt.Printf(", settle %+.0f px", lastPost-lastDwell)
		}
		fmt.Println()
	}

	// The question this run exists to answer: does ice in flight corrupt the
	// reading? Compare the settled level against what was being reported while
	// ice was actively falling.
	if len(inFlight) > 0 && len(settled) > 0 {
		flying, still := mean(inFlight), mean(settled)
		delta := flying - still
		fmt.Printf("\n=== falling ice ===\n")
		fmt.Printf("  while dispensing  %.0f px%s\n", flying, asFill(flying))
		fmt.Printf("  once settled      %.0f px%s\n", still, asFill(still))
		fmt.Printf("  difference        %+.0f px", delta)
		if fullPx > 0 {
			fmt.Printf(" (%+.0f%% of glass height)", delta/fullPx*100)
		}
		fmt.Println()

		bad := math.Abs(delta) > 0.05*math.Max(fullPx, still)
		fmt.Println()
		if bad {
			fmt.Printf("  FALLING ICE MATTERS. The reading during a dispense is off by more than\n")
			fmt.Printf("  5%% of the glass, so polling while the pin is open measures the stream as\n")
			fmt.Printf("  much as the pile. ice_dispense_min_sec cannot fix this — it only guards\n")
			fmt.Printf("  the first moments. The loop has to close the pin briefly before each\n")
			fmt.Printf("  check: pulse, pause, measure, repeat.\n")
		} else {
			fmt.Printf("  FALLING ICE IS FINE. The reading holds within 5%% of the settled level\n")
			fmt.Printf("  while ice is in flight, so the loop can poll continuously with the pin\n")
			fmt.Printf("  open. Set ice_dispense_min_sec just past the first-ice time above.\n")
		}
	}

	if len(order) > 1 {
		fmt.Printf("\n=== across %d runs ===\n", len(order))
		for _, name := range []struct {
			label string
			vals  []float64
		}{{"first ice (ms)", firstIce}, {"peak (px)", peaks}, {"settle (px)", settles}} {
			if len(name.vals) == 0 {
				continue
			}
			sorted := append([]float64(nil), name.vals...)
			sort.Float64s(sorted)
			fmt.Printf("  %-16s min %.0f, median %.0f, max %.0f\n",
				name.label, sorted[0], sorted[len(sorted)/2], sorted[len(sorted)-1])
		}
	}
	if len(missedTarget) > 0 {
		fmt.Printf("\n  %d run(s) never reached the target: %s\n", len(missedTarget), strings.Join(missedTarget, ", "))
	}
	return nil
}

// reportContrast sweeps the contrast window and, for each width, separates the
// frames that should show ice from the ones that should not. Both parameters
// fall out of the same table: the window that gives the widest separation
// without distorting the calibration, and a min-contrast in the middle of that
// separation.
//
// The window matters more than it looks. The ice surface is a gradual edge
// spanning tens of rows, so a narrow window measures only part of the step and
// the two populations overlap.
func reportContrast(dir string, frames []rawFrame, r roi) error {
	type rec struct {
		label string
		truth float64
		rows  []float64
	}
	var all []rec
	for _, f := range frames {
		rows, err := rowProfile(filepath.Join(dir, colorImage(f)), r)
		if err != nil {
			return err
		}
		all = append(all, rec{f.Label, f.Truth, rows})
	}

	fmt.Printf("  %-8s %10s %9s %8s %8s %8s\n", "window", "no-ice max", "ice min", "gap", "full px", "R²")
	type result struct {
		w         int
		gap, rec  float64
		slope, r2 float64
	}
	var best result
	var slopes []float64
	type row struct {
		w                  int
		mx, mn, slope, r2s float64
	}
	var table []row
	for _, w := range []int{8, 16, 24, 32, 40, 48, 56} {
		var noIce, ice []float64
		var pts []fitPoint
		for _, a := range all {
			step, idx := contrastStep(a.rows, w)
			// A level at or below the occlusion floor shows no ice however much
			// was poured, so it belongs to the no-ice population.
			if a.truth >= 0 && a.truth <= 0.30 {
				noIce = append(noIce, step)
				continue
			}
			if a.truth > 0.30 {
				ice = append(ice, step)
				if idx >= 0 {
					pts = append(pts, fitPoint{truth: a.truth, z: float64(len(a.rows) - idx), label: a.label})
				}
			}
		}
		if len(noIce) == 0 || len(ice) == 0 || len(pts) < 2 {
			continue
		}
		mx, mn := maxOf(noIce), minOf(ice)
		slope, _, r2 := leastSquares(pts)
		fmt.Printf("  %-8d %10.0f %9.0f %8.0f %8.0f %8.4f\n", w, mx, mn, mn-mx, slope, r2)
		table = append(table, row{w: w, mx: mx, mn: mn, slope: slope, r2s: r2})
		slopes = append(slopes, slope)
	}
	if len(table) == 0 {
		fmt.Printf("\n  no window produced both populations\n")
		return nil
	}
	// Full-glass pixels plateau across most windows and fall off once the window
	// starts clipping the scan range. R² stays high through that — the fit is
	// still linear, just rescaled — so the calibration has to be guarded
	// separately or the widest-gap window silently rescales the measurement.
	sortedSlopes := append([]float64(nil), slopes...)
	sort.Float64s(sortedSlopes)
	plateau := sortedSlopes[len(sortedSlopes)/2]
	var clipped []int
	for _, t := range table {
		if math.Abs(t.slope-plateau)/plateau > 0.03 {
			clipped = append(clipped, t.w)
			continue
		}
		if t.mn-t.mx > best.gap && t.r2s > 0.98 {
			best = result{w: t.w, gap: t.mn - t.mx, rec: (t.mn + t.mx) / 2, slope: t.slope, r2: t.r2s}
		}
	}
	if len(clipped) > 0 {
		fmt.Printf("\n  windows %v rejected: full px more than 3%% off the %.0f px plateau,\n", clipped, plateau)
		fmt.Printf("  which means the window is clipping the scan range and rescaling the\n")
		fmt.Printf("  measurement. R² does not catch this on its own.\n")
	}
	if best.w == 0 {
		fmt.Printf("\n  NO SEPARATION at any window. The step on an empty glass is as large as\n")
		fmt.Printf("  on a full one. Check the ROI first (--dump-profile); if it is right, the\n")
		fmt.Printf("  contrast method does not work at this pose.\n")
		return nil
	}
	fmt.Printf("\n  ice_contrast_window = %d\n", best.w)
	fmt.Printf("  ice_min_contrast    = %.0f   (midpoint of a gap of %.0f)\n", best.rec, best.gap)
	fmt.Printf("  ice_full_glass_px   = %.0f   (R² %.4f)\n", best.slope, best.r2)
	fmt.Printf("\n  Without a min-contrast the empty-glass frames report a surface near the\n")
	fmt.Printf("  rim — i.e. nearly full. That is the failure this value exists to stop,\n")
	fmt.Printf("  and it is why the gap matters more than the absolute numbers.\n")
	fmt.Printf("\n  One lighting condition only. Re-run over captures taken under different\n")
	fmt.Printf("  lighting before trusting the margin — the point of the contrast method is\n")
	fmt.Printf("  that it should not move when the lights do.\n")
	return nil
}

func minOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = math.Min(m, x)
	}
	return m
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

// dumpProfiles prints one row profile per label, for checking the ROI and
// picking a threshold against real numbers instead of guessing.
func dumpProfiles(dir string, frames []rawFrame, r roi) error {
	seen := map[string]bool{}
	var order []string
	profiles := map[string][]float64{}
	for _, f := range frames {
		if seen[f.Label] {
			continue
		}
		rows, err := rowProfile(filepath.Join(dir, colorImage(f)), r)
		if err != nil {
			return err
		}
		seen[f.Label] = true
		order = append(order, f.Label)
		profiles[f.Label] = rows
		if len(order) >= 8 {
			break
		}
	}
	fmt.Printf("  %-6s", "y")
	for _, l := range order {
		fmt.Printf(" %8s", truncate(l, 8))
	}
	fmt.Println()
	for i := 0; i < r.y1-r.y0; i += 10 {
		fmt.Printf("  %-6d", r.y0+i)
		for _, l := range order {
			fmt.Printf(" %8.0f", profiles[l][i])
		}
		fmt.Println()
	}
	fmt.Printf("\n  Ice reads bright, an empty glass reads dark. Put --thresh between them.\n")
	return nil
}

func writeLevelCSV(path string, order []string, truthOf map[string]float64, byLabel map[string][]float64, slope, intercept float64) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // flushed below
	if _, err := fmt.Fprintln(file, "label,truth,ice_px,reported"); err != nil {
		return err
	}
	for _, l := range order {
		for _, px := range byLabel[l] {
			if _, err := fmt.Fprintf(file, "%s,%.3f,%.0f,%.4f\n", l, truthOf[l], px, (px-intercept)/slope); err != nil {
				return err
			}
		}
	}
	fmt.Printf("  wrote %s\n", path)
	return file.Close()
}
