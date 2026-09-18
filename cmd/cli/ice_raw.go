package main

// Raw capture: saveRawFrame and readManifest back every capture command.
// ice-level is the analysis path.
//
// The machine is shared, so no analysis choice should have to be right while you
// are standing at it. A --raw-dir records every camera image, the poses, the
// joints and the frame system, and every decision about how to measure them is
// made later, offline, as many times as you like.
//
// Clouds, when captured, are saved in WORLD frame, already transformed, so
// offline analysis needs no frame system and no machine connection.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// manifestName is the append-only index of every raw frame in a capture dir.
const manifestName = "manifest.jsonl"

// sessionName holds the context shared by every frame in a capture dir.
const sessionName = "session.json"

// rawFrame is one captured frame's metadata: enough to analyze the cloud beside
// it without a machine, a frame system, or the flags used at capture time.
type rawFrame struct {
	Time        string     `json:"time"`
	Label       string     `json:"label"`
	Truth       float64    `json:"truth"`            // -1 when not given
	File        string     `json:"file"`             // PCD filename, relative to the dir
	Images      []string   `json:"images,omitempty"` // image filenames, relative to the dir
	CloudPoints int        `json:"cloud_points"`
	GripPoint   [3]float64 `json:"grip_point"`   // world mm
	CameraPoint [3]float64 `json:"camera_point"` // world mm, for reference
	CropMm      float64    `json:"crop_mm"`      // half-extent kept around the grip point, 0 = uncropped

	// Full poses keep the orientation the bare points above drop — without it an
	// image cannot be re-projected and a view direction cannot be reasoned about.
	GripPose   *poseJSON `json:"grip_pose,omitempty"`
	CameraPose *poseJSON `json:"camera_pose,omitempty"`

	// Joints are what make a capture re-derivable against ANY frame rather than
	// only the two resolved at capture time: with these plus the session's frame
	// system, every frame's pose can be recomputed offline.
	ArmName string    `json:"arm_name,omitempty"`
	Joints  []float64 `json:"joints,omitempty"`

	// Set only by ice-dispense: milliseconds since the ice pin went HIGH
	// (negative during pre-roll) and which phase of the run this frame is from.
	TMs   *int64 `json:"t_ms,omitempty"`
	Phase string `json:"phase,omitempty"`
}

// frameMeta is everything about a capture that isn't the pixels. It travels as
// one value because saveRawFrame otherwise takes a dozen positional arguments
// and the call sites stop being readable.
type frameMeta struct {
	Label, Slug string
	Truth       float64 // -1 when not given
	ArmName     string
	Joints      []float64
	GripPose    spatialmath.Pose
	CamToWorld  spatialmath.Pose
	CropMm      float64
	Index       int
	TMs         *int64 // nil outside a dispense run
	Phase       string
}

// poseJSON is a pose in the orientation-vector form used throughout the repo.
type poseJSON struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Z     float64 `json:"z"`
	OX    float64 `json:"o_x"`
	OY    float64 `json:"o_y"`
	OZ    float64 `json:"o_z"`
	Theta float64 `json:"theta"`
}

func toPoseJSON(p spatialmath.Pose) *poseJSON {
	pt := p.Point()
	ov := p.Orientation().OrientationVectorDegrees()
	return &poseJSON{X: pt.X, Y: pt.Y, Z: pt.Z, OX: ov.OX, OY: ov.OY, OZ: ov.OZ, Theta: ov.Theta}
}

// session is the per-directory context every frame shares: the frame system the
// captures were resolved against and what the camera actually is. Written once,
// it turns a capture dir into something fully re-analyzable — any frame's pose,
// any geometry, and the intrinsics needed to project 3D points into the images.
type session struct {
	Time            string          `json:"time"`
	CameraName      string          `json:"camera_name"`
	CameraFrame     string          `json:"camera_frame"`
	ArmName         string          `json:"arm_name"`
	GripFrame       string          `json:"grip_frame"`
	SupportsPCD     bool            `json:"supports_pcd"`
	MimeTypes       []string        `json:"mime_types,omitempty"`
	IntrinsicParams json.RawMessage `json:"intrinsic_params,omitempty"`
	FrameSystem     json.RawMessage `json:"frame_system,omitempty"`
}

// writeSession records the shared context once per capture dir. Best-effort on
// each piece: a camera that won't report properties should not cost you the run.
func writeSession(dir string, s session) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding session: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, sessionName), data, 0o600)
}

