package project

// git_setup.go — PUT THE PROJECT'S GIT STATE IN ORDER, AT THE MOMENT THE OPERATOR CREATES IT.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// GitSetupReport is what the setup found and did, so the caller can log it and the service can
// report it.
type GitSetupReport struct {
	// Needed is true when at least one corrective action was taken.
	Needed bool
	// Done lists the corrective actions, in order.
	Done []string
	// Skipped explains why the repository needed nothing, when Needed is false.
	Skipped string
	// Err is the failure that stopped setup, if any. Setup is best-effort: the project still exists.
	Err error
	// GitMissing means git itself is not installed on the host running this control plane, so NOTHING
	// could be checked.
	GitMissing bool
	// Warning is a condition that will NOT stop this project being created but WILL stop its runs doing
	// what the chosen strategy promises — today.
	Warning string
}

// Summary renders the report for a log line or an operator-facing message.
func (r GitSetupReport) Summary() string {
	switch {
	case r.Err != nil:
		return "git setup could not complete: " + r.Err.Error()
	case len(r.Done) == 0:
		if r.Skipped != "" {
			return r.Skipped
		}
		return "git setup found nothing to do"
	default:
		out := strings.Join(r.Done, "; ")
		if r.Warning != "" {
			out += " — " + r.Warning
		}
		return out
	}
}

// Loud reports whether this outcome must reach the operator in the transcript, not just the log.
func (r GitSetupReport) Loud() bool { return r.Err != nil || r.GitMissing || r.Warning != "" }

// bootstrapIdentity is the identity used for the FIRST commit of a repository that had none.
const (
	bootstrapIdentityName  = "Orchicon"
	bootstrapIdentityEmail = "orchicon@localhost"
	bootstrapMessage       = "chore: initial commit\n\n" +
		"Created by Orchicon so this project's git-backed runs have a revision to work from.\n" +
		"An empty repository cannot have a worktree, because `git worktree add` needs a\n" +
		"HEAD to cut a branch from. Replace this commit with your own history whenever\n" +
		"you like — nothing in Orchicon depends on this message.\n"
)

