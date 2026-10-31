package db

// prompt_mode_test.go — THE PROMPT MUST NOT TELL A WORKER TO DO WHAT THE PROJECT CANNOT SUPPORT.
//
// Reported: three runs produced fully green, fully committed code and still reported `failure`, because the
// canned prompt unconditionally told the worker to `git push`, `gh pr create`, `gh pr merge` — and the project
// had `git_strategy=local` with NO REMOTE. Every one of those commands can only fail there, so the worker
// spent its time-box on an impossible operation and reported the failure as its own.
//
// The unit under test is the guidance the dispatch actually emits (GitGuidanceBlock), because that is the
// prompt that reaches the worker; a canned AGENTS.md cannot override it.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestLocalStrategyNeverInstructsAnImpossiblePush is the regression assertion.
func TestLocalStrategyNeverInstructsAnImpossiblePush(t *testing.T) {
	block := GitGuidanceBlock(domain.WorktreeReady, "run-branch", "/proj", "local")

	for _, forbidden := range []string{
		"`gh pr create --base develop`",
		"then merge with `gh pr merge",
	} {
		if strings.Contains(block, forbidden) {
			t.Errorf("the local-strategy guidance still instructs %q — a project with no remote cannot do it", forbidden)
		}
	}
	// It must SAY the operations are impossible, not merely omit them: a model that is not told will try.
	for _, want := range []string{"no git remote", "git push", "commit", "SHA"} {
		if !strings.Contains(block, want) {
			t.Errorf("the local-strategy guidance must mention %q so the worker knows what to do instead; got:\n%s",
				want, block)
		}
	}
	// And it must say success is reachable without shipping.
	if !strings.Contains(block, "success") {
		t.Errorf("the local-strategy guidance must say how to report success, or the worker reports failure on "+
			"green work — the reported symptom:\n%s", block)
	}
}

// TestPRStrategyStillExpectsThePR — the other arm must not have been lost to the fix.
func TestPRStrategyStillExpectsThePR(t *testing.T) {
	block := GitGuidanceBlock(domain.WorktreeReady, "run-branch", "/proj", "pr")
	if !strings.Contains(block, "PR / merge ownership") {
		t.Errorf("the pr-strategy guidance dropped the ownership contract; got:\n%s", block)
	}
	if strings.Contains(block, "no git remote") {
		t.Error("the pr-strategy guidance claims there is no remote")
	}
}

// TestCommitAlwaysIsStated pins §7.2: neither canned prompt said it, so leaving work uncommitted was the
// worker's own judgement — and only committed work is visible to a later step.
func TestCommitAlwaysIsStated(t *testing.T) {
	for _, strategy := range []string{"local", "pr"} {
		block := GitGuidanceBlock(domain.WorktreeReady, "run-branch", "/proj", strategy)
		if !strings.Contains(block, "Commit everything in scope") {
			t.Errorf("%s strategy does not state the commit-always rule; got:\n%s", strategy, block)
		}
		if !strings.Contains(block, "git status --porcelain") {
			t.Errorf("%s strategy does not give the check that proves it; got:\n%s", strategy, block)
		}
	}
}

// TestTheSeedPromptsDoNotClaimDirectoriesAreGitignored pins §5.21.
//
// The Quick and DevOps prompts told workers that `architecture-notes/` and `design-notes/` are "gitignored
// working notes, just delete the files". A worker VERIFIED that against the repo and found .gitignore contains
// only `01*/` — so the instruction asserted something false, and a worker that trusted it would leave files
// that land in the next commit.
func TestTheSeedPromptsDoNotClaimDirectoriesAreGitignored(t *testing.T) {
	var b strings.Builder
	for _, w := range CannedWorkers() {
		b.WriteString(w.Role)
		b.WriteString(w.Skills)
		b.WriteString(w.Behavior)
		b.WriteString(w.AgentsMD)
	}
	all := b.String()
	for _, banned := range []string{"The notes are gitignored", "gitignored working notes"} {
		if strings.Contains(all, banned) {
			t.Errorf("a canned prompt still claims %q, which is false for this repository (.gitignore holds "+
				"only 01*/) — a worker checked and flagged the contradiction", banned)
		}
	}
}
