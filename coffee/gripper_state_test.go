package coffee

import (
	"context"
	"errors"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/spatialmath"
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
		name     string
		doResp   map[string]any
		doErr    error
		wantErr  bool
		wantMiss bool // expect errors.Is(err, errGripMissed)
	}{
		{name: "holding", doResp: map[string]any{"pos": 520.0}},
		{name: "miss", doResp: map[string]any{"pos": 357.0}, wantErr: true, wantMiss: true},
		{name: "read error not miss", doErr: errors.New("boom"), wantErr: true, wantMiss: false},
		{name: "malformed resp", doResp: map[string]any{}, wantErr: true, wantMiss: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := inject.NewGripper("g")
			g.GrabFunc = func(context.Context, map[string]any) (bool, error) { return true, nil }
			g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
				return tc.doResp, tc.doErr
			}
			s := &beanjaminCoffee{cfg: &Config{}, gripper: g, logger: logging.NewTestLogger(t)}
			err := s.grabAndVerifyHolding(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("grabAndVerifyHolding err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got := errors.Is(err, errGripMissed); got != tc.wantMiss {
				t.Errorf("errors.Is(err, errGripMissed) = %v, want %v (err=%v)", got, tc.wantMiss, err)
			}
		})
	}
}

// TestDropHeldContainer covers cancel's pre-recovery release: a cup/glass in the
// holding band is dropped (open→close) and its geometry detached, while a gripper
// closed on the filter handle, an already-open gripper, or an unreadable position
// leaves the jaws (and the portafilter) untouched.
func TestDropHeldContainer(t *testing.T) {
	cases := []struct {
		name      string
		pos       float64
		doErr     error
		noGripper bool
		wantOpen  bool
		wantGrab  bool
		wantErr   bool
	}{
		{name: "holding drops and detaches", pos: 520, wantOpen: true, wantGrab: true},
		{name: "closed on filter handle, no drop", pos: 357},
		{name: "already open, no drop", pos: 850},
		{name: "read error skips drop", doErr: errors.New("boom")},
		{name: "no gripper noop", noGripper: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := clawsStaticFS(t, spatialmath.NewZeroPose())
			s := &beanjaminCoffee{cfg: &Config{}, logger: logging.NewTestLogger(t), cachedFS: fs}
			if err := s.addHeldItemFrame(testBox(t, spatialmath.NewZeroPose())); err != nil {
				t.Fatalf("addHeldItemFrame: %v", err)
			}
			var opened, grabbed bool
			if !tc.noGripper {
				g := inject.NewGripper("g")
				g.OpenFunc = func(context.Context, map[string]any) error { opened = true; return nil }
				g.GrabFunc = func(context.Context, map[string]any) (bool, error) { grabbed = true; return true, nil }
				g.DoFunc = func(context.Context, map[string]any) (map[string]any, error) {
					return map[string]any{"pos": tc.pos}, tc.doErr
				}
				s.gripper = g
			}
			err := s.dropHeldContainer(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("dropHeldContainer err = %v, wantErr = %v", err, tc.wantErr)
			}
			if opened != tc.wantOpen {
				t.Errorf("Open called = %v, want %v", opened, tc.wantOpen)
			}
			if grabbed != tc.wantGrab {
				t.Errorf("Grab called = %v, want %v", grabbed, tc.wantGrab)
			}
			// The held-item geometry is detached iff we actually dropped.
			wantAttached := !tc.wantOpen
			if s.heldItemAttached != wantAttached {
				t.Errorf("heldItemAttached = %v, want %v", s.heldItemAttached, wantAttached)
			}
		})
	}
}

func TestNormalizeGripperAtStart(t *testing.T) {
	cases := []struct {
		name      string
		positions []float64 // successive gripperPos reads
		wantErr   bool
		wantGrab  bool
	}{
		{name: "already closed", positions: []float64{357}, wantGrab: false},
		{name: "open self-heals", positions: []float64{850, 357}, wantGrab: true},
		{name: "jammed", positions: []float64{850, 520}, wantErr: true, wantGrab: true},
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
