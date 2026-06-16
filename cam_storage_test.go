package beanjamin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
)

// newCamStorageTestCoffee builds a coffee service wired to a fake cam-storage mux
// and a temp pending-clips dir, returning the coffee, the fake, and the dir.
func newCamStorageTestCoffee(t *testing.T) (*beanjaminCoffee, *inject.GenericService, string) {
	t.Helper()
	dir := t.TempDir()
	cam := inject.NewGenericService("cam-mux")
	c := &beanjaminCoffee{
		logger:               logging.NewTestLogger(t),
		camStorage:           cam,
		pendingOrderClipsDir: dir,
	}
	return c, cam, dir
}

// TestCleanupSkipGate_GuaranteesClosedSegmentWindow pins the arithmetic invariant the
// removed clipTo clamp now relies on: when the cleanup skip gate passes (now ≥ gate),
// the recovered clipTo is at least one full segment in the past, so it lands in closed
// segments and can never exceed now. If anyone retunes these constants and breaks the
// relationship, this fails instead of silently producing truncated/invalid clips.
func TestCleanupSkipGate_GuaranteesClosedSegmentWindow(t *testing.T) {
	videoFrom := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	// Mirrors cleanupPendingClips: the gate it waits past, and the clipTo it then saves.
	gate := videoFrom.Add(maxBrewDuration + clipLead + segmentDuration + clipFlushMargin)
	clipTo := videoFrom.Add(maxBrewDuration + clipLead)

	lag := gate.Sub(clipTo)
	if lag < segmentDuration {
		t.Fatalf("skip gate lets clipTo sit only %s before the earliest allowed now; "+
			"need ≥ segmentDuration (%s) so the trailing segment is closed", lag, segmentDuration)
	}
	if want := segmentDuration + clipFlushMargin; lag != want {
		t.Errorf("gate-to-clipTo lag = %s, want %s", lag, want)
	}
	// At the earliest allowed now (== gate), clipTo must already be in the past.
	if !clipTo.Before(gate) {
		t.Errorf("clipTo %s is not before the earliest allowed now %s — clamp removal is unsafe", clipTo, gate)
	}
}

// TestSaveOrderVideoAndClear_ClearsOnlyOnSuccess is the core regression test for this
// branch: the pending-clip record must survive a failed save so cleanupPendingClips can
// retry it, and must be removed once the save succeeds.
func TestSaveOrderVideoAndClear_ClearsOnlyOnSuccess(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name      string
		doFunc    func(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error)
		wantClear bool
	}{
		{
			name: "success clears record",
			doFunc: func(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
				return map[string]interface{}{"filename": "clip.mp4"}, nil
			},
			wantClear: true,
		},
		{
			name: "transport error keeps record",
			doFunc: func(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
				return nil, context.DeadlineExceeded
			},
			wantClear: false,
		},
		{
			name: "per-store errors keep record",
			doFunc: func(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
				return map[string]interface{}{"errors": map[string]any{"store0": "filename too long"}}, nil
			},
			wantClear: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cam, dir := newCamStorageTestCoffee(t)
			cam.DoFunc = tc.doFunc

			order := NewOrder("espresso", "Ada", "hi", "bye")
			c.writePendingSave(order, now)
			path := filepath.Join(dir, order.ID+".json")
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("pending record was not written: %v", err)
			}

			c.saveOrderVideoAndClear(order, now.Add(-clipLead), now.Add(clipTrail), nil, c.logger)

			_, err := os.Stat(path)
			cleared := os.IsNotExist(err)
			if cleared != tc.wantClear {
				t.Fatalf("pending record cleared = %v, want %v (stat err: %v)", cleared, tc.wantClear, err)
			}
		})
	}
}

// TestSaveOrderVideoAndClear_NoStorageDropsRecord verifies the misconfig path: with no
// cam-storage mux, no save can ever run, so the pending record is dropped rather than
// left to accumulate forever.
func TestSaveOrderVideoAndClear_NoStorageDropsRecord(t *testing.T) {
	dir := t.TempDir()
	c := &beanjaminCoffee{
		logger:               logging.NewTestLogger(t),
		camStorage:           nil,
		pendingOrderClipsDir: dir,
	}
	order := NewOrder("espresso", "Ada", "hi", "bye")
	c.writePendingSave(order, time.Now().UTC())
	path := filepath.Join(dir, order.ID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending record was not written: %v", err)
	}

	c.saveOrderVideoAsync(order, time.Now().UTC(), nil)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected pending record dropped when no cam storage, stat err: %v", err)
	}
}

// TestIssueVideoSave_RequestShape locks down the save command: synchronous, tagged with
// the order ID, and minimal metadata (order_id + order_status only) so an unbounded value
// can't blow the filename limit.
func TestIssueVideoSave_RequestShape(t *testing.T) {
	c, cam, _ := newCamStorageTestCoffee(t)
	var got map[string]interface{}
	cam.DoFunc = func(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
		got = cmd
		return map[string]interface{}{}, nil
	}

	order := NewOrder("espresso", "Ada", "hi", "bye")
	from := time.Now().UTC().Add(-clipLead)
	to := time.Now().UTC().Add(clipTrail)

	if ok := c.issueVideoSave(order, from, to, context.DeadlineExceeded, c.logger); !ok {
		t.Fatalf("issueVideoSave returned false on a clean response")
	}

	if got["command"] != "save" {
		t.Errorf("command = %v, want save", got["command"])
	}
	if got["async"] != false {
		t.Errorf("async = %v, want false (sync save so failures surface)", got["async"])
	}
	tags, _ := got["tags"].([]string)
	if len(tags) != 1 || tags[0] != order.ID {
		t.Errorf("tags = %v, want [%s]", got["tags"], order.ID)
	}

	var meta map[string]string
	if err := json.Unmarshal([]byte(got["metadata"].(string)), &meta); err != nil {
		t.Fatalf("metadata not valid JSON: %v", err)
	}
	if len(meta) != 2 {
		t.Errorf("metadata has %d keys (%v), want exactly order_id+order_status", len(meta), meta)
	}
	if meta["order_id"] != order.ID {
		t.Errorf("metadata order_id = %q, want %q", meta["order_id"], order.ID)
	}
	if meta["order_status"] != "failed" {
		t.Errorf("metadata order_status = %q, want failed (execErr was set)", meta["order_status"])
	}
}
