// Package ipadcamera registers a viam:beanjamin:ipad-camera model that
// implements the rdk:component:camera API for a camera that lives in a browser.
//
// The web app opens the iPad's camera with getUserMedia and pushes JPEG frames
// to this component through DoCommand ({"push_frame": "<base64 jpeg>"}). The
// component keeps only the latest frame and serves it from Images. Frames older
// than max_frame_age_ms are refused, so a backgrounded or locked iPad produces
// an error instead of a stale image.
//
// Two optional guards keep stray browsers from feeding the camera: a shared
// source_token that every push must carry, and a lease that lets only one
// source_id push at a time.
package ipadcamera

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registers the JPEG decoder used by image.DecodeConfig
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/spatialmath"
	rdkutils "go.viam.com/rdk/utils"
)

var Model = resource.NewModel("viam", "beanjamin", "ipad-camera")

const (
	sourceName = "ipad"

	defaultMaxFrameAge = 5 * time.Second
	// maxFrameBytes bounds a single pushed frame. A 640x480 JPEG is tens of KB,
	// so this only rejects a client that is sending something it shouldn't.
	maxFrameBytes = 8 << 20

	defaultClaimTimeout = 10 * time.Second

	// errInUse and errBadToken are the messages a rejected push or claim carries.
	// The web app matches on them, so changing them means changing the hook too.
	errInUse    = "ipad-camera: in use by another source"
	errBadToken = "ipad-camera: invalid token"
)

func init() {
	resource.RegisterComponent(camera.API, Model,
		resource.Registration[camera.Camera, *Config]{
			Constructor: newIpadCamera,
		},
	)
}

type Config struct {
	// MaxFrameAgeMs is how old the latest frame may be before Images refuses to
	// return it. Defaults to 5000.
	MaxFrameAgeMs int `json:"max_frame_age_ms,omitempty"`
	// SourceToken, if set, must accompany every push_frame and claim as "token".
	// The kiosk gets it from ?ipadcam=<token>. It can't be "1", which the URL
	// reserves for "no token".
	SourceToken string `json:"source_token,omitempty"`
	// ClaimTimeoutMs is how long the source holding the lease may go quiet
	// before another source can take it over. Defaults to 10000.
	ClaimTimeoutMs int `json:"claim_timeout_ms,omitempty"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.MaxFrameAgeMs < 0 {
		return nil, nil, fmt.Errorf("%s: max_frame_age_ms must not be negative", path)
	}
	if cfg.ClaimTimeoutMs < 0 {
		return nil, nil, fmt.Errorf("%s: claim_timeout_ms must not be negative", path)
	}
	if cfg.SourceToken == "1" {
		return nil, nil, fmt.Errorf(`%s: source_token can't be "1", which ?ipadcam=1 uses to mean no token`, path)
	}
	return nil, nil, nil
}

func (cfg *Config) claimTimeout() time.Duration {
	if cfg.ClaimTimeoutMs > 0 {
		return time.Duration(cfg.ClaimTimeoutMs) * time.Millisecond
	}
	return defaultClaimTimeout
}

func (cfg *Config) maxFrameAge() time.Duration {
	if cfg.MaxFrameAgeMs > 0 {
		return time.Duration(cfg.MaxFrameAgeMs) * time.Millisecond
	}
	return defaultMaxFrameAge
}

type ipadCamera struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger
	cfg    *Config

	// now is time.Now; tests replace it.
	now func() time.Time

	mu       sync.RWMutex
	holder   string    // source_id that holds the lease
	heldAt   time.Time // last accepted push or claim from holder
	frame    []byte
	frameAt  time.Time
	width    int
	height   int
	received uint64
}

func newIpadCamera(_ context.Context, _ resource.Dependencies, rawConf resource.Config, logger logging.Logger) (camera.Camera, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}
	logger.Infow("ipad-camera starting",
		"max_frame_age", conf.maxFrameAge(),
		"claim_timeout", conf.claimTimeout(),
		"token_required", conf.SourceToken != "")
	return &ipadCamera{
		name:   rawConf.ResourceName(),
		logger: logger,
		cfg:    conf,
		now:    time.Now,
	}, nil
}

func (c *ipadCamera) Name() resource.Name {
	return c.name
}

