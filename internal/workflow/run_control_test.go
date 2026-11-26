package workflow

import (
	"encoding/json"
	"testing"
	"time"
)

// WHY THESE TESTS EXIST.
//
// Three behaviours were promised to the operator and each is easy to regress silently:
//
//  1. A RESUMED step must carry the dead attempt's knowledge. Before this, the re-arm wrote `{}`
//     and the worker started blind — the summary sat in the row we had just overwritten.
//  2. A carried context must NOT present itself as a live execution: `_prior_attempt` is history,
//     and a step that believed it owned a session id would try to resume a dead one.
//  3. The resume count must survive repeated resumes, because it is the honest answer to "is this
//     the first attempt?" that the dispatch prompt states to the worker.

func TestPriorAttemptContextCarriesTheSummaryAndTheWorkItem(t *testing.T) {
	prior := []byte(`{
		"_work_item_id": "01M84WC7KNEDYWJJFY27MYEQA8",
		"_output": "AC5 is not verified, so this step fails.",
		"_recovery_summary": "antigravity tests pass at HEAD; no agy credentials in the sandbox.",
		"_decision": "failure",
		"_verdict": "failure",
		"_parent_execution_id": "01M8B2SBZSG1F4CS4YYXVTNE15"
	}`)

	got := priorAttemptContext(prior)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("carried context is not valid JSON: %v", err)
	}

	// The summary is the whole point — it is what stops the next attempt re-deriving.
	if m["_prior_recovery_summary"] != "antigravity tests pass at HEAD; no agy credentials in the sandbox." {
		t.Errorf("_prior_recovery_summary = %v, want the dead attempt's summary carried forward", m["_prior_recovery_summary"])
	}
	if m["_prior_output"] == nil || m["_prior_output"] == "" {
		t.Error("_prior_output missing — the resumed worker cannot see what the previous attempt concluded")
	}
	// The work item id is a PLATFORM key: losing it strands the resumed step with no ticket.
	if m["_work_item_id"] != "01M84WC7KNEDYWJJFY27MYEQA8" {
		t.Errorf("_work_item_id = %v, want it preserved — dispatch reads it to find the ticket", m["_work_item_id"])
	}
	// Nothing is lost: the whole prior result survives as history.
	if m["_prior_attempt"] == nil {
		t.Error("_prior_attempt missing — the prior result should be preserved verbatim, not summarised away")
	}
}

// TestPriorAttemptContextDoesNotPromoteExecutionIdentity is the safety edge. Promotion would make a
// resumed step believe it owns an execution that is over.
func TestPriorAttemptContextDoesNotPromoteExecutionIdentity(t *testing.T) {
	prior := []byte(`{"_work_item_id":"wi-1","_checkpoint_ref":"ck-9","_execution_id":"exec-dead"}`)
	got := priorAttemptContext(prior)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	for _, live := range []string{"_checkpoint_ref", "_execution_id", "_parent_execution_id"} {
		if _, present := m[live]; present {
			t.Errorf("%s was promoted to a LIVE field — a resumed step must not claim a dead execution", live)
		}
	}
	// It is still recoverable as history.
	if m["_prior_attempt"] == nil {
		t.Error("the prior result should still survive inside _prior_attempt")
	}
}

// TestPriorAttemptContextWithNothingToCarry returns valid, empty JSON rather than nil — the reset
// writes this value, and a nil Result would be indistinguishable from "never ran".
func TestPriorAttemptContextWithNothingToCarry(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("not json"), []byte("{}")} {
		got := priorAttemptContext(in)
		if len(got) == 0 {
			t.Fatalf("priorAttemptContext(%q) returned empty — want {}", in)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("priorAttemptContext(%q) = %q, not valid JSON", in, got)
		}
	}
}

// TestMarkRunResumedColdCountsEveryResume pins the count that the dispatch prompt reads.
func TestMarkRunResumedColdCountsEveryResume(t *testing.T) {
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	ctx := []byte(`{"_pr_url":"https://example/pr/1"}`)

	for want := 1; want <= 3; want++ {
		next, err := markRunResumedCold(ctx, now)
		if err != nil {
			t.Fatalf("markRunResumedCold: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(next, &m); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
		if got := m["_resume_count"]; got != float64(want) {
			t.Fatalf("resume %d: _resume_count = %v, want %d", want, got, want)
		}
		if m["_resumed_cold"] != true {
			t.Errorf("resume %d: _resumed_cold not set — the UI cannot label a cold resume", want)
		}
		// Unrelated context must survive a resume.
		if want == 1 && m["_pr_url"] != "https://example/pr/1" {
			t.Errorf("run context lost _pr_url on resume: %v", m)
		}
		ctx = next
	}
}
