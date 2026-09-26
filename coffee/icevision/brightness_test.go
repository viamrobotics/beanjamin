package icevision

import (
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
	b := Params{}.Band()
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
		if got.Found != tc.found {
			t.Errorf("%s: brightness found=%v (row %d), want %v", tc.fixture, got.Found, got.Row, tc.found)
		}
		if step := contrastReading(rows, b); step.Found != tc.stepSees {
			t.Errorf("%s: contrast found=%v, want %v", tc.fixture, step.Found, tc.stepSees)
		}
	}
}

// TestBrightnessSurfaceRowNeedsARun: one bright row is a glint off the glass.
// The run length is the shadow's only glare defense, so a lone bright row must
// not read as a surface.
func TestBrightnessSurfaceRowNeedsARun(t *testing.T) {
	b := Params{}.Band()
	rows := make([]float64, b.Y1-b.Y0)
	for i := range rows {
		rows[i] = 80
	}
	rows[10] = 250
	if got := brightnessSurfaceRow(rows, b, 132, defaultIceBrightRun); got.Found {
		t.Errorf("a single bright row reported a surface at row %d", got.Row)
	}
	for i := 10; i < 10+defaultIceBrightRun; i++ {
		rows[i] = 250
	}
	got := brightnessSurfaceRow(rows, b, 132, defaultIceBrightRun)
	if !got.Found || got.Row != b.Y0+10 {
		t.Errorf("a run of %d bright rows: found=%v row=%d, want true %d", defaultIceBrightRun, got.Found, got.Row, b.Y0+10)
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
		shadow    Shadow
		firstSeen int // ms the contrast step first saw ice
		disagreed bool
	}{
		{"never saw ice", Shadow{on: true}, 19000, true},
		{"saw ice but never reached the stop row", Shadow{on: true, sawSurface: true, closestRow: 600}, 19000, true},
		{"ran ahead of the step", Shadow{on: true, sawSurface: true, firstSeen: 12 * time.Second, wouldStop: true, wouldStopAt: 18 * time.Second}, 19000, false},
		{"stopped on its first sighting", Shadow{on: true, sawSurface: true, firstSeen: 2 * time.Second, wouldStop: true, wouldStopAt: 2 * time.Second, stoppedOnFirstSighting: true}, 19000, true},
	} {
		line, disagreed := tc.shadow.Verdict(true, 23*time.Second, time.Duration(tc.firstSeen)*time.Millisecond)
		if line == "" {
			t.Fatalf("%s: no summary line", tc.name)
		}
		if disagreed != tc.disagreed {
			t.Errorf("%s: disagreed=%v, want %v (%s)", tc.name, disagreed, tc.disagreed, line)
		}
	}
	if line, _ := (&Shadow{}).Verdict(true, 0, 0); line != "" {
		t.Errorf("an off shadow produced a summary line: %q", line)
	}
}
