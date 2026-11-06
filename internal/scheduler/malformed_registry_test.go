package scheduler

// malformed_registry_test.go — THE DOUBLED WORKTREE REGISTRY, REPRODUCED AND REPAIRED.
//
// REPORTED across two cycles: three registrations of the shape
//
//	.git/worktrees/<runID>/C:/projects/weatherapp/.orchicon-worktrees/<runID>
//
// all prunable, one per late run, plus a stray `C:/...` tree in the project.
//
// THE PRODUCER IS NOW KNOWN, because it reproduces. `git worktree add` given a Windows path on a filesystem
// where `C:` is not a drive — i.e. inside the runtime container — treats it as a RELATIVE path:
//
//	$ git worktree add 'C:/projects/weatherapp/.orchicon-worktrees/01M6TEST'
//	$ cat .git/worktrees/01M6TEST/gitdir
//	/mnt/c/projects/weatherapp/C:/projects/weatherapp/.orchicon-worktrees/01M6TEST/.git
//
// The admin dir is named `<runID>` (the basename — which is why it looks legitimate), the recorded gitdir is a
// GUEST path the host cannot resolve (hence "prunable"), and the tree it names does not exist there.
//
// These tests drive the repair against that exact state, and pin that it deletes NOTHING it cannot prove is ours.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// poisonWorktreeRegistry recreates the reported defect in a real repository: a worktree added with a host-form
// path, which on this filesystem is a relative path. Returns the malformed path git recorded.
func poisonWorktreeRegistry(t *testing.T, repo, runID string) string {
	t.Helper()
	hostForm := "C:/projects/weatherapp/.orchicon-worktrees/" + runID
	if out, err := runGit(context.Background(), repo, "worktree", "add", hostForm); err != nil {
		t.Fatalf("worktree add %q: %v (%s)", hostForm, err, out)
	}
	// THE SHAPE THE REPORT DESCRIBED — asserted, not assumed, so this test cannot drift from the defect.
	gitdir, err := os.ReadFile(filepath.Join(repo, ".git", "worktrees", runID, "gitdir"))
	if err != nil {
		t.Fatalf("read admin gitdir: %v", err)
	}
	got := strings.TrimSpace(string(gitdir))
	if !strings.Contains(got, "C:/projects/weatherapp") {
		t.Fatalf("the defect did not reproduce: admin gitdir = %q", got)
	}
	return filepath.Join(repo, "C:", "projects", "weatherapp", ".orchicon-worktrees", runID)
}

// THE REPAIR: the prunable registration goes AND the stray tree goes, because that tree is an untracked entry
// that would otherwise block the operator's checkout refresh (it is not one of ours by name).
func TestMalformedRegistrationAndStrayTreeAreRepaired(t *testing.T) {
	h := newIntegrateHarness(t)
	ctx := context.Background()
	strayRoot := poisonWorktreeRegistry(t, h.repo, "01M6TESTRUN")

	// Premise, from the repository's own view.
	wts, err := listWorktrees(ctx, h.repo)
	if err != nil {
		t.Fatalf("listWorktrees: %v", err)
	}
	found := false
	for _, wt := range wts {
		if isMalformedWorktreePath(wt.path) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the poisoned registration is not detected as malformed; worktrees = %+v", wts)
	}
	// And it shows up as an untracked entry, which is the collateral damage.
	status, _ := runGit(ctx, h.repo, "status", "--porcelain")
	if !strings.Contains(status, "C:") {
		t.Fatalf("premise wrong: the stray tree does not appear in git status (%q)", status)
	}

	h.r.malformedWorktreeRegistrations(ctx, h.repo)

	// 1. THE REGISTRATION IS GONE.
	wts, err = listWorktrees(ctx, h.repo)
	if err != nil {
		t.Fatalf("listWorktrees after repair: %v", err)
	}
	for _, wt := range wts {
		if isMalformedWorktreePath(wt.path) {
			t.Errorf("the malformed registration survived the repair: %s", wt.path)
		}
	}
	// 2. THE STRAY TREE IS GONE.
	if _, serr := os.Stat(strayRoot); serr == nil {
		t.Errorf("the stray container-path tree survived at %s", strayRoot)
	}
	// 3. AND `git status` IS CLEAN AGAIN — the reason the tree mattered beyond cosmetics: it is an untracked
	//    entry that is not one of ours, so it would block the checkout refresh.
	status, _ = runGit(ctx, h.repo, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("git status is not clean after the repair: %q", status)
	}
	// 4. The real worktree is untouched.
	if _, serr := os.Stat(h.repo); serr != nil {
		t.Fatalf("the repository itself was damaged: %v", serr)
	}
}

