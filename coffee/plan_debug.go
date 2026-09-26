package coffee

// Plan persistence for offline debugging: each motion plan's request/response
// pair written under save_motion_requests_dir, nested in tag directories the
// Viam data manager turns into data-page tags.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
)

// Planning-outcome tag values for synced plan-request files (see planRequestTagDir).
const (
	tagPlanningSuccess = "planning_success"
	tagPlanningFailure = "planning_failure"
)

// savePlanRequestAndResponse persists a PlanRequest together with the plan it
// produced — nil on a planning failure — to a single JSON file, using RDK's
// WriteRequestAndResponseToFile so the pair round-trips through
// ReadRequestAndResponseFromFile. It is a no-op when SaveMotionRequestsDir is
// empty.
func (s *beanjaminCoffee) savePlanRequestAndResponse(req *armplanning.PlanRequest, plan motionplan.Plan, label string, planErr error) {
	logger := s.activeOrderLogger()
	dir := s.cfg.SaveMotionRequestsDir
	if dir == "" {
		return
	}
	outcome := tagPlanningSuccess
	if planErr != nil {
		outcome = tagPlanningFailure
	}
	orderID := s.queue.CurrentID()
	step, _ := s.currentStep.Load().(string)
	tagDir := planRequestTagDir(dir, orderID, step, label, outcome)
	if err := os.MkdirAll(tagDir, 0o755); err != nil {
		logger.Warnf("save plan request: create dir: %v", err)
		return
	}
	filename := filepath.Join(tagDir, fmt.Sprintf("%s_%s.json", time.Now().Format("20060102_150405.000"), label))
	if err := req.WriteRequestAndResponseToFile(filename, plan); err != nil {
		logger.Warnf("save plan request: %v", err)
		return
	}
	logger.Infof("saved plan request+response (%s) to %s", outcome, filename)
}

// planRequestTagDir nests the file under tag=<value> directories — order ID,
// step, motion label, and planning outcome — which the Viam data manager reads
// on sync to tag the uploaded file (see inferTagsAndDatasetIDsFromPath), making
// it filterable on the data page. Empty values (e.g. a plan issued outside an
// order) are skipped.
//
// These tag values are a cross-repo contract: the web app's motion-plan panel
// (web-app/app/home/data.ts, loadPlanRequestsForOrder) parses the "step_",
// "motion_" and "planning_" prefixes to group and label plans. It deploys
// separately, so renaming a prefix here empties that panel with no build
// failure on either side.
func planRequestTagDir(baseDir, orderID, step, label, outcome string) string {
	parts := []string{baseDir}
	for _, tag := range []string{orderID, stepTag(step), "motion_" + label, outcome} {
		if tag == "" {
			continue
		}
		parts = append(parts, "tag="+tag)
	}
	return filepath.Join(parts...)
}

// stepTag slugifies a step label ("Locking portafilter") into a tag-safe token
// ("step_locking_portafilter"), or "" when there is no active step.
func stepTag(step string) string {
	var b strings.Builder
	pendingUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(step)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingUnderscore && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingUnderscore = false
			b.WriteRune(r)
			continue
		}
		pendingUnderscore = true
	}
	if b.Len() == 0 {
		return ""
	}
	return "step_" + b.String()
}
