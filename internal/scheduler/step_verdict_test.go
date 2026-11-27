package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
)

// WHY THIS FILE EXISTS — the other half of the same wire.
//
// Run 01M8CSC92YWEZSC2FTRWC2K5A7 went to QA on attempt 4 instead of looping back to SSE, WITH the
// fix deployed. Two readers classify a failed step as "delivered a verdict" or "infrastructure
// fault", and I fixed only one:
//
//	loopVerdictForFailedStep   loop chain        read _verdict -> _decision -> work item   FIXED
//	stepDeliveredVerdict       pollTaskStep gate read _verdict ONLY                        MISSED
//
// QA's step run carries `_decision: "failure"` and no `_verdict`, so the gate returned "" — routing
// was skipped and polling fell through to the recovery branch. The verdict was there; the gate
// could not see it.
//
// These tests pin the GATE's resolution, using this run's real step-run shape.

// qaStepRunResult is the actual JSON of step-qa's result on that run: a decision, and no verdict.
const qaStepRunResult = `{
	"_work_item_id": "01M84WD1G42BHD1HTCB2YMQ82Z",
	"_worker_id": "w_se_qa_engineer",
	"_summary": "AC1-AC3 and AC7 pass; AC4-AC6 unverified",
	"_decision": "failure"
}`

func TestStepDeliveredVerdictReadsTheDecisionWhenNoVerdictIsStamped(t *testing.T) {
	var parsed struct {
		Verdict  string `json:"_verdict"`
		Decision string `json:"_decision"`
	}
	if err := json.Unmarshal([]byte(qaStepRunResult), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The row really has no verdict — that precondition is what made the bug invisible.
	if parsed.Verdict != "" {
		t.Fatalf("fixture drifted: _verdict is %q, expected empty", parsed.Verdict)
	}
	if got := loopVerdictForFailedStep(parsed.Decision, parsed.Verdict, nil); got != "failure" {
		t.Fatalf("resolved verdict = %q, want \"failure\" from _decision — this is the value the gate "+
			"needs to route QA to the loop instead of recovering it", got)
	}
}

// TestStepDeliveredVerdictConsultsTheDecision is the wiring assertion: the gate must call the SHARED
// resolver, not re-implement a verdict-only read. Fixing a helper that nothing calls is exactly how
// this survived a rebuild.
func TestStepDeliveredVerdictConsultsTheDecision(t *testing.T) {
	src := readWorkflowReconcilerSource(t)
	i := strings.Index(src, "func (r *WorkflowReconciler) stepDeliveredVerdict")
	if i < 0 {
		t.Fatal("stepDeliveredVerdict is gone")
	}
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "loopVerdictForFailedStep(stepResult.Decision, stepResult.Verdict, nil)") {
		t.Error("stepDeliveredVerdict no longer consults _decision. A step that reports " +
			"`ORCHICON WORKER SUMMARY: failure` with no derived _verdict is then classified as an " +
			"infrastructure fault, and the run recovers the QA step forever instead of looping to SSE.")
	}
	// The old verdict-only struct must not come back.
	if strings.Contains(body, "Verdict string `json:\"_verdict\"`\n\t}") &&
		!strings.Contains(body, "_decision") {
		t.Error("stepDeliveredVerdict is back to reading only _verdict")
	}
}
