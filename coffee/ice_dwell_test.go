package coffee

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
)

// recordingPin records every Set on the ice pin, which is what the tests about
// the pin actually assert: not that it was closed, but that it was closed on
// whichever path the test took.
type recordingPin struct {
	*inject.GPIOPin
	mu       sync.Mutex
	sets     []bool
	fail     error
	failHigh error // fails only the HIGH write, as a lost response does
}

func newRecordingBoard(t *testing.T) (*inject.Board, *recordingPin) {
	t.Helper()
	pin := &recordingPin{GPIOPin: &inject.GPIOPin{}}
	pin.SetFunc = func(_ context.Context, high bool, _ map[string]interface{}) error {
		pin.mu.Lock()
		defer pin.mu.Unlock()
		pin.sets = append(pin.sets, high)
		if high && pin.failHigh != nil {
			return pin.failHigh
		}
		return pin.fail
	}
	brd := inject.NewBoard("ice-board")
	brd.GPIOPinByNameFunc = func(string) (board.GPIOPin, error) { return pin, nil }
	return brd, pin
}

func (p *recordingPin) history() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.sets...)
}

// iceTestService builds a service with the dispense timings scaled down so the
// loop runs in milliseconds. The ratios are what the loop cares about.
func iceTestService(t *testing.T, cfg *Config) (*beanjaminCoffee, *recordingPin) {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.CanServeIced = true
	cfg.IceDispensePinName = "ice-pin"
	if cfg.IceCheckIntervalSec == 0 {
		cfg.IceCheckIntervalSec = 0.002
	}
	// 0 would mean "unset" and fall back to the real 2s hold, which is longer
	// than any ceiling here.
	if cfg.IceDispenseMinSec == 0 {
		cfg.IceDispenseMinSec = 0.001
	}
	if cfg.IceDispenseMaxSec == 0 {
		cfg.IceDispenseMaxSec = 0.5
	}
	if cfg.IceDispenseSec == 0 {
		cfg.IceDispenseSec = 0.05
	}
	brd, pin := newRecordingBoard(t)
	return &beanjaminCoffee{
		logger:   logging.NewTestLogger(t),
		cfg:      cfg,
		queue:    NewOrderQueue(),
		iceBoard: brd,
	}, pin
}

// readings replays a fixed sequence of measurements, holding the last one once
// it runs out, so a test states only the part of the run it is about.
func readings(found ...bool) func(context.Context) (iceMeasurement, error) {
	var i int
	var mu sync.Mutex
	return func(context.Context) (iceMeasurement, error) {
		mu.Lock()
		defer mu.Unlock()
		f := found[len(found)-1]
		if i < len(found) {
			f = found[i]
		}
		i++
		if !f {
			return iceMeasurement{}, nil
		}
		return iceMeasurement{contrast: iceReading{row: 570, step: 40, found: true}}, nil
	}
}

// shadowReadings replays the two methods independently, which is the only thing
// the shadow tests are about: what the contrast step decides must not move when
// the shadow says something else.
func shadowReadings(contrast, brightness []bool, brightnessRow int) func(context.Context) (iceMeasurement, error) {
	var i int
	var mu sync.Mutex
	pick := func(seq []bool, n int) bool {
		if n < len(seq) {
			return seq[n]
		}
		return seq[len(seq)-1]
	}
	return func(context.Context) (iceMeasurement, error) {
		mu.Lock()
		defer mu.Unlock()
		n := i
		i++
		m := iceMeasurement{shadow: true}
		if pick(contrast, n) {
			m.contrast = iceReading{row: 570, step: 40, found: true}
		}
		if pick(brightness, n) {
			m.brightness = iceReading{row: brightnessRow, step: 180, found: true}
		}
		return m, nil
	}
}

