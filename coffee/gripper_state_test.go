package coffee

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
)

func TestClassifyGripper(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}} // zero cfg → default thresholds 430/685
	cases := []struct {
		pos  float64
		want gripperState
	}{
		{pos: 0, want: gripperClosed},    // below band
		{pos: 510, want: gripperHolding}, // object in band
		{pos: 850, want: gripperOpen},    // above band
	}
	for _, tc := range cases {
		if got := s.classifyGripper(tc.pos); got != tc.want {
			t.Errorf("classifyGripper(%.0f) = %s, want %s", tc.pos, got, tc.want)
		}
	}
}

func TestGrabAndVerifyHolding(t *testing.T) {
	cases := []struct {
		name string
		// positions returned by successive reads; the last repeats once exhausted.
		// The first is the position before the grab, then the settle polls, then
		// the verifying read.
		positions  []float64
		badResp    bool
		doErr      error
		timeoutSec float64
		wantErr    bool
		wantMiss   bool // expect errors.Is(err, errGripMissed)
		wantOpen   bool // expect the jaws reopened
	}{
		{name: "holding", positions: []float64{850, 600, 520, 520}},
		{name: "miss reopens", positions: []float64{850, 357, 357, 357, 850}, wantErr: true, wantMiss: true, wantOpen: true},
		// Order 66936cfa: the jaws snagged at 755 with the cup still between them.
		{name: "snagged open reopens", positions: []float64{850, 755, 755, 755, 850}, wantErr: true, wantMiss: true, wantOpen: true},
		{name: "reopen fails not miss", positions: []float64{357}, timeoutSec: 0.001, wantErr: true, wantOpen: true},
		{name: "read error not miss", doErr: errors.New("boom"), wantErr: true},
		{name: "malformed resp", badResp: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads, opened := 0, false
			g := inject.NewGripper("g")
			g.GrabFunc = func(context.Context, map[string]any) (bool, error) { return true, nil }
			g.OpenFunc = func(context.Context, map[string]any) error { opened = true; return nil }
			g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
				reads++
				if tc.doErr != nil {
					return nil, tc.doErr
				}
				if tc.badResp {
					return map[string]any{}, nil
				}
				return map[string]any{"pos": tc.positions[min(reads-1, len(tc.positions)-1)]}, nil
			}
			s := &beanjaminCoffee{
				cfg:     &Config{GripperOpenTimeoutSec: tc.timeoutSec},
				gripper: g,
				logger:  logging.NewTestLogger(t),
			}
			err := s.grabAndVerifyHolding(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("grabAndVerifyHolding err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got := errors.Is(err, errGripMissed); got != tc.wantMiss {
				t.Errorf("errors.Is(err, errGripMissed) = %v, want %v (err=%v)", got, tc.wantMiss, err)
			}
			if opened != tc.wantOpen {
				t.Errorf("Open called = %v, want %v", opened, tc.wantOpen)
			}
		})
	}
}

// TestOpenAndVerifyOpen covers the release-side wait: it returns only once the
// jaws read open, surfaces errGripperNotOpen when they never get there, and keeps
// an unreadable position distinct from a stuck gripper.
func TestOpenAndVerifyOpen(t *testing.T) {
	cases := []struct {
		name string
		// positions returned by successive reads; the last repeats once exhausted.
		positions  []float64
		doErr      error
		timeoutSec float64
		wantErr    bool
		wantStuck  bool // expect errors.Is(err, errGripperNotOpen)
		wantReads  int
	}{
		{name: "open on first read", positions: []float64{850}, wantReads: 1},
		{name: "opens slowly", positions: []float64{357, 500, 850}, timeoutSec: 5, wantReads: 3},
		{name: "never opens", positions: []float64{357}, timeoutSec: 0.001, wantErr: true, wantStuck: true, wantReads: 1},
		{name: "read error not stuck", doErr: errors.New("boom"), wantErr: true, wantStuck: false, wantReads: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opened, reads := 0, 0
			g := inject.NewGripper("g")
			g.OpenFunc = func(context.Context, map[string]any) error { opened++; return nil }
			g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
				reads++
				if tc.doErr != nil {
					return nil, tc.doErr
				}
				return map[string]any{"pos": tc.positions[min(reads-1, len(tc.positions)-1)]}, nil
			}
			s := &beanjaminCoffee{
				cfg:     &Config{GripperOpenTimeoutSec: tc.timeoutSec},
				gripper: g,
				logger:  logging.NewTestLogger(t),
			}
			err := s.openAndVerifyOpen(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("openAndVerifyOpen err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got := errors.Is(err, errGripperNotOpen); got != tc.wantStuck {
				t.Errorf("errors.Is(err, errGripperNotOpen) = %v, want %v (err=%v)", got, tc.wantStuck, err)
			}
			if opened != 1 {
				t.Errorf("Open called %d times, want exactly 1 — the retry polls, it does not re-command", opened)
			}
			if reads != tc.wantReads {
				t.Errorf("position reads = %d, want %d", reads, tc.wantReads)
			}
		})
	}
}

// TestOpenAndVerifyOpenHonorsCancel checks the poll aborts on context
// cancellation rather than sleeping out the full timeout — an operator cancel
// must interrupt the wait.
func TestOpenAndVerifyOpenHonorsCancel(t *testing.T) {
	g := inject.NewGripper("g")
	g.OpenFunc = func(context.Context, map[string]any) error { return nil }
	g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"pos": 357.0}, nil // never opens
	}
	s := &beanjaminCoffee{
		cfg:     &Config{GripperOpenTimeoutSec: 60},
		gripper: g,
		logger:  logging.NewTestLogger(t),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.openAndVerifyOpen(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("openAndVerifyOpen err = %v, want context.Canceled", err)
	}
}

