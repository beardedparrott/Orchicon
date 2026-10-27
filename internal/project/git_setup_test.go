package project

// git_setup_test.go — THE SETUP THAT WOULD HAVE PREVENTED THE WINDOWS RUN FAILURES.
//
// Every case below was a real state an operator reached by creating a project and choosing a git strategy:
// nothing initialised, initialised with no commit, no commit identity (which a service-account control plane
// routinely lacks), or a project directory sitting INSIDE someone else's repository. The last one is the
// dangerous one — acting on it would commit into, or rewrite the identity of, a repository Orchicon does not own.
//
// These run REAL git in temp directories: the behaviour under test IS git's, and a stub would agree with
// whatever assumption was wrong. Skipped where git is unavailable.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireGitBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed — this test exercises real git state setup")
	}
}

// withHermeticGit keeps the test off the operator's config: no global or system config, so there is no identity
// to inherit — which is the state a service-account control plane is actually in, and the only state in which
// the identity handling means anything.
//
// IT DOES NOT SET GIT_AUTHOR_NAME etc. TO EMPTY. An EMPTY identity variable is not the same as an absent one:
// git takes it literally and refuses with "fatal: empty ident name (for <>)", so the fixture's own commits would
// fail for a reason that has nothing to do with the code under test. The fixture commits below pass their
// identity on the command line with -c instead.
func withHermeticGit(t *testing.T) {
	t.Helper()
	requireGitBinary(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// tempDirOutsideAnyRepo creates a temp directory that is NOT inside a git repository.
//
// THIS IS NOT TIDINESS. t.TempDir() honours GOTMPDIR, which this project sets to .dev/tools/gotmp — INSIDE the
// Orchicon checkout. A project directory there IS inside a repository, so EnsureGitState's nested-repo guard
// correctly declines to touch it and every test would silently assert the decline instead of the setup. Found
// exactly that way: TestEnsureGitStateIsRepeatable reported "needed=false" for a supposedly empty directory.
func tempDirOutsideAnyRepo(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "orchicon-gitsetup-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if root := repoRoot(context.Background(), dir); root != "" {
		t.Fatalf("test fixture dir %s is inside repository %s — this test needs a directory that is not", dir, root)
	}
	return dir
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOut(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// mustGitAs runs git WITH an explicit identity, for fixture commits that are meant to belong to a person.
//
// The identity is passed with -c rather than through the environment, because the hermetic environment above
// deliberately leaves git with no identity at all.
func mustGitAs(t *testing.T, dir, name, email string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=" + name, "-c", "user.email=" + email}, args...)
	return mustGit(t, dir, full...)
}

// TestNotARepositoryIsInitialisedAndCommitted is the case that produced `exit status 128` on every run: a
// project dir with no repository at all.
func TestNotARepositoryIsInitialisedAndCommitted(t *testing.T) {
	withHermeticGit(t)
	dir := tempDirOutsideAnyRepo(t)

	rep := EnsureGitState(context.Background(), dir, "pr")
	if rep.Err != nil {
		t.Fatalf("setup failed: %v", rep.Err)
	}
	if !rep.Needed {
		t.Fatalf("setup reported nothing to do for an empty directory; report: %s", rep.Summary())
	}
	if !isRepo(context.Background(), dir) {
		t.Fatal("the directory is still not a git repository")
	}
	commits := mustGit(t, dir, "rev-list", "--count", "--all")
	if commits != "1" {
		t.Errorf("commit count = %s, want exactly 1 bootstrap commit", commits)
	}
	// A BRANCH, not a detached HEAD — provisioning resolves a base ref from one.
	if _, err := gitOut(context.Background(), dir, "symbolic-ref", "--short", "HEAD"); err != nil {
		t.Errorf("HEAD is not on a named branch, so no base ref can be resolved: %v", err)
	}
	// AND IT IS USABLE: the exact operation that failed on Windows now works.
	if out, err := gitOut(context.Background(), dir, "worktree", "add", filepath.Join(tempDirOutsideAnyRepo(t), "wt"), "-b", "probe"); err != nil {
		t.Errorf("git worktree add still fails after setup — this is the operation the operator's runs failed on: %v\n%s", err, out)
	}
}

// TestIdentityIsConfiguredWhenAbsent pins the wall a SERVICE ACCOUNT hits: no global git identity, so the first
// commit is refused with "unable to auto-detect email address".
func TestIdentityIsConfiguredWhenAbsent(t *testing.T) {
	withHermeticGit(t)
	dir := tempDirOutsideAnyRepo(t)

	rep := EnsureGitState(context.Background(), dir, "local")
	if rep.Err != nil {
		t.Fatalf("setup failed: %v", rep.Err)
	}
	if !hasIdentity(context.Background(), dir) {
		t.Error("no commit identity was configured, so a worker's first commit would still be refused")
	}
	if got := mustGit(t, dir, "config", "user.email"); got != bootstrapIdentityEmail {
		t.Errorf("identity email = %q, want %q", got, bootstrapIdentityEmail)
	}
	// THE BOOTSTRAP COMMIT IS RECOGNISABLE AND EXPLAINS ITSELF, so it is not mistaken for the operator's work.
	body := mustGit(t, dir, "log", "-1", "--format=%B")
	if !strings.Contains(body, "Created by Orchicon") {
		t.Errorf("bootstrap commit message does not explain itself:\n%s", body)
	}
}

// TestAnExistingRepositoryIsLeftAlone is the idempotence contract: a healthy repo — with its own commit, its own
// branch and its own identity — must be untouched, because this runs on EVERY save.
func TestAnExistingRepositoryIsLeftAlone(t *testing.T) {
	withHermeticGit(t)
	dir := tempDirOutsideAnyRepo(t)
	mustGit(t, dir, "init", "-b", "trunk")
	mustGit(t, dir, "config", "user.name", "The Operator")
	mustGit(t, dir, "config", "user.email", "operator@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "README.md")
	mustGitAs(t, dir, "The Operator", "operator@example.invalid", "commit", "-m", "the operator's own first commit")
	before := mustGit(t, dir, "rev-parse", "HEAD")
	branchBefore := mustGit(t, dir, "symbolic-ref", "--short", "HEAD")

	rep := EnsureGitState(context.Background(), dir, "pr")
	if rep.Err != nil {
		t.Fatalf("setup failed on a healthy repo: %v", rep.Err)
	}
	if rep.Needed {
		t.Errorf("setup changed a healthy repository: %v", rep.Done)
	}
	if after := mustGit(t, dir, "rev-parse", "HEAD"); after != before {
		t.Errorf("history moved: %s -> %s", before, after)
	}
	if after := mustGit(t, dir, "symbolic-ref", "--short", "HEAD"); after != branchBefore {
		t.Errorf("branch was renamed: %s -> %s (the operator chose that name)", branchBefore, after)
	}
	if name := mustGit(t, dir, "config", "user.name"); name != "The Operator" {
		t.Errorf("identity was overwritten: user.name = %q", name)
	}
}

// TestNoneStrategyTouchesNothing pins that choosing `none` means git is not involved at all — no repository
// appears in the directory.
func TestNoneStrategyTouchesNothing(t *testing.T) {
	withHermeticGit(t)
	dir := tempDirOutsideAnyRepo(t)

	for _, strategy := range []string{"none", ""} {
		rep := EnsureGitState(context.Background(), dir, strategy)
		if rep.Err != nil {
			t.Fatalf("strategy %q: %v", strategy, rep.Err)
		}
		if rep.Needed {
			t.Errorf("strategy %q initialised a repository", strategy)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Error("a .git directory was created for the none strategy — the platform must not impose git on a project that opted out")
	}
}

// TestAProjectInsideAnotherRepositoryIsNeverModified is THE DANGEROUS CASE. `rev-parse --is-inside-work-tree` is
// true for any subdirectory, so a naive check would treat a nested project as "already a repo" and then write the
// bootstrap commit and the identity INTO THE ENCLOSING REPOSITORY — silently changing someone else's history.
func TestAProjectInsideAnotherRepositoryIsNeverModified(t *testing.T) {
	withHermeticGit(t)
	parent := tempDirOutsideAnyRepo(t)
	mustGit(t, parent, "init", "-b", "develop")
	mustGit(t, parent, "config", "user.name", "Parent Owner")
	mustGit(t, parent, "config", "user.email", "parent@example.invalid")
	if err := os.WriteFile(filepath.Join(parent, "keep.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, parent, "add", "keep.txt")
	mustGitAs(t, parent, "Parent Owner", "parent@example.invalid", "commit", "-m", "parent's commit")
	headBefore := mustGit(t, parent, "rev-parse", "HEAD")

	// The project directory is a SUBDIRECTORY of that repository.
	projectDir := filepath.Join(parent, "services", "my-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rep := EnsureGitState(context.Background(), projectDir, "pr")
	if rep.Err != nil {
		t.Fatalf("setup must not error on a nested project: %v", rep.Err)
	}
	if rep.Needed {
		t.Errorf("setup modified a nested project: %v", rep.Done)
	}
	if !strings.Contains(rep.Skipped, "inside an existing repository") {
		t.Errorf("the report must SAY it declined and why; got: %s", rep.Skipped)
	}

	// THE PARENT IS UNTOUCHED: same HEAD, same identity, and no nested repository was created.
	if after := mustGit(t, parent, "rev-parse", "HEAD"); after != headBefore {
		t.Errorf("the ENCLOSING repository's history moved: %s -> %s", headBefore, after)
	}
	if name := mustGit(t, parent, "config", "user.name"); name != "Parent Owner" {
		t.Errorf("the ENCLOSING repository's identity was overwritten: %q", name)
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".git")); err == nil {
		t.Error("a nested repository was created — splitting the operator's history in two")
	}
}

// TestUnreachableDirectoryIsReportedNotFatal pins the best-effort contract: the project must still be creatable
// when its directory is on a share this plane cannot see, and the operator must be told what is wrong.
func TestUnreachableDirectoryIsReportedNotFatal(t *testing.T) {
	requireGitBinary(t)
	missing := filepath.Join(tempDirOutsideAnyRepo(t), "does-not-exist")
	rep := EnsureGitState(context.Background(), missing, "pr")
	if rep.Err == nil {
		t.Fatal("an unreachable directory must be reported")
	}
	if !strings.Contains(rep.Err.Error(), missing) {
		t.Errorf("the failure must name the directory; got: %v", rep.Err)
	}
	if !strings.Contains(rep.Summary(), "could not complete") {
		t.Errorf("the summary must say the setup did not complete; got: %s", rep.Summary())
	}
}

// TestEnsureGitStateIsRepeatable pins that running it twice is indistinguishable from running it once — it runs
// on every project save, so a second run must be a no-op.
func TestEnsureGitStateIsRepeatable(t *testing.T) {
	withHermeticGit(t)
	dir := tempDirOutsideAnyRepo(t)

	first := EnsureGitState(context.Background(), dir, "pr")
	if first.Err != nil || !first.Needed {
		t.Fatalf("first run: err=%v needed=%v", first.Err, first.Needed)
	}
	head := mustGit(t, dir, "rev-parse", "HEAD")

	second := EnsureGitState(context.Background(), dir, "pr")
	if second.Err != nil {
		t.Fatalf("second run failed: %v", second.Err)
	}
	if second.Needed {
		t.Errorf("second run changed the repository again: %v", second.Done)
	}
	if after := mustGit(t, dir, "rev-parse", "HEAD"); after != head {
		t.Errorf("second run moved HEAD: %s -> %s", head, after)
	}
	if n := mustGit(t, dir, "rev-list", "--count", "--all"); n != "1" {
		t.Errorf("commit count = %s after two runs, want 1", n)
	}
}
