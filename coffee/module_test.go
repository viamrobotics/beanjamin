package coffee

import (
	"strings"
	"testing"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/services/vision"
)

// validBaseConfig returns a Config with every always-required field populated to
// a valid (zero-value) entry. Cup pickup is always vision-driven, so the
// cup-vision fields are part of the baseline; success-path tests can flip a
// single field to exercise that branch alone.
func validBaseConfig() *Config {
	return &Config{
		PoseSwitcherName:              "filter-switch",
		ClawsPoseSwitcherName:         "claws-switch",
		ArmName:                       "arm",
		GripperName:                   "gripper",
		CupVisionServiceName:          "vis",
		SrcCameraName:                 "cam",
		CameraObservePoseSwitcherName: "observe-switch",
		CupApproachRelativePose:       &RelativePose{},
		CupGrabRelativePose:           &RelativePose{},
		ServingApproachRelativePose:   &RelativePose{},
		ServingGrabRelativePose:       &RelativePose{},
	}
}

// validCanServeIcedConfig returns a Config with every field required by Validate
// when CanServeIced=true populated to a valid entry. Iced coffee also fetches a
// glass via vision, so it adds the ice + glass-vision fields on top of the base
// config.
func validCanServeIcedConfig() *Config {
	cfg := validBaseConfig()
	cfg.CanServeIced = true
	cfg.IceDispenseBoardName = "ice-board"
	cfg.IceDispensePinName = "ice-pin"
	cfg.GlassVisionServiceName = "glass-vis"
	cfg.GlassObservePoseSwitcherName = "glass-observe-switch"
	cfg.GlassApproachRelativePose = &RelativePose{}
	cfg.GlassGrabRelativePose = &RelativePose{}
	return cfg
}

func TestValidate_BaseConfig_Valid(t *testing.T) {
	cfg := validBaseConfig()
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_RequiresCupVisionServiceName(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CupVisionServiceName = ""
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_vision_service_name") {
		t.Fatalf("expected cup_vision_service_name required error, got %v", err)
	}
}

func TestValidate_RequiresSrcCameraName(t *testing.T) {
	cfg := validBaseConfig()
	cfg.SrcCameraName = ""
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "src_camera_name") {
		t.Fatalf("expected src_camera_name required error, got %v", err)
	}
}

func TestValidate_RequiresCameraObservePoseSwitcher(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CameraObservePoseSwitcherName = ""
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "camera_observe_pose_switcher_name") {
		t.Fatalf("expected camera_observe_pose_switcher_name required error, got %v", err)
	}
}

func TestValidate_RequiresCupApproachRelativePose(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CupApproachRelativePose = nil
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_approach_relative_pose") {
		t.Fatalf("expected cup_approach_relative_pose required error, got %v", err)
	}
}

func TestValidate_RequiresCupGrabRelativePose(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CupGrabRelativePose = nil
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_grab_relative_pose") {
		t.Fatalf("expected cup_grab_relative_pose required error, got %v", err)
	}
}

func TestValidate_RequiresServingApproachRelativePose(t *testing.T) {
	cfg := validBaseConfig()
	cfg.ServingApproachRelativePose = nil
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "serving_approach_relative_pose") {
		t.Fatalf("expected serving_approach_relative_pose required error, got %v", err)
	}
}

func TestValidate_RequiresServingGrabRelativePose(t *testing.T) {
	cfg := validBaseConfig()
	cfg.ServingGrabRelativePose = nil
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "serving_grab_relative_pose") {
		t.Fatalf("expected serving_grab_relative_pose required error, got %v", err)
	}
}

func TestValidate_RejectsNegativePhotosPerVantage(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CupPhotosPerVantage = -1
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_photos_per_vantage") {
		t.Fatalf("expected cup_photos_per_vantage error, got %v", err)
	}
}

func TestValidate_RejectsNegativeMaxAttempts(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CupPickupMaxAttempts = -1
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_pickup_max_attempts") {
		t.Fatalf("expected cup_pickup_max_attempts error, got %v", err)
	}
}