// TestDwellUntilFullStopsOnDisappearanceAfterSighting is the happy path: ice
// appears, rises, and leaves the top of the band.
func TestDwellUntilFullStopsOnDisappearanceAfterSighting(t *testing.T) {
	s, _ := iceTestService(t, nil)
	start := time.Now()
	res, err := s.dwellUntilFull(context.Background(), context.Background(),
		readings(false, false, true, true, false, false))
	if err != nil {
		t.Fatalf("dwellUntilFull: %v", err)
	}
	if res.timedOut || !res.sawSurface {
		t.Errorf("timedOut=%v sawSurface=%v, want false true — it stopped on the measurement", res.timedOut, res.sawSurface)
	}
	if elapsed := time.Since(start); elapsed >= secondsToDuration(s.iceDispenseMaxSec()) {
		t.Errorf("took %s, which is the ceiling — it stopped on the deadline, not on the measurement", elapsed)
	}
}

// A single miss after a sighting is a dropped frame, not a full glass.
func TestDwellUntilFullNeedsConfirmedDisappearance(t *testing.T) {
	s, _ := iceTestService(t, nil)
	// One miss between sightings, then a real disappearance. If a lone miss
	// stopped the dispense this would return before the ice had risen.
	measure, calls := countingReadings(true, true, false, true, true, false, false)
	if _, err := s.dwellUntilFull(context.Background(), context.Background(), measure); err != nil {
		t.Fatalf("dwellUntilFull: %v", err)
	}
	if got := calls(); got < 7 {
		t.Errorf("stopped after %d readings, want at least 7 — a single dropped frame ended the dispense", got)
	}
}

// The asymmetry that matters: one false sighting during the ~20 empty ticks
// before ice arrives must not arm the latch and let the next empty reading stop
// a dispense on an empty glass.
func TestDwellUntilFullIgnoresLoneFalseSighting(t *testing.T) {
	s, _ := iceTestService(t, &Config{IceDispenseMaxSec: 0.2})
	measure, calls := countingReadings(false, true, false, false, false, false, false, false, false, false)
	start := time.Now()
	res, err := s.dwellUntilFull(context.Background(), context.Background(), measure)
	if err != nil {
		t.Fatalf("dwellUntilFull: %v", err)
	}
	if !res.timedOut || res.sawSurface {
		t.Errorf("timedOut=%v sawSurface=%v, want true false — a lone glare frame armed the latch", res.timedOut, res.sawSurface)
	}
	// Nothing ever confirmed, so the only correct ending is the ceiling.
	if elapsed := time.Since(start); elapsed < secondsToDuration(0.2) {
		t.Errorf("returned after %s, before the %v ceiling — a lone glare frame armed the stop latch (%d readings)",
			elapsed, secondsToDuration(0.2), calls())
	}
}

// The ceiling serves the glass rather than failing the order: the espresso is
// already brewed, so a light drink beats a discarded one.
func TestDwellUntilFullCeilingServesTheGlass(t *testing.T) {
	s, _ := iceTestService(t, &Config{IceDispenseMaxSec: 0.05})
	res, err := s.dwellUntilFull(context.Background(), context.Background(), readings(false))
	if err != nil {
		t.Errorf("dwellUntilFull at the ceiling returned %v, want nil — the order must not fail over it", err)
	}
	if !res.timedOut {
		t.Error("dwellUntilFull did not report the ceiling, so the counter and the announcement never fire")
	}
}

func TestDwellUntilFullCancels(t *testing.T) {
	for _, tc := range []string{"ctx", "cancelCtx"} {
		t.Run(tc, func(t *testing.T) {
			s, _ := iceTestService(t, &Config{IceDispenseMaxSec: 5})
			ctx, cancelCtx := context.Background(), context.Background()
			var cancel context.CancelFunc
			if tc == "ctx" {
				ctx, cancel = context.WithCancel(ctx)
			} else {
				cancelCtx, cancel = context.WithCancel(cancelCtx)
			}
			go func() {
				time.Sleep(10 * time.Millisecond)
				cancel()
			}()
			_, err := s.dwellUntilFull(ctx, cancelCtx, readings(false))
			if err == nil {
				t.Fatal("dwellUntilFull returned nil on cancel, want an error")
			}
		})
	}
}

