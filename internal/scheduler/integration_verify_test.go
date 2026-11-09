package scheduler

// integration_verify_test.go — THE STRIP, AND THE VERIFICATION THAT WOULD HAVE CAUGHT IT.
//
// REPORTED, after six runs that all integrated correctly:
//
//	C:\projects\weatherapp contained only .git, .orchicon, .orchicon-worktrees. Zero source files.
//	`git status` showed 55 staged DELETIONS while develop held all 53.
//	Only `git reset --hard develop` recovered it.
//
// The reflog proved six real merges and the tests passed in a fresh checkout, so the integration was CORRECT
// and the checkout was empty. This file pins the two defects in the readiness detector that produced exactly
// that, using real git, and pins the verification that refuses to call a merge a success without it.
//
// The strip's own cause is now explained rather than hypothesised — see the two detector tests below. The
// malformed `.../<runID>/C:/projects/...` worktree registry entry the report flagged as the leading candidate is
// NOT the cause of the empty tree, and this file does not claim it is; it is detected and LOGGED so the next
// occurrence leaves a trace (see malformedWorktreeRegistrations).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE FIRST DEFECT: OUR OWN ARTIFACT DIRECTORY READ AS THE OPERATOR'S UNCOMMITTED WORK.
func TestOurOwnArtifactDirsAreNotTheOperatorsUncommittedWork(t *testing.T) {
	// Exactly what our own directories look like in `git status --porcelain`.
	for _, artifact := range []string{
		"?? .orchicon-worktrees/",
		"?? .orchicon-worktrees/01RUNID/",
		"?? .orchicon/",
		"?? .orchicon/sessions/",
		// Windows separators, since that is where this was reported.
		"?? .orchicon-worktrees\\01RUNID\\",
	} {
		if stale, why := staleCheckoutOnly(artifact); stale || why != "" {
			t.Errorf("%q was judged as the operator's work (%q) — it is the PLATFORM's directory, and treating "+
				"it as theirs is what refused to refresh the tree for six consecutive runs", artifact, why)
		}
	}

	// And a genuinely untracked file still refuses: this must not become a licence to overwrite real work.
	if stale, why := staleCheckoutOnly("?? notes.md"); stale || !strings.Contains(why, "not ours") {
		t.Errorf("a genuine untracked file was not refused (stale=%v reason=%q)", stale, why)
	}
}

// THE SECOND DEFECT: THE STALE SHAPE A REF MOVE ACTUALLY PRODUCES WAS REFUSED.
func TestBothStaleShapesAreRepairable(t *testing.T) {
	cases := map[string]string{
		"index behind HEAD (a ref moved with update-ref)": "D  f1.txt\nD  f2.txt\nD  src/app.js\n",
		"working tree behind the index":                    " D f1.txt\n D f2.txt\n",
		"both at once":                                     "D  a.js\n D b.js\n",
		"staged deletion plus our own artifact dir":        "D  a.js\n?? .orchicon-worktrees/\n",
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			stale, why := staleCheckoutOnly(status)
			if !stale {
				t.Fatalf("%q was NOT judged repairable (%q) — a hard reset fixes exactly this, and refusing it "+
					"leaves the operator's checkout empty forever", status, why)
			}
		})
	}

	// REAL WORK IS STILL REAL WORK. These must keep refusing, or the repair would destroy edits.
	for name, status := range map[string]string{
		"a modification":       " M src/app.js\n",
		"a staged addition":    "A  new.js\n",
		"a staged modification": "M  src/app.js\n",
	} {
		t.Run("refuse: "+name, func(t *testing.T) {
			if stale, _ := staleCheckoutOnly(status); stale {
				t.Errorf("%q was judged repairable — resetting would destroy the operator's work", status)
			}
		})
	}
}

