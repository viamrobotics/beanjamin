package coffee

import (
	"slices"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
)

// TestContainerDimensionsBoxDims verifies the held-item box extents: a square
// footprint of the diameter, and the container's height.
func TestContainerDimensionsBoxDims(t *testing.T) {
	d := &ContainerDimensions{DiameterMm: 70, HeightMm: 140}
	if got, want := d.boxDims(), (r3.Vector{X: 70, Y: 70, Z: 140}); got != want {
		t.Errorf("boxDims() = %v, want %v", got, want)
	}
}

func TestOrDefault(t *testing.T) {
	if got := orDefault(0, 5); got != 5 {
		t.Errorf("orDefault(0, 5) = %d, want 5", got)
	}
	if got := orDefault(3, 5); got != 3 {
		t.Errorf("orDefault(3, 5) = %d, want 3", got)
	}
	if got := orDefault(-2, 5); got != 5 {
		t.Errorf("orDefault(-2, 5) = %d, want 5 (non-positive falls to default)", got)
	}
	if got := orDefault(2.5, 1.0); got != 2.5 {
		t.Errorf("orDefault(2.5, 1.0) = %v, want 2.5", got)
	}
}

func validMugDrinksConfig() *Config {
	cfg := validBaseConfig()
	cfg.CanServeMugDrinks = true
	cfg.MugVisionServiceName = "mug-vis"
	cfg.MugObservePoseSwitcherName = "mug-observe-switch"
	cfg.MugApproachRelativePose = &RelativePose{}
	cfg.MugGrabRelativePose = &RelativePose{}
	cfg.MugDimensions = &ContainerDimensions{DiameterMm: 80, HeightMm: 97}
	cfg.MugHandleDimensions = &HandleDimensions{DepthMm: 39, WidthMm: 15}
	return cfg
}

func TestValidateMugDrinksRequiresFields(t *testing.T) {
	for field, clear := range map[string]func(*Config){
		"mug_vision_service_name":        func(c *Config) { c.MugVisionServiceName = "" },
		"mug_observe_pose_switcher_name": func(c *Config) { c.MugObservePoseSwitcherName = "" },
		"mug_approach_relative_pose":     func(c *Config) { c.MugApproachRelativePose = nil },
		"mug_grab_relative_pose":         func(c *Config) { c.MugGrabRelativePose = nil },
		"mug_dimensions":                 func(c *Config) { c.MugDimensions = nil },
		"mug_handle_dimensions":          func(c *Config) { c.MugHandleDimensions = nil },
	} {
		t.Run(field, func(t *testing.T) {
			cfg := validMugDrinksConfig()
			clear(cfg)
			_, _, err := cfg.Validate("path")
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("want error naming %q, got %v", field, err)
			}
		})
	}

	deps, _, err := validMugDrinksConfig().Validate("path")
	if err != nil {
		t.Fatalf("complete mug config: %v", err)
	}
	for _, want := range []string{"rdk:service:vision/mug-vis", "mug-observe-switch"} {
		if !slices.Contains(deps, want) {
			t.Errorf("deps %v missing %q", deps, want)
		}
	}

	if _, _, err := validBaseConfig().Validate("path"); err != nil {
		t.Fatalf("flag off needs no mug fields: %v", err)
	}
}

func TestHandleDimensionsValidate(t *testing.T) {
	var nilDims *HandleDimensions
	for name, h := range map[string]*HandleDimensions{
		"nil":        nilDims,
		"zero depth": {DepthMm: 0, WidthMm: 15},
		"zero width": {DepthMm: 39, WidthMm: 0},
	} {
		if err := h.validate("path", "mug_handle_dimensions"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if err := (&HandleDimensions{DepthMm: 39, WidthMm: 15}).validate("path", "mug_handle_dimensions"); err != nil {
		t.Errorf("valid dims: %v", err)
	}
}

// With mug drinks on, the shelf retreat must be level with the serving grab:
// the open jaws leave a released mug and must not catch its handle. The
// retreat is checked when one is set, the approach otherwise; with mug drinks
// off, a raised approach is fine.
func TestValidateServingRetreatLevelForMugs(t *testing.T) {
	cfg := validMugDrinksConfig()
	cfg.ServingGrabRelativePose = &RelativePose{Y: 20, Z: 4}
	cfg.ServingApproachRelativePose = &RelativePose{Y: -155, Z: 25}
	_, _, err := cfg.Validate("path")
	if err == nil || !strings.Contains(err.Error(), "serving_retreat_relative_pose") {
		t.Fatalf("no retreat and a raised approach: want an error naming the retreat field, got %v", err)
	}

	cfg.ServingRetreatRelativePose = &RelativePose{Y: -155, Z: 4.3}
	if _, _, err := cfg.Validate("path"); err != nil {
		t.Fatalf("retreat level with the grab: %v", err)
	}

	cfg.ServingRetreatRelativePose = &RelativePose{Y: -155, Z: 10}
	if _, _, err := cfg.Validate("path"); err == nil || !strings.Contains(err.Error(), "serving_retreat_relative_pose.z (10.0)") {
		t.Fatalf("retreat above the grab: want an error, got %v", err)
	}

	base := validBaseConfig()
	base.ServingGrabRelativePose = &RelativePose{Y: 20, Z: 4}
	base.ServingApproachRelativePose = &RelativePose{Y: -155, Z: 25}
	if _, _, err := base.Validate("path"); err != nil {
		t.Fatalf("mug drinks off: a raised approach needs no retreat: %v", err)
	}
}

func TestValidateMugApproachLevelWithGrab(t *testing.T) {
	cfg := validMugDrinksConfig()
	cfg.MugGrabRelativePose = &RelativePose{X: -10, Z: 12}
	cfg.MugApproachRelativePose = &RelativePose{X: -120, Z: 12.4}
	if _, _, err := cfg.Validate("path"); err != nil {
		t.Fatalf("approach within 0.5 mm of the grab: %v", err)
	}
	cfg.MugApproachRelativePose = &RelativePose{X: -120, Z: 30}
	_, _, err := cfg.Validate("path")
	if err == nil || !strings.Contains(err.Error(), "mug_approach_relative_pose.z") || !strings.Contains(err.Error(), "mug_grab_relative_pose.z") {
		t.Fatalf("want an error naming both z fields, got %v", err)
	}
}