// Vision being down must not cost a drink: after iceVisionMaxErrors in a row the
// loop finishes the dispense the open-loop way instead of running to the ceiling.
func TestDwellUntilFullFallsBackToFixedDwell(t *testing.T) {
	s, _ := iceTestService(t, &Config{IceDispenseMaxSec: 5, IceDispenseSec: 0.05})
	var calls int
	var mu sync.Mutex
	measure := func(context.Context) (iceMeasurement, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return iceMeasurement{}, errors.New("camera is down")
	}
	start := time.Now()
	res, err := s.dwellUntilFull(context.Background(), context.Background(), measure)
	if err != nil {
		t.Fatalf("dwellUntilFull: %v", err)
	}
	if res.timedOut {
		t.Error("the fixed-dwell fallback reported a timeout, which would bump the counter for a dispense that completed")
	}
	if elapsed := time.Since(start); elapsed >= secondsToDuration(5) {
		t.Errorf("took %s — it rode to the ceiling instead of falling back", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != iceVisionMaxErrors {
		t.Errorf("measured %d times, want %d — it should stop measuring once vision is written off", calls, iceVisionMaxErrors)
	}
}

// One transient failure is not vision being down.
func TestDwellUntilFullRecoversFromOneError(t *testing.T) {
	s, _ := iceTestService(t, nil)
	var i int
	var mu sync.Mutex
	measure := func(context.Context) (iceMeasurement, error) {
		mu.Lock()
		defer mu.Unlock()
		i++
		switch i {
		case 1, 3:
			return iceMeasurement{}, errors.New("transient")
		case 2, 4, 5:
			return iceMeasurement{contrast: iceReading{row: 570, step: 40, found: true}}, nil
		}
		return iceMeasurement{}, nil
	}
	if _, err := s.dwellUntilFull(context.Background(), context.Background(), measure); err != nil {
		t.Fatalf("dwellUntilFull: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if i < 6 {
		t.Errorf("stopped after %d readings — an error between sightings should not end the dispense", i)
	}
}

// pulseIcePin owns the pin. Whatever the dwell does, the pin must come back LOW.
func TestPulseIcePinAlwaysClosesThePin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     *Config
		cancel  bool
		wantErr bool
	}{
		{name: "fixed dwell", cfg: &Config{}},
		{name: "watched dispense", cfg: &Config{IceVisionEnabled: true, IceDispenseMaxSec: 0.05}},
		{name: "cancelled fixed dwell", cfg: &Config{IceDispenseSec: 5}, cancel: true, wantErr: true},
		{name: "cancelled watched dispense", cfg: &Config{IceVisionEnabled: true, IceDispenseMaxSec: 5}, cancel: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pin := iceTestService(t, tc.cfg)
			s.srcCamera = nil // a watched dispense with no camera is all errors, then the fallback
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				go func() {
					time.Sleep(10 * time.Millisecond)
					cancel()
				}()
			}
			err := s.pulseIcePin(ctx, context.Background())
			if tc.wantErr && err == nil {
				t.Error("pulseIcePin returned nil on cancel, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("pulseIcePin: %v", err)
			}
			got := pin.history()
			if len(got) != 2 || !got[0] || got[1] {
				t.Fatalf("pin sets = %v, want [true false] — the pin must be driven HIGH then LOW on every path", got)
			}
		})
	}
}

// The close is deferred before the HIGH write, so a Set that reaches the board
// and then loses its response — an error with the pin already open — is still
// followed by a close. Registering the defer after the open leaks the dispense.
func TestPulseIcePinClosesAfterAFailedOpen(t *testing.T) {
	s, pin := iceTestService(t, &Config{IceDispenseSec: 0.01})
	pin.failHigh = errors.New("response lost")

	if err := s.pulseIcePin(context.Background(), context.Background()); err == nil {
		t.Fatal("pulseIcePin returned nil when the pin could not be opened, want an error")
	}
	if got := pin.history(); len(got) != 2 || !got[0] || got[1] {
		t.Errorf("pin sets = %v, want [true false] — a failed open must still drive the pin LOW", got)
	}
}

// A pin that fails to close is worth an error even when the dispense itself
// worked: the ice machine is still running.
func TestPulseIcePinReportsAFailedClose(t *testing.T) {
	s, pin := iceTestService(t, &Config{IceDispenseSec: 0.01})
	pin.fail = errors.New("board is gone")
	if err := s.pulseIcePin(context.Background(), context.Background()); err == nil {
		t.Error("pulseIcePin returned nil when the pin could not be set, want an error")
	}
}

