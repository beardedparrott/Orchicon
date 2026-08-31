package scheduler

import (
	"encoding/json"
	"strconv"
	"strings"
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
			name:   "failure word routes failure",
			output: "ORCHICON WORKER SUMMARY: failure — the API contract is wrong.\n",
			want:   "failure",
		},
		{
			name: "explicit _decision: line does not override summary",
			output: "_decision: failure\n" +
				"ORCHICON WORKER SUMMARY: success — works fine.\n",
			want: "success",
		},
		{
			name:   "custom word passes through verbatim",
			output: "ORCHICON WORKER SUMMARY: customword — some narrative",
			want:   "customword",
		},
		{
			name:   "no marker means no decision",
			output: "Just some prose with no summary marker.",
			want:   "",
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
		{"unknown custom word is not decisive", []string{"oops", "success"}, "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregateLoopDecisions(tc.decisions, fail, ok); got != tc.want {
				t.Errorf("aggregateLoopDecisions(%v) = %q, want %q", tc.decisions, got, tc.want)
			}
		})
	}
}

// TestCapSummaryNarrativeShortSummaryPassthrough verifies the fast path: a
// summary under the budget passes through byte-identical.
func TestCapSummaryNarrativeShortSummaryPassthrough(t *testing.T) {
	in := "Did the work. Everything passes."
	if got := capSummaryNarrative(in, 500); got != in {
		t.Errorf("short summary must pass through untouched, got %q", got)
	}
	if got := capSummaryNarrative("", 500); got != "" {
		t.Errorf("empty summary must stay empty, got %q", got)
	}
}

// TestCapSummaryNarrativePreservesFactsAndRouting verifies the structural
// preservation contract: the cap strips narrative verbosity but NEVER strips
// FACTS LEARNED lines or the ORCHICON WORKER SUMMARY routing line. The facts
// must remain extractable via extractFactsLearned after the cap.
func TestCapSummaryNarrativePreservesFactsAndRouting(t *testing.T) {
	narrative := strings.Repeat("long rambling narrative line that pads the summary with filler words\n", 200) // ~11k chars
	lastNarrativeLine := "long rambling narrative line that pads the summary with filler words"
	in := narrative +
		"ORCHICON WORKER SUMMARY: failure — the login flow loses the session token.\n" +
		"FACTS LEARNED: the runtime container supervisor runs the pre-feature daemon self-copy.\n" +
		"FACTS LEARNED: /tmp is noexec in the runtime containers.\n" +
		"FACTS LEARNED (from Senior Software Engineer): a step-attributed fact quoted back from the facts_learned file.\n"

	got := capSummaryNarrative(in, 500)
	if len(got) >= len(in) {
		t.Errorf("narrative was not truncated; len=%d (in=%d)", len(got), len(in))
	}
	// The tail of the narrative must be gone (only the leading lines survive).
	if tail := strings.LastIndex(got, lastNarrativeLine); tail >= 0 {
		// The narrative's last surviving occurrence must not be the final
		// occurrence of that line in the original — i.e. later lines were cut.
		if strings.Count(got, lastNarrativeLine) >= 200 {
			t.Errorf("all 200 narrative lines survived; len=%d", len(got))
		}
	}
	if !strings.Contains(got, summaryMarker) || !strings.Contains(got, "failure") {
		t.Errorf("routing line must survive the cap; got:\n%s", got)
	}
	if !strings.Contains(got, "FACTS LEARNED: the runtime container supervisor runs the pre-feature daemon self-copy.") {
		t.Errorf("FACTS LEARNED line lost by cap; got:\n%s", got)
	}
	if !strings.Contains(got, "FACTS LEARNED: /tmp is noexec in the runtime containers.") {
		t.Errorf("FACTS LEARNED line lost by cap; got:\n%s", got)
	}
	if !strings.Contains(got, "FACTS LEARNED (from Senior Software Engineer): a step-attributed fact quoted back from the facts_learned file.") {
		t.Errorf("step-attributed FACTS LEARNED line lost by cap; got:\n%s", got)
	}
	facts := extractFactsLearned(got)
	if len(facts) != 2 {
		t.Errorf("extractFactsLearned after cap = %d facts, want 2: %v", len(facts), facts)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected a truncation marker, got:\n%s", got)
	}
}

