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
// trail waits before save so post-order seconds are still recorded (runs in a background goroutine).
// maxBrewDuration caps the recovery window when a pending save is replayed after a restart.
const (
	zooCamClipLead        = 15 * time.Second
	zooCamClipTrail       = 15 * time.Second
	zooCamMaxBrewDuration = 180 * time.Second
)

// formatClipTimestampUTC formats t for video-store save/fetch DoCommand (UTC, ...Z).
func formatClipTimestampUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02_15-04-05") + "Z"
}

// pendingSave is written to disk when an order starts and removed when it completes,
// so a restart can recover the video save for any order that was interrupted.
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
		s.logger.Warnf("zoo cam: failed to marshal pending save for order %s: %v", order.ID, err)
		return
	}
	if err := os.WriteFile(filepath.Join(s.pendingOrderClipsDir, order.ID+".json"), data, 0o644); err != nil {
		s.logger.Warnf("zoo cam: failed to write pending save for order %s: %v", order.ID, err)
	}
}

func (s *beanjaminCoffee) clearPendingSave(orderID string) {
	if s.pendingOrderClipsDir == "" {
		return
	}
	path := filepath.Join(s.pendingOrderClipsDir, orderID+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.logger.Warnf("zoo cam: failed to clear pending save for order %s: %v", orderID, err)
	}
}

// cleanupPendingClips attempts a video save for every remaining pending-clip record,
// then removes the record. Intended to be called via a Viam scheduled job to catch
// any orders interrupted before they could save (e.g. machine restart mid-brew).
func (s *beanjaminCoffee) cleanupPendingClips() (map[string]any, error) {
	if s.pendingOrderClipsDir == "" {
		return map[string]any{"skipped": "no pending_order_clips_dir configured"}, nil
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
			s.logger.Warnf("zoo cam: cleanup: failed to read %s: %v", entry.Name(), err)
			skipped++
			continue
		}
		var ps pendingSave
		if err := json.Unmarshal(data, &ps); err != nil {
			// Corrupt file won't get better — remove it.
			s.logger.Warnf("zoo cam: cleanup: corrupt pending clip %s removed without save: %v", entry.Name(), err)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				s.logger.Warnf("zoo cam: cleanup: failed to remove corrupt file %s: %v", entry.Name(), err)
			}
			skipped++
			continue
		}
		// Skip records that may still be in progress.
		if time.Now().UTC().Before(ps.VideoFrom.Add(zooCamMaxBrewDuration + zooCamClipLead)) {
			skipped++
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			s.logger.Warnf("zoo cam: cleanup: failed to remove %s: %v", entry.Name(), err)
			skipped++
			continue
		}
		s.logger.Infof("zoo cam: cleanup: attempting save for interrupted order %s (%s)", ps.Order.ID, ps.Order.CustomerName)
		clipFrom := ps.VideoFrom.Add(-zooCamClipLead)
		clipTo := ps.VideoFrom.Add(zooCamMaxBrewDuration + zooCamClipLead)
		if now := time.Now().UTC(); clipTo.After(now) {
			clipTo = now
		}
		s.issueVideoSave(ps.Order, clipFrom, clipTo, fmt.Errorf("interrupted: recovered by scheduled cleanup"))
		saved++
	}
	return map[string]any{"saved": saved, "skipped": skipped}, nil
}

// saveOrderVideoAsync launches a background goroutine that waits for the post-order trail,
// then asks the optional zoo cam (viam:video:storage) to slice [from, now] and queue cloud upload.
// See https://github.com/viam-modules/video-store — uses async "save" so the in-progress segment can finish.
// execErr is nil when the order finished the brew sequence; non-nil records failure (including panic) in metadata.
func (s *beanjaminCoffee) saveOrderVideoAsync(order Order, from time.Time, execErr error) {
	if s.zooCam == nil {
		return
	}
	clipFrom := from.Add(-zooCamClipLead)
	go func() {
		// Post-roll is not tied to service/caller cancellation—we still want to queue the clip.
		time.Sleep(zooCamClipTrail)
		s.issueVideoSave(order, clipFrom, time.Now().UTC(), execErr)
	}()
}

func (s *beanjaminCoffee) issueVideoSave(order Order, clipFrom, clipTo time.Time, execErr error) {
	metaObj := map[string]string{
		"order_id":       order.ID,
		"customer_name":  order.CustomerName,
		"drink":          order.Drink,
		"coffee_service": s.name.ShortName(),
		"order_status":   "ok",
	}
	if execErr != nil {
		metaObj["order_status"] = "failed"
		metaObj["error"] = execErr.Error()
	}
	meta, err := json.Marshal(metaObj)
	if err != nil {
		s.logger.Warnf("zoo cam: skip save for order %s: metadata: %v", order.ID, err)
		return
	}
	cmd := map[string]any{
		"command":  "save",
		"from":     formatClipTimestampUTC(clipFrom),
		"to":       formatClipTimestampUTC(clipTo),
		"metadata": string(meta),
		"tags":     []string{order.ID},
		"async":    true,
	}
	resp, err := s.zooCam.DoCommand(context.Background(), cmd)
	if err != nil {
		s.logger.Warnf("zoo cam: save failed for order %s: %v", order.ID, err)
		return
	}
	s.logger.Infof("zoo cam: queued upload for order %s (response: %+v)", order.ID, resp)
}