// saveRawFrame writes one cloud as world-frame PCD and appends its manifest
// entry. Points further than cropMm from the grip point in any axis are dropped:
// a full room scan is ~2.5MB a frame and none of it more than a glass-width away
// can matter, so cropping generously keeps a batch small without foreclosing any
// analysis.
func saveRawFrame(
	dir string,
	cloud pointcloud.PointCloud,
	images []camera.NamedImage,
	m frameMeta,
) (rawFrame, error) {
	grip := m.GripPose.Point()
	camToWorld, cropMm := m.CamToWorld, m.CropMm
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return rawFrame{}, fmt.Errorf("creating %s: %w", dir, err)
	}

	world := pointcloud.NewBasicEmpty()
	kept := 0
	if cloud != nil {
		cloud.Iterate(0, 0, func(p r3.Vector, d pointcloud.Data) bool {
			w := spatialmath.Compose(camToWorld, spatialmath.NewPoseFromPoint(p)).Point()
			if cropMm > 0 &&
				(math.Abs(w.X-grip.X) > cropMm || math.Abs(w.Y-grip.Y) > cropMm || math.Abs(w.Z-grip.Z) > cropMm) {
				return true
			}
			if err := world.Set(w, d); err != nil {
				return false
			}
			kept++
			return true
		})
	}

	stamp := strings.ReplaceAll(time.Now().Format("20060102_150405.000"), ".", "_")
	name := fmt.Sprintf("%s_%s_%03d.pcd", stamp, m.Slug, m.Index)
	if cloud != nil {
		file, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			return rawFrame{}, fmt.Errorf("creating %s: %w", name, err)
		}
		if err := pointcloud.ToPCD(world, file, pointcloud.PCDBinary); err != nil {
			file.Close() //nolint:errcheck // write already failed
			return rawFrame{}, fmt.Errorf("writing %s: %w", name, err)
		}
		if err := file.Close(); err != nil {
			return rawFrame{}, fmt.Errorf("closing %s: %w", name, err)
		}
	} else {
		name = ""
	}

	imageNames, err := saveImages(dir, images, stamp, m.Slug, m.Index)
	if err != nil {
		return rawFrame{}, err
	}

	camPoint := camToWorld.Point()
	frame := rawFrame{
		Time:        time.Now().Format(time.RFC3339),
		Label:       m.Label,
		Truth:       m.Truth,
		File:        name,
		Images:      imageNames,
		CloudPoints: kept,
		GripPoint:   [3]float64{grip.X, grip.Y, grip.Z},
		CameraPoint: [3]float64{camPoint.X, camPoint.Y, camPoint.Z},
		CropMm:      cropMm,
		GripPose:    toPoseJSON(m.GripPose),
		CameraPose:  toPoseJSON(camToWorld),
		ArmName:     m.ArmName,
		Joints:      m.Joints,
		TMs:         m.TMs,
		Phase:       m.Phase,
	}
	return frame, appendManifest(dir, frame)
}

// saveImages writes each of the camera's images beside the cloud, in whatever
// format the camera sent — no decode, no re-encode, so nothing is lost to a
// conversion before anyone has decided what the images are for.
func saveImages(dir string, images []camera.NamedImage, stamp, slug string, index int) ([]string, error) {
	names := make([]string, 0, len(images))
	for i := range images {
		bytes, err := images[i].Bytes(context.Background())
		if err != nil {
			return nil, fmt.Errorf("reading image %d: %w", i, err)
		}
		source := labelSlug(images[i].SourceName)
		if source == "capture" {
			source = fmt.Sprintf("img%d", i)
		}
		name := fmt.Sprintf("%s_%s_%02d_%s%s", stamp, slug, index, source, imageExt(images[i].MimeType()))
		if err := os.WriteFile(filepath.Join(dir, name), bytes, 0o600); err != nil {
			return nil, fmt.Errorf("writing %s: %w", name, err)
		}
		names = append(names, name)
	}
	return names, nil
}

// imageExt maps a mime type to a file extension, defaulting to .bin so an
// unrecognized format is still written rather than dropped.
func imageExt(mimeType string) string {
	switch {
	case strings.Contains(mimeType, "png"):
		return ".png"
	case strings.Contains(mimeType, "jpeg"), strings.Contains(mimeType, "jpg"):
		return ".jpg"
	case strings.Contains(mimeType, "depth"):
		return ".depth"
	default:
		return ".bin"
	}
}

func appendManifest(dir string, frame rawFrame) error {
	path := filepath.Join(dir, manifestName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // flushed by Encode + explicit Close below
	if err := json.NewEncoder(file).Encode(frame); err != nil {
		return fmt.Errorf("writing manifest entry: %w", err)
	}
	return file.Close()
}

func readManifest(dir string) ([]rawFrame, error) {
	path := filepath.Join(dir, manifestName)
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // read-only

	var frames []rawFrame
	decoder := json.NewDecoder(file)
	for decoder.More() {
		var frame rawFrame
		if err := decoder.Decode(&frame); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		frames = append(frames, frame)
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("%s is empty — capture with --raw-dir first", path)
	}
	return frames, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// inputsToFloats copies joint inputs for the manifest. referenceframe.Input is
// an alias for float64, so this is a defensive copy, not a conversion.
func inputsToFloats(inputs []referenceframe.Input) []float64 {
	return append([]float64(nil), inputs...)
}
