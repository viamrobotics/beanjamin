package beanjamin

import (
	"context"
	"encoding/json"
	"time"
)

// formatClipTimestampUTC formats t for video-store save/fetch DoCommand (UTC, ...Z).
func formatClipTimestampUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02_15-04-05") + "Z"
}

// saveOrderVideo asks the optional zoo cam (viam:video:storage) to slice [from, now] and queue cloud upload.
// See https://github.com/viam-modules/video-store — uses async "save" so the in-progress segment can finish.
// execErr is nil when the order finished the brew sequence; non-nil records failure (including panic) in metadata.
func (s *beanjaminCoffee) saveOrderVideo(ctx context.Context, order Order, from time.Time, execErr error) {
	if s.zooCam == nil {
		return
	}
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
	to := time.Now().UTC()
	cmd := map[string]interface{}{
		"command":  "save",
		"from":     formatClipTimestampUTC(from),
		"to":       formatClipTimestampUTC(to),
		"metadata": string(meta),
		"tags":     []string{order.ID},
		"async":    true,
	}
	resp, err := s.zooCam.DoCommand(ctx, cmd)
	if err != nil {
		s.logger.Warnf("zoo cam: save failed for order %s: %v", order.ID, err)
		return
	}
	s.logger.Infof("zoo cam: queued upload for order %s (response: %+v)", order.ID, resp)
}
