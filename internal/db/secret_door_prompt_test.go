package db

import (
	"strings"
	"testing"
)

// WHY THIS FILE EXISTS.
//
// The platform now gives a worker a way to fetch a credential on demand (`orchicon secret get`), and
// that door is useless if nobody tells the workers it exists. The operator's point:
//
//	"we probably need to update the canned workers so they know this secret calling RPC is something
//	 they should do to get credentials as well, unless they should already know that through other
//	 means/context."
//
// They should NOT "already know": nothing in the prompt mentioned it, and a worker that expects a
// token in its environment will report "no credentials available" — which is exactly the complaint
// that started this. So the door must be stated, in the block EVERY worker receives (safetyBlock),
// and it must state the three things a worker gets wrong:
//
//  1. the command and how to use it (stdout only, so $(...) works)
//  2. that the GRANT is the operator's selection, so a failure is not workable-around
//  3. that reads are AUDITED, and the value must never be printed
func TestSafetyBlockTeachesTheSecretDoor(t *testing.T) {
	for _, want := range []string{
		"orchicon secret get",
		"stdout",
		"orchicon secret get",
		"audited",
	} {
		if !strings.Contains(safetyBlock, want) {
			t.Errorf("safetyBlock does not mention %q. A worker that does not know the door exists "+
				"reports \"no credentials available\" and refuses work it could have done.", want)
		}
	}
}

// TestSecretDoorNamesTheDoNotImproviseRule — the failure mode a grant refusal must not trigger.
func TestSecretDoorNamesTheDoNotImproviseRule(t *testing.T) {
	if !strings.Contains(safetyBlock, "Do NOT improvise") {
		t.Error("safetyBlock does not forbid improvising around a failed fetch, so a worker that hits " +
			"one may go hunting a keyring or another worker's environment")
	}
	if !strings.Contains(safetyBlock, "never print the value") {
		t.Error("safetyBlock does not forbid printing the value — the audit trail records the read, " +
			"and printing it turns the audit into the leak")
	}
}

// TestEveryWorkerCarriesTheSecretDoor: the rule reaches workers ONLY through the stable prompt
// prefix (safetyBlock), never through a per-worker AGENTS.md duplicate.
func TestEveryWorkerCarriesTheSecretDoor(t *testing.T) {
	if !strings.Contains(safetyBlock, "CREDENTIALS: FETCH THEM WITH") {
		t.Fatal("the secret door is not in safetyBlock, so no adapter's system prompt carries it")
	}
	for _, w := range CannedWorkers() {
		if strings.Contains(seedAgentsMD(w), safetyBlock) {
			t.Errorf("%s duplicates the whole safety block into its AGENTS.md", w.Name)
		}
	}
}