// THE WHOLE STRIP, END TO END, IN REAL GIT: a ref moved with `update-ref` while the tree stays put leaves an
// empty checkout and staged deletions — and the readiness check now says "repairable" instead of refusing.
func TestARefMoveWithNoCheckoutLeavesStagedDeletionsAndIsRepairable(t *testing.T) {
	h := newIntegrateHarness(t)
	ctx := context.Background()
	// The project legitimately holds the platform's own worktree directory, unignored — the reported state.
	if err := os.MkdirAll(filepath.Join(h.repo, worktreeDirName, "01RUNID"), 0o755); err != nil {
		t.Fatalf("mkdir worktree dir: %v", err)
	}
	sha := h.commitOn(t, "run-strip", "app.js", "console.log(1)\n")

	rd := h.r.mainCheckoutRefreshReadiness(ctx, h.repo, defaultIntegrationBranch)
	if !rd.onTarget || !rd.repairable {
		t.Fatalf("a checkout carrying only our own artifact dir was judged unrefreshable (onTarget=%v "+
			"repairable=%v reason=%q)", rd.onTarget, rd.repairable, rd.reason)
	}
	if err := h.r.setBranchRef(ctx, h.repo, defaultIntegrationBranch, sha, "test: move"); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}
	// THE STRIP: the ref now holds src/app.js and the file is not on disk.
	if _, err := os.Stat(filepath.Join(h.repo, "app.js")); err == nil {
		t.Fatal("test premise wrong: the tree already shows the new file")
	}
	status, _ := runGit(ctx, h.repo, "status", "--porcelain")
	if !strings.Contains(status, "D  app.js") {
		t.Fatalf("the stale state did not produce the reported signature; status = %q", status)
	}
	// And the detector must call that repairable, not "staged or modified changes".
	if stale, why := staleCheckoutOnly(status); !stale {
		t.Fatalf("the real stale state was refused: %q", why)
	}

	// The repair materialises the tree.
	if err := h.r.refreshMainCheckout(ctx, h.repo, defaultIntegrationBranch, rd); err != nil {
		t.Fatalf("refreshMainCheckout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.repo, "app.js")); err != nil {
		t.Fatalf("after the repair the file is STILL missing: %v", err)
	}
	// And the platform's own directory survived it.
	if _, err := os.Stat(filepath.Join(h.repo, worktreeDirName, "01RUNID")); err != nil {
		t.Fatalf("the repair deleted the platform's own worktree directory: %v", err)
	}
}

// VERIFICATION REFUSES TO CALL A MERGE A SUCCESS WITHOUT A MATERIALISED TREE.
func TestVerificationFailsWhenTheTreeIsNotMaterialised(t *testing.T) {
	h := newIntegrateHarness(t)
	ctx := context.Background()
	sha := h.commitOn(t, "run-unverified", "app.js", "console.log(1)\n")
	if err := h.r.setBranchRef(ctx, h.repo, defaultIntegrationBranch, sha, "test: move"); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}
	// The ref moved and the tree did NOT: exactly the reported state.
	ok, detail := h.r.verifyIntegration(ctx, h.repo, defaultIntegrationBranch, "run-unverified", "")
	if ok {
		t.Fatalf("verification PASSED on a checkout whose tree is empty — this is the defect that let six "+
			"runs report success into an empty directory (detail=%q)", detail)
	}
	if !strings.Contains(detail, "MISSING") && !strings.Contains(detail, "stale") {
		t.Errorf("the failure detail does not say what was wrong: %q", detail)
	}

	// Repair it and the same call must pass.
	rd := h.r.mainCheckoutRefreshReadiness(ctx, h.repo, defaultIntegrationBranch)
	if err := h.r.refreshMainCheckout(ctx, h.repo, defaultIntegrationBranch, rd); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if ok, detail := h.r.verifyIntegration(ctx, h.repo, defaultIntegrationBranch, "run-unverified", ""); !ok {
		t.Fatalf("verification failed on a materialised tree: %q", detail)
	}
}

// A BRANCH THAT IS NOT AN ANCESTOR NEVER VERIFIES, whatever the tree looks like — the merge itself is re-asserted
// after the fact instead of trusted from the code path that performed it.
func TestVerificationRequiresAncestry(t *testing.T) {
	h := newIntegrateHarness(t)
	ctx := context.Background()
	h.commitOn(t, "run-side", "side.txt", "side\n") // a branch of its own, deliberately not merged
	ok, detail := h.r.verifyIntegration(ctx, h.repo, defaultIntegrationBranch, "run-side", "")
	if ok {
		t.Fatal("verification passed for a branch that is not an ancestor of the integration branch")
	}
	if !strings.Contains(detail, "not an ancestor") && !strings.Contains(detail, "NOT an ancestor") {
		t.Errorf("detail does not name the cause: %q", detail)
	}
}

