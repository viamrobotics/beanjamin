package order

import (
	"fmt"
	"strings"
	"time"
)

// Severities an issue report can carry, least to most severe.
const (
	SeverityMinor    = "minor"
	SeverityMedium   = "medium"
	SeverityCritical = "critical"
)

// IssueOther is the issue key of a freeform report: the customer describes the
// problem and picks its severity.
const IssueOther = "other"

// maxIssueDescriptionLen bounds a freeform description, which comes straight
// from a kiosk text box.
const maxIssueDescriptionLen = 500

// maxOrderIDLen bounds order_id. Order IDs are UUIDs; the bound only keeps a
// malformed request from writing an arbitrarily long value to tabular data.
const maxOrderIDLen = 64

// IssueKind is one issue customers can pick without describing it.
type IssueKind struct {
	Key      string
	Label    string
	Severity string
}

// IssueKinds is the catalog of suggested issues, each with a fixed severity.
// The kiosk mirrors it in web-app/app/order/issues.ts; a key missing there
// cannot be picked, and one missing here is refused.
var IssueKinds = []IssueKind{
	{Key: "small_spill", Label: "Small spill", Severity: SeverityMinor},
	{Key: "large_spill", Label: "Large spill", Severity: SeverityCritical},
	{Key: "surprising_movements", Label: "Surprising movements", Severity: SeverityMinor},
	{Key: "light_collision", Label: "Light collision", Severity: SeverityMedium},
	{Key: "bad_collision", Label: "Bad collision", Severity: SeverityCritical},
	{Key: "empty_cup", Label: "Empty cup", Severity: SeverityMedium},
	{Key: "dropped_cup", Label: "Dropped cup/glass", Severity: SeverityCritical},
}

func issueKind(key string) (IssueKind, bool) {
	for _, k := range IssueKinds {
		if k.Key == key {
			return k, true
		}
	}
	return IssueKind{}, false
}

func validSeverity(s string) bool {
	return s == SeverityMinor || s == SeverityMedium || s == SeverityCritical
}

// IssueRequest is a decoded report_issue command.
type IssueRequest struct {
	// OrderID is the order the issue was seen on: the ID it carries in
	// order-events, or the one being made right now.
	OrderID string
	// Issue is an IssueKinds key or IssueOther.
	Issue       string
	Description string
	Severity    string
}

// ParseIssueRequest decodes the report_issue command's value:
//
//	{"order_id": "<id>", "issue": "<key>"|"other",
//	 "description": "...", "severity": "minor"|"medium"|"critical"}
//
// A suggested issue carries its catalog severity, so severity may be omitted;
// one that disagrees with the catalog is refused rather than silently replaced.
// A freeform report ("other") needs both a description and a severity.
func ParseIssueRequest(raw any) (IssueRequest, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return IssueRequest{}, fmt.Errorf("report_issue must be an object, got %T", raw)
	}
	str := func(key string) (string, error) {
		v, ok := m[key]
		if !ok || v == nil {
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("report_issue.%s must be a string, got %T", key, v)
		}
		return strings.TrimSpace(s), nil
	}
	var req IssueRequest
	var err error
	if req.OrderID, err = str("order_id"); err != nil {
		return IssueRequest{}, err
	}
	if req.Issue, err = str("issue"); err != nil {
		return IssueRequest{}, err
	}
	if req.Description, err = str("description"); err != nil {
		return IssueRequest{}, err
	}
	if req.Severity, err = str("severity"); err != nil {
		return IssueRequest{}, err
	}

	if req.OrderID == "" {
		return IssueRequest{}, fmt.Errorf("report_issue.order_id is required")
	}
	if len(req.OrderID) > maxOrderIDLen {
		return IssueRequest{}, fmt.Errorf("report_issue.order_id is limited to %d characters", maxOrderIDLen)
	}
	if len(req.Description) > maxIssueDescriptionLen {
		return IssueRequest{}, fmt.Errorf("report_issue.description is limited to %d characters", maxIssueDescriptionLen)
	}
	if req.Severity != "" && !validSeverity(req.Severity) {
		return IssueRequest{}, fmt.Errorf("report_issue.severity must be %q, %q or %q, got %q",
			SeverityMinor, SeverityMedium, SeverityCritical, req.Severity)
	}

	if req.Issue == IssueOther {
		if req.Description == "" {
			return IssueRequest{}, fmt.Errorf("report_issue: an %q issue needs a description", IssueOther)
		}
		if req.Severity == "" {
			return IssueRequest{}, fmt.Errorf("report_issue: an %q issue needs a severity", IssueOther)
		}
		return req, nil
	}
	kind, ok := issueKind(req.Issue)
	if !ok {
		return IssueRequest{}, fmt.Errorf("report_issue.issue %q is not a known issue (or %q)", req.Issue, IssueOther)
	}
	if req.Severity != "" && req.Severity != kind.Severity {
		return IssueRequest{}, fmt.Errorf("report_issue: %q is a %s issue, not %s", req.Issue, kind.Severity, req.Severity)
	}
	req.Severity = kind.Severity
	return req, nil
}

// IssueReport is one customer report about an order, handed to the
// order-issue sensor to be captured as tabular data.
type IssueReport struct {
	IssueRequest
	ReportedAt time.Time
}
