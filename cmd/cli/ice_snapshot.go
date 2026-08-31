package main

// ice-snapshot: capture frames from the machine for offline analysis.
//
// Moves nothing. It captures at whatever pose the arm is already in, saves every
// camera image plus (optionally) the point cloud, and records the poses, joint
// positions and calibration context needed to analyze them later without a
// machine. See ICE_LEVEL_PLAN.md.
//
// Typical use — one run per fill level, --truth recording what you actually
// poured, all into one --raw-dir:
//
//	ice-snapshot --address $M --label 60 --truth 60 --repeat 5 --raw-dir icedata
//
// Then measure offline with ice-level. The point-cloud sampling flags
// (--diameter, --z-lo, --z-hi) belong to the depth investigation, which found
// depth unusable at the dispense pose; they are kept only for re-analyzing the
// existing captures.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/geo/r3"
	"github.com/viam-labs/motion-tools/draw"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

// The visualizer's drag-and-drop loader only accepts files whose name starts
// with this, and it splits the name on "." to resolve the extension — so
// timestamps and labels are kept dot-free.
const snapshotPrefix = "visualization_snapshot"

// csvHeader is written when --csv names a file that doesn't exist yet. Existing
// files are appended to, so one file accumulates every level across many runs.
var csvHeader = []string{
	"time", "label", "truth", "cloud_points", "points", "z_min", "z_p10", "z_p50", "z_p90", "z_max", "grip_z", "fraction",
}

// capture is one frame's worth of measurement.
type capture struct {
	cloudPoints int       // whole cloud, before any filtering
	points      int       // points inside the sample volume
	zs          []float64 // sorted world Z of every point in the sample volume
	gripZ       float64
	fraction    float64 // -1 when --base-z / --glass-height weren't given
}

// diagnosis decomposes an empty sample volume into the axis that missed. The
// counts are deliberately independent — a cloud with points in the XY cylinder
// but none in the Z band means the volume is at the wrong height, and vice
// versa.
type diagnosis struct {
	inCylinder int     // inside the XY footprint, any Z
	inZBand    int     // inside the Z band, any X/Y
	nearestDXY float64 // horizontal distance from the grip axis to the nearest point
	nearestDZ  float64 // that point's Z, relative to the grip frame
	zLo, zHi   float64 // Z range of the points inside the XY cylinder, relative to grip
	haveZRange bool
	worldMin   r3.Vector
	worldMax   r3.Vector
}