func TestValidate_AcceptsCupAndGlassDimensions(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CupDimensions = &ContainerDimensions{DiameterMm: 80, HeightMm: 95}
	cfg.GlassDimensions = &ContainerDimensions{DiameterMm: 70, HeightMm: 140}
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_RejectsNonPositiveCupDiameter(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CupDimensions = &ContainerDimensions{DiameterMm: 0, HeightMm: 95}
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_dimensions.diameter_mm") {
		t.Fatalf("expected cup_dimensions.diameter_mm error, got %v", err)
	}
}

func TestValidate_RejectsNonPositiveCupHeight(t *testing.T) {
	cfg := validBaseConfig()
	cfg.CupDimensions = &ContainerDimensions{DiameterMm: 80, HeightMm: -1}
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_dimensions.height_mm") {
		t.Fatalf("expected cup_dimensions.height_mm error, got %v", err)
	}
}

func TestValidate_RejectsNonPositiveGlassDimensions(t *testing.T) {
	cfg := validBaseConfig()
	cfg.GlassDimensions = &ContainerDimensions{DiameterMm: 70, HeightMm: 0}
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "glass_dimensions.height_mm") {
		t.Fatalf("expected glass_dimensions.height_mm error, got %v", err)
	}
}

func TestValidate_AppendsCupDeps(t *testing.T) {
	cfg := validBaseConfig()
	req, _, err := cfg.Validate("")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	wantVis := vision.Named("vis").String()
	wantCam := camera.Named("cam").String()
	var sawVision, sawCamera, sawObserveSwitch bool
	for _, d := range req {
		if d == wantVis {
			sawVision = true
		}
		if d == wantCam {
			sawCamera = true
		}
		if d == "observe-switch" {
			sawObserveSwitch = true
		}
	}
	if !sawVision {
		t.Fatalf("expected vision dep in required deps, got %v", req)
	}
	if !sawCamera {
		t.Fatalf("expected camera dep in required deps, got %v", req)
	}
	if !sawObserveSwitch {
		t.Fatalf("expected cup observe switch dep in required deps, got %v", req)
	}
}

func TestValidate_CanServeIced_Valid(t *testing.T) {
	cfg := validCanServeIcedConfig()
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_CanServeIced_RequiresIceBoardName(t *testing.T) {
	cfg := validCanServeIcedConfig()
	cfg.IceDispenseBoardName = ""
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "ice_board_name") {
		t.Fatalf("expected ice_board_name required error, got %v", err)
	}
}

func TestValidate_CanServeIced_RequiresIcePinName(t *testing.T) {
	cfg := validCanServeIcedConfig()
	cfg.IceDispensePinName = ""
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "ice_pin_name") {
		t.Fatalf("expected ice_pin_name required error, got %v", err)
	}
}

func TestValidate_CanServeIced_RequiresGlassVisionServiceName(t *testing.T) {
	cfg := validCanServeIcedConfig()
	cfg.GlassVisionServiceName = ""
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "glass_vision_service_name") {
		t.Fatalf("expected glass_vision_service_name required error, got %v", err)
	}
}

func TestValidate_CanServeIced_RequiresGlassObserveSwitcher(t *testing.T) {
	cfg := validCanServeIcedConfig()
	cfg.GlassObservePoseSwitcherName = ""
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "glass_observe_pose_switcher_name") {
		t.Fatalf("expected glass_observe_pose_switcher_name required error, got %v", err)
	}
}

func TestValidate_CanServeIced_RequiresGlassApproachRelativePose(t *testing.T) {
	cfg := validCanServeIcedConfig()
	cfg.GlassApproachRelativePose = nil
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "glass_approach_relative_pose") {
		t.Fatalf("expected glass_approach_relative_pose required error, got %v", err)
	}
}

func TestParseCupFlowCount(t *testing.T) {
	cases := []struct {
		name    string
		in      interface{}
		want    int
		wantErr bool
	}{
		{"number", float64(5), 5, false},
		{"true means one", true, 1, false},
		{"zero rejected", float64(0), 0, true},
		{"negative rejected", float64(-2), 0, true},
		{"large count ok", float64(500), 500, false},
		{"wrong type rejected", "3", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCupFlowCount(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got count %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}
