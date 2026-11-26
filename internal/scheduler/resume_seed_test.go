package scheduler

import (
	"strings"
	"testing"
)

// WHY THIS TEST EXISTS.
//
// The retry-resume carry writes the dead attempt's summary into the step run under `_prior_*`
// keys. That work is worthless unless something READS it — and the reader was the gap that made
// the whole carry pointless: the recovery-seed path only ever looked for `_recovery_*`, which the
// recovery ENGINE writes during an in-flight recovery. A retried run has no recovery row, so the
// summary sat in the row while the worker restarted blind.
//
// These tests pin the reader, not the writer, because the writer is only half the wire.

func TestResumeCarriedSeedReadsTheRecoverySummary(t *testing.T) {
	stepRun := []byte(`{
		"_work_item_id": "wi-1",
		"_prior_recovery_summary": "antigravity tests pass at HEAD; no agy credentials in the sandbox.",
		"_prior_output": "AC5 is not verified, so this step fails."
	}`)

	seed := resumeCarriedSeed(stepRun, nil)
	if seed == nil {
		t.Fatal("a retry-resumed step run with a carried summary produced no seed — the resumed worker restarts blind")
	}
	if !strings.Contains(seed.Summary, "no agy credentials") {
		t.Errorf("seed summary = %q, want the carried recovery summary", seed.Summary)
	}
	if !seed.Resumed {
		t.Error("seed.Resumed is false — it would be treated as a RECOVERY seed and gated on a failed " +
			"execution identity that a retry does not have")
	}
}

// TestResumeCarriedSeedFallsBackToTheOutput covers an attempt that reported but left no summary:
// the report itself is the only thing worth inheriting.
func TestResumeCarriedSeedFallsBackToTheOutput(t *testing.T) {
	stepRun := []byte(`{"_prior_output": "I could not reach the model; no credentials were available."}`)
	seed := resumeCarriedSeed(stepRun, nil)
	if seed == nil || !strings.Contains(seed.Summary, "no credentials were available") {
		t.Fatalf("seed = %+v, want the prior output used as the summary", seed)
	}
}

// TestResumeCarriedSeedReadsTheWorkItemToo: the summary is written on the WORK ITEM by the
// recovery engine, so a reader that only consults the step run misses it.
func TestResumeCarriedSeedReadsTheWorkItemToo(t *testing.T) {
	wi := []byte(`{"_prior_recovery_summary": "carried on the work item"}`)
	if seed := resumeCarriedSeed(nil, wi); seed == nil || !strings.Contains(seed.Summary, "carried on the work item") {
		t.Fatalf("seed = %+v, want the work-item copy read", seed)
	}
}

// TestResumeCarriedSeedIsSilentWithoutAPriorAttempt: a first attempt must NOT get a "resumed"
// preamble. Telling a first-timer it is not its first attempt is worse than saying nothing.
func TestResumeCarriedSeedIsSilentWithoutAPriorAttempt(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("{}"), []byte(`{"_work_item_id":"wi-1"}`)} {
		if seed := resumeCarriedSeed(in, in); seed != nil {
			t.Errorf("resumeCarriedSeed(%q) = %+v, want nil — this is a first attempt", in, seed)
		}
	}
}

// TestTrimForSeedCapsARunawayReport keeps the carried text from swallowing the prompt.
func TestTrimForSeedCapsARunawayReport(t *testing.T) {
	long := strings.Repeat("x", 9000)
	got := trimForSeed(long)
	if len(got) > 4200 {
		t.Fatalf("trimForSeed left %d chars — it must cap the carried report", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Error("a truncated seed must say so, or the worker believes the report ended there")
	}
}