// TestCapSummaryNarrativeAllFactsNoTruncation verifies a summary that is
// entirely facts exceeds the byte budget yet must be returned verbatim — the
// cap only ever truncates narrative, never structural content.
func TestCapSummaryNarrativeAllFactsNoTruncation(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 30; i++ {
		sb.WriteString("FACTS LEARNED: established fact number ")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(" that is intentionally verbose so the whole block blows the token budget.\n")
	}
	in := sb.String()
	if len(in) <= maxSummaryTokens*4 {
		t.Fatal("test setup: summary must exceed the budget")
	}
	got := capSummaryNarrative(in, 500)
	if got != in {
		t.Errorf("all-facts summary must pass through untouched, got:\n%s", got)
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

// TestExtractPRFieldsHappyPath verifies the happy path for PR URL/state extraction.
func TestExtractPRFieldsHappyPath(t *testing.T) {
	prURL, prState := extractPRFields("PR_URL: https://github.com/OWNER/REPO/pull/42\nPR_STATE: merged\nORCHICON WORKER SUMMARY: success\n")
	if prURL != "https://github.com/OWNER/REPO/pull/42" {
		t.Errorf("prURL = %q, want %q", prURL, "https://github.com/OWNER/REPO/pull/42")
	}
	if prState != "merged" {
		t.Errorf("prState = %q, want %q", prState, "merged")
	}
}

// TestExtractPRFieldsLastOccurrenceWins verifies last occurrence wins.
func TestExtractPRFieldsLastOccurrenceWins(t *testing.T) {
	output := "PR_URL: https://github.com/OWNER/REPO/pull/10\nPR_STATE: open\n" +
		"Some work done...\n" +
		"PR_URL: https://github.com/OWNER/REPO/pull/42\nPR_STATE: merged\n" +
		"ORCHICON WORKER SUMMARY: success\n"
	prURL, prState := extractPRFields(output)
	if prURL != "https://github.com/OWNER/REPO/pull/42" {
		t.Errorf("prURL = %q, want %q", prURL, "https://github.com/OWNER/REPO/pull/42")
	}
	if prState != "merged" {
		t.Errorf("prState = %q, want %q", prState, "merged")
	}
}

// TestExtractPRFieldsBulletFenceStripping verifies markdown bullets and fences are stripped.
func TestExtractPRFieldsBulletFenceStripping(t *testing.T) {
	output := "```\n- PR_URL: https://github.com/OWNER/REPO/pull/42\n* PR_STATE: draft\n```\nORCHICON WORKER SUMMARY: success\n"
	prURL, prState := extractPRFields(output)
	if prURL != "https://github.com/OWNER/REPO/pull/42" {
		t.Errorf("prURL = %q, want %q", prURL, "https://github.com/OWNER/REPO/pull/42")
	}
	if prState != "draft" {
		t.Errorf("prState = %q, want %q", prState, "draft")
	}
}

// TestExtractPRFieldsInvalidURLs verifies invalid URLs are ignored.
func TestExtractPRFieldsInvalidURLs(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"relative path", "PR_URL: /OWNER/REPO/pull/42"},
		{"ftp scheme", "PR_URL: ftp://example.com/pull/42"},
		{"no host", "PR_URL: https://"},
		{"empty value", "PR_URL:"},
		{"just whitespace", "PR_URL:    "},
		{"missing scheme", "PR_URL: github.com/OWNER/REPO/pull/42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prURL, prState := extractPRFields(tc.line + "\nPR_STATE: open")
			if prURL != "" {
				t.Errorf("prURL = %q, want empty for %q", prURL, tc.line)
			}
			if prState != "open" {
				t.Errorf("prState = %q, want %q", prState, "open")
			}
		})
	}
}

// TestExtractPRFieldsUnknownState verifies unknown states are ignored.
func TestExtractPRFieldsUnknownState(t *testing.T) {
	prURL, prState := extractPRFields("PR_URL: https://github.com/OWNER/REPO/pull/42\nPR_STATE: unknown_state\n")
	if prURL != "https://github.com/OWNER/REPO/pull/42" {
		t.Errorf("prURL = %q, want %q", prURL, "https://github.com/OWNER/REPO/pull/42")
	}
	if prState != "" {
		t.Errorf("prState = %q, want empty", prState)
	}
}

