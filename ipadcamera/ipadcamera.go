// Package ipadcamera registers a viam:beanjamin:ipad-camera model that
// implements the rdk:component:camera API for a camera that lives in a browser.
//
// The web app opens the iPad's camera with getUserMedia and pushes JPEG frames
// to this component through DoCommand ({"push_frame": "<base64 jpeg>"}). The
// component keeps only the latest frame and serves it from Images. Frames older
// than max_frame_age_ms are refused, so a backgrounded or locked iPad produces
// an error instead of a stale image.
package ipadcamera

import (
	"bytes"
	"context"
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
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.MaxFrameAgeMs < 0 {
		return nil, nil, fmt.Errorf("%s: max_frame_age_ms must not be negative", path)
	}
	return nil, nil, nil
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

	mu       sync.RWMutex
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
	logger.Infow("ipad-camera starting", "max_frame_age", conf.maxFrameAge())
	return &ipadCamera{
		name:   rawConf.ResourceName(),
		logger: logger,
		cfg:    conf,
	}, nil
}

func (c *ipadCamera) Name() resource.Name {
	return c.name
}

// DoCommand accepts:
//
//	{"push_frame": "<base64 JPEG>"}  store a new frame (a data: URL prefix is tolerated)
//	{"status": true}                 report frame age, size, and count
func (c *ipadCamera) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	if raw, ok := cmd["push_frame"]; ok {
		s, ok := raw.(string)
		if !ok {
			return nil, errors.New("push_frame must be a base64 string")
		}
		if err := c.pushFrame(s); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	if _, ok := cmd["status"]; ok {
		return c.snapshot(), nil
	}
	return nil, fmt.Errorf("unknown command %v", cmd)
}

func (c *ipadCamera) pushFrame(b64 string) error {
	if i := strings.Index(b64, ","); i >= 0 && strings.HasPrefix(b64, "data:") {
		b64 = b64[i+1:]
	}
	if base64.StdEncoding.DecodedLen(len(b64)) > maxFrameBytes {
		return fmt.Errorf("frame exceeds %d bytes", maxFrameBytes)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decoding frame: %w", err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("frame is not a decodable image: %w", err)
	}
	if format != "jpeg" {
		return fmt.Errorf("frame must be JPEG, got %s", format)
	}

	c.mu.Lock()
	c.frame = raw
	c.frameAt = time.Now()
	c.width, c.height = cfg.Width, cfg.Height
	c.received++
	c.mu.Unlock()
	return nil
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
		out["age_ms"] = float64(time.Since(c.frameAt).Milliseconds())
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
	if age := time.Since(at); age > c.cfg.maxFrameAge() {
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
