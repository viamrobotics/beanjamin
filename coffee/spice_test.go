package coffee

import (
	"context"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/spatialmath"

	"beanjamin/coffee/order"
)

var testSpices = []SpiceOption{{Name: "cinnamon", Spot: "a"}, {Name: "nutmeg", Spot: "b"}}

// validSpiceConfig returns a latte Config with spice_options and every field
// they require.
func validSpiceConfig() *Config {
	cfg := validCanServeIcedLatteConfig()
	cfg.SpiceOptions = testSpices
	cfg.SpiceShakerDimensions = &ContainerDimensions{DiameterMm: 45, HeightMm: 110}
	cfg.SpiceShakeApproachRelativePose = &RelativePose{}
	cfg.SpiceShakeRelativePose = &RelativePose{}
	return cfg
}

func TestValidate_SpiceOptions_Valid(t *testing.T) {
	if _, _, err := validSpiceConfig().Validate(""); err != nil {
		t.Fatalf("expected a valid spice config, got %v", err)
	}
}

// No spice_options means no spice is offered, so none of the spice fields are
// required.
func TestValidate_SpiceOptions_OptionalOnLatte(t *testing.T) {
	if _, _, err := validCanServeIcedLatteConfig().Validate(""); err != nil {
		t.Fatalf("a latte config without spices should be valid, got %v", err)
	}
}

func TestValidate_SpiceOptions_RequiresIcedLatte(t *testing.T) {
	cfg := validSpiceConfig()
	cfg.CanServeIcedLatte = false
	if _, _, err := cfg.Validate(""); err == nil || !strings.Contains(err.Error(), "can_serve_iced_latte") {
		t.Errorf("expected a can_serve_iced_latte error, got %v", err)
	}
}

func TestValidate_SpiceOptions_RequiresSpiceFields(t *testing.T) {
	for _, tt := range []struct {
		field string
		clear func(*Config)
	}{
		{"spice_shaker_dimensions", func(c *Config) { c.SpiceShakerDimensions = nil }},
		{"spice_shake_approach_relative_pose", func(c *Config) { c.SpiceShakeApproachRelativePose = nil }},
		{"spice_shake_relative_pose", func(c *Config) { c.SpiceShakeRelativePose = nil }},
	} {
		t.Run(tt.field, func(t *testing.T) {
			cfg := validSpiceConfig()
			tt.clear(cfg)
			if _, _, err := cfg.Validate(""); err == nil || !strings.Contains(err.Error(), tt.field) {
				t.Errorf("expected a %s error, got %v", tt.field, err)
			}
		})
	}
}

// A spice's spot is spliced into its pose names and its name is sent by
// clients, so the same rules as milk_options apply.
func TestValidate_SpiceOptions_RejectsBadOptions(t *testing.T) {
	for _, spices := range [][]SpiceOption{
		{{Name: "Cinnamon", Spot: "a"}},
		{{Name: "cinnamon", Spot: ""}},
		{{Name: "cinnamon", Spot: "a"}, {Name: "cinnamon", Spot: "b"}},
		{{Name: "cinnamon", Spot: "a"}, {Name: "nutmeg", Spot: "a"}},
		{{Name: "cinnamon", Spot: "shake"}}, // spice_shake_approach is the shake pose
	} {
		cfg := validSpiceConfig()
		cfg.SpiceOptions = spices
		if _, _, err := cfg.Validate(""); err == nil || !strings.Contains(err.Error(), "spice_options") {
			t.Errorf("spice_options %+v: expected a spice_options error, got %v", spices, err)
		}
	}
}

func TestValidate_SpiceOptions_RejectsNegativeShake(t *testing.T) {
	cfg := validSpiceConfig()
	cfg.SpiceShakeCount = -1
	if _, _, err := cfg.Validate(""); err == nil || !strings.Contains(err.Error(), "spice_shake_count") {
		t.Errorf("expected a spice_shake_count error, got %v", err)
	}
	cfg = validSpiceConfig()
	cfg.SpiceShakeAmplitudeMm = -5
	if _, _, err := cfg.Validate(""); err == nil || !strings.Contains(err.Error(), "spice_shake_amplitude_mm") {
		t.Errorf("expected a spice_shake_amplitude_mm error, got %v", err)
	}
}

