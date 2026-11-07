// Package verify turns "the worker said it worked" into "the platform checked".
//
// THE DEFECT THIS EXISTS TO CLOSE. TaskReconciler.OnResult decided an execution's outcome from one
// signal: the word after `ORCHICON WORKER SUMMARY:`. `success` meant success, and — because a
// truncated stream or a reporting omission must not fail a run — the ABSENCE of a marker also meant
// success. Nothing compared the claim to the repository. Measured consequence on this platform: work
// item 01M5X62Z1X8N08KD7KAY0G6FSV shipped as `succeeded` with an acceptance review reading "confirmed
// the fix is already merged... no duplicate PR was created", when the work had changed nothing at all.
// The worker's own verdict was the completion criterion, and every gate downstream inherited it.
//
// WHAT THIS PACKAGE IS. A pure, dependency-free decision function: given the facts of a completed
// step (what the worker claimed, what git recorded, what the baseline said), decide whether that
// claim is SUBSTANTIATED. It runs no commands and reads no files — the callers own every side effect
// (WorktreeEvidence's collector, the reconciler's git probes) — which is what makes the rules unit-
// testable against real recorded cases rather than only against a live repository.
//
// WHAT IT DELIBERATELY DOES NOT DO. It does not judge whether the work is GOOD. Semantic correctness
// ("does this do what the user wanted") is not mechanically decidable for arbitrary projects, and a
// system that pretends otherwise fails confidently. This package decides whether the EVIDENCE that
// work happened exists and is consistent. The design rule for everything below is therefore:
//
//	THE PLATFORM MUST BE ABLE TO SAY "I DON'T KNOW" — AND THAT ANSWER MUST NOT LOOK LIKE SUCCESS.
//
// That is what VerdictUnverified is for. It is a first-class, non-success, non-failure state: the
// work was not shown to be wrong, and it was not shown to be right. Collapsing it into either
// direction is the defect this package exists to prevent.
package verify

import (
	"fmt"
	"sort"
	"strings"
)

// Verdict is the evidence-based outcome of a completed step.
//
// THREE states, and the third is the one that matters. Before this package there were two (the
// worker's word, and the process exit code), and both of them resolve to a boolean — so "nothing was
// demonstrated" had no way to be expressed and was silently rounded to success.
type Verdict string

const (
	// VerdictVerified — the claim is substantiated: the work exists, it is consistent with what was
	// claimed, and nothing that used to pass now fails.
	VerdictVerified Verdict = "verified"

	// VerdictUnsubstantiated — the claim is contradicted by evidence. The worker said work happened
	// and the repository shows it did not (or shows it broke something). This is a real failure and
	// must fail the step.
	VerdictUnsubstantiated Verdict = "unsubstantiated"

	// VerdictUnverified — no determination could be made. Distinct from both of the above: it is NOT
	// a failure (the work may be perfect) and it must NOT be reported as success (nothing confirmed
	// it either). The operator's whole complaint — "sometimes it works, sometimes it doesn't, and I
	// find out later" — is this state being silently reported as success.
	VerdictUnverified Verdict = "unverified"

	// VerdictNotApplicable — no check APPLIED to this step at all, so the step was neither confirmed nor
	// left ambiguous. It is the honest verdict for a plan-only or approval step: nothing about it is
	// mechanically checkable, and saying "verified" would claim a confirmation that did not happen.
	//
	// WHY THIS IS SEPARATE FROM UNVERIFIED. `unverified` means "a check that should have run could not" —
	// it is a gap in the evidence and an operator should treat it as one. `not_applicable` means "this step
	// is not the kind of thing that is checked" — a plan, an approval, graph plumbing. Collapsing the two
	// would flood the acceptance review with "unverified" on every architect/approver step until the word
	// stopped meaning anything, which is exactly how a signal gets ignored.
	VerdictNotApplicable Verdict = "not_applicable"
)

// IsSuccess reports whether the verdict permits the step to be treated as a CONFIRMED success.
//
// ONLY VerdictVerified does. This is the single most important line in the package: it is where
// "unverified" stops being rounded up to "fine", and where `not_applicable` correctly refuses to claim a
// confirmation that did not happen.
func (v Verdict) IsSuccess() bool { return v == VerdictVerified }

// String renders the verdict for logs, records and the UI.
func (v Verdict) String() string { return string(v) }

// Check is one named, self-describing piece of evidence.
//
// Every check reports its own outcome rather than a bare bool, because the point of the exercise is
// a record a human can read when a step is questioned — "why was this unverified?" must have a
// concrete answer, not a shrug. Detail carries the observed value (a count, a list, a failing test
// name) so the answer is in the record rather than requiring a re-run to reconstruct.
type Check struct {
	// Name is a stable machine identifier (snake_case) — what the check is, not what it found.
	Name string
	// Status is the outcome.
	Status CheckStatus
	// Detail says WHAT WAS OBSERVED, in one line. Required for anything that is not CheckPass: a
	// check that fails or abstains without saying what it saw is not evidence.
	Detail string
}

// CheckStatus is the outcome of a single check.
type CheckStatus string

