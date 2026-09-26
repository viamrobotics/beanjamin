// Package icevision measures the ice level in a held glass from the arm-mounted
// camera: the contrast step that decides a watched dispense, the brightness
// shadow logged beside it, the check_ice_level report, and the annotated frames
// saved for tuning. It reads frames and pixels only; driving the ice pin is the
// coffee service's job.
package icevision

import (
	"go.viam.com/rdk/components/camera"
)

// Params are the measurement's tunables as configured. A zero field means
// unset and falls back to its default, except BrightnessThresh, whose zero is
// the shadow's off switch.
type Params struct {
	StopRowPx        int     // ice_stop_row_px
	ContrastWindow   int     // ice_contrast_window
	MinContrast      float64 // ice_min_contrast
	ROIX0            int     // ice_roi_x0
	ROIX1            int     // ice_roi_x1
	ROIY1            int     // ice_roi_y1
	BrightnessThresh float64 // ice_brightness_thresh
	BrightRun        int     // ice_bright_run
	// SaveDir is save_motion_requests_dir, which annotated frames are written
	// under; unset, no frame is saved.
	SaveDir string
}

// Detector reads ice-level frames from the arm-mounted camera and measures them.
type Detector struct {
	cam camera.Camera
	p   Params
}

// NewDetector returns a Detector reading cam. A nil cam is allowed: every
// measurement then fails with "no source camera".
func NewDetector(cam camera.Camera, p Params) *Detector {
	return &Detector{cam: cam, p: p}
}

// Params returns the tunables the Detector measures with.
func (d *Detector) Params() Params {
	return d.p
}

func orDefault[T ~int | ~float64](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}
