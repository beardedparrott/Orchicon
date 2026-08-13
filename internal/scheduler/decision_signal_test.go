package scheduler

import (
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestSummaryWordIsSingleDecisionSignal verifies the core contract: the
// workflow decision comes ONLY from the first word after ORCHICON WORKER
// SUMMARY:. A standalone `_decision:` line or a stray `_issues:` block
// must never override it.
func TestSummaryWordIsSingleDecisionSignal(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   string
	}{
		{
			name: "success with inline nitpick mentions of _issues:",
			output: "### Nitpicks (non-blocking, not `_issues:`)\n" +
				"- stale docstring\n" +
				"ORCHICON WORKER SUMMARY: success — everything passes.\n",
			want: "success",
		},
		{
			name: "failure word routes failure",
			output: "ORCHICON WORKER SUMMARY: failure — the API contract is wrong.\n",
			want: "failure",
		},
		{
			name: "explicit _decision: line does not override summary",
			output: "_decision: failure\n" +
				"ORCHICON WORKER SUMMARY: success — works fine.\n",
			want: "success",
		},
		{
			name: "no marker means no decision",
			output: "Just some prose with no summary marker.",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := map[string]any{}
			// Mirror the reconciler: the decision key is only written
			// when a marker was found (never an empty override).
			if d := extractSummaryDecision(tc.output); d != "" {
				results["_decision"] = d
			}
			extractIssuesLine(tc.output, results)
			if tc.want == "" {
				if _, ok := results["_decision"]; ok {
					t.Errorf("_decision key set to %q, want absent (no marker)", results["_decision"])
				}
				return
			}
			if got := results["_decision"]; got != tc.want {
				t.Errorf("_decision = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIssuesLineIsInformationalOnly verifies _issues: is captured only
// from a line-start match and never flips the decision.
func TestIssuesLineIsInformationalOnly(t *testing.T) {
	output := "**No blocking issues.** housekeeping (not `_issues:`): nothing\n" +
		"## Findings\n" +
		"- `_issues:` stale scratch file qa_scratch_e2e.py should be deleted\n" +
		"ORCHICON WORKER SUMMARY: success — done.\n"

	results := map[string]any{}
	if d := extractSummaryDecision(output); d != "" {
		results["_decision"] = d
	}
	extractIssuesLine(output, results)

	// The inline "(not `_issues:`)" must NOT be captured as an issues block.
	if _, ok := results["_issues"]; ok {
		t.Errorf("_issues was captured but must be informational-only/absent: %q", results["_issues"])
	}
	if got := results["_decision"]; got != "success" {
		t.Errorf("_decision = %q, want success (issues must not flip decision)", got)
	}
}

// TestIssuesLineCapturesLineStart verifies a genuine `_issues:` at line
// start (after a bullet) is captured for the run view.
func TestIssuesLineCapturesLineStart(t *testing.T) {
	output := "- _issues: blocker: the login flow loses the session token\n" +
		"ORCHICON WORKER SUMMARY: failure — found a blocker.\n"
	results := map[string]any{}
	if d := extractSummaryDecision(output); d != "" {
		results["_decision"] = d
	}
	extractIssuesLine(output, results)
	if got := results["_issues"]; got != "blocker: the login flow loses the session token" {
		t.Errorf("_issues = %q, want the line-start issues text", got)
	}
	if got := results["_decision"]; got != "failure" {
		t.Errorf("_decision = %q, want failure", got)
	}
}

// TestMergeBudgets verifies tenant default (base) + worker override
// (per-field wins) merge into one budget JSON.
func TestMergeBudgets(t *testing.T) {
	tenant := []byte(`{"wall_clock_seconds":3600,"tokens":1000000}`)
	worker := []byte(`{"wall_clock_seconds":60,"tool_call_count":50}`)
	got := mergeBudgets(tenant, worker)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	if v, ok := m["wall_clock_seconds"].(float64); !ok || v != 60 {
		t.Errorf("worker override lost for wall_clock_seconds: %v", m["wall_clock_seconds"])
	}
	if v, ok := m["tokens"].(float64); !ok || v != 1000000 {
		t.Errorf("tenant default lost for tokens: %v", m["tokens"])
	}
	if v, ok := m["tool_call_count"].(float64); !ok || v != 50 {
		t.Errorf("worker field missing: %v", m["tool_call_count"])
	}
	// Empty inputs → empty JSON, not nil/error.
	if got := mergeBudgets(nil, nil); string(got) != "{}" {
		t.Errorf("empty merge = %s, want {}", got)
	}
}

// TestCountReaskRuns verifies the re-ask budget counts ACTUAL reviewer
// re-asks (step runs created by reaskDecisionStep with StepName
// "Reviewer (re-ask)") — NOT the reviewer's loop iteration count. A
// reviewer that looped back via explicit _decision: failure decisions was
// never re-asked and must not consume the budget; otherwise a truncated
// final turn (missing signal) fails on a pre-spent budget without ever
// getting a genuine re-ask.
func TestCountReaskRuns(t *testing.T) {
	ordinary := db.WorkflowStepRunRow{StepName: "PR Reviewer"}
	loop := db.WorkflowStepRunRow{StepName: "PR Reviewer (loop)"}
	reask1 := db.WorkflowStepRunRow{StepName: reaskRunName}
	reask2 := db.WorkflowStepRunRow{StepName: reaskRunName, SupersededBy: "some-other-id"}

	// Only genuine re-ask runs count, including superseded ones.
	if got := countReaskRuns([]db.WorkflowStepRunRow{ordinary, loop, reask1, reask2}); got != 2 {
		t.Fatalf("countReaskRuns = %d, want 2 (only re-ask runs)", got)
	}
	// No re-asks despite many loop iterations → budget untouched.
	if got := countReaskRuns([]db.WorkflowStepRunRow{ordinary, ordinary, loop, loop}); got != 0 {
		t.Fatalf("countReaskRuns = %d, want 0 (loop-backs are not re-asks)", got)
	}
}

// TestAggregateLoopDecisions verifies the fan-in gate aggregation: ANY
// upstream failure loops back (failure is decisive), otherwise the gate
// proceeds only when ALL upstreams succeeded; empty means no decision.
func TestAggregateLoopDecisions(t *testing.T) {
	const (
		fail = "failure"
		ok   = "success"
	)
	cases := []struct {
		name      string
		decisions []string
		want      string
	}{
		{"single success", []string{"success"}, "success"},
		{"single failure", []string{"failure"}, "failure"},
		{"both success (parallel PR + QA)", []string{"success", "success"}, "success"},
		{"one failure wins over success", []string{"success", "failure"}, "failure"},
		{"failure wins regardless of order", []string{"failure", "success"}, "failure"},
		{"no decisions -> empty (re-ask)", []string{"", ""}, ""},
		{"empty inputs", nil, ""},
		{"mixed unknown and success", []string{"", "success"}, "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregateLoopDecisions(tc.decisions, fail, ok); got != tc.want {
				t.Errorf("aggregateLoopDecisions(%v) = %q, want %q", tc.decisions, got, tc.want)
			}
		})
	}
}

// TestExtractFactsLearned verifies the facts-ledger extraction: only
// FACTS LEARNED lines are captured, case-insensitively, with bullet and
// marker prefixes stripped, while other lines are ignored.
func TestExtractFactsLearned(t *testing.T) {
	summary := "Verified the boot sequence live.\n" +
		"FACTS LEARNED: the supervisor runs the pre-feature daemon self-copy (old binary), so the sandbox plane does not auto-boot until the daemon rebuilds.\n" +
		"Also ran build + tests.\n" +
		"- FACTS LEARNED: /tmp is noexec so guard tests fail there.\n" +
		"FACTS learned: gofmt drift is pre-existing at base.\n"
	got := extractFactsLearned(summary)
	want := []string{
		"the supervisor runs the pre-feature daemon self-copy (old binary), so the sandbox plane does not auto-boot until the daemon rebuilds.",
		"/tmp is noexec so guard tests fail there.",
		"gofmt drift is pre-existing at base.",
	}
	if len(got) != len(want) {
		t.Fatalf("extractFactsLearned = %d facts, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fact[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// No facts -> empty slice.
	if got := extractFactsLearned("just a plain summary with no markers"); len(got) != 0 {
		t.Errorf("expected no facts, got %v", got)
	}
	// Empty summary -> empty.
	if got := extractFactsLearned(""); len(got) != 0 {
		t.Errorf("expected no facts for empty summary, got %v", got)
	}
}
