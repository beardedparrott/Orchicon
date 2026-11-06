package scheduler

// verify_context.go — THE VERIFIER MUST NOT INHERIT THE IMPLEMENTER'S OPTIMISM.
//
// A reviewer is currently told to read `.orchicon/<run>/summary` before it starts, and that file is the
// implementer's own account of what it did — written by the same model that believes the work is
// finished. Handing it to the reviewer is not neutral: it primes the reviewer with the conclusion it is
// supposed to reach independently. This is the clean-room problem, and the fix is the one every review
// process uses: the verifier gets the REQUIREMENT and the ARTIFACT, not the author's narrative.
//
// The distinction is not "hide information from the reviewer" — the reviewer still gets the full diff,
// the acceptance criteria, the work item description, the facts ledger and the issues file. What it does
// not get is the implementer's summary, because that summary is the claim under review.
//
// WHY THE FACT LEDGER SURVIVES this isolation: it contains falsifiable claims ("the sandbox plane runs
// the pre-feature daemon"), each of which the reviewer can check, and re-deriving environment facts on
// every step is the cost the ledger exists to remove. A summary, by contrast, is the implementer's
// verdict — which is precisely the thing being tested.

import "strings"

// verifierStepPatterns identify the steps whose job is to INDEPENDENTLY JUDGE another step's output.
//
// Matching is by worker name / step id fragment, deliberately, rather than by a workflow config flag:
// these are canned workers whose names are stable, the pattern is readable, and a config-based marker
// would need every existing workflow edited before the isolation applied — meaning it would apply to
// nothing until an operator re-saved each one.
var verifierStepPatterns = []string{
	"reviewer",
	"verifier",
	"qa engineer",
	"approver",
	"auditor",
}

// isVerifierStep reports whether this step's job is to judge rather than to produce.
func isVerifierStep(stepID, stepName, workerRole string) bool {
	haystack := strings.ToLower(strings.Join([]string{stepID, stepName, workerRole}, " "))
	for _, p := range verifierStepPatterns {
		if strings.Contains(haystack, p) {
			return true
		}
	}
	return false
}

// verifierIsolationBlock is prepended to a verifier's instructions when its context has been isolated.
// It states what was withheld and — more importantly — that the reviewer must reach its own conclusion
// from the artifact. Without saying so, a reviewer that notices a missing summary will go looking for it.
const verifierIsolationBlock = "\n## You are reviewing independently\n\n" +
	"You have deliberately NOT been given the previous step's summary. That step's account of what it did " +
	"is the CLAIM you are testing, and reading it first would hand you the conclusion you are supposed to " +
	"reach on your own. You have the requirement (the acceptance criteria and description above), the " +
	"facts ledger, the issues file, and the repository itself. Form your verdict from those.\n\n" +
	"If you believe you need the implementer's summary to understand the change, that is itself a finding: " +
	"a change whose intent cannot be established from its diff and its acceptance criteria is not " +
	"reviewable, and you should say so rather than reconstruct the intent.\n\n"
