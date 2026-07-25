package coffee

// Detection debug snapshots.
//
// Every vision observation during dynamic cup/glass pickup (observeVantage) can
// write a motion-tools snapshot to SaveMotionRequestsDir: the whole frame system
// resolved at the joint configuration the arm held when the photo was taken, plus
// — per detection — the point cloud the vision service returned and the
// world-frame bounding box the grasp was derived from. Drop the file onto a
// motion-tools visualizer to replay exactly what the robot saw and where it
// thought the items were.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/viam-labs/motion-tools/draw"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// detectionSnapshotPrefix is mandatory: the visualizer's drag-and-drop loader
// only accepts files whose name starts with it.
const detectionSnapshotPrefix = "visualization_snapshot"

// detectionBoxColor sets the detected bounding boxes apart from the frame
// system's own geometries, which motion-tools draws magenta.
const detectionBoxColor = "limegreen"

// detectionSnapshotItem is one vision detection as it goes into the snapshot: the
// point cloud the vision service returned (in camera frame, as captured) and the
// world-frame bounding box pickup derived from it. Either may be nil — a
// detection that yielded no usable geometry still carries a cloud worth seeing.
type detectionSnapshotItem struct {
	cloud pointcloud.PointCloud
	box   spatialmath.Geometry
}

// buildDetectionSnapshot assembles the motion-tools scene: every frame-system
// geometry resolved at fsInputs (so the arm, gripper and camera appear where they
// actually were, not at rest), then each detection's point cloud — placed by
// camToWorld, the camera frame's world pose at those same inputs — and its
// world-frame bounding box. The two entities of one detection share the
// "<label>_<i>" naming so they line up in the visualizer.
func buildDetectionSnapshot(
	label string,
	fs *referenceframe.FrameSystem,
	fsInputs referenceframe.FrameSystemInputs,
	camToWorld spatialmath.Pose,
	items []detectionSnapshotItem,
) (*draw.Snapshot, error) {
	snapshot := draw.NewSnapshot()
	if _, err := snapshot.DrawFrameSystemGeometries(draw.DrawFrameSystemGeometriesOptions{
		FrameSystem: fs,
		Inputs:      fsInputs,
	}); err != nil {
		return nil, fmt.Errorf("draw frame system: %w", err)
	}

	for i, item := range items {
		name := fmt.Sprintf("%s_%d", label, i)
		if item.cloud != nil && item.cloud.Size() > 0 {
			// The cloud keeps its camera-frame coordinates and is anchored by the
			// camera's world pose, so it lands where the detection was seen from.
			if _, err := snapshot.DrawPointCloud(draw.DrawPointCloudOptions{
				Name:       name + "_cloud",
				Parent:     referenceframe.World,
				Pose:       camToWorld,
				PointCloud: item.cloud,
			}); err != nil {
				return nil, fmt.Errorf("draw %s point cloud: %w", name, err)
			}
		}
		if item.box == nil {
			continue
		}
		// The box already carries its world pose, so it is drawn at the identity
		// pose of the world frame.
		if _, err := snapshot.DrawGeometry(draw.DrawGeometryOptions{
			Name:     name,
			Parent:   referenceframe.World,
			Geometry: item.box,
			Color:    draw.ColorFromName(detectionBoxColor),
		}); err != nil {
			return nil, fmt.Errorf("draw %s bounding box: %w", name, err)
		}
	}
	return snapshot, nil
}

// saveDetectionSnapshot writes one observation's snapshot to
// SaveMotionRequestsDir. It is a no-op when that directory is unset or nothing
// was detected. Failures are logged, never returned — this is debugging output
// and must not break a pickup.
func (s *beanjaminCoffee) saveDetectionSnapshot(
	label string,
	fs *referenceframe.FrameSystem,
	fsInputs referenceframe.FrameSystemInputs,
	camToWorld spatialmath.Pose,
	items []detectionSnapshotItem,
) {
	logger := s.activeOrderLogger()
	dir := s.cfg.SaveMotionRequestsDir
	if dir == "" || len(items) == 0 {
		return
	}
	snapshot, err := buildDetectionSnapshot(label, fs, fsInputs, camToWorld, items)
	if err != nil {
		logger.Warnf("save %s detection snapshot: %v", label, err)
		return
	}
	// Gzipped binary protobuf: the point clouds make JSON an order of magnitude
	// bigger, and the visualizer loads .pb.gz directly.
	data, err := snapshot.MarshalBinaryGzip()
	if err != nil {
		logger.Warnf("save %s detection snapshot: marshal: %v", label, err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warnf("save %s detection snapshot: create dir: %v", label, err)
		return
	}
	filename := detectionSnapshotPath(dir, label, time.Now())
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		logger.Warnf("save %s detection snapshot: %v", label, err)
		return
	}
	logger.Infof("saved %s detection snapshot (%d detection(s), %d bytes) to %s",
		label, len(items), len(data), filename)
}

// detectionSnapshotPath builds the snapshot filename. The name must start with
// detectionSnapshotPrefix, and the timestamp is kept dot-free because the
// visualizer's loader splits the filename on "." to resolve the extension.
func detectionSnapshotPath(dir, label string, at time.Time) string {
	stamp := strings.ReplaceAll(at.Format("20060102_150405.000"), ".", "_")
	return filepath.Join(dir, fmt.Sprintf("%s_%s_%s.pb.gz", detectionSnapshotPrefix, stamp, label))
}