func runIceSnapshot(args []string) error {
	flagSet := flag.NewFlagSet("ice-snapshot", flag.ExitOnError)
	conn := addConnFlags(flagSet)

	cameraName := flagSet.String("camera", "cam", "Camera component to capture from (the service's src_camera_name)")
	cameraFrame := flagSet.String("camera-frame", "", "Camera's frame-system frame (defaults to --camera)")
	armName := flagSet.String("arm", "arm", "Arm component whose joint positions resolve the frame system")
	gripFrame := flagSet.String("grip-frame", "grip-point", "Frame the held glass hangs from; centers the sample volume")
	diameter := flagSet.Float64("diameter", 80, "Sample cylinder diameter, mm")
	zLo := flagSet.Float64("z-lo", -150, "Bottom of the sample volume, mm relative to the grip frame")
	zHi := flagSet.Float64("z-hi", 20, "Top of the sample volume, mm relative to the grip frame")
	label := flagSet.String("label", "ice", "Free-form capture label, e.g. empty / ice / foil / half / third")
	truth := flagSet.Float64("truth", -1, "True fill level you filled to, as a fraction (0.33) or percent (33). Recorded in the CSV")
	baseZ := flagSet.Float64("base-z", math.NaN(), "World Z of the glass interior floor, mm. Read it off an empty capture's z_min")
	glassHeight := flagSet.Float64("glass-height", 0, "Glass interior height, mm. With --base-z, turns each capture into a fill fraction")
	repeat := flagSet.Int("repeat", 1, "Frames to capture at this level; more frames show how noisy one frame is")
	csvPath := flagSet.String("csv", "", "Append one row per frame to this CSV, creating it with a header if absent")
	rawDir := flagSet.String("raw-dir", "", "Save each frame's world-frame cloud here for offline ice-analyze. Machine time stops mattering once this is set")
	rawCrop := flagSet.Float64("raw-crop", 300, "Half-extent kept around the grip point when saving raw clouds, mm. 0 saves everything")
	outDir := flagSet.String("out", ".", "Directory to write snapshots into")
	noWrite := flagSet.Bool("no-write", false, "Print the numbers only, skip the snapshot file")

	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if err := conn.validate(); err != nil {
		return err
	}
	if *zLo >= *zHi {
		return fmt.Errorf("--z-lo (%.0f) must be below --z-hi (%.0f)", *zLo, *zHi)
	}
	if *repeat < 1 {
		return fmt.Errorf("--repeat must be at least 1")
	}
	if *glassHeight < 0 {
		return fmt.Errorf("--glass-height must be positive")
	}
	// A fill level can't legitimately exceed 1, so a larger number is a percent.
	trueFill := *truth
	if trueFill > 1 {
		trueFill /= 100
	}
	camFrameName := *cameraFrame
	if camFrameName == "" {
		camFrameName = *cameraName
	}
	slug := labelSlug(*label)

	ctx := context.Background()
	logger := logging.NewLogger("ice-snapshot")

	machine, err := conn.connect(ctx, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := machine.Close(ctx); err != nil {
			logger.Warnf("closing machine: %v", err)
		}
	}()

	cam, err := camera.FromProvider(machine, *cameraName)
	if err != nil {
		return fmt.Errorf("getting camera %q: %w", *cameraName, err)
	}
	robotArm, err := arm.FromProvider(machine, *armName)
	if err != nil {
		return fmt.Errorf("getting arm %q: %w", *armName, err)
	}
	fsCfg, err := machine.FrameSystemConfig(ctx)
	if err != nil {
		return fmt.Errorf("getting frame system config: %w", err)
	}
	fs, err := referenceframe.NewFrameSystem("robot", fsCfg.Parts, nil)
	if err != nil {
		return fmt.Errorf("building frame system: %w", err)
	}

	fmt.Printf("capture %q", *label)
	if trueFill >= 0 {
		fmt.Printf(" (true fill %.0f%%)", trueFill*100)
	}
	if *repeat > 1 {
		fmt.Printf(", %d frames", *repeat)
	}
	fmt.Println()

	// One session record per directory, written before anything else: it is what
	// turns the dir into something fully re-analyzable offline — any frame's
	// pose, any geometry, and the intrinsics to project points into the images.
	if *rawDir != "" {
		sess := session{
			Time:        time.Now().Format(time.RFC3339),
			CameraName:  *cameraName,
			CameraFrame: camFrameName,
			ArmName:     *armName,
			GripFrame:   *gripFrame,
		}
		if props, err := cam.Properties(ctx); err == nil {
			sess.SupportsPCD = props.SupportsPCD
			sess.MimeTypes = props.MimeTypes
			if props.IntrinsicParams != nil {
				if raw, err := json.Marshal(props.IntrinsicParams); err == nil {
					sess.IntrinsicParams = raw
				}
			}
		} else {
			logger.Warnf("camera properties unavailable: %v", err)
		}
		if raw, err := json.Marshal(fsCfg.Parts); err == nil {
			sess.FrameSystem = raw
		} else {
			logger.Warnf("frame system not recorded: %v", err)
		}
		if err := writeSession(*rawDir, sess); err != nil {
			return err
		}
	}

	captures := make([]capture, 0, *repeat)
	for i := 0; i < *repeat; i++ {
		// Re-resolve the arm every frame rather than once: if the arm gets
		// nudged mid-series, the sample volume follows it instead of silently
		// measuring the wrong column.
		joints, err := robotArm.JointPositions(ctx, nil)
		if err != nil {
			return fmt.Errorf("getting joint positions: %w", err)
		}
		inputs := referenceframe.NewZeroInputs(fs)
		inputs[*armName] = joints

		camToWorld, err := frameToWorld(fs, inputs, camFrameName)
		if err != nil {
			return err
		}
		gripToWorld, err := frameToWorld(fs, inputs, *gripFrame)
		if err != nil {
			return err
		}
		cloud, err := cam.NextPointCloud(ctx, nil)
		if err != nil {
			return fmt.Errorf("capturing point cloud from %q: %w", *cameraName, err)
		}
		// Grab the images too when raw-capturing. Whether the answer comes from
		// depth or from RGB is precisely the decision --raw-dir exists to defer,
		// and a second trip to a shared machine costs far more than the disk.
		var images []camera.NamedImage
		if *rawDir != "" {
			images, _, err = cam.Images(ctx, nil, nil)
			if err != nil {
				logger.Warnf("capturing images from %q: %v — saving the cloud only", *cameraName, err)
			}
		}

		center := gripToWorld.Point()
		zs := sampleColumn(cloud, camToWorld, center, *diameter, *zLo, *zHi)
		sort.Float64s(zs)
		c := capture{cloudPoints: cloud.Size(), points: len(zs), zs: zs, gripZ: center.Z, fraction: -1}
		if len(zs) > 0 && !math.IsNaN(*baseZ) && *glassHeight > 0 {
			c.fraction = clamp((percentile(zs, 0.90)-*baseZ)/(*glassHeight), 0, 1)
		}
		captures = append(captures, c)

		if *rawDir != "" {
			frame, err := saveRawFrame(*rawDir, cloud, images, frameMeta{
				Label: *label, Slug: slug, Truth: trueFill,
				ArmName: *armName, Joints: inputsToFloats(joints),
				GripPose: gripToWorld, CamToWorld: camToWorld,
				CropMm: *rawCrop, Index: i,
			})
			if err != nil {
				return err
			}
			if i == 0 {
				fmt.Printf("  raw              %s (%d points kept, %d image(s))\n",
					filepath.Join(*rawDir, frame.File), frame.CloudPoints, len(frame.Images))
			}
		}

		if i == 0 {
			fmt.Printf("  cloud            %d points total\n", cloud.Size())
			fmt.Printf("  %-16s x=%.1f y=%.1f z=%.1f (world, mm)\n", *gripFrame, center.X, center.Y, center.Z)
			fmt.Printf("  sample volume    %.0fmm dia, z %+.0f..%+.0f relative to %s\n", *diameter, *zLo, *zHi, *gripFrame)
			// An empty volume is the one result that can't be tuned blind, so
			// spend the extra pass over the cloud to say which axis missed.
			// With --raw-dir the cloud is on disk and ice-analyze --sweep answers
			// this better, so don't spend the pass.
			if len(zs) == 0 && *rawDir == "" {
				reportDiagnosis(diagnose(cloud, camToWorld, center, *diameter, *zLo, *zHi), *diameter, *zLo, *zHi)
			}
			// One snapshot per invocation: the frames are the same scene, and
			// the point clouds make these files big.
			if !*noWrite {
				path, err := writeSnapshot(fs, inputs, camToWorld, cloud, center, *diameter, *zLo, *zHi, slug, *outDir)
				if err != nil {
					return err
				}
				fmt.Printf("  snapshot         %s\n", path)
			}
		}
	}

	report(captures, *repeat)

	if *csvPath != "" {
		if err := appendCSV(*csvPath, *label, trueFill, captures); err != nil {
			return err
		}
		fmt.Printf("  csv              appended %d row(s) to %s\n", len(captures), *csvPath)
	}
	if !*noWrite {
		fmt.Printf("\nDrag the snapshot onto a motion-tools visualizer. The green box is the sample volume.\n")
	}
	return nil
}

