package beanjamin

import (
	"context"
	"errors"
	"testing"

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
		name     string
		doResp   map[string]interface{}
		doErr    error
		wantErr  bool
		wantMiss bool // expect errors.Is(err, errGripMissed)
	}{
		{name: "holding", doResp: map[string]interface{}{"pos": 520.0}},
		{name: "miss", doResp: map[string]interface{}{"pos": 357.0}, wantErr: true, wantMiss: true},
		{name: "read error not miss", doErr: errors.New("boom"), wantErr: true, wantMiss: false},
		{name: "malformed resp", doResp: map[string]interface{}{}, wantErr: true, wantMiss: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := inject.NewGripper("g")
			g.GrabFunc = func(context.Context, map[string]interface{}) (bool, error) { return true, nil }
			g.DoFunc = func(context.Context, map[string]interface{}) (map[string]interface{}, error) {
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
			g.GrabFunc = func(context.Context, map[string]interface{}) (bool, error) {
				grabbed = true
				return true, nil
			}
			i := 0
			g.DoFunc = func(context.Context, map[string]interface{}) (map[string]interface{}, error) {
				pos := tc.positions[i]
				if i < len(tc.positions)-1 {
					i++
				}
				return map[string]interface{}{"pos": pos}, nil
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