// TestExtractPRFieldsCaseNormalization verifies state is lowercased.
func TestExtractPRFieldsCaseNormalization(t *testing.T) {
	cases := []struct {
		input  string
		output string
	}{
		{"PR_STATE: Merged", "merged"},
		{"PR_STATE: OPEN", "open"},
		{"PR_STATE: Draft", "draft"},
		{"PR_STATE: CLOSED", "closed"},
		{"PR_STATE: None", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			_, prState := extractPRFields(tc.input)
			if prState != tc.output {
				t.Errorf("prState = %q, want %q", prState, tc.output)
			}
		})
	}
}

// TestExtractPRFieldsURLOnly verifies a URL-only report doesn't clobber existing state.
func TestExtractPRFieldsURLOnly(t *testing.T) {
	prURL, prState := extractPRFields("PR_URL: https://github.com/OWNER/REPO/pull/42\n")
	if prURL != "https://github.com/OWNER/REPO/pull/42" {
		t.Errorf("prURL = %q, want %q", prURL, "https://github.com/OWNER/REPO/pull/42")
	}
	if prState != "" {
		t.Errorf("prState = %q, want empty", prState)
	}
}

// TestExtractPRFieldsStateOnly verifies a state-only report doesn't clobber existing URL.
func TestExtractPRFieldsStateOnly(t *testing.T) {
	prURL, prState := extractPRFields("PR_STATE: merged\n")
	if prURL != "" {
		t.Errorf("prURL = %q, want empty", prURL)
	}
	if prState != "merged" {
		t.Errorf("prState = %q, want %q", prState, "merged")
	}
}

// TestExtractPRFieldsNoLines verifies empty/empty when no valid lines.
func TestExtractPRFieldsNoLines(t *testing.T) {
	prURL, prState := extractPRFields("ORCHICON WORKER SUMMARY: success — done.\n")
	if prURL != "" || prState != "" {
		t.Errorf("prURL = %q, prState = %q, want empty/empty", prURL, prState)
	}
}

// TestExtractPRFieldsEmptyOutput verifies empty/empty for empty input.
func TestExtractPRFieldsEmptyOutput(t *testing.T) {
	prURL, prState := extractPRFields("")
	if prURL != "" || prState != "" {
		t.Errorf("prURL = %q, prState = %q, want empty/empty", prURL, prState)
	}
}

// TestExtractPRFieldsFencedBlockBeforeSummary verifies lines inside a fenced block before the summary marker.
func TestExtractPRFieldsFencedBlockBeforeSummary(t *testing.T) {
	output := "```\nPR_URL: https://github.com/OWNER/REPO/pull/42\nPR_STATE: merged\n```\nORCHICON WORKER SUMMARY: success — merged into develop\n"
	prURL, prState := extractPRFields(output)
	if prURL != "https://github.com/OWNER/REPO/pull/42" {
		t.Errorf("prURL = %q, want %q", prURL, "https://github.com/OWNER/REPO/pull/42")
	}
	if prState != "merged" {
		t.Errorf("prState = %q, want %q", prState, "merged")
	}
}

// TestExtractPRFieldsAcceptedStates verifies all accepted states.
func TestExtractPRFieldsAcceptedStates(t *testing.T) {
	states := []string{"open", "merged", "draft", "closed", "none"}
	for _, s := range states {
		t.Run(s, func(t *testing.T) {
			_, prState := extractPRFields("PR_STATE: " + s)
			if prState != s {
				t.Errorf("prState = %q, want %q", prState, s)
			}
		})
	}
}

// TestExtractPRFieldsHTTPSAndHTTP verify both http and https schemes accepted.
func TestExtractPRFieldsHTTPSAndHTTP(t *testing.T) {
	prURL, _ := extractPRFields("PR_URL: https://github.com/OWNER/REPO/pull/42\n")
	if prURL != "https://github.com/OWNER/REPO/pull/42" {
		t.Errorf("HTTPS prURL = %q, want %q", prURL, "https://github.com/OWNER/REPO/pull/42")
	}
	prURL, _ = extractPRFields("PR_URL: http://github.com/OWNER/REPO/pull/42\n")
	if prURL != "http://github.com/OWNER/REPO/pull/42" {
		t.Errorf("HTTP prURL = %q, want %q", prURL, "http://github.com/OWNER/REPO/pull/42")
	}
}