// closeIcePin is the shutdown path: Close cancels the sequence context, which
// does not itself put the pin down.
func TestCloseDrivesThePinLow(t *testing.T) {
	s, pin := iceTestService(t, nil)
	_, s.cancelFunc = context.WithCancel(context.Background())
	s.queueStop = make(chan struct{})
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := pin.history()
	if len(got) != 1 || got[0] {
		t.Errorf("pin sets = %v, want [false] — Close must drive the ice pin LOW", got)
	}
}

// countingReadings is readings with a call count, for the tests that assert how
// far the loop got rather than only that it finished.
func countingReadings(found ...bool) (func(context.Context) (iceMeasurement, error), func() int) {
	var mu sync.Mutex
	var calls int
	inner := readings(found...)
	return func(ctx context.Context) (iceMeasurement, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return inner(ctx)
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return calls
		}
}

// TestDwellUntilFullDefersItsReporting is the ordering invariant the whole
// result struct exists for: holdIcePin's deferred pin close does not run until
// dwellUntilFull returns, so anything the loop logged or wrote from inside
// itself would be a sensor RPC or a JPEG encode with the ice still running.
func TestDwellUntilFullDefersItsReporting(t *testing.T) {
	dir := t.TempDir()
	s, _ := iceTestService(t, &Config{SaveMotionRequestsDir: dir})
	ctx := withIceFrameSaving(context.Background())
	frame := loadFixture(t, "fill_40.jpg")
	var i int
	measure := func(context.Context) (iceMeasurement, error) {
		i++
		// Seen twice then gone twice: the stop path, with a real frame behind it
		// so the annotation actually runs.
		return iceMeasurement{contrast: iceReading{row: 570, step: 40, found: i <= 2}, frame: frame}, nil
	}

	res, err := s.dwellUntilFull(ctx, context.Background(), measure)
	if err != nil {
		t.Fatalf("dwellUntilFull: %v", err)
	}
	if wrote := savedFrames(t, dir); len(wrote) != 0 {
		t.Errorf("the loop wrote %v before returning, while the pin was still open", wrote)
	}

	s.reportIceDispense(ctx, res)
	if wrote := savedFrames(t, dir); len(wrote) != 1 {
		t.Errorf("reportIceDispense wrote %d frames, want 1", len(wrote))
	}
}

// TestDwellUntilFullReportsHowItEnded: the outcome slug becomes a tag on the
// data page and picks the caption, and the two exits that end on a fault hand
// back the last frame that measured cleanly — with a note, because a frame from
// before the fault is evidence about the run and not a picture of its end.
func TestDwellUntilFullReportsHowItEnded(t *testing.T) {
	frame := loadFixture(t, "fill_40.jpg")
	seen := func(found bool) iceMeasurement {
		return iceMeasurement{contrast: iceReading{row: 570, step: 40, found: found}, frame: frame}
	}

	for _, tc := range []struct {
		name     string
		outcome  string
		wantNote bool
		compare  bool
		measure  func(cancel context.CancelFunc) func(context.Context) (iceMeasurement, error)
	}{
		{"stopped", "stopped", false, true, func(context.CancelFunc) func(context.Context) (iceMeasurement, error) {
			var i int
			return func(context.Context) (iceMeasurement, error) { i++; return seen(i <= 2), nil }
		}},
		{"ceiling", "timeout", false, true, func(context.CancelFunc) func(context.Context) (iceMeasurement, error) {
			return func(context.Context) (iceMeasurement, error) { return seen(false), nil }
		}},
		{"camera gives out", "vision_fallback", true, false, func(context.CancelFunc) func(context.Context) (iceMeasurement, error) {
			var i int
			return func(context.Context) (iceMeasurement, error) {
				if i++; i == 1 {
					return seen(false), nil
				}
				return iceMeasurement{}, errors.New("camera is down")
			}
		}},
		{"cancelled mid-run", "cancelled", true, false, func(cancel context.CancelFunc) func(context.Context) (iceMeasurement, error) {
			var i int
			return func(context.Context) (iceMeasurement, error) {
				if i++; i == 2 {
					cancel()
				}
				return seen(false), nil
			}
		}},
	} {
		s, _ := iceTestService(t, &Config{IceDispenseMaxSec: 0.05})
		ctx, cancel := context.WithCancel(context.Background())
		res, _ := s.dwellUntilFull(ctx, context.Background(), tc.measure(cancel))
		cancel()
		if res.outcome != tc.outcome {
			t.Errorf("%s: outcome %q, want %q", tc.name, res.outcome, tc.outcome)
		}
		if gotNote := res.note != ""; gotNote != tc.wantNote {
			t.Errorf("%s: note %q, want one: %v", tc.name, res.note, tc.wantNote)
		}
		if res.compare != tc.compare {
			t.Errorf("%s: compare=%v, want %v — the shadow comparison only means something where the measurement ran to an answer",
				tc.name, res.compare, tc.compare)
		}
	}
}

