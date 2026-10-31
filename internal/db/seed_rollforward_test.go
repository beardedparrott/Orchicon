package db

// seed_rollforward_test.go — A PROMPT CHANGE THAT NEVER REACHES A WORKER IS NOT A CHANGE.
//
// The seeder syncs a canned worker's role/skills/behavior/agents_md onto its current published version, so a
// prompt fix in this package reaches a live worker only if the seeder DECIDES to sync. That decision used to be
// keyed on hand-written marker strings — seedSafetyMarker, sandboxPlaneMarker, and a per-worker RollMarker.
//
// A marker is a presence check. It can only notice a change someone invented a new marker for, so an ordinary
// edit to a prompt left every existing install running the old text with no log line and no error.
//
// MEASURED on exactly that: the Quick Software Engineer's shipping section was rewritten to be mode-aware
// (commit-only for a `local` project, push + PR only when there is a remote). Its RollMarker is the static
// phrase "GATED ON A GREEN RESULT", which the edit left in place — so the seeder saw nothing to do, and the
// fix was dead on every pre-existing install. Testing the prompt TEXT could not catch it: the source was
// right; the delivery path was not.
//
// These assertions are on the decision, which is the seam the failure lived in.

import (
	"strings"
	"testing"
)

// quickCanned returns the seeded Quick Software Engineer definition.
func quickCanned(t *testing.T) cannedWorker {
	t.Helper()
	for _, w := range CannedWorkers() {
		if w.ID == "01M1ERH9921YS9S1QWGZV8D1VM" {
			return w
		}
	}
	t.Fatal("the Quick Software Engineer is not in the canned set")
	return cannedWorker{}
}

// storedWithEverythingButTheFingerprint renders what an EXISTING install holds: every legacy marker present,
// and the content-fingerprint comment absent (it did not exist before this change).
func storedWithEverythingButTheFingerprint(w cannedWorker) string {
	md := seedAgentsMD(w)
	// Strip the fingerprint comment and append each legacy marker so the old predicate would have been
	// satisfied — this is precisely the state that hid the dead fix.
	md = strings.ReplaceAll(md, cannedSeedFingerprintMarker(w), "")
	for _, m := range []string{seedMarkerComment, sandboxPlaneBlock} {
		if !strings.Contains(md, m) {
			md += m
		}
	}
	if w.RollMarker != "" && !strings.Contains(md, w.RollMarker) {
		md += w.RollMarker
	}
	return md
}

// TestAnOrdinaryPromptEditRollsForward is the regression assertion.
func TestAnOrdinaryPromptEditRollsForward(t *testing.T) {
	w := quickCanned(t)
	stored := storedWithEverythingButTheFingerprint(w)

	// Sanity: the old predicate would NOT have synced this — which is the bug.
	if !strings.Contains(stored, seedSafetyMarker) || !strings.Contains(stored, sandboxPlaneMarker) ||
		!strings.Contains(stored, w.RollMarker) {
		t.Fatal("the fixture does not carry every legacy marker, so it does not model the reported state")
	}

	if !cannedSeedNeedsSync(w, stored, false) {
		t.Fatal("a stored version that predates the seed content was left alone: the prompt change never " +
			"reaches a worker on any existing install, with no log line and no error — this is the reported " +
			"failure, and testing the prompt text cannot catch it")
	}
}

// TestTheSameContentDoesNotRollAgain — the sync must be idempotent, or every boot would mint a version.
func TestTheSameContentDoesNotRollAgain(t *testing.T) {
	w := quickCanned(t)
	if cannedSeedNeedsSync(w, seedAgentsMD(w), false) {
		t.Fatal("a version already carrying this seed's content was re-synced — every boot would mint a new " +
			"published version")
	}
}

// TestAChangedFieldChangesTheKey — the mechanism, not just the one worker.
func TestAChangedFieldChangesTheKey(t *testing.T) {
	w := quickCanned(t)
	stored := seedAgentsMD(w)

	for _, mutate := range []struct {
		name string
		fn   func(*cannedWorker)
	}{
		{"agents_md", func(x *cannedWorker) { x.AgentsMD += "\nan extra instruction\n" }},
		{"behavior", func(x *cannedWorker) { x.Behavior += " and also do this" }},
		{"skills", func(x *cannedWorker) { x.Skills += " • Terraform" }},
		{"role", func(x *cannedWorker) { x.Role += " Be terse." }},
	} {
		edited := w
		mutate.fn(&edited)
		if !cannedSeedNeedsSync(edited, stored, false) {
			t.Errorf("changing %s did not roll the worker forward — that field is synced by the seeder, so an "+
				"edit to it must reach a live worker", mutate.name)
		}
	}
}

// TestADraftIsNeverSyncedHere — the promote block owns publishing a draft current version.
func TestADraftIsNeverSyncedHere(t *testing.T) {
	w := quickCanned(t)
	if cannedSeedNeedsSync(w, "anything at all", true) {
		t.Error("a DRAFT current version must not be synced here — the promote path owns publishing it")
	}
}

// TestDroppedSafetyStillRollsForward — the legacy markers keep doing their job: a stored version whose safety
// or sandbox contract was edited away is re-synced even when the fingerprint still matches.
func TestDroppedSafetyStillRollsForward(t *testing.T) {
	w := quickCanned(t)
	stripped := strings.ReplaceAll(seedAgentsMD(w), seedSafetyMarker, "removed by an edit")
	if !cannedSeedNeedsSync(w, stripped, false) {
		t.Error("a version with the safety marker stripped was left alone — the fingerprint alone cannot detect " +
			"an edit elsewhere in the text")
	}
}

// TestTheQuickProfileDoesNotAssertThePRFlowUnconditionally — the profile is prompt text.
//
// WorkerIdentityBlock renders Role/Skills/Behavior/AGENTS.md into every composite prompt, so the Quick worker's
// Behavior line IS an instruction the worker reads. It said "push to origin, then open a PR into develop and
// merge it" with no notion of strategy, which directly contradicted the mode-aware shipping section in the same
// prompt for a project with no remote — and a worker reading two conflicting instructions follows the one it
// read last, or worse, tries both.
func TestTheQuickProfileDoesNotAssertThePRFlowUnconditionally(t *testing.T) {
	w := quickCanned(t)
	profile := w.Description + "\n" + w.Purpose + "\n" + w.Behavior
	for _, banned := range []string{
		"pushes to origin, and opens + merges the PR",              // Description
		"commit to the run branch, push to origin, then open a PR", // Behavior
	} {
		if strings.Contains(profile, banned) {
			t.Errorf("the Quick worker's profile still asserts the PR flow unconditionally (%q). The profile is "+
				"rendered into the prompt, so it contradicts the mode-aware shipping section on a project with "+
				"no remote.\nprofile:\n%s", banned, profile)
		}
	}
	// And it must say what to do instead, in BOTH modes — the same rule the delivery block states.
	for _, want := range []string{"git strategy", "git_strategy=local"} {
		if !strings.Contains(profile, want) {
			t.Errorf("the profile does not mention %q, so it names no alternative to pushing", want)
		}
	}
}
