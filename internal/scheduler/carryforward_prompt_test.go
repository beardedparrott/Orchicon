package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// sampleReview is a realistic acceptance review shaped like the ones the
// WorkflowReconciler stamps onto a recurring work item after each fire
// (buildAcceptanceReview): a delivery section whose per-step summaries
// embed the workers' FACTS LEARNED lines.
const sampleReview = `## Acceptance Review

**Run:** 01TEST · **Status:** completed

### What was delivered

- Research — Planner — success (succeeded): Produced research/plan.md for the Analyst.

FACTS LEARNED: Reddit .json is hard IP-blocked (HTTP 403) this run — use HN Algolia + GitHub issue search for demand proxies.
- Research — Analyst — success (succeeded): grounded all 8 existence checks.

FACTS LEARNED: A prior run already spawned umbrella epic 01UMBRELLA covering the landscape; must not be re-spawned.
`

// TestCarriedFactsBlockEmpty verifies empty/whitespace-only reviews render
// an empty block — first fires, manual runs, and non-recurring items must
// produce prompts byte-identical to before this feature.
func TestCarriedFactsBlockEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t\n"} {
		if got := carriedFactsBlock(in); got != "" {
			t.Errorf("carriedFactsBlock(%q) = %q, want empty", in, got)
		}
	}
}

// TestCarriedFactsBlockExtractsFactsSkipsInstructional verifies the
// FACTS LEARNED extraction: worker-emitted fact lines are carried,
// while the composite's instructional example line (the contract's own
// "record it as a `FACTS LEARNED:` line" boilerplate) is skipped so the
// contract never echoes back into the prompt as a fake fact.
func TestCarriedFactsBlockExtractsFactsSkipsInstructional(t *testing.T) {
	review := sampleReview +
		"\nFACTS LEARNED (from Research Analyst): Tenant budget_envelope jsonb exists but is never enforced as a cap.\n" +
		"\nThe composite contract reads: when you establish a fact, record it as a `FACTS LEARNED:` line inside your final summary.\n"
	out := carriedFactsBlock(review)

	for _, want := range []string{
		"## Carry-forward — facts learned by the previous fire",
		"recurring work item",
		"FACTS LEARNED: Reddit .json is hard IP-blocked",
		"FACTS LEARNED: A prior run already spawned umbrella epic 01UMBRELLA",
		"FACTS LEARNED (from Research Analyst): Tenant budget_envelope jsonb",
		"### What was delivered",
		"Produced research/plan.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("carry-forward block missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "record it as a `FACTS LEARNED:` line inside") {
		t.Errorf("instructional contract line leaked into carry-forward; got:\n%s", out)
	}
	// The run-provenance header is not useful per-fact payload; the
	// extraction keeps only fact lines + the delivery tail.
	if strings.Contains(out, "**Run:** 01TEST") {
		t.Errorf("review header should not be carried verbatim in fact mode; got:\n%s", out)
	}
	// Facts ride exactly once: the delivery-summary tail is scrubbed of
	// FACTS LEARNED lines (they are already carried in the fact list),
	// so a fact is never duplicated within the block.
	for _, fact := range []string{
		"FACTS LEARNED: Reddit .json is hard IP-blocked",
		"FACTS LEARNED: A prior run already spawned umbrella epic 01UMBRELLA",
		"FACTS LEARNED (from Research Analyst): Tenant budget_envelope jsonb",
	} {
		if n := strings.Count(out, fact); n != 1 {
			t.Errorf("fact carried %d times, want exactly 1: %q\nblock:\n%s", n, fact, out)
		}
	}
	// The delivery tail keeps the real payload (the delivered bullets).
	if !strings.Contains(out, "Produced research/plan.md") {
		t.Errorf("delivery bullets missing from tail; got:\n%s", out)
	}
}

// TestCarriedFactsBlockFallbackTail verifies the no-facts fallback: a
// review without fact lines still forwards the review's tail (bounded)
// so the fire starts from the previous delivery context.
func TestCarriedFactsBlockFallbackTail(t *testing.T) {
	review := "## Acceptance Review\n\n### What was delivered\n\n- Step succeeded with no facts worth carrying.\n"
	out := carriedFactsBlock(review)
	if !strings.Contains(out, "Step with no facts worth carrying") && !strings.Contains(out, "no facts worth carrying") {
		t.Errorf("fallback tail missing delivery content; got:\n%s", out)
	}

	// Oversized review: the tail is bounded.
	long := strings.Repeat("x", carriedFactsMaxChars*2)
	out2 := carriedFactsBlock(long)
	if len(out2) > carriedFactsMaxChars+2 { // + trailing newline allowance
		t.Errorf("carry-forward not capped: %d chars", len(out2))
	}
}

// TestCompositePromptCarriesFactsFromAcceptanceReview verifies the
// workflow composite renders the carry-forward for a recurring item whose
// AcceptanceReview holds the previous fire's review — and renders nothing
// when the review is empty (the every-existing-prompt-shape guarantee).
func TestCompositePromptCarriesFactsFromAcceptanceReview(t *testing.T) {
	ctx := context.Background()
	// No ProjectID/ContextFiles → buildCompositePrompt never touches the tx.
	worker := db.WorkerVersionRow{Role: "Researcher"}
	r := &WorkflowReconciler{}

	bare, err := r.buildCompositePrompt(ctx, nil, "tnt_test", db.WorkItemRow{
		Title: "Research", Status: "pending", RuntimeImage: "orchicon-dev:latest",
	}, worker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bare, "## Carry-forward") {
		t.Errorf("empty review must not render a carry-forward; got:\n%s", bare)
	}

	item := db.WorkItemRow{
		Title:            "Research",
		Status:           "pending",
		RuntimeImage:     "orchicon-dev:latest",
		AcceptanceReview: sampleReview,
	}
	out, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, worker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Carry-forward — facts learned by the previous fire",
		"FACTS LEARNED: Reddit .json is hard IP-blocked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composite missing carry-forward %q; got:\n%s", want, out)
		}
	}
	// Placement sanity: the block is task context — it must precede the
	// instructions section.
	if strings.Index(out, "## Carry-forward") > strings.Index(out, "# Instructions") {
		t.Errorf("carry-forward must precede # Instructions")
	}
}

// TestStandaloneCompositeCarriesFacts verifies the standalone dispatch
// path renders the same carry-forward block (with an unreachable pool the
// function degrades gracefully — the block itself needs no DB).
func TestStandaloneCompositeCarriesFacts(t *testing.T) {
	app := func(review string) string {
		return buildStandaloneComposite(nil, db.ExecutionRow{TenantID: "tnt_test"}, db.WorkItemRow{
			TenantID:         "tnt_test",
			Title:            "Research",
			Description:      "Build the thing.",
			AcceptanceReview: review,
		}, db.WorkerVersionRow{Role: "Senior Engineer"}, "", "")
	}
	if got := app(""); strings.Contains(got, "## Carry-forward") {
		t.Errorf("empty review must not render a carry-forward")
	}
	got := app(sampleReview)
	for _, want := range []string{
		"# Task",
		"## Carry-forward — facts learned by the previous fire",
		"FACTS LEARNED: A prior run already spawned umbrella epic 01UMBRELLA",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("standalone composite missing %q; got:\n%s", want, got)
		}
	}
}