const (
	// CheckPass — the check ran and the evidence supports the claim.
	CheckPass CheckStatus = "pass"
	// CheckFail — the check ran and CONTRADICTS the claim. Any CheckFail makes the verdict
	// unsubstantiated: contradiction outranks absence.
	CheckFail CheckStatus = "fail"
	// CheckAbstain — the check could not run or could not conclude (no baseline available, the
	// project declares no gate, git could not be read). Absence of evidence, not evidence of
	// absence: it drives the verdict toward Unverified, never toward Unsubstantiated.
	CheckAbstain CheckStatus = "abstain"
	// CheckSkip — the check does not apply to this step at all (a plan-only step has no code
	// deliverable, so the commit check is not a failure, it is inapplicable). A skip is not an
	// abstention: it carries no information about the claim and never lowers confidence.
	CheckSkip CheckStatus = "skip"
)

// Result is the full evidence record for one step.
type Result struct {
	Verdict Verdict
	Checks  []Check
	// Reason is a one-line human summary of WHY the verdict is what it is — the first failing or
	// abstaining check, rendered. Empty when Verdict is VerdictVerified with no failed checks.
	Reason string
}

// Add appends a check.
func (r *Result) Add(name string, status CheckStatus, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail})
}

// Failed returns the checks that contradicted the claim.
func (r *Result) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			out = append(out, c)
		}
	}
	return out
}

// Abstained returns the checks that could not conclude.
func (r *Result) Abstained() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == CheckAbstain {
			out = append(out, c)
		}
	}
	return out
}

// Conclude derives the verdict from the collected checks.
//
// THE PRECEDENCE IS THE DESIGN:
//
//  1. ANY FAIL → Unsubstantiated. A contradiction is a positive fact; it outranks every absence.
//  2. NO FAIL, ANY ABSTAIN → Unverified. Something that should have been checked could not be, so
//     the step is not demonstrated. This is where a project with no declared gate, or a run whose
//     baseline could not be captured, lands — visibly, instead of as a silent success.
//  3. NO FAIL, NO ABSTAIN → Verified. Every applicable check ran and passed.
//
// Note what is NOT a criterion: the number of checks. A plan-only step whose only applicable check
// is "HEAD moved" can be verified by that alone. Adding checks raises confidence; it does not define
// the verdict, and demanding a fixed count would make honest steps unverifiable.
func (r *Result) Conclude() Result {
	r.Verdict = VerdictVerified
	r.Reason = ""

	if failed := r.Failed(); len(failed) > 0 {
		r.Verdict = VerdictUnsubstantiated
		r.Reason = describe(failed, "not substantiated: ")
		return *r
	}
	if abstained := r.Abstained(); len(abstained) > 0 {
		r.Verdict = VerdictUnverified
		r.Reason = describe(abstained, "unverified: ")
		return *r
	}
	// NO CHECK APPLIED AT ALL is not the same as every check passing. A verdict of `verified` asserts that
	// something was confirmed; with every check skipped, nothing was — and claiming otherwise is the silent
	// success this package exists to remove, just wearing a different hat.
	if !r.anyCheckApplied() {
		r.Verdict = VerdictNotApplicable
		r.Reason = "no mechanical check applies to this step (it produces no repository evidence to examine)"
	}
	return *r
}

// anyCheckApplied reports whether at least one check actually reached a conclusion.
func (r *Result) anyCheckApplied() bool {
	for _, c := range r.Checks {
		if c.Status == CheckPass || c.Status == CheckFail || c.Status == CheckAbstain {
			return true
		}
	}
	return false
}

// describe renders up to two checks as a one-line reason. Two, not all: the reason is a summary for
// a record and a UI banner, and a wall of every abstention buries the first one, which is the one
// that explains the verdict.
func describe(checks []Check, prefix string) string {
	parts := make([]string, 0, 2)
	for i, c := range checks {
		if i == 2 {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(checks)-2))
			break
		}
		if c.Detail == "" {
			parts = append(parts, c.Name)
			continue
		}
		parts = append(parts, c.Name+": "+c.Detail)
	}
	return prefix + strings.Join(parts, "; ")
}

// FormatRecords renders the evidence as a human-readable block for the acceptance review and for
// the failure reason on an execution row.
//
// It is Markdown because that is what the operator reads — the acceptance review is the document
// they open when deciding whether to trust a run, and evidence that is not in it may as well not
// exist. Ordered so the verdict leads: a reader who stops after one line still knows the answer.
func (r Result) FormatRecords() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Verification: %s**", r.Verdict)
	if r.Reason != "" {
		fmt.Fprintf(&b, " — %s", r.Reason)
	}
	b.WriteString("\n")
	// Stable order: failures first (they are the reason for a bad verdict), then abstentions, then
	// the rest. Within a status, sorted by name so two renders of the same evidence agree.
	order := map[CheckStatus]int{CheckFail: 0, CheckAbstain: 1, CheckPass: 2, CheckSkip: 3}
	checks := append([]Check(nil), r.Checks...)
	sort.SliceStable(checks, func(i, j int) bool {
		if order[checks[i].Status] != order[checks[j].Status] {
			return order[checks[i].Status] < order[checks[j].Status]
		}
		return checks[i].Name < checks[j].Name
	})
	for _, c := range checks {
		sym := map[CheckStatus]string{
			CheckPass:    "✓",
			CheckFail:    "✗",
			CheckAbstain: "?",
			CheckSkip:    "–",
		}[c.Status]
		if sym == "" {
			sym = "?"
		}
		if c.Detail == "" {
			fmt.Fprintf(&b, "- %s `%s`\n", sym, c.Name)
			continue
		}
		fmt.Fprintf(&b, "- %s `%s` — %s\n", sym, c.Name, c.Detail)
	}
	return b.String()
}
