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
			raw:          map[string]any{"order_id": "o1", "issue": "large_spill"},
			wantSeverity: SeverityCritical,
		},
		{
			name:         "suggested issue with its own severity",
			raw:          map[string]any{"order_id": "o1", "issue": "small_spill", "severity": "minor"},
			wantSeverity: SeverityMinor,
		},
		{
			name:         "suggested issue with a note",
			raw:          map[string]any{"order_id": "o1", "issue": "empty_cup", "description": "only foam"},
			wantSeverity: SeverityMedium,
		},
		{
			name:    "suggested issue with a conflicting severity",
			raw:     map[string]any{"order_id": "o1", "issue": "small_spill", "severity": "critical"},
			wantErr: "is a minor issue",
		},
		{
			name:         "freeform issue",
			raw:          map[string]any{"order_id": "o1", "issue": "other", "description": " foam everywhere ", "severity": "medium"},
			wantSeverity: SeverityMedium,
		},
		{
			name:    "freeform issue without a description",
			raw:     map[string]any{"order_id": "o1", "issue": "other", "severity": "medium"},
			wantErr: "needs a description",
		},
		{
			name:    "freeform issue without a severity",
			raw:     map[string]any{"order_id": "o1", "issue": "other", "description": "x"},
			wantErr: "needs a severity",
		},
		{
			name:    "unknown severity",
			raw:     map[string]any{"order_id": "o1", "issue": "other", "description": "x", "severity": "huge"},
			wantErr: "severity must be",
		},
		{
			name:    "unknown issue",
			raw:     map[string]any{"order_id": "o1", "issue": "sad"},
			wantErr: "not a known issue",
		},
		{
			name:    "missing order_id",
			raw:     map[string]any{"issue": "small_spill"},
			wantErr: "order_id is required",
		},
		{
			name:    "order_id too long",
			raw:     map[string]any{"order_id": strings.Repeat("x", maxOrderIDLen+1), "issue": "small_spill"},
			wantErr: "order_id is limited",
		},
		{
			name:    "description too long",
			raw:     map[string]any{"order_id": "o1", "issue": "other", "severity": "minor", "description": strings.Repeat("a", maxIssueDescriptionLen+1)},
			wantErr: "limited to",
		},
		{
			name:    "not an object",
			raw:     "small_spill",
			wantErr: "must be an object",
		},
		{
			name:    "wrong field type",
			raw:     map[string]any{"order_id": "o1", "issue": 3.0},
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
		"order_id": "o1", "issue": "other", "description": "  foam  ", "severity": "minor",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Description != "foam" {
		t.Fatalf("description: want %q got %q", "foam", req.Description)
	}
}
