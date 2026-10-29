package ipadcamera

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/jpeg"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

func newTestCamera(t *testing.T, maxAgeMs int) *ipadCamera {
	t.Helper()
	conf := resource.Config{Name: "ipad-cam", ConvertedAttributes: &Config{MaxFrameAgeMs: maxAgeMs}}
	cam, err := newIpadCamera(context.Background(), nil, conf, logging.NewTestLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	return cam.(*ipadCamera)
}

func jpegB64(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestImagesBeforeAnyFrame(t *testing.T) {
	cam := newTestCamera(t, 0)
	if _, _, err := cam.Images(context.Background(), nil, nil); err == nil {
		t.Fatal("expected an error before any frame is pushed")
	}
}

func TestPushThenImages(t *testing.T) {
	cam := newTestCamera(t, 0)
	ctx := context.Background()
	if _, err := cam.DoCommand(ctx, map[string]any{"push_frame": jpegB64(t, 64, 48)}); err != nil {
		t.Fatal(err)
	}
	imgs, md, err := cam.Images(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].SourceName != "ipad" || imgs[0].MimeType() != "image/jpeg" {
		t.Fatalf("unexpected images: %+v", imgs)
	}
	if md.CapturedAt.IsZero() {
		t.Fatal("expected CapturedAt to be set")
	}
	st, _ := cam.Status(ctx)
	if st["width"] != float64(64) || st["height"] != float64(48) || st["frames_received"] != float64(1) {
		t.Fatalf("unexpected status: %v", st)
	}
}

func TestDataURLPrefixTolerated(t *testing.T) {
	cam := newTestCamera(t, 0)
	_, err := cam.DoCommand(context.Background(), map[string]any{"push_frame": "data:image/jpeg;base64," + jpegB64(t, 8, 8)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStaleFrameRefused(t *testing.T) {
	cam := newTestCamera(t, 20)
	ctx := context.Background()
	if _, err := cam.DoCommand(ctx, map[string]any{"push_frame": jpegB64(t, 8, 8)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, _, err := cam.Images(ctx, nil, nil); err == nil {
		t.Fatal("expected a stale-frame error")
	}
}

func TestBadFramesRejected(t *testing.T) {
	cam := newTestCamera(t, 0)
	ctx := context.Background()
	for name, v := range map[string]any{
		"not a string": 42,
		"bad base64":   "!!!",
		"not an image": base64.StdEncoding.EncodeToString([]byte("hello")),
	} {
		if _, err := cam.DoCommand(ctx, map[string]any{"push_frame": v}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
