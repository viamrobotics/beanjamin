package main

// ice-dispense: the G6 probe from ICE_LEVEL_PLAN.md — what a live dispense
// actually looks like over time.
//
// This command drives the ice pin itself rather than asking you to run a
// DoCommand in another terminal. That is the whole point: every captured frame
// is stamped with milliseconds since the pin went HIGH, and without that
// alignment the trace cannot answer the three questions it exists to answer —
// how long the falling stream fools the reading (ice_dispense_min_sec), how long
// the pile takes to settle after the pin closes, and how long reaching the
// target takes (ice_dispense_max_sec).
//
// The pin is driven LOW on every exit path, including Ctrl-C, using a context
// that cannot already be cancelled. An ice machine left running is the one
// failure this must never have.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/referenceframe"
)

func runIceDispense(args []string) error {
	flagSet := flag.NewFlagSet("ice-dispense", flag.ExitOnError)
	conn := addConnFlags(flagSet)

	cameraName := flagSet.String("camera", "cam", "Camera component to capture from")
	cameraFrame := flagSet.String("camera-frame", "", "Camera's frame-system frame (defaults to --camera)")
	armName := flagSet.String("arm", "arm", "Arm component whose joint positions resolve the frame system")
	gripFrame := flagSet.String("grip-frame", "grip-point", "Frame the held glass hangs from")
	boardName := flagSet.String("board", "", "Board component driving the ice machine (ice_board_name) (required)")
	pinName := flagSet.String("pin", "", "GPIO pin held HIGH to dispense (ice_pin_name) (required)")
	sources := flagSet.String("sources", "color", "Comma-separated camera sources to save; empty saves all")
	withCloud := flagSet.Bool("cloud", false, "Also capture point clouds. Slow and large; depth is useless at the dispense pose")
	preSec := flagSet.Float64("pre", 3, "Seconds of baseline captured before the pin opens")
	dwellSec := flagSet.Float64("dwell", 20, "Seconds to hold the pin HIGH")
	postSec := flagSet.Float64("post", 10, "Seconds to keep capturing after the pin closes, to see the pile settle")
	hz := flagSet.Float64("hz", 2, "Capture rate")
	rawDir := flagSet.String("raw-dir", "", "Directory to write the run into (required)")
	label := flagSet.String("label", "dispense", "Run label")
	rawCrop := flagSet.Float64("raw-crop", 300, "Half-extent kept around the grip point when saving clouds, mm")

	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if err := conn.validate(); err != nil {
		return err
	}
	switch {
	case *boardName == "":
		return fmt.Errorf("--board is required (the machine's ice_board_name)")
	case *pinName == "":
		return fmt.Errorf("--pin is required (the machine's ice_pin_name)")
	case *rawDir == "":
		return fmt.Errorf("--raw-dir is required")
	case *hz <= 0:
		return fmt.Errorf("--hz must be positive")
	case *dwellSec <= 0:
		return fmt.Errorf("--dwell must be positive")
	}
	camFrameName := *cameraFrame
	if camFrameName == "" {
		camFrameName = *cameraName
	}

	// Ctrl-C must close the pin, not just kill the process. The capture loop
	// watches this context; the pin's own shutdown deliberately does not.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	logger := logging.NewLogger("ice-dispense")
	machine, err := conn.connect(ctx, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := machine.Close(context.Background()); err != nil {
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
	iceBoard, err := board.FromProvider(machine, *boardName)
	if err != nil {
		return fmt.Errorf("getting board %q: %w", *boardName, err)
	}
	pin, err := iceBoard.GPIOPinByName(*pinName)
	if err != nil {
		return fmt.Errorf("getting pin %q on %q: %w", *pinName, *boardName, err)
	}
	fsCfg, err := machine.FrameSystemConfig(ctx)
	if err != nil {
		return fmt.Errorf("getting frame system config: %w", err)
	}
	fs, err := referenceframe.NewFrameSystem("robot", fsCfg.Parts, nil)
	if err != nil {
		return fmt.Errorf("building frame system: %w", err)
	}

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
	}
	if raw, err := json.Marshal(fsCfg.Parts); err == nil {
		sess.FrameSystem = raw
	}
	if err := writeSession(*rawDir, sess); err != nil {
		return err
	}

	// The pin's shutdown uses a fresh context so the write still lands when ctx
	// is already cancelled — the same invariant pulseIcePin holds in the service.
	pinIsHigh := false
	closePin := func() {
		if !pinIsHigh {
			return
		}
		if err := pin.Set(context.Background(), false, nil); err != nil {
			fmt.Fprintf(os.Stderr, "\nWARNING: failed to close ice pin %q: %v\n", *pinName, err)
			fmt.Fprintf(os.Stderr, "CLOSE IT BY HAND before leaving the machine.\n")
			return
		}
		pinIsHigh = false
	}
	defer closePin()

	capturer := &dispenseCapture{
		dir: *rawDir, label: *label, slug: labelSlug(*label),
		cam: cam, arm: robotArm, fs: fs, armName: *armName,
		camFrame: camFrameName, gripFrame: *gripFrame,
		sources: splitSources(*sources), withCloud: *withCloud, cropMm: *rawCrop,
		logger: logger,
	}

	interval := time.Duration(float64(time.Second) / *hz)
	fmt.Printf("pre-roll %.0fs, dwell %.0fs, post-roll %.0fs at %.1fHz -> ~%d frames\n",
		*preSec, *dwellSec, *postSec, *hz, int((*preSec+*dwellSec+*postSec)**hz))
	fmt.Printf("pin %q on board %q\n\n", *pinName, *boardName)

	// Pre-roll: the baseline the trace is read against. t is negative here.
	openAt := time.Now().Add(time.Duration(*preSec * float64(time.Second)))
	if err := capturer.run(ctx, interval, openAt, openAt, "pre"); err != nil {
		return err
	}

	if err := pin.Set(ctx, true, nil); err != nil {
		return fmt.Errorf("opening ice pin %q: %w", *pinName, err)
	}
	pinIsHigh = true
	openedAt := time.Now()
	fmt.Printf("  pin HIGH\n")

	dwellErr := capturer.run(ctx, interval, openedAt.Add(time.Duration(*dwellSec*float64(time.Second))), openedAt, "dwell")
	closePin()
	fmt.Printf("  pin LOW\n")
	if dwellErr != nil {
		return dwellErr
	}

	// Post-roll answers whether the pile keeps moving after the pin closes,
	// which is what decides if a settle delay belongs in the loop.
	if *postSec > 0 {
		if err := capturer.run(ctx, interval,
			time.Now().Add(time.Duration(*postSec*float64(time.Second))), openedAt, "post"); err != nil {
			return err
		}
	}

	fmt.Printf("\n%d frames in %s\n", capturer.count, *rawDir)
	fmt.Printf("analyze with: beanjamin-cli ice-level --raw-dir %s --series\n", *rawDir)
	return nil
}

// dispenseCapture holds the per-run context the capture loop needs.
type dispenseCapture struct {
	dir, label, slug    string
	cam                 camera.Camera
	arm                 arm.Arm
	fs                  *referenceframe.FrameSystem
	armName             string
	camFrame, gripFrame string
	sources             []string
	withCloud           bool
	cropMm              float64
	logger              logging.Logger
	count               int
}

// run captures until deadline, stamping each frame with its offset from origin.
func (d *dispenseCapture) run(ctx context.Context, interval time.Duration, deadline, origin time.Time, phase string) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if time.Now().After(deadline) {
			return nil
		}
		if err := d.capture(ctx, origin, phase); err != nil {
			return err
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("interrupted during %s phase", phase)
		}
	}
}

