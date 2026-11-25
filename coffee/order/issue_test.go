package order

import (
	"strings"
	"testing"
)

func TestParseIssueRequest(t *testing.T) {
	tests := []struct {
		name         string
		raw          any
		wantSeverity string
		wantErr      string
	}{
		{
			name:         "suggested issue takes its catalog severity",
			raw:          map[string]any{"order": "previous", "issue": "large_spill"},
			wantSeverity: SeverityCritical,
		},
		{
			name:         "suggested issue with its own severity",
			raw:          map[string]any{"order": "current", "issue": "small_spill", "severity": "minor"},
			wantSeverity: SeverityMinor,
		},
		{
			name:    "suggested issue with a conflicting severity",
			raw:     map[string]any{"order": "current", "issue": "small_spill", "severity": "critical"},
			wantErr: "is a minor issue",
		},
		{
			name:         "freeform issue",
			raw:          map[string]any{"order": "current", "issue": "other", "description": " foam everywhere ", "severity": "medium"},
			wantSeverity: SeverityMedium,
		},
		{
			name:    "freeform issue without a description",
			raw:     map[string]any{"order": "current", "issue": "other", "severity": "medium"},
			wantErr: "needs a description",
		},
		{
			name:    "freeform issue without a severity",
			raw:     map[string]any{"order": "current", "issue": "other", "description": "x"},
			wantErr: "needs a severity",
		},
		{
			name:    "unknown severity",
			raw:     map[string]any{"order": "current", "issue": "other", "description": "x", "severity": "huge"},
			wantErr: "severity must be",
		},
		{
			name:    "unknown issue",
			raw:     map[string]any{"order": "current", "issue": "sad"},
			wantErr: "not a known issue",
		},
		{
			name:    "unknown order",
			raw:     map[string]any{"order": "next", "issue": "small_spill"},
			wantErr: "report_issue.order",
		},
		{
			name:    "description too long",
			raw:     map[string]any{"order": "current", "issue": "other", "severity": "minor", "description": strings.Repeat("a", maxIssueDescriptionLen+1)},
			wantErr: "limited to",
		},
		{
			name:    "not an object",
			raw:     "small_spill",
			wantErr: "must be an object",
		},
		{
			name:    "wrong field type",
			raw:     map[string]any{"order": "current", "issue": 3.0},
			wantErr: "must be a string",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseIssueRequest(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if req.Severity != tc.wantSeverity {
				t.Fatalf("severity: want %q got %q", tc.wantSeverity, req.Severity)
			}
		})
	}
}

func TestParseIssueRequest_TrimsDescription(t *testing.T) {
	req, err := ParseIssueRequest(map[string]any{
		"order": "current", "issue": "other", "description": "  foam  ", "severity": "minor",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Description != "foam" {
		t.Fatalf("description: want %q got %q", "foam", req.Description)
	}
}

func TestQueue_Reportable(t *testing.T) {
	q := NewQueue()
	if c, p := q.Reportable(); c != nil || p != nil {
		t.Fatalf("empty queue: want no reportable orders, got %v %v", c, p)
	}

	q.Enqueue(Order{ID: "a"})
	q.Enqueue(Order{ID: "b"})
	q.Start()
	if c, p := q.Reportable(); c == nil || c.ID != "a" || p != nil {
		t.Fatalf("first order running: got current=%v previous=%v", c, p)
	}

	q.Complete()
	q.Start()
	c, p := q.Reportable()
	if c == nil || c.ID != "b" || p == nil || p.ID != "a" {
		t.Fatalf("second order running: got current=%v previous=%v", c, p)
	}
	if p.CompletedAt.IsZero() {
		t.Fatal("previous order should carry its completion time")
	}

	// A wipe still leaves the drink that was made reportable.
	q.Clear()
	if c, p := q.Reportable(); c != nil || p == nil || p.ID != "a" {
		t.Fatalf("after Clear: got current=%v previous=%v", c, p)
	}
}
