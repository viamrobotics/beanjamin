package coffee

import (
	"context"
	"testing"
	"time"
)

// TestBrightnessSurfaceRowFixtures pins the shadow against the same glasses the
// contrast step is pinned against, at the threshold the committed fixtures
// suggest: the midpoint of the worst empty reading and the weakest true
// surface. The point of the table is the last column — the shadow names rows
// the step structurally cannot, which is the whole reason to measure it.
func TestBrightnessSurfaceRowFixtures(t *testing.T) {
	const thresh = 132.0
	b := iceBandFromConfig(&Config{})
	for _, tc := range []struct {
		fixture string
		found   bool
		// stepSees is what the contrast method reports on the same frame, so a
		// change to either method shows up here as a disagreement rather than a
		// silently updated number.
		stepSees bool
	}{
		{"fill_0.jpg", false, false},
		{"fill_30.jpg", false, false},
		{"fill_40.jpg", true, true},
		{"fill_80.jpg", true, false},
		{"fill_100.jpg", true, false},
		{"rim379_empty.jpg", false, false},
		{"rim379_rising.jpg", true, true},
		{"rim379_passed.jpg", true, false},
	} {
		img := loadFixture(t, tc.fixture)
		rows, err := rowBrightness(img, b)
		if err != nil {
			t.Fatalf("%s: %v", tc.fixture, err)
		}
		got := brightnessSurfaceRow(rows, b, thresh, defaultIceBrightRun)
		if got.found != tc.found {
			t.Errorf("%s: brightness found=%v (row %d), want %v", tc.fixture, got.found, got.row, tc.found)
		}
		if step := contrastReading(rows, b); step.found != tc.stepSees {
			t.Errorf("%s: contrast found=%v, want %v", tc.fixture, step.found, tc.stepSees)
		}
	}
}

// TestBrightnessSurfaceRowNeedsARun: one bright row is a glint off the glass.
// The run length is the shadow's only glare defense, so a lone bright row must
// not read as a surface.
func TestBrightnessSurfaceRowNeedsARun(t *testing.T) {
	b := iceBandFromConfig(&Config{})
	rows := make([]float64, b.y1-b.y0)
	for i := range rows {
		rows[i] = 80
	}
	rows[10] = 250
	if got := brightnessSurfaceRow(rows, b, 132, defaultIceBrightRun); got.found {
		t.Errorf("a single bright row reported a surface at row %d", got.row)
	}
	for i := 10; i < 10+defaultIceBrightRun; i++ {
		rows[i] = 250
	}
	got := brightnessSurfaceRow(rows, b, 132, defaultIceBrightRun)
	if !got.found || got.row != b.y0+10 {
		t.Errorf("a run of %d bright rows: found=%v row=%d, want true %d", defaultIceBrightRun, got.found, got.row, b.y0+10)
	}
}

// TestBrightnessShadowNeverDecides is the invariant the whole feature rests on.
// The shadow is fed the opposite of the contrast step on every frame — it sees
// a surface at the stop row from the first tick, which is its own stop
// condition — and the dispense must end exactly as it does without it.
func TestBrightnessShadowNeverDecides(t *testing.T) {
	contrast := []bool{false, false, true, true, false, false}
	for _, tc := range []struct {
		name    string
		measure func(context.Context) (iceMeasurement, error)
	}{
		{"shadow off", readings(contrast...)},
		{"shadow stopping immediately", shadowReadings(contrast, []bool{true}, 500)},
		{"shadow never seeing anything", shadowReadings(contrast, []bool{false}, 0)},
	} {
		s, _ := iceTestService(t, &Config{IceBrightnessThresh: 132})
		res, err := s.dwellUntilFull(context.Background(), context.Background(), tc.measure)
		if err != nil {
			t.Fatalf("%s: dwellUntilFull: %v", tc.name, err)
		}
		if res.timedOut || !res.sawSurface {
			t.Errorf("%s: timedOut=%v sawSurface=%v, want false true — the shadow changed the outcome",
				tc.name, res.timedOut, res.sawSurface)
		}
	}
}

// TestBrightnessShadowVerdicts covers the four outcomes the summary line has to
// tell apart. Only a difference in kind counts as a disagreement: the two
// methods stop on different signals, so a timing gap — in either direction, and
// including running ahead of the step's own first sighting — is the measurement
// and not a fault.
func TestBrightnessShadowVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		shadow    brightnessShadow
		firstSeen int // ms the contrast step first saw ice
		disagreed bool
	}{
		{"never saw ice", brightnessShadow{on: true}, 19000, true},
		{"saw ice but never reached the stop row", brightnessShadow{on: true, sawSurface: true, closestRow: 600}, 19000, true},
		{"ran ahead of the step", brightnessShadow{on: true, sawSurface: true, firstSeen: 12 * time.Second, wouldStop: true, wouldStopAt: 18 * time.Second}, 19000, false},
		{"stopped on its first sighting", brightnessShadow{on: true, sawSurface: true, firstSeen: 2 * time.Second, wouldStop: true, wouldStopAt: 2 * time.Second, stoppedOnFirstSighting: true}, 19000, true},
	} {
		line, disagreed := tc.shadow.verdict(true, 23*time.Second, time.Duration(tc.firstSeen)*time.Millisecond)
		if line == "" {
			t.Fatalf("%s: no summary line", tc.name)
		}
		if disagreed != tc.disagreed {
			t.Errorf("%s: disagreed=%v, want %v (%s)", tc.name, disagreed, tc.disagreed, line)
		}
	}
	if line, _ := (&brightnessShadow{}).verdict(true, 0, 0); line != "" {
		t.Errorf("an off shadow produced a summary line: %q", line)
	}
}