// IT DELETES NOTHING IT CANNOT PROVE IS OURS.
//
// The repair walks a directory tree and removes it, so the failure that matters is removing something real. A
// drive-letter directory that holds ANY file other than a worktree pointer into this repository must survive.
func TestARepairNeverDeletesRealContent(t *testing.T) {
	h := newIntegrateHarness(t)
	ctx := context.Background()

	// A `C:` directory that a USER populated — not a worktree pointer anywhere in it.
	dir := filepath.Join(h.repo, "C:", "projects", "weatherapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("real work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	h.r.malformedWorktreeRegistrations(ctx, h.repo)

	if _, err := os.Stat(filepath.Join(dir, "notes.md")); err != nil {
		t.Fatalf("the repair DELETED a user file inside a drive-letter directory: %v", err)
	}
}

// AN EMPTY DRIVE-LETTER DIRECTORY IS ALSO LEFT ALONE: an empty directory is not evidence of anything, and
// "it looked like our mess" is not proof that it is.
func TestAnEmptyDriveLetterDirectoryIsNotReclaimed(t *testing.T) {
	h := newIntegrateHarness(t)
	ctx := context.Background()
	dir := filepath.Join(h.repo, "C:", "projects")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h.r.malformedWorktreeRegistrations(ctx, h.repo)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the repair removed an empty drive-letter directory it could not prove was ours: %v", err)
	}
}

// A MALFORMED-LOOKING REGISTRATION WHOSE PATH RESOLVES IS REPORTED, NEVER DELETED.
//
// A path that exists is a real worktree at a surprising name. Deleting it would destroy a checkout — so the
// repair's precondition is "the recorded path does not exist", not "the path looks odd".
func TestARealWorktreeAtAnOddNameIsNotPruned(t *testing.T) {
	h := newIntegrateHarness(t)
	ctx := context.Background()
	// A genuine worktree, deliberately at a path whose NAME looks doubled but which RESOLVES.
	odd := filepath.Join(h.repo, worktreeDirName, "01ABC", "C:", "..", "01ABC")
	if out, err := runGit(ctx, h.repo, "worktree", "add", odd); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}
	before, _ := listWorktrees(ctx, h.repo)
	h.r.malformedWorktreeRegistrations(ctx, h.repo)
	after, _ := listWorktrees(ctx, h.repo)
	if len(after) < len(before) {
		t.Fatalf("the repair pruned a worktree whose path RESOLVES (%d registrations before, %d after) — a real "+
			"checkout was destroyed", len(before), len(after))
	}
}

// THE DETECTOR ITSELF, on the shapes that matter. Kept beside the repair so the two cannot drift.
func TestIsMalformedWorktreePath(t *testing.T) {
	cases := map[string]bool{
		// The reported shape, guest and host forms.
		`/repo/.orchicon-worktrees/01ABC/C:/projects/weatherapp/.orchicon-worktrees/01ABC`:  true,
		`/repo/C:/projects/weatherapp/.orchicon-worktrees/01ABC`:                           true,
		`C:/projects/weatherapp/C:/projects/weatherapp/.orchicon-worktrees/01ABC`:          true,
		`/repo/.orchicon-worktrees/01ABC/C:\projects\weatherapp\.orchicon-worktrees\01ABC`: true,
		// Legitimate shapes.
		`/repo/.orchicon-worktrees/01ABC`:       false,
		`/repo/.orchicon-worktrees/01ABC/01DEF`: false, // a parallel-branch child worktree
		`/home/me/projects/other`:               false,
	}
	for path, want := range cases {
		if got := isMalformedWorktreePath(path); got != want {
			t.Errorf("isMalformedWorktreePath(%q) = %v, want %v", path, got, want)
		}
	}
}