// report prints the measurement, as a mean ± spread across frames when there is
// more than one.
func report(captures []capture, repeat int) {
	if len(captures) == 0 {
		return
	}
	counts := make([]float64, len(captures))
	p90s := make([]float64, 0, len(captures))
	fracs := make([]float64, 0, len(captures))
	for i, c := range captures {
		counts[i] = float64(c.points)
		if len(c.zs) > 0 {
			p90s = append(p90s, percentile(c.zs, 0.90))
		}
		if c.fraction >= 0 {
			fracs = append(fracs, c.fraction)
		}
	}
	fmt.Printf("  points in volume %s\n", summarize(counts, "%.0f"))
	if len(p90s) == 0 {
		fmt.Printf("  nothing in the sample volume — widen --diameter / --z-lo / --z-hi,\n")
		fmt.Printf("  or the camera is returning no depth here at all\n")
		return
	}
	first := captures[0].zs
	fmt.Printf("  contents top Z   %s mm (world, p90)\n", summarize(p90s, "%.1f"))
	fmt.Printf("  contents base Z  min=%.1f  p10=%.1f  (first frame)\n", first[0], percentile(first, 0.10))
	fmt.Printf("  spread           %.1f mm between p10 and p90 (first frame)\n",
		percentile(first, 0.90)-percentile(first, 0.10))
	if len(fracs) > 0 {
		pct := make([]float64, len(fracs))
		for i, f := range fracs {
			pct[i] = f * 100
		}
		fmt.Printf("  reported fill    %s %%\n", summarize(pct, "%.1f"))
	}
	if repeat > 1 {
		fmt.Printf("  (± is one standard deviation across %d frames — this is the G5 noise number)\n", repeat)
	}
}

