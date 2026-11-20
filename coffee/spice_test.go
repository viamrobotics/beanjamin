package coffee

import (
	"context"
	"strings"
	"testing"

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

func TestValidate_SpiceOptions_RejectsNegativePourCount(t *testing.T) {
	cfg := validSpiceConfig()
	cfg.SpicePourCount = -1
	if _, _, err := cfg.Validate(""); err == nil || !strings.Contains(err.Error(), "spice_pour_count") {
		t.Errorf("expected a spice_pour_count error, got %v", err)
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
	for _, name := range []string{clawPoseSpiceShakeApproach, clawPoseSpiceShake} {
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
