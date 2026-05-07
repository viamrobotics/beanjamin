package beanjamin

import (
	"strings"
	"testing"
)

func validBaseConfig() *Config {
	return &Config{
		PoseSwitcherName:      "filter-switch",
		ClawsPoseSwitcherName: "claws-switch",
		ArmName:               "arm",
		GripperName:           "gripper",
	}
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

func TestValidate_DynamicCupPickup_DefaultsMaxDistance(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.CupMaxDistanceFromTargetMm != 300 {
		t.Fatalf("expected default 300mm, got %f", cfg.CupMaxDistanceFromTargetMm)
	}
}

func TestValidate_DynamicCupPickup_PreservesExplicitMaxDistance(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	cfg.CupMaxDistanceFromTargetMm = 500
	if _, _, err := cfg.Validate(""); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.CupMaxDistanceFromTargetMm != 500 {
		t.Fatalf("expected 500mm preserved, got %f", cfg.CupMaxDistanceFromTargetMm)
	}
}

func TestValidate_DynamicCupPickup_RejectsNegativeRetries(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	cfg.CupDetectionRetries = -1
	_, _, err := cfg.Validate("")
	if err == nil || !strings.Contains(err.Error(), "cup_detection_retries") {
		t.Fatalf("expected cup_detection_retries error, got %v", err)
	}
}

func TestValidate_DynamicCupPickup_AppendsDeps(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DynamicCupPickup = true
	cfg.CupVisionServiceName = "vis"
	cfg.SrcCameraName = "cam"
	cfg.ExpectedCupPositionMm = &Vec3Mm{}
	req, _, err := cfg.Validate("")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	var sawVision, sawCamera bool
	for _, d := range req {
		if strings.Contains(d, "vis") {
			sawVision = true
		}
		if strings.Contains(d, "cam") {
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