// summarize renders a mean, plus a standard deviation when there is more than
// one sample.
func summarize(vals []float64, format string) string {
	if len(vals) == 0 {
		return "n/a"
	}
	mean := 0.0
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	if len(vals) == 1 {
		return fmt.Sprintf(format, mean)
	}
	variance := 0.0
	for _, v := range vals {
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(len(vals) - 1)
	return fmt.Sprintf(format+" ± "+format, mean, math.Sqrt(variance))
}

// appendCSV adds one row per frame, writing the header first if the file is new.
// Appending rather than overwriting is what lets a single file accumulate every
// fill level across separate runs.
func appendCSV(path, label string, truth float64, captures []capture) error {
	_, statErr := os.Stat(path)
	isNew := os.IsNotExist(statErr)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // flushed explicitly below

	writer := csv.NewWriter(file)
	if isNew {
		if err := writer.Write(csvHeader); err != nil {
			return fmt.Errorf("writing csv header: %w", err)
		}
	}
	stamp := time.Now().Format(time.RFC3339)
	for _, c := range captures {
		row := []string{stamp, label, optFloat(truth, 3), strconv.Itoa(c.cloudPoints), strconv.Itoa(c.points)}
		if len(c.zs) > 0 {
			row = append(row,
				fmt.Sprintf("%.2f", c.zs[0]),
				fmt.Sprintf("%.2f", percentile(c.zs, 0.10)),
				fmt.Sprintf("%.2f", percentile(c.zs, 0.50)),
				fmt.Sprintf("%.2f", percentile(c.zs, 0.90)),
				fmt.Sprintf("%.2f", c.zs[len(c.zs)-1]),
			)
		} else {
			row = append(row, "", "", "", "", "")
		}
		row = append(row, fmt.Sprintf("%.2f", c.gripZ), optFloat(c.fraction, 4))
		if err := writer.Write(row); err != nil {
			return fmt.Errorf("writing csv row: %w", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("flushing %s: %w", path, err)
	}
	return file.Close()
}

// optFloat renders a value that may be unset, as marked by a negative number.
func optFloat(v float64, precision int) string {
	if v < 0 {
		return ""
	}
	return strconv.FormatFloat(v, 'f', precision, 64)
}

var labelUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// labelSlug makes an arbitrary label safe for a filename. The visualizer splits
// snapshot names on "." to resolve the extension, so a label like "1.5x" would
// otherwise break its loader.
func labelSlug(label string) string {
	slug := strings.Trim(labelUnsafe.ReplaceAllString(strings.ToLower(label), "_"), "_")
	if slug == "" {
		return "capture"
	}
	return slug
}

// frameToWorld resolves a frame's world pose at the given inputs.
func frameToWorld(fs *referenceframe.FrameSystem, inputs referenceframe.FrameSystemInputs, frame string) (spatialmath.Pose, error) {
	if fs.Frame(frame) == nil {
		return nil, fmt.Errorf("frame %q not found in the machine's frame system", frame)
	}
	tf, err := fs.Transform(inputs.ToLinearInputs(), referenceframe.NewPoseInFrame(frame, spatialmath.NewZeroPose()), referenceframe.World)
	if err != nil {
		return nil, fmt.Errorf("transforming %q to world: %w", frame, err)
	}
	return tf.(*referenceframe.PoseInFrame).Pose(), nil
}

// sampleColumn returns the world Z of every cloud point falling inside an
// upright cylinder centered on the grip point — i.e. inside the held glass.
// The cloud is in camera coordinates, so each point is lifted through camToWorld
// first.
func sampleColumn(
	cloud pointcloud.PointCloud,
	camToWorld spatialmath.Pose,
	center r3.Vector,
	diameter, zLo, zHi float64,
) []float64 {
	radius := diameter / 2
	var zs []float64
	cloud.Iterate(0, 0, func(p r3.Vector, _ pointcloud.Data) bool {
		w := spatialmath.Compose(camToWorld, spatialmath.NewPoseFromPoint(p)).Point()
		if math.Hypot(w.X-center.X, w.Y-center.Y) > radius {
			return true
		}
		if dz := w.Z - center.Z; dz < zLo || dz > zHi {
			return true
		}
		zs = append(zs, w.Z)
		return true
	})
	return zs
}

// diagnose works out why a sample volume came back empty, by relaxing one axis
// at a time. Only called when the volume caught nothing — the interesting case
// is always "the cloud is full, so where did it go?"
func diagnose(
	cloud pointcloud.PointCloud,
	camToWorld spatialmath.Pose,
	center r3.Vector,
	diameter, zLo, zHi float64,
) diagnosis {
	radius := diameter / 2
	d := diagnosis{
		nearestDXY: math.Inf(1),
		worldMin:   r3.Vector{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)},
		worldMax:   r3.Vector{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)},
		zLo:        math.Inf(1),
		zHi:        math.Inf(-1),
	}
	cloud.Iterate(0, 0, func(p r3.Vector, _ pointcloud.Data) bool {
		w := spatialmath.Compose(camToWorld, spatialmath.NewPoseFromPoint(p)).Point()
		d.worldMin = r3.Vector{X: math.Min(d.worldMin.X, w.X), Y: math.Min(d.worldMin.Y, w.Y), Z: math.Min(d.worldMin.Z, w.Z)}
		d.worldMax = r3.Vector{X: math.Max(d.worldMax.X, w.X), Y: math.Max(d.worldMax.Y, w.Y), Z: math.Max(d.worldMax.Z, w.Z)}

		dxy := math.Hypot(w.X-center.X, w.Y-center.Y)
		dz := w.Z - center.Z
		if dxy < d.nearestDXY {
			d.nearestDXY, d.nearestDZ = dxy, dz
		}
		if dxy <= radius {
			d.inCylinder++
			d.zLo = math.Min(d.zLo, dz)
			d.zHi = math.Max(d.zHi, dz)
			d.haveZRange = true
		}
		if dz >= zLo && dz <= zHi {
			d.inZBand++
		}
		return true
	})
	return d
}

// reportDiagnosis turns the decomposition into the flag change to make next.
func reportDiagnosis(d diagnosis, diameter, zLo, zHi float64) {
	fmt.Printf("\n  nothing landed in the sample volume — diagnosing\n")
	fmt.Printf("    cloud world bounds  x %.0f..%.0f  y %.0f..%.0f  z %.0f..%.0f (mm)\n",
		d.worldMin.X, d.worldMax.X, d.worldMin.Y, d.worldMax.Y, d.worldMin.Z, d.worldMax.Z)
	fmt.Printf("    in XY cylinder      %d points (any height)\n", d.inCylinder)
	fmt.Printf("    in Z band           %d points (any X/Y)\n", d.inZBand)
	fmt.Printf("    nearest point       %.0fmm off the grip axis, %+.0fmm in Z\n", d.nearestDXY, d.nearestDZ)

	switch {
	case d.inCylinder > 0 && d.haveZRange:
		// The column is right, the height band is wrong — the common case when
		// the glass sits above the grip point rather than hanging below it.
		fmt.Printf("\n    The XY cylinder is on target but the Z band misses: those %d points\n", d.inCylinder)
		fmt.Printf("    span %+.0f..%+.0f relative to the grip frame, and you sampled %+.0f..%+.0f.\n",
			d.zLo, d.zHi, zLo, zHi)
		fmt.Printf("    Re-run with:  --z-lo %.0f --z-hi %.0f\n", math.Floor(d.zLo/10)*10, math.Ceil(d.zHi/10)*10)
	case d.inZBand > 0:
		fmt.Printf("\n    The Z band has points but none are near the grip axis — the nearest is\n")
		fmt.Printf("    %.0fmm out, against a %.0fmm radius. Either --grip-frame is not where the\n", d.nearestDXY, diameter/2)
		fmt.Printf("    glass is, or --camera-frame is wrong and the cloud is landing in the\n")
		fmt.Printf("    wrong place entirely. Check the snapshot before widening --diameter.\n")
	default:
		fmt.Printf("\n    Neither axis is close. The cloud is %.0fmm from the grip point at its\n", d.nearestDXY)
		fmt.Printf("    nearest — that is a frame problem, not a tuning problem. Open the\n")
		fmt.Printf("    snapshot: if the cloud does not sit on the scene, --camera-frame is wrong.\n")
	}
}

// percentile returns the p-th percentile of an already-sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(p*float64(len(sorted)-1))]
}

