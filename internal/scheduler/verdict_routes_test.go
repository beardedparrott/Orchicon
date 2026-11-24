package scheduler

// verdict_routes_test.go — A WORKER'S VERDICT IS A ROUTING DECISION, NOT AN INFRASTRUCTURE FAULT.
//
// THE REPORTED FAILURE (run 01M70EPBN9ZK0FM1N6HJBGH6H5). The QA worker ran to completion and ended with
// `ORCHICON WORKER SUMMARY: failure — the layout is broken…`. OnResult downgraded the execution to failed
// (#902, correct on its own), which made step-qa's step run FAILED — and a loop_decision treats a failed
// upstream as a fault to RECOVER, not a decision to route. So recovery re-dispatched QA six times, each
// attempt re-reviewing unchanged work and rejecting it again, and the loop's
// `case cfg.FailureValue → loopDecisionReenter(loop_branch = step-sse)` never executed once.
//
// Two things must therefore hold, and they are asserted here rather than inferred:
//   1. a delivered verdict is NOT a recoverable fault (so recovery's six attempts never start);
//   2. the verdict reaches the routing layer STRUCTURALLY (`_verdict`), not by matching error text.

import (
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// THE DECISION. A failed upstream is a fault only when nobody judged it.
func TestAVerdictIsNotAnInfrastructureFault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		verdict string
		want    bool
		why     string
	}{
		{"succeeded", domain.StepRunSucceeded, "", false, "a succeeded step is never a fault"},
		{"succeeded carrying a verdict", domain.StepRunSucceeded, "failure", false, "same"},
		{"worker rejected the work", domain.StepRunFailed, "failure", false,
			"recovery would re-run the same review against unchanged work — the six-attempt burn"},
		{"crash with no verdict", domain.StepRunFailed, "", true,
			"a crash, stall or tool error is what recovery exists for"},
		{"blocked with no verdict", domain.StepRunBlocked, "", true, "still nobody's judgement"},
	} {
		if got := upstreamFaultedByInfra(tc.status, tc.verdict); got != tc.want {
			t.Errorf("%s: upstreamFaultedByInfra(%q, %q) = %v, want %v — %s",
				tc.name, tc.status, tc.verdict, got, tc.want, tc.why)
		}
	}
}

// AND THE DOWNGRADE ITSELF IS DOWNGRADE-ONLY: a crashed adapter outranks anything the model emitted.
func TestVerdictDowngradeOnlyLowersTheOutcome(t *testing.T) {
	const failed = "Work.\n\nORCHICON WORKER SUMMARY: failure — the layout is broken.\n"
	const ok = "Work.\n\nORCHICON WORKER SUMMARY: success — shipped.\n"
	for _, tc := range []struct {
		name    string
		adapter bool
		output  string
		want    string
	}{
		{"clean process, failure verdict", true, failed, "failure"},
		{"clean process, success verdict", true, ok, ""},
		{"crashed process, failure verdict", false, failed, ""}, // never double-counts a crash as a verdict
		{"crashed process, success verdict", false, ok, ""},     // text may only LOWER the outcome
		{"no summary at all", true, "just some output", ""},
		{"empty output", true, "", ""},
	} {
		if got := verdictDowngrade(tc.adapter, tc.output); got != tc.want {
			t.Errorf("%s: verdictDowngrade(%v, …) = %q, want %q", tc.name, tc.adapter, got, tc.want)
		}
	}
}

// THE MARKER MUST REACH THE LOOP, and this asserts WIRING rather than behaviour — deliberately labelled,
// because the behaviour half cannot be asserted here: transitionWorkItemOnResult is DB-bound and the
// scheduler's DB suite does not run in CI (ORCHICON_TEST_DSN is set by neither the Makefile nor any
// workflow), so a DB test would SKIP and prove nothing.
//
// What it pins is the half a refactor silently drops: the verdict is written STRUCTURALLY onto the work
// item's results, and the loop reads it before deciding to recover.
func TestTheVerdictReachesTheLoopStructurally(t *testing.T) {
	res, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatalf("read reconciler.go: %v", err)
	}
	src := stripCommentsForGate(string(res))
	if !strings.Contains(src, `results["_verdict"] = verdict`) {
		t.Fatal("transitionWorkItemOnResult no longer writes results[\"_verdict\"] — the loop_decision then " +
			"cannot tell a DELIVERED verdict from a crash and recovers the review instead of routing it, " +
			"which is the six-attempt burn in run 01M70EPBN9ZK0FM1N6HJBGH6H5")
	}

	loop, err := os.ReadFile("workflow_reconciler.go")
	if err != nil {
		t.Fatalf("read workflow_reconciler.go: %v", err)
	}
	lsrc := stripCommentsForGate(string(loop))
	if !strings.Contains(lsrc, `wiResult["_verdict"]`) {
		t.Error("the loop_decision no longer reads _verdict from the failed upstream's work item, so a " +
			"verdict-shaped failure is unreachable from the routing side")
	}
	// THE CALL, NOT THE DEFINITION: the function is declared in this same file, so a bare identifier check
	// passes even when nothing calls it — a gate satisfied by its neighbour proves nothing.
	if !strings.Contains(lsrc, "if !upstreamFaultedByInfra(u.run.Status, u.verdict)") {
		t.Error("the anyFailed branch no longer consults upstreamFaultedByInfra with the upstream's status " +
			"and verdict, so every failed upstream is a recoverable fault again")
	}
}

