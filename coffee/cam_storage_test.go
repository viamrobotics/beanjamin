package coffee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		doFunc    func(ctx context.Context, cmd map[string]any) (map[string]any, error)
		wantClear bool
	}{
		{
			name: "success clears record",
			doFunc: func(_ context.Context, _ map[string]any) (map[string]any, error) {
				return map[string]any{"filename": "clip.mp4"}, nil
			},
			wantClear: true,
		},
		{
			name: "transport error keeps record",
			doFunc: func(_ context.Context, _ map[string]any) (map[string]any, error) {
				return nil, context.DeadlineExceeded
			},
			wantClear: false,
		},
		{
			name: "per-store errors keep record",
			doFunc: func(_ context.Context, _ map[string]any) (map[string]any, error) {
				return map[string]any{"errors": map[string]any{"store0": "filename too long"}}, nil
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
	var got map[string]any
	cam.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
		got = cmd
		return map[string]any{}, nil
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

// writeStalePendingSave writes a pending record old enough to pass the cleanup skip gate
// and returns its path.
func writeStalePendingSave(t *testing.T, c *beanjaminCoffee, dir string) string {
	t.Helper()
	order := NewOrder("espresso", "Ada", "hi", "bye")
	c.writePendingSave(order, time.Now().UTC().Add(-time.Hour))
	path := filepath.Join(dir, order.ID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending record was not written: %v", err)
	}
	return path
}

// TestCleanupPendingClips_NoStorageKeepsRecords covers a record left behind after
// cam_storage_mux_name was removed: the sweep must report the misconfiguration without
// dereferencing the nil mux, and keep the record so the clip is recoverable later.
func TestCleanupPendingClips_NoStorageKeepsRecords(t *testing.T) {
	c, _, dir := newCamStorageTestCoffee(t)
	path := writeStalePendingSave(t, c, dir)
	c.camStorage = nil

	resp, err := c.cleanupPendingClips()
	if err == nil {
		t.Fatalf("expected error for pending records with no cam storage, got resp %v", resp)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending record should be kept when no cam storage, stat err: %v", err)
	}
}

// TestCleanupPendingClips_NoStorageNoRecords keeps the no-mux, nothing-pending case a
// quiet success so the scheduled job doesn't report a spurious failure.
func TestCleanupPendingClips_NoStorageNoRecords(t *testing.T) {
	c := &beanjaminCoffee{
		logger:               logging.NewTestLogger(t),
		pendingOrderClipsDir: t.TempDir(),
	}
	resp, err := c.cleanupPendingClips()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp["saved"] != 0 || resp["failed"] != 0 || resp["skipped"] != 0 {
		t.Errorf("resp = %v, want all zero counts", resp)
	}
}

// TestCleanupPendingClips_RemovesOnlyOnSuccess verifies a recovered record is deleted
// and counted as saved only when its save succeeds; a failed save keeps it for retry.
func TestCleanupPendingClips_RemovesOnlyOnSuccess(t *testing.T) {
	cases := []struct {
		name        string
		doFunc      func(ctx context.Context, cmd map[string]any) (map[string]any, error)
		wantSaved   int
		wantFailed  int
		wantRemoved bool
	}{
		{
			name: "success removes record",
			doFunc: func(_ context.Context, _ map[string]any) (map[string]any, error) {
				return map[string]any{"filename": "clip.mp4"}, nil
			},
			wantSaved:   1,
			wantRemoved: true,
		},
		{
			name: "transport error keeps record",
			doFunc: func(_ context.Context, _ map[string]any) (map[string]any, error) {
				return nil, context.DeadlineExceeded
			},
			wantFailed: 1,
		},
		{
			name: "per-store errors keep record",
			doFunc: func(_ context.Context, _ map[string]any) (map[string]any, error) {
				return map[string]any{"errors": map[string]any{"store0": "filename too long"}}, nil
			},
			wantFailed: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cam, dir := newCamStorageTestCoffee(t)
			cam.DoFunc = tc.doFunc
			path := writeStalePendingSave(t, c, dir)

			resp, err := c.cleanupPendingClips()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp["saved"] != tc.wantSaved || resp["failed"] != tc.wantFailed || resp["skipped"] != 0 {
				t.Errorf("resp = %v, want saved=%d failed=%d skipped=0", resp, tc.wantSaved, tc.wantFailed)
			}
			_, statErr := os.Stat(path)
			if removed := os.IsNotExist(statErr); removed != tc.wantRemoved {
				t.Fatalf("record removed = %v, want %v (stat err: %v)", removed, tc.wantRemoved, statErr)
			}
		})
	}
}

// TestIssueVideoSave_BoundedByTimeout ensures a wedged video store can't hang the save:
// the DoCommand context must carry a deadline no later than clipSaveTimeout.
func TestIssueVideoSave_BoundedByTimeout(t *testing.T) {
	c, cam, _ := newCamStorageTestCoffee(t)
	var deadline time.Time
	var hasDeadline bool
	cam.DoFunc = func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		deadline, hasDeadline = ctx.Deadline()
		return map[string]any{}, nil
	}

	start := time.Now()
	order := NewOrder("espresso", "Ada", "hi", "bye")
	c.issueVideoSave(order, start.Add(-clipLead), start.Add(clipTrail), nil, c.logger)

	if !hasDeadline {
		t.Fatal("save DoCommand context has no deadline")
	}
	if deadline.After(start.Add(clipSaveTimeout + time.Second)) {
		t.Errorf("deadline %s exceeds clipSaveTimeout (%s) from start", deadline.Sub(start), clipSaveTimeout)
	}
}

// The pending record outlives the order when a save can't run, so it holds only
// what recovery needs and is readable by the module's user alone.
func TestWritePendingSave_StoresNoCustomerDataAndIsPrivate(t *testing.T) {
	c, _, dir := newCamStorageTestCoffee(t)
	order := NewOrder("espresso", "Placeholder Name", "hello Placeholder Name", "bye")
	order.ModifiedCustomerName = "Placeholdr Name"
	order.CustomerEmail = "customer@example.com"
	videoFrom := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	c.writePendingSave(order, videoFrom)

	path := filepath.Join(dir, order.ID+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("pending record was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	for _, leaked := range []string{"Placeholder", "Placeholdr", "customer@example.com", "hello"} {
		if strings.Contains(string(data), leaked) {
			t.Errorf("record contains %q: %s", leaked, data)
		}
	}
	var ps pendingSave
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if ps.Order.ID != order.ID || ps.Order.Drink != "espresso" || !ps.VideoFrom.Equal(videoFrom) {
		t.Errorf("record = %+v, want order %s / espresso / %s", ps, order.ID, videoFrom)
	}
}

// Records holding a whole marshalled Order must still decode, so the cleanup
// sweep recovers clips for orders interrupted before an upgrade.
func TestCleanupPendingClips_RecoversFullOrderRecord(t *testing.T) {
	c, cam, dir := newCamStorageTestCoffee(t)
	var tags []string
	cam.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
		tags, _ = cmd["tags"].([]string)
		return map[string]any{}, nil
	}

	order := NewOrder("lungo", "Placeholder Name", "hi", "bye")
	order.CustomerEmail = "customer@example.com"
	fullRecord, err := json.Marshal(struct {
		Order     Order     `json:"order"`
		VideoFrom time.Time `json:"video_from"`
	}{order, time.Now().UTC().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("marshal full-order record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, order.ID+".json"), fullRecord, 0o600); err != nil {
		t.Fatalf("write full-order record: %v", err)
	}

	resp, err := c.cleanupPendingClips()
	if err != nil {
		t.Fatalf("cleanupPendingClips error: %v", err)
	}
	if resp["saved"] != 1 {
		t.Errorf("saved = %v, want 1 (resp %v)", resp["saved"], resp)
	}
	if len(tags) != 1 || tags[0] != order.ID {
		t.Errorf("save tags = %v, want [%s]", tags, order.ID)
	}
}
