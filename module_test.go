package beanjamin

import (
	"strings"
	"testing"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/services/vision"
)

func validBaseConfig() *Config {
	return &Config{
		PoseSwitcherName:      "filter-switch",
		ClawsPoseSwitcherName: "claws-switch",
		ArmName:               "arm",
		GripperName:           "gripper",
	}
}

// validDynamicConfig returns a Config with every field required by Validate
// when DynamicCupPickup=true populated to a valid (zero-value) entry, so
// success-path tests can flip a single field to exercise that branch alone.
func validDynamicConfig() *Config {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	cfg.CupApproachRelativePose = &RelativePose{}
	cfg.CupGrabRelativePose = &RelativePose{}
	return cfg
}

func TestValidate_DynamicCupPickup_OffLeavesUnsetFieldsAlone(t *testing.T) {
	cfg := validBaseConfig()
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_RequiresVisionServiceName(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_vision_service_name") {
		t.Fatalf("expected cup_vision_service_name required error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_RequiresSrcCameraName(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "src_camera_name") {
		t.Fatalf("expected src_camera_name required error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_RequiresExpectedCupPosition(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "expected_cup_position_mm") {
		t.Fatalf("expected expected_cup_position_mm required error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_RequiresApproachRelativePose(t *testing.T) {
	cfg := validDynamicConfig()
	cfg.CupApproachRelativePose = nil
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_approach_relative_pose") {
		t.Fatalf("expected cup_approach_relative_pose required error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_RequiresGrabRelativePose(t *testing.T) {
	cfg := validDynamicConfig()
	cfg.CupGrabRelativePose = nil
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_grab_relative_pose") {
		t.Fatalf("expected cup_grab_relative_pose required error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_DefaultsMaxDistance(t *testing.T) {
	cfg := validDynamicConfig()
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.CupMaxDistanceFromTargetMm != 300 {
		t.Fatalf("expected default 300mm, got %f", cfg.CupMaxDistanceFromTargetMm)
	}
}

func TestValidate_DynamicCupPickup_PreservesExplicitMaxDistance(t *testing.T) {
	cfg := validDynamicConfig()
	cfg.CupMaxDistanceFromTargetMm = 500
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.CupMaxDistanceFromTargetMm != 500 {
		t.Fatalf("expected 500mm preserved, got %f", cfg.CupMaxDistanceFromTargetMm)
	}
}

func TestValidate_DynamicCupPickup_RejectsNegativeRetries(t *testing.T) {
	cfg := validDynamicConfig()
	cfg.CupDetectionRetries = -1
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_detection_retries") {
		t.Fatalf("expected cup_detection_retries error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_RejectsNegativeMaxAttempts(t *testing.T) {
	cfg := validDynamicConfig()
	cfg.CupPickupMaxAttempts = -1
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_pickup_max_attempts") {
		t.Fatalf("expected cup_pickup_max_attempts error, got %v", err)
	}
}

func TestValidate_PlaceCupOnShelf_RequiresDynamicCupPickup(t *testing.T) {
	cfg := validBaseConfig()
	cfg.PlaceCupOnShelf = true
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "place_cup_on_shelf") || !strings.Contains(err.Error(), "dynamic_cup_pickup") {
		t.Fatalf("expected place_cup_on_shelf requires dynamic_cup_pickup error, got %v", err)
	}
}

func TestValidate_PlaceCupOnShelf_AcceptedWithDynamicCupPickup(t *testing.T) {
	cfg := validDynamicConfig()
	cfg.PlaceCupOnShelf = true
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_AppendsDeps(t *testing.T) {
	cfg := validDynamicConfig()
	req, _, err := cfg.Validate("")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	wantVis := vision.Named("vis").String()
	wantCam := camera.Named("cam").String()
	var sawVision, sawCamera bool
	for _, d := range req {
		if d == wantVis {
			sawVision = true
		}
		if d == wantCam {
			sawCamera = true
		}
	}
	if !sawVision {
		t.Fatalf("expected vision dep in required deps, got %v", req)
	}
	if !sawCamera {
		t.Fatalf("expected camera dep in required deps, got %v", req)
	}
}