// DoCommand accepts:
//
//	{"push_frame": "<base64 JPEG>"}  store a new frame (a data: URL prefix is tolerated)
//	{"claim": true}                  take or keep the lease without sending a frame
//	{"status": true}                 report frame age, size, and count
//
// push_frame and claim also take "source_id" (who is pushing) and "token" (the
// configured source_token, if any). A bad token is refused. A source_id other
// than the lease holder's is refused until the holder has been quiet for
// claim_timeout_ms.
func (c *ipadCamera) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	if raw, ok := cmd["push_frame"]; ok {
		s, ok := raw.(string)
		if !ok {
			return nil, errors.New("push_frame must be a base64 string")
		}
		jpeg, w, h, err := decodeFrame(s)
		if err != nil {
			return nil, err
		}
		if err := c.authorize(cmd, func() {
			c.frame, c.frameAt = jpeg, c.now()
			c.width, c.height = w, h
			c.received++
		}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	if _, ok := cmd["claim"]; ok {
		if err := c.authorize(cmd, func() {}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	if _, ok := cmd["status"]; ok {
		return c.snapshot(), nil
	}
	return nil, fmt.Errorf("unknown command %v", cmd)
}

// authorize checks the token and the lease, then runs accept under the lock
// with the lease held by the caller, so a rejected push never touches the frame.
func (c *ipadCamera) authorize(cmd map[string]any, accept func()) error {
	if want := c.cfg.SourceToken; want != "" {
		got, _ := cmd["token"].(string)
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return errors.New(errBadToken)
		}
	}
	id, _ := cmd["source_id"].(string)

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.holder != id && !c.heldAt.IsZero() && now.Sub(c.heldAt) < c.cfg.claimTimeout() {
		return errors.New(errInUse)
	}
	if c.holder != id {
		c.logger.Infow("ipad-camera source changed", "from", c.holder, "to", id)
	}
	c.holder, c.heldAt = id, now
	accept()
	return nil
}

// decodeFrame returns the JPEG bytes and dimensions of a base64 frame.
func decodeFrame(b64 string) ([]byte, int, int, error) {
	if i := strings.Index(b64, ","); i >= 0 && strings.HasPrefix(b64, "data:") {
		b64 = b64[i+1:]
	}
	if base64.StdEncoding.DecodedLen(len(b64)) > maxFrameBytes {
		return nil, 0, 0, fmt.Errorf("frame exceeds %d bytes", maxFrameBytes)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decoding frame: %w", err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("frame is not a decodable image: %w", err)
	}
	if format != "jpeg" {
		return nil, 0, 0, fmt.Errorf("frame must be JPEG, got %s", format)
	}
	return raw, cfg.Width, cfg.Height, nil
}

func (c *ipadCamera) snapshot() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := map[string]any{
		"frames_received": float64(c.received),
		"width":           float64(c.width),
		"height":          float64(c.height),
	}
	if !c.frameAt.IsZero() {
		out["age_ms"] = float64(c.now().Sub(c.frameAt).Milliseconds())
	}
	return out
}

func (c *ipadCamera) Status(context.Context) (map[string]any, error) {
	return c.snapshot(), nil
}

func (c *ipadCamera) Images(_ context.Context, filterSourceNames []string, _ map[string]any) ([]camera.NamedImage, resource.ResponseMetadata, error) {
	if len(filterSourceNames) > 0 && !contains(filterSourceNames, sourceName) {
		return nil, resource.ResponseMetadata{}, nil
	}

	c.mu.RLock()
	frame, at := c.frame, c.frameAt
	c.mu.RUnlock()

	if frame == nil {
		return nil, resource.ResponseMetadata{}, errors.New("no frame received from the iPad yet")
	}
	if age := c.now().Sub(at); age > c.cfg.maxFrameAge() {
		return nil, resource.ResponseMetadata{}, fmt.Errorf("latest frame is %s old; is the iPad page still open and awake?", age.Round(time.Millisecond))
	}

	img, err := camera.NamedImageFromBytes(frame, sourceName, rdkutils.MimeTypeJPEG, data.Annotations{})
	if err != nil {
		return nil, resource.ResponseMetadata{}, err
	}
	return []camera.NamedImage{img}, resource.ResponseMetadata{CapturedAt: at}, nil
}

func (c *ipadCamera) NextPointCloud(context.Context, map[string]any) (pointcloud.PointCloud, error) {
	return nil, errors.New("ipad-camera does not support point clouds")
}

func (c *ipadCamera) Properties(context.Context) (camera.Properties, error) {
	return camera.Properties{
		SupportsPCD: false,
		MimeTypes:   []string{rdkutils.MimeTypeJPEG},
	}, nil
}

func (c *ipadCamera) Geometries(context.Context, map[string]any) ([]spatialmath.Geometry, error) {
	return nil, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
