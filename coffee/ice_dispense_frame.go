package coffee

// Saving the frame a dispense ended on.
//
// The shadow's log lines say what the two methods decided; they cannot say
// whether the stop row still matches how this glass was gripped. That question
// is only answerable from the picture, and the seating changes grab to grab.
//
// It rides the same path as the motion-plan requests: written into
// save_motion_requests_dir under tag= segments, which the Viam data manager
// reads on sync to tag the uploaded file, so the frames are filterable on the
// data page by order and by how the dispense ended.
//
// Every dispense inside an order leaves a frame; a hand-run action leaves one
// only when the DoCommand asked with "annotate": true. An order's dispense is
// the one nobody was standing there to watch, so its frame is the one worth
// keeping. A hand-run action is usually a tuning loop fired as fast as it can
// be typed, and stays opt-in.

import (
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// annotateFramesKey carries the per-request "annotate" flag from DoCommand down
// to the measurement. Threading a debugging toggle through every step signature
// to reach it would cost more than it is worth, and request-scoped and read-only
// is what a context value is for.
type annotateFramesKey struct{}

// withIceFrameSaving marks ctx as a hand-run action that should leave annotated
// frames behind.
func withIceFrameSaving(ctx context.Context) context.Context {
	return context.WithValue(ctx, annotateFramesKey{}, true)
}

// iceFrameSavingOn reports whether this call asked for frames.
func iceFrameSavingOn(ctx context.Context) bool {
	on, _ := ctx.Value(annotateFramesKey{}).(bool)
	return on
}

// iceFramesWanted reports whether this measurement should leave a frame: every
// one an order takes, and a hand-run action's only when it asked.
func (s *beanjaminCoffee) iceFramesWanted(ctx context.Context) bool {
	return iceFrameSavingOn(ctx) || s.queue.CurrentID() != ""
}

// iceFrameJPEGQuality keeps a saved frame near 150 KB rather than the ~900 KB a
// PNG of the same annotation costs. The source is a JPEG off the camera, so a
// second lossy pass costs nothing the drawing needs — the annotation is flat
// color over a photo, not fine detail.
const iceFrameJPEGQuality = 85

// saveIceDispenseFrame annotates and writes one frame. outcome names how the
// run ended and becomes a path segment, so it is a slug and not prose; note
// says anything the picture cannot be trusted on by itself, such as a frame
// that predates the failure it documents.
//
// A no-op unless save_motion_requests_dir is set and this measurement wants a
// frame, and when there is no frame at all — the loop may never have got one, and
// the loop's own tests inject readings without one. Failures are logged, never
// returned: this is debugging output and must not cost a drink that is already
// poured.
func (s *beanjaminCoffee) saveIceDispenseFrame(ctx context.Context, m iceMeasurement, outcome, note string, elapsed time.Duration) {
	logger := s.activeOrderLogger()
	dir := s.cfg.SaveMotionRequestsDir
	if dir == "" || m.frame == nil || !s.iceFramesWanted(ctx) {
		return
	}
	caption := fmt.Sprintf("%s after %s — contrast %s, shadow %s",
		strings.ReplaceAll(outcome, "_", " "), elapsed.Round(time.Millisecond),
		describeReading(m.contrast), describeShadow(m))
	if note != "" {
		caption += " — " + note
	}
	annotated, err := annotateIceFrame(s.iceBand(), m, caption)
	if err != nil {
		logger.Warnf("dispensing ice: annotating the final frame: %v", err)
		return
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, annotated, &jpeg.Options{Quality: iceFrameJPEGQuality}); err != nil {
		logger.Warnf("dispensing ice: encoding the final frame: %v", err)
		return
	}

	tagDir := iceFrameTagDir(dir, s.queue.CurrentID(), outcome)
	if err := os.MkdirAll(tagDir, 0o755); err != nil {
		logger.Warnf("dispensing ice: saving the final frame: create dir: %v", err)
		return
	}
	name := filepath.Join(tagDir, fmt.Sprintf("%s_ice_dispense.jpg", time.Now().Format("20060102_150405.000")))
	if err := os.WriteFile(name, buf.Bytes(), 0o600); err != nil {
		logger.Warnf("dispensing ice: saving the final frame: %v", err)
		return
	}
	logger.Infof("dispensing ice: saved the annotated final frame (%d bytes) to %s", buf.Len(), name)
}

// iceFrameTagDir nests the frame under the same tag= segments the plan requests
// use, so the data manager tags it on sync and the data page can filter ice
// frames by order and by outcome. An order ID is absent for a hand-run action,
// and is then simply skipped.
func iceFrameTagDir(baseDir, orderID, outcome string) string {
	parts := []string{baseDir}
	for _, tag := range []string{orderID, "ice_dispense", "ice_" + outcome} {
		if tag == "" {
			continue
		}
		parts = append(parts, "tag="+tag)
	}
	return filepath.Join(parts...)
}

// describeReading renders one method's reading for the caption. Which side of
// the stop row it falls on is left to the picture, where the stop row is drawn
// a few pixels away and says it better than a clause would.
func describeReading(r iceReading) string {
	if !r.found {
		return "no surface"
	}
	return fmt.Sprintf("row %d", r.row)
}

func describeShadow(m iceMeasurement) string {
	if !m.shadow {
		return "off"
	}
	return describeReading(m.brightness)
}
