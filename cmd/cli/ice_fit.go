package main

// ice-fit: reads an ice-snapshot CSV and fits the glass geometry out of it,
// rather than making you measure it up front.
//
// HISTORICAL. This served the depth path, which does not work at the dispense
// pose (the glass sits inside the camera's minimum range). Kept because the
// least-squares reporting is still how a calibration is read; ice-level does
// the equivalent for the RGB measurement that shipped.
//
// Every capture records the measured contents height (z_p90 by default, world
// mm; --column picks another) next to the fill level you actually filled to
// (truth, 0..1). Across several levels those are a straight line:
//
//	height = glassHeight * truth + baseZ
//
// so the least-squares slope IS the glass interior height and the intercept IS
// the interior floor — the two numbers --base-z and --glass-height would have
// asked for. R² answers G4 directly: whether the measurement tracks reality.
//
// A fitted intercept over several levels also beats a measured one: a single
// empty-glass capture is one noisy reading, and clear plastic is exactly what a
// depth camera is worst at.

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
)

// fitPoint is one capture reduced to the two columns the fit needs.
type fitPoint struct {
	truth float64 // fill level as filled, 0..1
	z     float64 // measured contents height from the chosen column, world mm
	label string
}

func runIceFit(args []string) error {
	flagSet := flag.NewFlagSet("ice-fit", flag.ExitOnError)
	csvPath := flagSet.String("csv", "", "CSV written by ice-snapshot or ice-analyze (required)")
	column := flagSet.String("column", "z_p90", "Which measured-height column to fit: z_p90, z_p50, z_max, z_p10, z_min")
	absolute := flagSet.Bool("absolute", false, "Fit world Z instead of height above the grip point. Only correct if the arm never moved between captures")
	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if *csvPath == "" {
		return fmt.Errorf("--csv is required")
	}

	points, err := readFitPoints(*csvPath, *column, *absolute)
	if err != nil {
		return err
	}
	levels := groupByTruth(points)
	if len(levels) < 2 {
		return fmt.Errorf("need captures at 2 or more distinct --truth levels to fit, found %d", len(levels))
	}

	height, baseZ, r2 := leastSquares(points)
	if height <= 0 {
		return fmt.Errorf("fit slope is %.1f mm — the measurement falls as the glass fills, which means it is not measuring the contents", height)
	}

	datum := "above the grip point"
	if *absolute {
		datum = "in world Z"
	}
	fmt.Printf("fit over %d captures at %d levels, fitting %s %s\n\n", len(points), len(levels), *column, datum)
	fmt.Printf("  glass height     %.1f mm   (fit slope)\n", height)
	fmt.Printf("  glass floor      %+.1f mm  (fit intercept — the interior floor, %s)\n", baseZ, datum)
	fmt.Printf("  R²               %.3f\n\n", r2)

	fmt.Printf("  %-16s %6s %8s %9s %8s %9s\n", "level", "truth", *column, "reported", "error", "1-frame sd")
	var absErr, worstSD float64
	var prevZ float64
	monotonic := true
	replicated := false
	for i, lv := range levels {
		meanZ := mean(lv.zs)
		reported := (meanZ - baseZ) / height
		err := reported - lv.truth
		absErr += math.Abs(err)
		if i > 0 && meanZ <= prevZ {
			monotonic = false
		}
		prevZ = meanZ

		// Per-level scatter, converted to fill fraction — what one frame's
		// reading would wobble by. The loop confirms off single frames, so this
		// matters more than how well the level means line up.
		sd := stddev(lv.zs) / height
		sdCol := "     —"
		if len(lv.zs) > 1 {
			replicated = true
			worstSD = math.Max(worstSD, sd)
			sdCol = fmt.Sprintf("%5.1f%%", sd*100)
		}
		fmt.Printf("  %-16s %5.0f%% %8.1f %8.0f%% %+7.0f%% %9s\n",
			lv.label, lv.truth*100, meanZ, reported*100, err*100, sdCol)
	}
	mae := absErr / float64(len(levels))
	fmt.Printf("\n  mean abs error   %.1f%% of glass height (across level means)\n", mae*100)
	if replicated {
		fmt.Printf("  worst 1-frame sd %.1f%% of glass height\n", worstSD*100)
	}

	fmt.Println()
	switch {
	case !monotonic:
		fmt.Println("  FAIL — the reading does not rise monotonically with fill level.")
		fmt.Println("  The measurement is not usable; go to the classifier fallback (ICE_LEVEL_PLAN.md §1).")
	case mae > 0.10:
		fmt.Printf("  FAIL — mean abs error %.0f%% exceeds G4's 10%% bar.\n", mae*100)
		fmt.Println("  Monotonic though, so this may be a sample-volume problem rather than a vision one:")
		fmt.Println("  check the green box actually sits on the glass, then re-run.")
	default:
		fmt.Println("  PASS (G4) — use this in the coffee service config:")
		fmt.Printf("    glass_dimensions.height_mm: %.0f\n", height)
	}

	// G5 is a separate question from G4 and can fail while G4 passes: averaging
	// several frames per level hides scatter that a single frame would show.
	switch {
	case !replicated:
		fmt.Println("\n  G5 not answered — every level has one frame. Re-run a level with")
		fmt.Println("  --repeat 5 to find out how much a single reading wobbles.")
	case worstSD > 0.05:
		fmt.Printf("\n  G5 FAIL — a single frame varies by %.0f%% of glass height.\n", worstSD*100)
		fmt.Println("  Raise iceConfirmations above 2, or take a median of 3 frames inside")
		fmt.Println("  glassFillFraction — otherwise the loop will confirm off a noise spike.")
	default:
		fmt.Printf("\n  G5 PASS — a single frame is good to %.0f%% of glass height;\n", worstSD*100)
		fmt.Println("  iceConfirmations = 2 is enough.")
	}
	return nil
}