// stripCommentsForGate removes line comments so an assertion cannot be satisfied by PROSE — this file's
// own explanation names the same identifiers.
func stripCommentsForGate(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// THE SAME RULE, APPLIED TO THE LOOP GATE (run 01M85764NHHH69ACHZ5BDFR6K9)
//
// #940 fixed the RECOVERY path: a delivered verdict routes instead of being recovered. It did not touch
// the D3 hold-at-PENDING gate, which still read raw status:
//
//	if step.Kind == StepKindLoopDecision && sr.Iteration > 0 &&
//		r.loopDecisionUpstreamFailed(step, runByID) { continue }
//
// So a Reviewer that REJECTED work failed its step, the loop_decision was held at PENDING forever, and
// the run died with the work never handed back to the engineer:
//
//	PR Reviewer: "The code change is correct and AC1-AC3 pass ... I'm reporting failure only because
//	             AC5 (the full gate) fails on one test"
//	-> step-9gfybwnv (loop_branch: step-sse) never fired. SSE stayed at 1 of 6 attempts. QA and DevOps
//	   never ran. The run was marked failed with the code unmerged.
//
// That INVERTS the loop: the branch fired only when the reviewer was happy, which is the one case that
// does not need it. The assertions below are source-level for the WIRING (the call site must pass what
// the decision needs) plus unit-level for the DECISION, because the gate's own body is only reachable
// through a live reconciler pass.
// ---------------------------------------------------------------------------

// THE WIRING: the gate must be given the run context it needs to resolve a verdict at all. Before this
// change it was a pure-status predicate, so it could not distinguish a rejection from a crash.
func TestTheLoopGateConsultsTheVerdictNotJustTheStatus(t *testing.T) {
	raw, err := os.ReadFile("workflow_reconciler.go")
	if err != nil {
		t.Fatalf("read workflow_reconciler.go: %v", err)
	}
	src := string(raw)

	// The gate signature carries the context a verdict lookup requires.
	if !strings.Contains(src, "func (r *WorkflowReconciler) loopDecisionUpstreamFailed(ctx context.Context, tx pgx.Tx, tenantID string, step workflow.StepWire, runs map[string]db.WorkflowStepRunRow) bool") {
		t.Error("loopDecisionUpstreamFailed no longer takes the run context. Without ctx/tx/tenant it cannot " +
			"read a failed upstream's _verdict, which is the only thing that separates a REJECTION (route " +
			"the loop) from a CRASH (hold for recovery).")
	}
	// And it must actually use the shared predicate rather than re-deriving "failed means hold".
	body := src[strings.Index(src, "func (r *WorkflowReconciler) loopDecisionUpstreamFailed"):]
	body = body[:strings.Index(body, "\n}\n")+3]
	if !strings.Contains(body, "upstreamFaultedByInfra") {
		t.Error("the loop gate does not consult upstreamFaultedByInfra, so it is back to treating every " +
			"failed upstream as a fault — the regression that killed run 01M85764NHHH69ACHZ5BDFR6K9")
	}
	// The verdict resolution lives beside it and reads the work item's marker on the failure path only.
	if !strings.Contains(src, "func loopDecisionUpstreamVerdict(") {
		t.Error("loopDecisionUpstreamVerdict is gone — the gate has no way to obtain a verdict, and would " +
			"have to fall back to bare status")
	}
}

// THE DECISION, stated as the incident: a rejecting reviewer must ROUTE, a crashed one must HOLD.
func TestARejectionRoutesAndACrashHolds(t *testing.T) {
	// The reviewer's own words, from the run that died.
	const rejected = domain.StepRunFailed
	if upstreamFaultedByInfra(rejected, "failure") {
		t.Error("a rejecting reviewer is still classified as an infrastructure fault, so the loop is held " +
			"and the run dies instead of handing the work back — exactly run 01M85764NHHH69ACHZ5BDFR6K9")
	}
	if !upstreamFaultedByInfra(rejected, "") {
		t.Error("a crash with no verdict is no longer a fault, so recovery would never pick up a genuine " +
			"environment failure")
	}
}

// THE CALL SITE still passes the context through — a refactor that drops it fails to compile, but this
// catches a future caller reintroducing a status-only variant beside the real one.
func TestTheLoopGateCallSitePassesTheContext(t *testing.T) {
	raw, err := os.ReadFile("workflow_reconciler.go")
	if err != nil {
		t.Fatalf("read workflow_reconciler.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "r.loopDecisionUpstreamFailed(ctx, ttx.Tx, tenantID, step, runByID)") {
		t.Error("the loop gate call site no longer passes ctx/tx/tenantID, so a failed upstream's verdict " +
			"cannot be resolved and every failure holds the loop again")
	}
}
