package beanjamin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fixed clip padding around each order (not configurable). Pre-roll is limited by the camera ring buffer;
// trail extends the clip past the order so post-order seconds are still captured.
// maxBrewDuration caps the clip window when a pending save is replayed after an interruption.
const (
	clipLead        = 15 * time.Second
	clipTrail       = 15 * time.Second
	maxBrewDuration = 180 * time.Second

	// The video-store writes video in fixed-length segments and a synchronous
	// ("async":false) save can only slice segments that have already rolled over and
	// closed on disk — so the clip's `to` must sit at least one segment + a flush margin
	// in the past before we issue the save, or the trailing footage won't be there yet.
	//
	// segmentDuration mirrors the module's hardcoded `segmentSeconds` (30s). It is NOT
	// operator-configurable; if the module changes it, update this. Defined at:
	// https://github.com/viam-modules/video-store/blob/main/videostore/videostore.go#L33
	segmentDuration = 30 * time.Second
	clipFlushMargin = 5 * time.Second
)

// formatClipTimestampUTC formats t for video-store save/fetch DoCommand (UTC, ...Z).
func formatClipTimestampUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02_15-04-05") + "Z"
}

// pendingSave is written to disk when an order starts and removed when it completes,
// so a scheduled job can recover the video save for any order that was interrupted.
type pendingSave struct {
	Order     Order     `json:"order"`
	VideoFrom time.Time `json:"video_from"`
}

func (s *beanjaminCoffee) writePendingSave(order Order, videoFrom time.Time) {
	if s.pendingOrderClipsDir == "" {
		return
	}
	data, err := json.Marshal(pendingSave{Order: order, VideoFrom: videoFrom})
	if err != nil {
		s.logger.Warnf("cam storage: failed to marshal pending save for order %s: %v", order.ID, err)
		return
	}
	if err := os.WriteFile(filepath.Join(s.pendingOrderClipsDir, order.ID+".json"), data, 0o644); err != nil {
		s.logger.Warnf("cam storage: failed to write pending save for order %s: %v", order.ID, err)
	}
}

func (s *beanjaminCoffee) clearPendingSave(orderID string) {
	if s.pendingOrderClipsDir == "" {
		return
	}
	path := filepath.Join(s.pendingOrderClipsDir, orderID+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.logger.Warnf("cam storage: failed to clear pending save for order %s: %v", orderID, err)
	}
}

// cleanupPendingClips attempts a video save for every remaining pending-clip record,
// then removes the record. Intended to be called via a Viam scheduled job to catch
// any orders interrupted before they could save (e.g. machine restart mid-brew).
func (s *beanjaminCoffee) cleanupPendingClips() (map[string]any, error) {
	s.logger.Infof("cam storage: cleanup job starting")
	if s.pendingOrderClipsDir == "" {
		s.logger.Infof("cam storage: cleanup job nothing to do — no data_dir configured")
		return map[string]any{"saved": 0, "skipped": 0}, nil
	}
	entries, err := os.ReadDir(s.pendingOrderClipsDir)
	if err != nil {
		return nil, fmt.Errorf("read pending clips dir: %w", err)
	}
	var saved, skipped int
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.pendingOrderClipsDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			s.logger.Warnf("cam storage: cleanup: failed to read %s: %v", entry.Name(), err)
			skipped++
			continue
		}
		var ps pendingSave
		if err := json.Unmarshal(data, &ps); err != nil {
			// Corrupt file won't get better — remove it.
			s.logger.Warnf("cam storage: cleanup: corrupt pending clip %s removed without save: %v", entry.Name(), err)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				s.logger.Warnf("cam storage: cleanup: failed to remove corrupt file %s: %v", entry.Name(), err)
			}
			skipped++
			continue
		}
		// Skip records that may still be in progress, or whose trailing segment
		// hasn't closed yet (a sync slice can only read closed segments).
		if time.Now().UTC().Before(ps.VideoFrom.Add(maxBrewDuration + clipLead + segmentDuration + clipFlushMargin)) {
			skipped++
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			s.logger.Warnf("cam storage: cleanup: failed to remove %s: %v", entry.Name(), err)
			skipped++
			continue
		}
		s.logger.Infof("cam storage: cleanup: attempting save for interrupted order %s (%s)", ps.Order.ID, ps.Order.CustomerName)
		clipFrom := ps.VideoFrom.Add(-clipLead)
		// The skip gate above guarantees this is already ≥ segmentDuration in the past,
		// so it lands in closed segments and never exceeds now.
		clipTo := ps.VideoFrom.Add(maxBrewDuration + clipLead)
		s.issueVideoSave(ps.Order, clipFrom, clipTo, fmt.Errorf("interrupted: recovered by scheduled cleanup"))
		saved++
	}
	return map[string]any{"saved": saved, "skipped": skipped}, nil
}

