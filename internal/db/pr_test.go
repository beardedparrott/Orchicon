package db_test

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestPrFromRunContext covers the read-time PR surface parse from the run's
// structured run_context JSONB. Missing/non-string keys must yield empty
// values so the UI falls back to the deterministic pull/new/{branch} link.
func TestPrFromRunContext(t *testing.T) {
	cases := []struct {
		name       string
		runContext string
		wantURL    string
		wantState  string
	}{
		{"empty context", "", "", ""},
		{"no pr keys", `{"worktree_branch":"x"}`, "", ""},
		{"worker-authored pr", `{"pr_url":"https://github.com/a/b/pull/12","pr_state":"open"}`, "https://github.com/a/b/pull/12", "open"},
		{"merged state", `{"pr_url":"https://github.com/a/b/pull/9","pr_state":"merged"}`, "https://github.com/a/b/pull/9", "merged"},
		{"non-string pr_url ignored", `{"pr_url":123}`, "", ""},
		{"invalid json", `{`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, state := db.PrFromRunContext([]byte(tc.runContext))
			if url != tc.wantURL {
				t.Fatalf("PrFromRunContext url = %q, want %q", url, tc.wantURL)
			}
			if state != tc.wantState {
				t.Fatalf("PrFromRunContext state = %q, want %q", state, tc.wantState)
			}
		})
	}
}