func TestRequiredPosesSpice(t *testing.T) {
	hasPose := func(poses []requiredPose, name string) bool {
		for _, p := range poses {
			if p.poseName == name {
				return true
			}
		}
		return false
	}
	latte := (&beanjaminCoffee{cfg: &Config{CanServeIced: true, CanServeIcedLatte: true, MilkOptions: testMilks}}).requiredPoses()
	if hasPose(latte, spiceApproachPose("a")) {
		t.Error("a latte machine without spice_options should not require spice poses")
	}
	spiced := (&beanjaminCoffee{cfg: &Config{CanServeIced: true, CanServeIcedLatte: true, MilkOptions: testMilks, SpiceOptions: testSpices}}).requiredPoses()
	// Named after the spot, not the spice.
	for _, name := range []string{"spice_a_approach", "spice_a_grab", "spice_b_approach", "spice_b_grab"} {
		if !hasPose(spiced, name) {
			t.Errorf("spice_options should require %q", name)
		}
	}
	for _, name := range []string{clawPoseSpiceShakeApproach, clawPoseSpiceShake, clawPoseSpiceShakeLift} {
		if hasPose(spiced, name) {
			t.Errorf("%q is resolved against the staged glass and must not be required on a switch", name)
		}
	}
}

// spice_options is reported only when spices are configured: its absence is
// the kiosk's cue to hide the spice choice.
func TestStatusReportsSpiceOptions(t *testing.T) {
	s := newStatusService(t, &Config{CanServeIcedLatte: true, MilkOptions: testMilks})
	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if v, ok := st["spice_options"]; ok {
		t.Errorf("spice_options = %v, want absent with no spices configured", v)
	}

	s = newStatusService(t, &Config{CanServeIcedLatte: true, MilkOptions: testMilks, SpiceOptions: testSpices})
	s.queue.Enqueue(order.Order{ID: "o1", Drink: "iced_latte", CustomerName: "Ada", Milk: "oat", Spice: "nutmeg"})
	st, err = s.Status(context.Background())
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	// []any, not []string: structpb rejects typed slices.
	if spices, ok := st["spice_options"].([]any); !ok || len(spices) != 2 || spices[0] != "cinnamon" || spices[1] != "nutmeg" {
		t.Errorf("spice_options = %v (%T), want []any{cinnamon, nutmeg}", st["spice_options"], st["spice_options"])
	}
	if spice := st["orders"].([]any)[0].(map[string]any)["spice"]; spice != "nutmeg" {
		t.Errorf("order spice = %v, want nutmeg", spice)
	}
}

// The top of each shake is the shake pose raised straight up by the amplitude,
// keeping the shaker's tilt.
func TestResolveSpiceShakeLift(t *testing.T) {
	s := newPourTestCoffee(t)
	s.cfg.SpiceShakeRelativePose = &RelativePose{X: 10, Z: 60, OX: 1, OZ: -1}
	s.cfg.SpiceShakeAmplitudeMm = 25
	withStagedGlass(t, s, spatialmath.NewPoseFromPoint(r3.Vector{X: 560, Y: -200, Z: 290}))

	shake, err := s.resolvePose(context.Background(), s.clawsSw, clawPoseSpiceShake)
	if err != nil {
		t.Fatalf("resolve spice_shake: %v", err)
	}
	lift, err := s.resolvePose(context.Background(), s.clawsSw, clawPoseSpiceShakeLift)
	if err != nil {
		t.Fatalf("resolve spice_shake_lift: %v", err)
	}
	if want := shake.pose.Point().Add(r3.Vector{Z: 25}); lift.pose.Point().Sub(want).Norm() > vecTolMm {
		t.Errorf("spice_shake_lift at %v, want %v", lift.pose.Point(), want)
	}
	if !spatialmath.OrientationAlmostEqual(lift.pose.Orientation(), shake.pose.Orientation()) {
		t.Errorf("spice_shake_lift orientation = %v, want the shake's %v", lift.pose.Orientation(), shake.pose.Orientation())
	}
	// Resolving the lift must not edit the configured offset it is built from.
	if s.cfg.SpiceShakeRelativePose.Z != 60 {
		t.Errorf("resolving the lift changed spice_shake_relative_pose.z to %v", s.cfg.SpiceShakeRelativePose.Z)
	}
}