// TestDwellUntilFullCapsAfterTheFirstSighting: the absolute ceiling has to clear
// a whole fill, so it is far above a full glass on a run whose surface is never
// confirmed past the stop row — and that overflow is ice, not a light drink.
// The cap since the first sighting is what bounds it.
func TestDwellUntilFullCapsAfterTheFirstSighting(t *testing.T) {
	s, _ := iceTestService(t, &Config{IceDispenseMaxSec: 5, IceAfterFirstSeenMaxSec: 0.05})
	start := time.Now()
	// Ice appears and stays visible: the disappearance the loop stops on never
	// comes, so without the cap this rides the 5s ceiling.
	res, err := s.dwellUntilFull(context.Background(), context.Background(), readings(false, true))
	if err != nil {
		t.Fatalf("dwellUntilFull: %v", err)
	}
	if res.outcome != "surface_cap" || !res.cappedAfterFirstSeen {
		t.Errorf("outcome %q capped=%v, want surface_cap true", res.outcome, res.cappedAfterFirstSeen)
	}
	if !res.timedOut || !res.sawSurface {
		t.Errorf("timedOut=%v sawSurface=%v, want true true — the cap must still announce and count", res.timedOut, res.sawSurface)
	}
	if elapsed := time.Since(start); elapsed >= secondsToDuration(5) {
		t.Errorf("took %s — it rode the absolute ceiling instead of the cap", elapsed)
	}
}

// The cap runs from the first sighting, not from the pin opening: a dispense
// that takes longer than the cap to show any ice at all must still get its full
// cap's worth once it does.
func TestDwellUntilFullCapRunsFromTheSighting(t *testing.T) {
	s, _ := iceTestService(t, &Config{IceDispenseMaxSec: 5, IceAfterFirstSeenMaxSec: 0.08, IceCheckIntervalSec: 0.01})
	// Empty for longer than the cap, then ice that never leaves the band.
	measure, calls := countingReadings(false, false, false, false, false, false, false, false, false, false, true)
	res, err := s.dwellUntilFull(context.Background(), context.Background(), measure)
	if err != nil {
		t.Fatalf("dwellUntilFull: %v", err)
	}
	if !res.sawSurface {
		t.Fatalf("never saw a surface after %d readings — the cap fired before ice arrived", calls())
	}
	if res.outcome != "surface_cap" {
		t.Errorf("outcome %q, want surface_cap", res.outcome)
	}
	if res.firstSeen < secondsToDuration(0.08) {
		t.Errorf("first seen at %s, want after the %v cap — the test no longer covers a late sighting",
			res.firstSeen, secondsToDuration(0.08))
	}
}

// Nothing about the normal stop changes: the cap is a ceiling, not a timer the
// dispense races.
func TestDwellUntilFullCapLeavesTheNormalStopAlone(t *testing.T) {
	s, _ := iceTestService(t, &Config{IceAfterFirstSeenMaxSec: 5})
	res, err := s.dwellUntilFull(context.Background(), context.Background(),
		readings(false, false, true, true, false, false))
	if err != nil {
		t.Fatalf("dwellUntilFull: %v", err)
	}
	if res.outcome != "stopped" || res.timedOut {
		t.Errorf("outcome %q timedOut=%v, want stopped false", res.outcome, res.timedOut)
	}
}
