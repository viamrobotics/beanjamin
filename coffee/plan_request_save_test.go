package coffee

import (
	"errors"
	"os"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
)

func TestStepTag(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"---", ""},
		{"Grinding", "step_grinding"},
		{"Locking portafilter", "step_locking_portafilter"},
		{"Cup flow 1/3", "step_cup_flow_1_3"},
		{"  Serving  ", "step_serving"},
	}
	for _, c := range cases {
		if got := stepTag(c.in); got != c.want {
			t.Errorf("stepTag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPlanRequestTagDir(t *testing.T) {
	cases := []struct {
		name                          string
		orderID, step, label, outcome string
		want                          string
	}{
		{
			name:    "all tags",
			orderID: "oid", step: "Locking portafilter", label: "move", outcome: tagPlanningFailure,
			want: "/base/tag=oid/tag=step_locking_portafilter/tag=motion_move/tag=planning_failure",
		},
		{
			name:    "no order or step",
			orderID: "", step: "", label: "carry", outcome: tagPlanningSuccess,
			want: "/base/tag=motion_carry/tag=planning_success",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planRequestTagDir("/base", c.orderID, c.step, c.label, c.outcome); got != c.want {
				t.Errorf("planRequestTagDir = %q, want %q", got, c.want)
			}
		})
	}
}

func TestBuildPlanRequestDataURL(t *testing.T) {
	if got := buildPlanRequestDataURL("", "oid"); got != "" {
		t.Errorf("empty locationID: got %q, want \"\"", got)
	}
	want := "https://app.viam.com/data/all?locationId=loc1&tags=oid-42&view=files"
	if got := buildPlanRequestDataURL("loc1", "oid-42"); got != want {
		t.Errorf("buildPlanRequestDataURL = %q, want %q", got, want)
	}
}

// minimalPlanRequest builds a PlanRequest that marshals and round-trips through
// the RDK request/response file helpers without needing a real robot.
func minimalPlanRequest() *armplanning.PlanRequest {
	return &armplanning.PlanRequest{
		FrameSystem: referenceframe.NewEmptyFrameSystem("test"),
		StartState:  armplanning.NewPlanState(nil, referenceframe.FrameSystemInputs{}),
	}
}

func TestSavePlanRequestAndResponse_NoOpWhenDirUnset(t *testing.T) {
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t), cfg: &Config{}}
	// Must not panic when no directory is configured.
	c.savePlanRequestAndResponse(minimalPlanRequest(), nil, "move", errors.New("boom"))
}

func TestSavePlanRequestAndResponse_FailureTags(t *testing.T) {
	dir := t.TempDir()
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t), cfg: &Config{SaveMotionRequestsDir: dir}}
	c.currentOrderID.Store("order-123")
	c.currentStep.Store("Locking portafilter")

	c.savePlanRequestAndResponse(minimalPlanRequest(), nil, "move", errors.New("boom"))

	wantDir := planRequestTagDir(dir, "order-123", "Locking portafilter", "move", tagPlanningFailure)
	entries, err := os.ReadDir(wantDir)
	if err != nil {
		t.Fatalf("read tag dir %q: %v", wantDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d files in %q, want 1", len(entries), wantDir)
	}

	// The combined file must read back with the RDK reader; a planning failure
	// carries no plan, so the response is nil.
	path := wantDir + string(os.PathSeparator) + entries[0].Name()
	gotReq, gotPlan, err := armplanning.ReadRequestAndResponseFromFile(path)
	if err != nil {
		t.Fatalf("ReadRequestAndResponseFromFile: %v", err)
	}
	if gotReq == nil {
		t.Error("expected a request in the saved file")
	}
	if gotPlan != nil {
		t.Error("expected no plan for a planning failure")
	}
}

func TestSavePlanRequestAndResponse_SuccessOutcome(t *testing.T) {
	dir := t.TempDir()
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t), cfg: &Config{SaveMotionRequestsDir: dir}}
	c.currentOrderID.Store("order-9")
	c.currentStep.Store("Grinding")

	c.savePlanRequestAndResponse(minimalPlanRequest(), nil, "circular", nil)

	wantDir := planRequestTagDir(dir, "order-9", "Grinding", "circular", tagPlanningSuccess)
	if _, err := os.ReadDir(wantDir); err != nil {
		t.Fatalf("expected success-tagged dir %q: %v", wantDir, err)
	}
}