// THE PROJECT'S OWN CHECK, WHEN IT DECLARES ONE — and its absence means no check, not a pass we invent.
func TestDeclaredVerifyCommandIsRunAndCanFailTheIntegration(t *testing.T) {
	h := newIntegrateHarness(t)
	ctx := context.Background()
	sha := h.commitOn(t, "run-checked", "app.js", "console.log(1)\n")
	if err := h.r.setBranchRef(ctx, h.repo, defaultIntegrationBranch, sha, "test: move"); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}
	rd := h.r.mainCheckoutRefreshReadiness(ctx, h.repo, defaultIntegrationBranch)
	if err := h.r.refreshMainCheckout(ctx, h.repo, defaultIntegrationBranch, rd); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// No declaration → no check.
	if cmd := projectVerifyCommand(h.repo); cmd != "" {
		t.Fatalf("a project with no .orchicon/verify declared a command: %q", cmd)
	}
	if ok, detail := h.r.verifyIntegration(ctx, h.repo, defaultIntegrationBranch, "run-checked", ""); !ok {
		t.Fatalf("verification failed without any declared check: %q", detail)
	}

	// A declaration that FAILS must fail the integration, and the record must carry the output.
	writeVerify(t, h.repo, "printf 'boom: 3 tests failed\\n'; exit 1")
	ok, detail := h.r.verifyIntegration(ctx, h.repo, defaultIntegrationBranch, "run-checked", "")
	if ok {
		t.Fatal("a failing project check was reported as verification success — 'must not be recorded as " +
			"success' is the requirement")
	}
	if !strings.Contains(detail, "3 tests failed") {
		t.Errorf("the failure detail does not carry the check's output: %q", detail)
	}

	// A declaration that PASSES must verify.
	writeVerify(t, h.repo, "printf 'all good\\n'")
	if ok, detail := h.r.verifyIntegration(ctx, h.repo, defaultIntegrationBranch, "run-checked", ""); !ok {
		t.Fatalf("a passing project check failed verification: %q", detail)
	}
}

// A CHECKOUT LEFT ALONE BECAUSE THE OPERATOR HAS UNCOMMITTED WORK IS NOT A FAILURE.
func TestAnUntouchedCheckoutIsNotAVerificationFailure(t *testing.T) {
	h := newIntegrateHarness(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(h.repo, "mine.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	sha := h.commitOn(t, "run-untouched", "app.js", "console.log(1)\n")
	if err := h.r.setBranchRef(ctx, h.repo, defaultIntegrationBranch, sha, "test: move"); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}
	rd := h.r.mainCheckoutRefreshReadiness(ctx, h.repo, defaultIntegrationBranch)
	if rd.repairable || rd.reason == "" {
		t.Fatalf("a dirty checkout was judged refreshable (repairable=%v reason=%q)", rd.repairable, rd.reason)
	}
	ok, detail := h.r.verifyIntegration(ctx, h.repo, defaultIntegrationBranch, "run-untouched", rd.reason)
	if !ok {
		t.Fatalf("an integration was FAILED because the operator has uncommitted work — that would block "+
			"their project for their own edits: %q", detail)
	}
	if !strings.Contains(detail, "not verified") {
		t.Errorf("the pass does not say the tree was not verified: %q", detail)
	}
	// And their work is intact.
	if _, err := os.Stat(filepath.Join(h.repo, "mine.txt")); err != nil {
		t.Fatalf("the operator's uncommitted file was destroyed: %v", err)
	}
}

// A MALFORMED WORKTREE REGISTRATION IS DETECTED AND LOGGED.
func TestMalformedWorktreeRegistrationIsDetected(t *testing.T) {
	cases := map[string]bool{
		// The reported shape: a worktree path with an absolute path appended under it.
		`/repo/.orchicon-worktrees/01ABC/C:/projects/weatherapp/.orchicon-worktrees/01ABC`: true,
		`/repo/.orchicon-worktrees/01ABC/C:\projects\weatherapp\.orchicon-worktrees\01ABC`: true,
		// Ordinary worktree paths are not malformed.
		`/repo/.orchicon-worktrees/01ABC`:       false,
		`/repo/.orchicon-worktrees/01ABC/01DEF`: false,
		`/home/me/projects/other`:               false,
	}
	for path, wantMalformed := range cases {
		if got := isMalformedWorktreePath(path); got != wantMalformed {
			t.Errorf("isMalformedWorktreePath(%q) = %v, want %v", path, got, wantMalformed)
		}
	}
}

// writeVerify declares a project check.
func writeVerify(t *testing.T, repo, command string) {
	t.Helper()
	dir := filepath.Join(repo, ".orchicon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .orchicon: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verify"), []byte("# declared check\n"+command+"\n"), 0o644); err != nil {
		t.Fatalf("write .orchicon/verify: %v", err)
	}
}