func (d *dispenseCapture) capture(ctx context.Context, origin time.Time, phase string) error {
	offset := time.Since(origin)
	joints, err := d.arm.JointPositions(ctx, nil)
	if err != nil {
		return fmt.Errorf("getting joint positions: %w", err)
	}
	inputs := referenceframe.NewZeroInputs(d.fs)
	inputs[d.armName] = joints
	camToWorld, err := frameToWorld(d.fs, inputs, d.camFrame)
	if err != nil {
		return err
	}
	gripToWorld, err := frameToWorld(d.fs, inputs, d.gripFrame)
	if err != nil {
		return err
	}

	images, _, err := d.cam.Images(ctx, d.sources, nil)
	if err != nil {
		return fmt.Errorf("capturing images: %w", err)
	}
	// Depth is unusable at this pose, so the cloud is opt-in: skipping it is what
	// lets the loop actually hit its requested rate.
	var cloud pointcloud.PointCloud
	if d.withCloud {
		if cloud, err = d.cam.NextPointCloud(ctx, nil); err != nil {
			return fmt.Errorf("capturing point cloud: %w", err)
		}
	}

	ms := offset.Milliseconds()
	frame, err := saveRawFrame(d.dir, cloud, images, frameMeta{
		Label: d.label, Slug: d.slug, Truth: -1,
		ArmName: d.armName, Joints: inputsToFloats(joints),
		GripPose: gripToWorld, CamToWorld: camToWorld,
		CropMm: d.cropMm, Index: d.count,
		TMs: &ms, Phase: phase,
	})
	if err != nil {
		return err
	}
	d.count++
	saved := "(no image)"
	if len(frame.Images) > 0 {
		saved = frame.Images[0]
	}
	fmt.Printf("  %+7dms  %-5s  %s\n", ms, phase, saved)
	return nil
}

// splitSources parses the comma-separated --sources list. Empty means "all".
func splitSources(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