func TestNormalizeGripperAtStart(t *testing.T) {
	cases := []struct {
		name string
		// successive gripperPos reads: the check, then (after a close) the
		// position before the close, the settle polls and the final read.
		positions []float64
		wantErr   bool
		wantGrab  bool
	}{
		{name: "already closed", positions: []float64{357}, wantGrab: false},
		{name: "open self-heals", positions: []float64{850, 850, 600, 357, 357}, wantGrab: true},
		{name: "jammed", positions: []float64{850, 850, 520, 520}, wantErr: true, wantGrab: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := inject.NewGripper("g")
			var grabbed bool
			g.GrabFunc = func(context.Context, map[string]any) (bool, error) {
				grabbed = true
				return true, nil
			}
			i := 0
			g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
				pos := tc.positions[i]
				if i < len(tc.positions)-1 {
					i++
				}
				return map[string]any{"pos": pos}, nil
			}
			s := &beanjaminCoffee{cfg: &Config{}, gripper: g, logger: logging.NewTestLogger(t)}
			err := s.normalizeGripperAtStart(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("normalizeGripperAtStart err = %v, wantErr = %v", err, tc.wantErr)
			}
			if grabbed != tc.wantGrab {
				t.Errorf("Grab called = %v, want %v", grabbed, tc.wantGrab)
			}
		})
	}
}

// TestActuateGripperSettles covers the wait after a plain open or close: it
// returns once the jaws settle, never on a reading taken before they started
// moving, and falls back to the fixed gripperPause without ever failing when
// the jaws are slow or unreadable.
func TestActuateGripperSettles(t *testing.T) {
	cases := []struct {
		name   string
		action gripperAction
		// positions returned by successive reads, starting with the position
		// before the command; the last repeats once exhausted.
		positions []float64
		doErr     error
		wantReads int // 0 skips the check
		wantWarn  bool
		// wantFullPause expects the wait to last gripperPause, else less.
		wantFullPause bool
	}{
		{
			name: "open settles in the open band", action: openGripperAction,
			positions: []float64{357, 500, 850}, wantReads: 3,
		},
		{
			name: "close settles on a stable holding read", action: closeGripperAction,
			positions: []float64{850, 600, 520, 521}, wantReads: 4,
		},
		{
			name: "close settles on a stable closed read", action: closeGripperAction,
			positions: []float64{850, 600, 357, 357}, wantReads: 4,
		},
		{
			name: "close ignores stale reads before the jaws move", action: closeGripperAction,
			positions: []float64{850, 850, 850, 600, 520, 520}, wantReads: 6,
		},
		{
			name: "close from closed settles at once", action: closeGripperAction,
			positions: []float64{357, 357, 357}, wantReads: 3,
		},
		{
			name: "open that never settles times out without error", action: openGripperAction,
			positions: []float64{357}, wantWarn: true, wantFullPause: true,
		},
		{
			name: "close that stays at its start times out without error", action: closeGripperAction,
			positions: []float64{850}, wantWarn: true, wantFullPause: true,
		},
		{
			name: "unreadable gripper falls back to the fixed pause", action: closeGripperAction,
			doErr: errors.New("boom"), wantReads: 1, wantFullPause: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads, commands := 0, 0
			g := inject.NewGripper("g")
			g.OpenFunc = func(context.Context, map[string]any) error { commands++; return nil }
			g.GrabFunc = func(context.Context, map[string]any) (bool, error) { commands++; return true, nil }
			g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
				reads++
				if tc.doErr != nil {
					return nil, tc.doErr
				}
				return map[string]any{"pos": tc.positions[min(reads-1, len(tc.positions)-1)]}, nil
			}
			logger, logs := logging.NewObservedTestLogger(t)
			s := &beanjaminCoffee{cfg: &Config{}, gripper: g, logger: logger}

			start := time.Now()
			if err := s.actuateGripper(context.Background(), tc.action); err != nil {
				t.Fatalf("actuateGripper err = %v, want nil", err)
			}
			elapsed := time.Since(start)

			if commands != 1 {
				t.Errorf("gripper commanded %d times, want 1", commands)
			}
			if tc.wantReads != 0 && reads != tc.wantReads {
				t.Errorf("position reads = %d, want %d", reads, tc.wantReads)
			}
			if got := logs.FilterMessageSnippet("not settled").Len() > 0; got != tc.wantWarn {
				t.Errorf("settle warning logged = %v, want %v", got, tc.wantWarn)
			}
			if tc.wantFullPause {
				if elapsed < gripperPause || elapsed > gripperPause+3*gripperPollInterval {
					t.Errorf("waited %s, want about %s", elapsed, gripperPause)
				}
			} else if elapsed >= gripperPause {
				t.Errorf("waited %s, want less than the fixed %s", elapsed, gripperPause)
			}
		})
	}
}

// TestActuateGripperHonorsCancel checks a cancel interrupts the settle wait.
func TestActuateGripperHonorsCancel(t *testing.T) {
	g := inject.NewGripper("g")
	g.OpenFunc = func(context.Context, map[string]any) error { return nil }
	g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"pos": 357.0}, nil // never opens
	}
	s := &beanjaminCoffee{cfg: &Config{}, gripper: g, logger: logging.NewTestLogger(t)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.actuateGripper(ctx, openGripperAction); !errors.Is(err, context.Canceled) {
		t.Errorf("actuateGripper err = %v, want context.Canceled", err)
	}
}
