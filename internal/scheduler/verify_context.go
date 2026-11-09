package scheduler

// verify_context.go — THE VERIFIER MUST NOT INHERIT THE IMPLEMENTER'S OPTIMISM.

import "strings"

// verifierStepPatterns identify the steps whose job is to INDEPENDENTLY JUDGE another step's
// output.
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

// verifierIsolationBlock is prepended to a verifier's instructions when its context has been
// isolated.
const verifierIsolationBlock = "\n## You are reviewing independently\n\n" +
	"You have deliberately NOT been given the previous step's summary. That step's account of what it did " +
	"is the CLAIM you are testing, and reading it first would hand you the conclusion you are supposed to " +
	"reach on your own. You have the requirement (the acceptance criteria and description above), the " +
	"facts ledger, the issues file, and the repository itself. Form your verdict from those.\n\n" +
	"If you believe you need the implementer's summary to understand the change, that is itself a finding: " +
	"a change whose intent cannot be established from its diff and its acceptance criteria is not " +
	"reviewable, and you should say so rather than reconstruct the intent.\n\n"
