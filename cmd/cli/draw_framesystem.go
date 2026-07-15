package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	viz "github.com/viam-labs/motion-tools/client/api"
	"go.viam.com/rdk/referenceframe"
)

// runDrawFrameSystem reads a serialized referenceframe.FrameSystem from a JSON
// file — e.g. a <timestamp>_<cup|glass>_framesystem.json snapshot written by the
// coffee service to its save_motion_requests_dir — and draws it to a local
// motion-tools visualizer. No machine connection is needed: it talks to the
// visualizer directly over HTTP on localhost:3030 (v2 default).
//
// The snapshot is a pure frame system with no joint inputs, so it is drawn at
// zero inputs: world-parented frames (including the cup/glass geometries) render
// at their true positions; the arm renders at its home configuration.
func runDrawFrameSystem(args []string) error {
	flagSet := flag.NewFlagSet("draw-framesystem", flag.ExitOnError)
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	path := flagSet.Arg(0)
	if path == "" {
		return fmt.Errorf("usage: beanjamin-cli draw-framesystem <framesystem.json>")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}

	var fs referenceframe.FrameSystem
	if err := json.Unmarshal(data, &fs); err != nil {
		return fmt.Errorf("parse frame system from %q: %w", path, err)
	}

	if _, err := viz.DrawFrameSystem(viz.DrawFrameSystemOptions{
		FrameSystem: &fs,
		Inputs:      referenceframe.NewZeroInputs(&fs),
	}); err != nil {
		return fmt.Errorf("draw frame system: %w", err)
	}

	fmt.Printf("drew frame system %q (%d frames) from %s\n", fs.Name(), len(fs.FrameNames()), path)
	return nil
}