func clamp(v, lo, hi float64) float64 {
	return math.Min(math.Max(v, lo), hi)
}

// writeSnapshot renders the scene the way the coffee service's own detection
// snapshots do — every frame-system geometry resolved at these inputs, the raw
// cloud anchored at the camera's world pose — plus a box showing exactly what
// volume the counts above were taken from.
func writeSnapshot(
	fs *referenceframe.FrameSystem,
	inputs referenceframe.FrameSystemInputs,
	camToWorld spatialmath.Pose,
	cloud pointcloud.PointCloud,
	center r3.Vector,
	diameter, zLo, zHi float64,
	slug, outDir string,
) (string, error) {
	snapshot := draw.NewSnapshot()
	if _, err := snapshot.DrawFrameSystemGeometries(draw.DrawFrameSystemGeometriesOptions{
		FrameSystem: fs,
		Inputs:      inputs,
	}); err != nil {
		return "", fmt.Errorf("drawing frame system: %w", err)
	}
	// The cloud keeps its camera-frame coordinates and is anchored by the
	// camera's world pose, so it lands where it was actually seen from.
	if _, err := snapshot.DrawPointCloud(draw.DrawPointCloudOptions{
		Name:       slug + "_cloud",
		Parent:     referenceframe.World,
		Pose:       camToWorld,
		PointCloud: cloud,
	}); err != nil {
		return "", fmt.Errorf("drawing point cloud: %w", err)
	}
	volume, err := spatialmath.NewBox(
		spatialmath.NewPoseFromPoint(r3.Vector{X: center.X, Y: center.Y, Z: center.Z + (zLo+zHi)/2}),
		r3.Vector{X: diameter, Y: diameter, Z: zHi - zLo},
		slug+"_sample_volume",
	)
	if err != nil {
		return "", fmt.Errorf("building sample volume: %w", err)
	}
	if _, err := snapshot.DrawGeometry(draw.DrawGeometryOptions{
		Name:     slug + "_sample_volume",
		Parent:   referenceframe.World,
		Geometry: volume,
		Color:    draw.ColorFromName("limegreen"),
	}); err != nil {
		return "", fmt.Errorf("drawing sample volume: %w", err)
	}

	// Gzipped binary protobuf: the point cloud makes JSON an order of magnitude
	// bigger, and the visualizer loads .pb.gz directly.
	data, err := snapshot.MarshalBinaryGzip()
	if err != nil {
		return "", fmt.Errorf("marshaling snapshot: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", outDir, err)
	}
	stamp := strings.ReplaceAll(time.Now().Format("20060102_150405.000"), ".", "_")
	path := filepath.Join(outDir, fmt.Sprintf("%s_%s_%s.pb.gz", snapshotPrefix, stamp, slug))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}
