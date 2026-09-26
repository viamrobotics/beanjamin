// Package clips saves each order's camera clip through the video-store
// multiplexer and keeps an on-disk pending-clip record per order so a scheduled
// sweep can recover the clip for any order interrupted before its save ran.
package clips

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"beanjamin/coffee/order"

	"go.viam.com/rdk/logging"
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

	// clipSaveTimeout bounds a single synchronous save so a wedged video store fails the
	// save (keeping its pending record for retry) instead of hanging the cleanup job or
	// leaking the detached post-order save goroutine.
	clipSaveTimeout = 60 * time.Second
)

// DoCommander is the one method Saver needs from the video-store multiplexer.
// Keeping it this small lets tests pass in a fake.
type DoCommander interface {
	DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error)
}

// Saver requests order clips from the video-store multiplexer and manages the
// pending-clip records that make interrupted saves recoverable. Configured,
// WritePendingSave, and SaveOrderVideoAsync treat a nil *Saver as one with no
// multiplexer and no records directory; CleanupPendingClips needs a real one.
type Saver struct {
	mux        DoCommander // nil if cam_storage_mux_name unset
	recordsDir string      // "" when no data_dir is configured
	logger     logging.Logger
}

// NewSaver builds a Saver over mux (nil when cam_storage_mux_name is unset),
// writing pending-clip records to recordsDir ("" disables them). logger is the
// service logger the cleanup sweep logs through.
func NewSaver(mux DoCommander, recordsDir string, logger logging.Logger) *Saver {
	return &Saver{mux: mux, recordsDir: recordsDir, logger: logger}
}

// Configured reports whether a video-store multiplexer is wired in, i.e.
// whether order clips are requested at all.
func (s *Saver) Configured() bool {
	return s != nil && s.mux != nil
}

func (s *Saver) dir() string {
	if s == nil {
		return ""
	}
	return s.recordsDir
}

// formatClipTimestampUTC formats t for video-store save/fetch DoCommand (UTC, ...Z).
func formatClipTimestampUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02_15-04-05") + "Z"
}

// pendingSave is written to disk when an order starts and removed when it completes,
// so a scheduled job can recover the video save for any order that was interrupted.
type pendingSave struct {
	Order     pendingClipOrder `json:"order"`
	VideoFrom time.Time        `json:"video_from"`
}

// pendingClipOrder is the slice of an Order the recovery path needs. The record
// sits on disk for as long as a save stays unrecovered, so it deliberately
// carries no customer name, email, or greeting. The json keys match Order's,
// so records holding a full marshalled Order still decode.
type pendingClipOrder struct {
	ID    string `json:"id"`
	Drink string `json:"drink"`
}

func (p pendingClipOrder) order() order.Order {
	return order.Order{ID: p.ID, Drink: p.Drink}
}

// WritePendingSave records that order's clip window opens at videoFrom, so
// CleanupPendingClips can recover the clip if the order never reaches its save.
// It is a no-op when no records directory is configured.
func (s *Saver) WritePendingSave(order order.Order, videoFrom time.Time, logger logging.Logger) {
	if s.dir() == "" {
		return
	}
	data, err := json.Marshal(pendingSave{
		Order:     pendingClipOrder{ID: order.ID, Drink: order.Drink},
		VideoFrom: videoFrom,
	})
	if err != nil {
		logger.Warnf("cam storage: failed to marshal pending save: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(s.recordsDir, order.ID+".json"), data, 0o600); err != nil {
		logger.Warnf("cam storage: failed to write pending save: %v", err)
	}
}

func (s *Saver) clearPendingSave(orderID string, logger logging.Logger) {
	if s.dir() == "" {
		return
	}
	path := filepath.Join(s.recordsDir, orderID+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logger.Warnf("cam storage: failed to clear pending save: %v", err)
	}
}

// CleanupPendingClips attempts a video save for every remaining pending-clip record,
// removing each record only once its save succeeds. Intended to be called via a Viam
// scheduled job to catch any orders interrupted before they could save (e.g. machine
// restart mid-brew).
func (s *Saver) CleanupPendingClips() (map[string]any, error) {
	s.logger.Infof("cam storage: cleanup job starting")
	if s.recordsDir == "" {
		s.logger.Infof("cam storage: cleanup job nothing to do — no data_dir configured")
		return map[string]any{"saved": 0, "failed": 0, "skipped": 0}, nil
	}
	entries, err := os.ReadDir(s.recordsDir)
	if err != nil {
		return nil, fmt.Errorf("read pending clips dir: %w", err)
	}
	var records []os.DirEntry
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			records = append(records, entry)
		}
	}
	if s.mux == nil {
		if len(records) == 0 {
			return map[string]any{"saved": 0, "failed": 0, "skipped": 0}, nil
		}
		// Records can outlive a cam_storage_mux_name removal (or a process that died
		// mid-order). Leave them on disk: restoring the mux lets the next sweep recover
		// the clips, and deleting them here would lose footage irrecoverably.
		return nil, fmt.Errorf("cam storage: %d pending clip record(s) in %s left in place: no cam_storage_mux_name configured",
			len(records), s.recordsDir)
	}
	var saved, failed, skipped int
	for _, entry := range records {
		path := filepath.Join(s.recordsDir, entry.Name())
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
		// Recovery for an interrupted order — tag with its ID. The cleanup job
		// runs off any order goroutine, so build the tagged logger from the
		// record rather than from an in-flight order's logger.
		orderLogger := s.logger.WithFields("order_id", ps.Order.ID)
		orderLogger.Infof("cam storage: cleanup: attempting save for interrupted %s order", ps.Order.Drink)
		clipFrom := ps.VideoFrom.Add(-clipLead)
		// The skip gate above guarantees this is already ≥ segmentDuration in the past,
		// so it lands in closed segments and never exceeds now.
		clipTo := ps.VideoFrom.Add(maxBrewDuration + clipLead)
		if !s.issueVideoSave(ps.Order.order(), clipFrom, clipTo, fmt.Errorf("interrupted: recovered by scheduled cleanup"), orderLogger) {
			// Keep the record so the next sweep retries the save.
			failed++
			continue
		}
		saved++
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			orderLogger.Warnf("cam storage: cleanup: clip saved but failed to remove %s: %v", entry.Name(), err)
		}
	}
	return map[string]any{"saved": saved, "failed": failed, "skipped": skipped}, nil
}

