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

// clockedCamera builds a camera with the given config and a clock the test
// advances by hand.
func clockedCamera(t *testing.T, cfg *Config) (*ipadCamera, *time.Time) {
	t.Helper()
	conf := resource.Config{Name: "ipad-cam", ConvertedAttributes: cfg}
	cam, err := newIpadCamera(context.Background(), nil, conf, logging.NewTestLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	c := cam.(*ipadCamera)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	return c, &now
}

func push(c *ipadCamera, frame, id, token string) error {
	cmd := map[string]any{"push_frame": frame, "source_id": id}
	if token != "" {
		cmd["token"] = token
	}
	_, err := c.DoCommand(context.Background(), cmd)
	return err
}

func TestTokenRequiredWhenConfigured(t *testing.T) {
	cam, _ := clockedCamera(t, &Config{SourceToken: "s3cret"})
	frame := jpegB64(t, 8, 8)
	for _, token := range []string{"", "wrong", "s3cre"} {
		if err := push(cam, frame, "a", token); err == nil || err.Error() != errBadToken {
			t.Fatalf("token %q: want %q, got %v", token, errBadToken, err)
		}
	}
	if _, _, err := cam.Images(context.Background(), nil, nil); err == nil {
		t.Fatal("a rejected push must not store a frame")
	}
	if err := push(cam, frame, "a", "s3cret"); err != nil {
		t.Fatalf("correct token refused: %v", err)
	}
}

func TestNoTokenConfiguredAcceptsAnything(t *testing.T) {
	cam, _ := clockedCamera(t, &Config{})
	if err := push(cam, jpegB64(t, 8, 8), "a", "ignored"); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseBlocksSecondSource(t *testing.T) {
	cam, now := clockedCamera(t, &Config{ClaimTimeoutMs: 10_000})
	frame := jpegB64(t, 8, 8)
	if err := push(cam, frame, "ipad", ""); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(9 * time.Second)
	if err := push(cam, frame, "laptop", ""); err == nil || err.Error() != errInUse {
		t.Fatalf("want %q, got %v", errInUse, err)
	}
	st, _ := cam.Status(context.Background())
	if st["frames_received"] != float64(1) {
		t.Fatalf("a refused push must not count: %v", st)
	}
	// The holder pushing again renews the lease, so the other source stays out.
	if err := push(cam, frame, "ipad", ""); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(9 * time.Second)
	if err := push(cam, frame, "laptop", ""); err == nil {
		t.Fatal("holder's push should have renewed the lease")
	}
}

func TestLeaseExpires(t *testing.T) {
	cam, now := clockedCamera(t, &Config{ClaimTimeoutMs: 10_000})
	frame := jpegB64(t, 8, 8)
	if err := push(cam, frame, "ipad", ""); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Second)
	if err := push(cam, frame, "laptop", ""); err != nil {
		t.Fatalf("lease should have expired: %v", err)
	}
	// The old holder is now the outsider.
	if err := push(cam, frame, "ipad", ""); err == nil {
		t.Fatal("the new holder's lease should block the old one")
	}
}

func TestSameSourceReclaimsAfterReload(t *testing.T) {
	cam, now := clockedCamera(t, &Config{})
	frame := jpegB64(t, 8, 8)
	if err := push(cam, frame, "ipad", ""); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	if err := push(cam, frame, "ipad", ""); err != nil {
		t.Fatalf("a reloaded page with the same source_id must not wait out the lease: %v", err)
	}
}

func TestClaimTakesLeaseWithoutAFrame(t *testing.T) {
	cam, now := clockedCamera(t, &Config{})
	ctx := context.Background()
	if _, err := cam.DoCommand(ctx, map[string]any{"claim": true, "source_id": "ipad"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cam.DoCommand(ctx, map[string]any{"claim": true, "source_id": "laptop"}); err == nil {
		t.Fatal("claim should be refused while another source holds the lease")
	}
	if _, _, err := cam.Images(ctx, nil, nil); err == nil {
		t.Fatal("claim must not produce a frame")
	}
	*now = now.Add(11 * time.Second)
	if _, err := cam.DoCommand(ctx, map[string]any{"claim": true, "source_id": "laptop"}); err != nil {
		t.Fatalf("claim after expiry: %v", err)
	}
}

func TestClaimChecksToken(t *testing.T) {
	cam, _ := clockedCamera(t, &Config{SourceToken: "s3cret"})
	if _, err := cam.DoCommand(context.Background(), map[string]any{"claim": true, "source_id": "a", "token": "nope"}); err == nil || err.Error() != errBadToken {
		t.Fatalf("want %q, got %v", errBadToken, err)
	}
}

func TestValidate(t *testing.T) {
	for name, cfg := range map[string]Config{
		"negative max age":       {MaxFrameAgeMs: -1},
		"negative claim timeout": {ClaimTimeoutMs: -1},
		"token 1":                {SourceToken: "1"},
	} {
		if _, _, err := cfg.Validate("p"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	ok := Config{SourceToken: "abc", ClaimTimeoutMs: 5000}
	if _, _, err := ok.Validate("p"); err != nil {
		t.Errorf("valid config refused: %v", err)
	}
}
