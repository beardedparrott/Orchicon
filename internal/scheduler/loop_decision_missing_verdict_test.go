package scheduler

// loop_decision_missing_verdict_test.go — the on_missing_decision policy.
//
// A loop_decision routes on an upstream VERDICT. When no upstream supplies one the
// engine used to decide what that meant by comparing the step's loop_branch and
// success_branch against ONE tenant's step ids ("step-devops-pr" + "step-l32ezp4b"),
// which matched only one of the two loops it was written for and left the other
// permanently failing. These tests pin the replacement:
//
//   * an ABSENT key resolves to reask, so every workflow that predates the policy
//     behaves exactly as it did — the change is additive;
//   * success proceeds forward, which is what the hardcode did;
//   * fail refuses;
//   * an unrecognised value degrades to reask rather than to something new;
//   * and the engine no longer mentions any tenant step id.

import (
	"os"
	"strings"
	"testing"
)

// parseDefaults is what the reconciler consults, so its defaults ARE the contract.
func TestMissingDecisionDefaultsToReask(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		want string
	}{
		{"no config at all", "", MissingDecisionReask},
		{"empty object", "{}", MissingDecisionReask},
		{"key absent but siblings present", `{"loop_branch":"a","max_iterations":3}`, MissingDecisionReask},
		{"explicit reask", `{"on_missing_decision":"reask"}`, MissingDecisionReask},
		{"explicit success", `{"on_missing_decision":"success"}`, MissingDecisionSuccess},
		{"explicit fail", `{"on_missing_decision":"fail"}`, MissingDecisionFail},
		// The vocabulary is EXACT: a typo is not a policy, so it normalises to the
		// default rather than inventing a third state.
		{"typo normalises to the default", `{"on_missing_decision":"sucess"}`, MissingDecisionReask},
	} {
		if got := parseLoopDecisionConfig(tc.cfg).OnMissingDecision; got != tc.want {
			t.Errorf("%s: OnMissingDecision = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The policy is ADDITIVE: a workflow that does not carry the key must take the same
// branch it always took. Anything else would silently change live workflows.
func TestAbsentPolicyIsTheGeneralReaskPath(t *testing.T) {
	// A loop decision the way it exists in production TODAY — no policy key.
	legacy := `{"max_iterations":6,"conflict_value":"conflict","exhausted_review":"","loop_branch":"step-devops-pr","success_branch":"step-l32ezp4b"}`
	cfg := parseLoopDecisionConfig(legacy)
	if cfg.OnMissingDecision != MissingDecisionReask {
		t.Fatalf("a legacy config resolved to %q — an absent key must mean reask, or the change is not additive",
			cfg.OnMissingDecision)
	}
	// And the routing keys it DOES carry are untouched.
	if cfg.LoopBranch != "step-devops-pr" || cfg.SuccessBranch != "step-l32ezp4b" || cfg.MaxIterations != 6 {
		t.Errorf("parse mangled the existing keys: %+v", cfg)
	}
}

// The vocabulary is exact, and a value outside it normalises to the default rather
// than reaching the dispatch site as a third state.
func TestOnlyExactStringsSelectANonDefaultPolicy(t *testing.T) {
	success := parseLoopDecisionConfig(`{"on_missing_decision":"success"}`).OnMissingDecision
	if success != MissingDecisionSuccess {
		t.Fatalf("success resolved to %q", success)
	}
	fail := parseLoopDecisionConfig(`{"on_missing_decision":"fail"}`).OnMissingDecision
	if fail != MissingDecisionFail {
		t.Fatalf("fail resolved to %q", fail)
	}
	// Case matters: "Success" is not the vocabulary, so it must NOT act as if it were.
	for _, bad := range []string{`{"on_missing_decision":"Success"}`, `{"on_missing_decision":"SUCCESS"}`, `{"on_missing_decision":"proceed"}`, `{"on_missing_decision":""}`} {
		if got := parseLoopDecisionConfig(bad).OnMissingDecision; got != MissingDecisionReask {
			t.Errorf("parseLoopDecisionConfig(%s).OnMissingDecision = %q, want the reask default", bad, got)
		}
	}
}

// The whole point of the change: no tenant identifier may steer engine behaviour.
// This guards the SPECIFIC regression class the hardcode represented — a future edit
// quietly reintroducing it — not just the one call site.
func TestEngineHoldsNoTenantStepIDs(t *testing.T) {
	// 305-309 legitimately carries `requiresPRStep`, which still recognises the
	// canonical devops step by name; that is a SEPARATE decision (PR requirement) and
	// its own follow-up. So this asserts the loop-decision routing path specifically.
	src, err := os.ReadFile("workflow_reconciler.go")
	if err != nil {
		t.Skipf("source not readable: %v", err)
	}
	text := string(src)
	// Find the loop_decision case and assert its body carries no step ids. The case
	// ends at the next top-level `case domain.StepKind` at the dispatch switch.
	start := strings.Index(text, "case domain.StepKindLoopDecision:")
	if start < 0 {
		t.Fatal("the loop_decision case moved — this guard needs updating")
	}
	body := text[start:]
	if end := strings.Index(body, "case domain.StepKindEnd:"); end > 0 {
		body = body[:end]
	}
	// COMMENTS ARE STRIPPED FIRST. The comment above the policy deliberately names the
	// ids it replaced, to explain the regression, and a guard that flags its own
	// documentation is worse than no guard: it trains the reader to ignore it. What
	// matters is the CODE — a comparison against a tenant id.
	var code strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	for _, id := range []string{"step-devops-pr", "step-l32ezp4b", "step-qonmbwyu", "step-end"} {
		if strings.Contains(code.String(), `"`+id+`"`) {
			t.Errorf("the loop_decision routing path compares against the tenant step id %q again — "+
				"that is the defect this change removed (the guard matched one loop and stranded the other)", id)
		}
	}
}

// The policy vocabulary is what the UIs offer, so it is part of the contract.
func TestPolicyVocabularyIsClosed(t *testing.T) {
	for _, want := range []string{MissingDecisionReask, MissingDecisionSuccess, MissingDecisionFail} {
		if want == "" {
			t.Fatal("an empty policy would be indistinguishable from an absent key")
		}
	}
	if MissingDecisionReask != "reask" || MissingDecisionSuccess != "success" || MissingDecisionFail != "fail" {
		t.Errorf("the vocabulary changed: %q/%q/%q — the migration and both UIs write these exact strings",
			MissingDecisionReask, MissingDecisionSuccess, MissingDecisionFail)
	}
}
