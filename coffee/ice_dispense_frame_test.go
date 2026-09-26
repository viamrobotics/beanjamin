package coffee

import (
	"context"
	"path/filepath"
	"testing"

	"beanjamin/coffee/order"
)

// TestIceFrameTagDir: the tag= segments are what the Viam data manager turns
// into tags on sync, so they are the whole reason the frames are findable on
// the data page. A dispense outside an order has no order ID and must not leave
// an empty segment behind, which would tag the upload with "".
func TestIceFrameTagDir(t *testing.T) {
	for _, tc := range []struct {
		orderID, outcome string
		want             string
	}{
		{"oid", "stopped", filepath.Join("/base", "tag=oid", "tag=ice_dispense", "tag=ice_stopped")},
		{"", "timeout", filepath.Join("/base", "tag=ice_dispense", "tag=ice_timeout")},
	} {
		if got := iceFrameTagDir("/base", tc.orderID, tc.outcome); got != tc.want {
			t.Errorf("iceFrameTagDir(%q, %q) = %q, want %q", tc.orderID, tc.outcome, got, tc.want)
		}
	}
}

// TestSaveIceDispenseFrameIsOptInOffAnOrder: a hand-run action writes nothing
// without "annotate" on the DoCommand, however good the frame and however
// configured the directory. That is what keeps a tuning loop from burying the
// orders in the same directory.
func TestSaveIceDispenseFrameIsOptInOffAnOrder(t *testing.T) {
	dir := t.TempDir()
	s, _ := iceTestService(t, &Config{SaveMotionRequestsDir: dir})
	m := iceMeasurement{frame: loadFixture(t, "fill_40.jpg")}

	s.saveIceDispenseFrame(context.Background(), m, "stopped", "", 0)
	if wrote := savedFrames(t, dir); len(wrote) != 0 {
		t.Errorf("wrote %v without the annotate flag", wrote)
	}

	// With the flag: written, and written where iceFrameTagDir says. The path
	// segments are the contract with the data manager, which turns them into the
	// tags the data page filters on.
	s.saveIceDispenseFrame(withIceFrameSaving(context.Background()), m, "stopped", "", 0)
	wrote, err := filepath.Glob(filepath.Join(dir, "tag=ice_dispense", "tag=ice_stopped", "*_ice_dispense.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 1 {
		t.Errorf("wrote %d frames under the tag dirs with the annotate flag, want 1", len(wrote))
	}
}

// TestSaveIceDispenseFrameDuringAnOrder: an order's steps carry no DoCommand to
// set the flag on, and an order's dispense is the one nobody watched — so it
// writes on its own, under its order ID.
func TestSaveIceDispenseFrameDuringAnOrder(t *testing.T) {
	dir := t.TempDir()
	s, _ := iceTestService(t, &Config{SaveMotionRequestsDir: dir})
	s.queue.Enqueue(order.Order{ID: "oid"})
	if _, ok := s.queue.Start(); !ok {
		t.Fatal("Start found nothing to make")
	}

	s.saveIceDispenseFrame(context.Background(), iceMeasurement{frame: loadFixture(t, "fill_40.jpg")}, "stopped", "", 0)
	wrote, err := filepath.Glob(filepath.Join(dir, "tag=oid", "tag=ice_dispense", "tag=ice_stopped", "*_ice_dispense.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 1 {
		t.Errorf("an order's dispense wrote %d frames, want 1", len(wrote))
	}
}

// TestSaveIceDispenseFrameNeedsADirAndAFrame: an unset directory writes
// nothing, and a measurement with no frame behind it — which is every unit test
// of the loop — must not panic on the way past.
func TestSaveIceDispenseFrameNeedsADirAndAFrame(t *testing.T) {
	ctx := withIceFrameSaving(context.Background())
	s, _ := iceTestService(t, &Config{})
	s.saveIceDispenseFrame(ctx, iceMeasurement{frame: loadFixture(t, "fill_40.jpg")}, "stopped", "", 0)

	dir := t.TempDir()
	s.cfg.SaveMotionRequestsDir = dir
	s.saveIceDispenseFrame(ctx, iceMeasurement{}, "stopped", "", 0)
	if wrote := savedFrames(t, dir); len(wrote) != 0 {
		t.Errorf("a measurement with no frame wrote %v", wrote)
	}
}

func savedFrames(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "tag=*", "tag=*", "*.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}