// EnsureGitState brings a project directory's git state up to what a git-backed strategy requires.
func EnsureGitState(ctx context.Context, dir, strategy string) GitSetupReport {
	if strings.TrimSpace(strategy) == "" || strategy == "none" {
		return GitSetupReport{Skipped: "git strategy is none — the project keeps no repository"}
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return GitSetupReport{Skipped: "the project has no directory yet"}
	}
	// GIT IS REQUIRED FOR EVERYTHING BELOW, so it is checked ONCE here rather than surfacing as a
	// generic error from whichever command happened to run first.
	if _, err := exec.LookPath("git"); err != nil {
		return GitSetupReport{GitMissing: true, Err: fmt.Errorf(
			"git is not installed on the host running this control plane, so this project's repository could "+
				"not be checked or prepared — install git on that host (on Windows that is the native host "+
				"running orchicon, not the WSL distro), then re-save the project: %w", err)}
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		// NOT AN ERROR FOR THE PROJECT — the directory may be on a share this plane cannot see. Say what is
		// true so the operator can fix it, and let the project exist.
		return GitSetupReport{Err: fmt.Errorf("project directory %s is not reachable from the control plane", dir)}
	}

	rep := GitSetupReport{}
	// EACH STEP IS GATED ON THE PREVIOUS ONE SUCCEEDING, and each is phrased as a question about the
	// repository's ACTUAL state, so re-running is safe by construction rather than by bookkeeping.

	// THE DIRECTORY MUST BE ITS OWN REPOSITORY ROOT, or nothing here is safe to touch.
	if root := repoRoot(ctx, dir); root != "" && !sameDir(root, dir) {
		return GitSetupReport{Skipped: fmt.Sprintf(
			"the project directory is inside an existing repository (%s) — Orchicon will not create a nested one "+
				"or commit to the enclosing one, so git-backed runs here will use that repository as it stands",
			root)}
	}

	if !isRepo(ctx, dir) {
		if err := gitRun(ctx, dir, "init"); err != nil {
			rep.Err = fmt.Errorf("initialise the repository: %w", err)
			return rep
		}
		rep.Done = append(rep.Done, "initialised a git repository")
		rep.Needed = true
	}

	hasCommit, err := hasAnyCommit(ctx, dir)
	if err != nil {
		rep.Err = fmt.Errorf("inspect the repository's history: %w", err)
		return rep
	}
	if !hasCommit {
		// An identity is needed for exactly this one commit, and only if none is configured — a repo that
		// already has one keeps it.
		if !hasIdentity(ctx, dir) {
			if err := setLocalIdentity(ctx, dir); err != nil {
				rep.Err = fmt.Errorf("configure a commit identity: %w", err)
				return rep
			}
			rep.Done = append(rep.Done, "configured a local commit identity (Orchicon <orchicon@localhost>)")
			rep.Needed = true
		}
		if err := createBootstrapCommit(ctx, dir); err != nil {
			rep.Err = fmt.Errorf("create the first commit: %w", err)
			return rep
		}
		rep.Done = append(rep.Done, "created the first commit")
		rep.Needed = true
	}

	// THE INTEGRATION BRANCH MUST EXIST, and this is the step the first version of this file got wrong.
	if err := ensureIntegrationBranch(ctx, dir, rep.Needed); err != nil {
		rep.Err = err
		return rep
	}
	if branch, err := integrationBranchNow(ctx, dir); err == nil && branch == integrationBranchName {
		if rep.Needed {
			rep.Done = append(rep.Done, "put the repository on "+integrationBranchName)
		}
	}

	// ---- the remote check: loud, because its absence breaks what the strategy PROMISES ----
	//
	// `local` means "push branch only" and `pr` means "push + PR" (db/project.go). BOTH need an `origin`, and
	// without one the run does its work and then cannot deliver it — the failure lands at the END of a run,
	// where the operator has least context. This file will not invent a remote (that would mean guessing a
	// URL), so it says so instead, while they are looking at the project.
	//
	// Reported for the `none` strategy too? No: an ephemeral run keeps no branch, so it needs no remote, and
	// warning about one would be noise on a strategy that is working as designed.
	if !hasRemote(ctx, dir, "origin") {
		rep.Warning = fmt.Sprintf(
			"this repository has no `origin` remote, so the %q strategy cannot push the run's branch (or open a "+
				"PR) when work is done — add one (git remote add origin <url>) or choose the %q strategy, which "+
				"keeps no branch and needs no remote", strategy, "none")
	}

	if !rep.Needed {
		rep.Skipped = "the repository is already usable for git-backed runs"
	}
	return rep
}

// integrationBranchName is where git-backed work LANDS.
const integrationBranchName = "develop"

// ensureIntegrationBranch makes the repository's default and the integration branch correct.
func ensureIntegrationBranch(ctx context.Context, dir string, createdSomething bool) error {
	// The repo's own default, so this directory's next `git init` — and any reconciliation by git — agrees.
	if err := gitRun(ctx, dir, "config", "init.defaultBranch", integrationBranchName); err != nil {
		return fmt.Errorf("set the repository's default branch: %w", err)
	}
	exists, err := branchCommitExists(ctx, dir, integrationBranchName)
	if err != nil {
		return fmt.Errorf("look for the integration branch: %w", err)
	}
	if !exists {
		if err := gitRun(ctx, dir, "branch", integrationBranchName); err != nil {
			return fmt.Errorf("create the integration branch %s: %w", integrationBranchName, err)
		}
	}
	// A fresh repository is put onto it. An EXISTING repository keeps whatever branch the operator chose — the
	// integration branch is there for runs to be cut from, which does not require moving their HEAD.
	if createdSomething {
		if err := gitRun(ctx, dir, "symbolic-ref", "HEAD", "refs/heads/"+integrationBranchName); err != nil {
			return fmt.Errorf("switch the new repository to %s: %w", integrationBranchName, err)
		}
	}
	return nil
}

