package db

// THE SHARED-CHECKOUT BOUNDARY, PINNED AS PROMPT TEXT.
//
// This rule is prompt-only: nothing in the platform stops a worker from writing into the operator's
// checkout (the guard blocks paths OUTSIDE the project, and the worktree is INSIDE it, so the guard is
// silent here by design). That makes the prompt the whole mechanism, which is why the text is asserted
// rather than assumed.
//
// What it is guarding against actually happened, twice: a worker's test fixture ran `git init` and
// `git config` believing `-C <dir>` scoped them. It does not — GIT_DIR/GIT_WORK_TREE override both -C
// and discovery — so the fixture wrote `core.worktree` and a bootstrap identity into the operator's
// `.git/config`, which then redirected their own `git pull` into a worker's worktree.

import (
	"strings"
	"testing"
)

// The boundary must be stated in the block EVERY canned worker carries, and it must hold in both modes.
func TestSafetyBlockStatesTheSharedCheckoutBoundary(t *testing.T) {
	for _, want := range []struct{ fragment, why string }{
		{"belongs to the operator",
			"the checkout must be named as the operator's, or a worker reading \"only touch files inside " +
				"the project directory\" concludes the checkout is fair game — it IS inside the project"},
		{"your work goes in YOUR worktree",
			"the worktree must be named as the destination for the work"},
		{"never touch its `.git`",
			"the shared config is the specific thing that was damaged, and it redirects EVERY worktree"},
		{"This holds in BOTH modes",
			"local mode is where the boundary is least obvious: the run is in-process on the host with " +
				"deliberately more access, so without this clause the extra access reads as extra permission"},
		{"boundary is on the REPOSITORY, not the filesystem",
			"the distinction matters — local runs must keep /tmp, installs and reads"},
		{"never on `develop`",
			"the branch half of the rule"},
	} {
		if !strings.Contains(safetyBlock, want.fragment) {
			t.Errorf("safetyBlock does not state %q — %s", want.fragment, want.why)
		}
	}
}

// Local mode must NOT be told it has less access than it does: the operator explicitly wants workers able
// to use /tmp, install things and write on the host. A boundary that over-reaches would be obeyed too,
// and would break real work.
func TestTheBoundaryDoesNotForbidLocalHostAccess(t *testing.T) {
	for _, banned := range []string{
		"do not write anywhere on the host",
		"never write outside your worktree",
		"no access to the host filesystem",
		"only write inside .orchicon-worktrees",
	} {
		if strings.Contains(safetyBlock, banned) {
			t.Errorf("safetyBlock over-reaches: %q forbids local-mode host access the operator explicitly "+
				"grants (/tmp, installs, disposable sandbox planes). The rule is about REPOSITORY writes.", banned)
		}
	}
	// And it must positively acknowledge the access, so the two halves cannot be read as a contradiction.
	if !strings.Contains(safetyBlock, "deliberately has more access") {
		t.Error("safetyBlock states the boundary without acknowledging that a local run legitimately has " +
			"more access — a worker left to reconcile the two will resolve it either way, and one of " +
			"those ways breaks its work")
	}
}

// The command-level companion. The boundary says WHERE; this says HOW, because the damaging mistake was
// made by someone who believed they were being careful.
func TestGitHygieneBlockCarriesTheSpecificCommands(t *testing.T) {
	for _, want := range []struct{ fragment, why string }{
		{"git rev-parse --show-toplevel",
			"the one command that tells a worker whether it is in its worktree or the operator's checkout"},
		{"git rev-parse --abbrev-ref HEAD",
			"the command that reveals which branch it would commit to"},
		{"env -u GIT_DIR -u GIT_WORK_TREE",
			"the exact incantation for a scratch repo — without it a fixture writes the shared repo"},
		{"does NOT scope a command",
			"the mechanism must be stated, or \"I used -C\" looks like a safety argument"},
		{"git worktree add --detach /tmp/orchicon",
			"the sanctioned way to get a sibling worktree for a base-commit comparison"},
	} {
		if !strings.Contains(gitHygieneBlock, want.fragment) {
			t.Errorf("gitHygieneBlock does not carry %q — %s", want.fragment, want.why)
		}
	}
}

// EVERY canned worker must carry the hygiene block, or the rule reaches 10 of 11 and the one it misses is
// the one that does the damage. This is the sweep that a per-worker edit forgets.
func TestEveryCannedWorkerCarriesTheGitHygieneBlock(t *testing.T) {
	for _, w := range CannedWorkers() {
		agents := seedAgentsMD(w)
		if !strings.Contains(agents, "## Git hygiene") {
			t.Errorf("%s (%s) does not carry the git hygiene block — a worker without it has no statement "+
				"of the mechanism that wrote the operator's repo config", w.Name, w.ID)
		}
	}
}

// The safety rules ride the prompt PREFIX, not each AGENTS.md (see TestEveryWorkerCarriesTheModelRule).
// The hygiene block is different on purpose: it is emitted per worker so the roll-forward reaches an
// existing published version, and asserting it here pins that choice.
func TestGitHygieneBlockSurvivesTheAgentsMDBuild(t *testing.T) {
	if !strings.Contains(seedAgentsMDBase(CannedWorkers()[0]), gitHygieneBlock) {
		t.Error("the hygiene block is missing from the built AGENTS.md. seedAgentsMDBase strips ONLY " +
			"safetyBlock (because those rules ride the prompt prefix); if it has started stripping this " +
			"block too, the roll-forward will not carry the commands to existing published workers")
	}
}