// SaveOrderVideoAsync launches a background goroutine that waits for the trailing segment
// to close, then asks the cam storage multiplexer to slice the order's [from, to] window.
// See https://github.com/viam-modules/video-store. The clip window is fixed at call time
// (≈ order end + clipTrail); we issue a synchronous save once that window is safely inside
// closed segments, so slice failures (e.g. an over-long filename) surface instead of being
// silently dropped as they were with async saves.
// execErr is nil when the order finished the brew sequence; non-nil records failure (including panic) in metadata.
// logger is the order-scoped logger, captured by the detached goroutine because
// the save outlives the order.
func (s *Saver) SaveOrderVideoAsync(order order.Order, from time.Time, execErr error, logger logging.Logger) {
	if !s.Configured() {
		logger.Infof("cam storage: skip save — no cam_storage_mux_name configured")
		// No saver will ever run, so the pending record is unrecoverable noise—drop it.
		s.clearPendingSave(order.ID, logger)
		return
	}
	clipFrom := from.Add(-clipLead)
	clipTo := time.Now().UTC().Add(clipTrail) // fixed end ≈ order end + post-roll
	logger.Infof("cam storage: scheduling save — [%s, %s], waiting for trailing segment to close",
		formatClipTimestampUTC(clipFrom), formatClipTimestampUTC(clipTo))
	go func() {
		// Not tied to service/caller cancellation—we still want the clip. The segment
		// containing clipTo closes at most segmentDuration after clipTo; wait that out
		// (+margin) so the sync slice reads only closed segments.
		if wait := time.Until(clipTo.Add(segmentDuration + clipFlushMargin)); wait > 0 {
			time.Sleep(wait)
		}
		s.saveOrderVideoAndClear(order, clipFrom, clipTo, execErr, logger)
	}()
}

// saveOrderVideoAndClear issues the save and clears the pending-clip record only once
// the save actually succeeds. If the save fails—or the process dies before this runs—the
// record survives so CleanupPendingClips can recover the clip on the next scheduled sweep.
func (s *Saver) saveOrderVideoAndClear(order order.Order, clipFrom, clipTo time.Time, execErr error, logger logging.Logger) {
	if s.issueVideoSave(order, clipFrom, clipTo, execErr, logger) {
		s.clearPendingSave(order.ID, logger)
	}
}

// issueVideoSave performs the synchronous save and reports whether it succeeded.
// Callers use the result to decide whether to clear the pending-clip record: a
// failed save keeps the record so CleanupPendingClips can retry it later.
func (s *Saver) issueVideoSave(order order.Order, clipFrom, clipTo time.Time, execErr error, logger logging.Logger) bool {
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
		logger.Warnf("cam storage: skip save: metadata: %v", err)
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
	logger.Infof("cam storage: issuing save — from=%s to=%s",
		formatClipTimestampUTC(clipFrom), formatClipTimestampUTC(clipTo))
	ctx, cancel := context.WithTimeout(context.Background(), clipSaveTimeout)
	defer cancel()
	resp, err := s.mux.DoCommand(ctx, cmd)
	if err != nil {
		logger.Errorf("cam storage: save failed: %v", err)
		return false
	}
	if errs, ok := resp["errors"].(map[string]any); ok && len(errs) > 0 {
		for store, msg := range errs {
			logger.Errorf("cam storage: save failed on %q: %v", store, msg)
		}
		return false
	}
	logger.Infof("cam storage: saved clip (response: %+v)", resp)
	return true
}