// stddev returns the sample standard deviation, or 0 for fewer than 2 values.
func stddev(vals []float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	m := mean(vals)
	variance := 0.0
	for _, v := range vals {
		variance += (v - m) * (v - m)
	}
	return math.Sqrt(variance / float64(len(vals)-1))
}

// truthLevel is every capture taken at one fill level.
type truthLevel struct {
	truth float64
	label string
	zs    []float64
}

func groupByTruth(points []fitPoint) []truthLevel {
	byTruth := map[float64]*truthLevel{}
	for _, p := range points {
		lv, ok := byTruth[p.truth]
		if !ok {
			lv = &truthLevel{truth: p.truth, label: p.label}
			byTruth[p.truth] = lv
		}
		lv.zs = append(lv.zs, p.z)
	}
	levels := make([]truthLevel, 0, len(byTruth))
	for _, lv := range byTruth {
		levels = append(levels, *lv)
	}
	sort.Slice(levels, func(i, j int) bool { return levels[i].truth < levels[j].truth })
	return levels
}

// leastSquares fits z = slope*truth + intercept and returns the fit with its R².
func leastSquares(points []fitPoint) (slope, intercept, r2 float64) {
	n := float64(len(points))
	var sumX, sumY, sumXY, sumXX float64
	for _, p := range points {
		sumX += p.truth
		sumY += p.z
		sumXY += p.truth * p.z
		sumXX += p.truth * p.truth
	}
	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		return 0, 0, 0
	}
	slope = (n*sumXY - sumX*sumY) / denom
	intercept = (sumY - slope*sumX) / n

	meanY := sumY / n
	var ssRes, ssTot float64
	for _, p := range points {
		fitted := slope*p.truth + intercept
		ssRes += (p.z - fitted) * (p.z - fitted)
		ssTot += (p.z - meanY) * (p.z - meanY)
	}
	if ssTot == 0 {
		return slope, intercept, 0
	}
	return slope, intercept, 1 - ssRes/ssTot
}

// readFitPoints pulls the truth and chosen height columns, skipping rows missing
// either — captures taken without --truth, and levels where nothing was detected.
func readFitPoints(path, column string, absolute bool) ([]fitPoint, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // read-only

	reader := csv.NewReader(file)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading %s header: %w", path, err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[name] = i
	}
	for _, required := range []string{"label", "truth", column} {
		if _, ok := col[required]; !ok {
			return nil, fmt.Errorf("%s has no %q column — is it an ice-snapshot CSV?", path, required)
		}
	}

	var points []fitPoint
	var skipped int
	var gripSpread spread
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		truth, truthErr := strconv.ParseFloat(row[col["truth"]], 64)
		z, zErr := strconv.ParseFloat(row[col[column]], 64)
		if truthErr != nil || zErr != nil {
			skipped++
			continue
		}
		// Measure the contents from the gripper, not from the world floor. The
		// glass is rigidly held, so its contents sit at a fixed height above the
		// grip point no matter where the arm is — but world Z moves with the arm,
		// so fitting that folds every arm reposition into the residual.
		if gripCol, ok := col["grip_z"]; ok && !absolute {
			gripZ, gripErr := strconv.ParseFloat(row[gripCol], 64)
			if gripErr != nil {
				skipped++
				continue
			}
			z -= gripZ
			gripSpread.add(gripZ)
		}
		points = append(points, fitPoint{truth: truth, z: z, label: row[col["label"]]})
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "skipped %d row(s) missing truth or z_p90\n", skipped)
	}
	if len(points) == 0 {
		return nil, fmt.Errorf("%s has no rows with both truth and %s set — capture with --truth", path, column)
	}
	// Worth saying out loud: it means the arm moved between captures, which is
	// harmless measured from the gripper and corrupting measured in world Z.
	if r := gripSpread.rng(); r > 1 {
		fmt.Printf("note: the grip point moved %.0fmm across these captures; heights are\n", r)
		fmt.Printf("      measured from it, so that costs nothing (--absolute would not be).\n\n")
	}
	return points, nil
}

// spread tracks the range of a value across rows.
type spread struct {
	lo, hi float64
	seen   bool
}

func (s *spread) add(v float64) {
	if !s.seen {
		s.lo, s.hi, s.seen = v, v, true
		return
	}
	s.lo, s.hi = math.Min(s.lo, v), math.Max(s.hi, v)
}

func (s *spread) rng() float64 {
	if !s.seen {
		return 0
	}
	return s.hi - s.lo
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	total := 0.0
	for _, v := range vals {
		total += v
	}
	return total / float64(len(vals))
}