// integrationBranchNow reports the branch HEAD is on ("" when detached).
func integrationBranchNow(ctx context.Context, dir string) (string, error) {
	out, err := gitOut(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// branchCommitExists reports whether a local branch exists AND has a commit.
func branchCommitExists(ctx context.Context, dir, branch string) (bool, error) {
	out, err := gitOut(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		// --quiet exits non-zero with NO output when the ref is absent; that is the answer, not a failure.
		if strings.TrimSpace(out) == "" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// hasRemote reports whether the repository has a remote under that name with at least one URL.
func hasRemote(ctx context.Context, dir, name string) bool {
	out, err := gitOut(ctx, dir, "remote")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

// isRepo reports whether dir is inside a git work tree.
func isRepo(ctx context.Context, dir string) bool {
	out, err := gitOut(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// repoRoot returns the enclosing repository's work-tree root, or "" when there is none.
func repoRoot(ctx context.Context, dir string) string {
	out, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// sameDir reports whether two paths name the same directory, resolving symlinks where possible.
func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		a, b = ra, rb
	}
	aa, errA2 := filepath.Abs(a)
	bb, errB2 := filepath.Abs(b)
	if errA2 == nil && errB2 == nil {
		a, b = aa, bb
	}
	// Case-insensitive on the platforms whose filesystems are, so "C:\Proj" and "c:\proj" agree.
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// hasAnyCommit reports whether the repository has at least one commit to its name.
func hasAnyCommit(ctx context.Context, dir string) (bool, error) {
	out, err := gitOut(ctx, dir, "rev-list", "-n", "1", "--all", "--count")
	if err != nil {
		// A "fatal: your current branch does not have any commits yet" here means exactly that: no commit.
		if strings.Contains(err.Error(), "does not have any commits") {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0", nil
}

// hasIdentity reports whether a commit identity would already resolve in this repository (local OR global).
func hasIdentity(ctx context.Context, dir string) bool {
	name, errName := gitOut(ctx, dir, "config", "user.name")
	email, errEmail := gitOut(ctx, dir, "config", "user.email")
	return errName == nil && errEmail == nil &&
		strings.TrimSpace(name) != "" && strings.TrimSpace(email) != ""
}

// setLocalIdentity writes the bootstrap identity into the REPOSITORY's config, never the operator's global one.
func setLocalIdentity(ctx context.Context, dir string) error {
	if err := gitRun(ctx, dir, "config", "user.name", bootstrapIdentityName); err != nil {
		return err
	}
	return gitRun(ctx, dir, "config", "user.email", bootstrapIdentityEmail)
}

// createBootstrapCommit creates the repository's first commit.
func createBootstrapCommit(ctx context.Context, dir string) error {
	return gitRun(ctx, dir, "commit", "--allow-empty", "-m", bootstrapMessage)
}

// gitRun runs git in dir and returns a GitFailure (with git's own output) on failure.
func gitRun(ctx context.Context, dir string, args ...string) error {
	_, err := gitOut(ctx, dir, args...)
	return err
}

// gitOut runs git in dir, returning its combined output and a diagnostic error on failure. THE
// ERROR MUST CARRY GIT'S OUTPUT.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git is not installed on the control plane host")
	}
	cmd := procwin.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// A worker or the planner must never block on an interactive credential prompt during project setup.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return string(out), fmt.Errorf("git %s (in %s): %s", strings.Join(args, " "), dir, firstLine(msg))
	}
	return string(out), nil
}

// firstLine keeps an error message to one line: a git failure often arrives with a multi-line hint block, and
// the summary belongs in a log field while the rest is noise there.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