// saveOrderVideoAsync launches a background goroutine that waits for the trailing segment
// to close, then asks the cam storage multiplexer to slice the order's [from, to] window.
// See https://github.com/viam-modules/video-store. The clip window is fixed at call time
// (≈ order end + clipTrail); we issue a synchronous save once that window is safely inside
// closed segments, so slice failures (e.g. an over-long filename) surface instead of being
// silently dropped as they were with async saves.
// execErr is nil when the order finished the brew sequence; non-nil records failure (including panic) in metadata.
func (s *beanjaminCoffee) saveOrderVideoAsync(order Order, from time.Time, execErr error) {
	if s.camStorage == nil {
		s.logger.Infof("cam storage: skip save for order %s — no cam_storage_mux_name configured", order.ID)
		// No saver will ever run, so the pending record is unrecoverable noise—drop it.
		s.clearPendingSave(order.ID)
		return
	}
	clipFrom := from.Add(-clipLead)
	clipTo := time.Now().UTC().Add(clipTrail) // fixed end ≈ order end + post-roll
	s.logger.Infof("cam storage: scheduling save for order %s — [%s, %s], waiting for trailing segment to close",
		order.ID, formatClipTimestampUTC(clipFrom), formatClipTimestampUTC(clipTo))
	go func() {
		// Not tied to service/caller cancellation—we still want the clip. The segment
		// containing clipTo closes at most segmentDuration after clipTo; wait that out
		// (+margin) so the sync slice reads only closed segments.
		if wait := time.Until(clipTo.Add(segmentDuration + clipFlushMargin)); wait > 0 {
			time.Sleep(wait)
		}
		s.saveOrderVideoAndClear(order, clipFrom, clipTo, execErr)
	}()
}

// saveOrderVideoAndClear issues the save and clears the pending-clip record only once
// the save actually succeeds. If the save fails—or the process dies before this runs—the
// record survives so cleanupPendingClips can recover the clip on the next scheduled sweep.
func (s *beanjaminCoffee) saveOrderVideoAndClear(order Order, clipFrom, clipTo time.Time, execErr error) {
	if s.issueVideoSave(order, clipFrom, clipTo, execErr) {
		s.clearPendingSave(order.ID)
	}
}

// issueVideoSave performs the synchronous save and reports whether it succeeded.
// Callers use the result to decide whether to clear the pending-clip record: a
// failed save keeps the record so cleanupPendingClips can retry it later.
func (s *beanjaminCoffee) issueVideoSave(order Order, clipFrom, clipTo time.Time, execErr error) bool {
	// The video-store bakes this metadata into the clip filename and nothing more (it's
	// not queryable cloud metadata — clips are linked to orders via the `tags` field, and
	// failure detail lives queryably on the order sensor). So keep it minimal: just enough
	// to eyeball a clip's order and outcome in a file listing. Unbounded values here overflow
	// the filesystem filename limit and make the save fail.
	status := "ok"
	if execErr != nil {
		status = "failed"
	}
	meta, err := json.Marshal(map[string]string{
		"order_id":     order.ID,
		"order_status": status,
	})
	if err != nil {
		s.logger.Warnf("cam storage: skip save for order %s: metadata: %v", order.ID, err)
		return false
	}
	cmd := map[string]any{
		"command":  "save",
		"from":     formatClipTimestampUTC(clipFrom),
		"to":       formatClipTimestampUTC(clipTo),
		"metadata": string(meta),
		"tags":     []string{order.ID},
		// Synchronous: the save blocks on producing the local clip and returns a slice-level
		// error, so failures (over-long filename, missing segments, disk) are reported here
		// instead of being silently lost in a background worker.
		"async": false,
	}
	s.logger.Infof("cam storage: issuing save for order %s — from=%s to=%s",
		order.ID, formatClipTimestampUTC(clipFrom), formatClipTimestampUTC(clipTo))
	resp, err := s.camStorage.DoCommand(context.Background(), cmd)
	if err != nil {
		s.logger.Errorf("cam storage: save failed for order %s: %v", order.ID, err)
		return false
	}
	if errs, ok := resp["errors"].(map[string]any); ok && len(errs) > 0 {
		for store, msg := range errs {
			s.logger.Errorf("cam storage: save failed for order %s on %q: %v", order.ID, store, msg)
		}
		return false
	}
	s.logger.Infof("cam storage: saved clip for order %s (response: %+v)", order.ID, resp)
	return true
}
