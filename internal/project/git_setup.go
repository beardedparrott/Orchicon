package project

// git_setup.go — PUT THE PROJECT'S GIT STATE IN ORDER, AT THE MOMENT THE OPERATOR CREATES IT.
//
// THE FAILURE THIS PREVENTS, end to end. An operator created a project on Windows, chose a git strategy, and
// kicked off a run. Every attempt failed at worktree provisioning with `git worktree add: exit status 128` —
// and the underlying repository was in a state no strategy other than `none` can use at all:
//
//   - not a git repository (nothing was ever `git init`-ed in the project dir);
//   - or initialised with NO COMMIT, so `git worktree add` has no HEAD to cut a branch from;
//   - or a repo with no `user.name`/`user.email`, so the first commit a worker tries is refused;
//   - or tracking a remote whose default branch cannot be resolved (see worktree_baseref.go).
//
// None of that is something a user should have to discover from a `fatal:` line. Choosing a git strategy is a
// statement that "this project's work happens in git", so the platform — not the agent, and not the operator —
// brings the repository up to the state that statement requires. The operator's own steer: "if someone chooses
// a git strategy other than none, then when the project is created, Orchicon itself should run all necessary
// commands to set up the git init for the user".
//
// WHAT IT WILL NOT DO, each a deliberate line:
//
//   - It never touches a repository that is already usable. The whole check is idempotent: a healthy repo is
//     left byte-for-byte alone, including its upstream and its default branch.
//   - It never creates a commit in a repository that already HAS commits. History is the operator's.
//   - It never invents a remote. Adding one would mean guessing a URL.
//   - It never rewrites the identity of a repo that already has one configured.
//   - BEST-EFFORT BY CONTRACT: a failure is reported, never fatal to project creation. A project whose
//     directory is on an unreachable share must still be creatable — the operator then fixes the directory and
//     re-saves, which re-runs this.
//
// The bootstrap commit exists only because git cannot do anything else in an empty repo: `worktree add` needs a
// HEAD to cut from, and a branch needs a commit to point at. It is created with an explicit Orchicon identity
// and a message that says why it is there, so nobody later mistakes it for the operator's work.

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

// GitSetupReport is what the setup found and did, so the caller can log it and the service can report it.
//
// Done is the ordered list of actions taken, in the operator's language ("initialised an empty git repository",
// "created the first commit"). Skipped says why nothing was needed when that is the case. Both are deliberately
// bite-sized: they end up in a log line and, when something was created, in the project's event stream.
type GitSetupReport struct {
	// Needed is true when at least one corrective action was taken.
	Needed bool
	// Done lists the corrective actions, in order.
	Done []string
	// Skipped explains why the repository needed nothing, when Needed is false.
	Skipped string
	// Err is the failure that stopped setup, if any. Setup is best-effort: the project still exists.
	Err error
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
		return strings.Join(r.Done, "; ")
	}
}

// bootstrapIdentity is the identity used for the FIRST commit of a repository that had none.
//
// IT IS EXPLICIT RATHER THAN INHERITED, and that is a correctness point, not a preference: a control plane
// running as a service account on Linux or as a scheduled task on Windows routinely has NO global git identity,
// so `git commit` fails with "unable to auto-detect email address" — which is exactly the wall an operator
// would hit on their first run, long after creating the project.
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
//
// It is IDEMPOTENT: call it on create, on edit, and on every save; a healthy repository is untouched.
// strategy is the project's git strategy — "none" (or empty) means git is not involved and this is a no-op.
func EnsureGitState(ctx context.Context, dir, strategy string) GitSetupReport {
	if strings.TrimSpace(strategy) == "" || strategy == "none" {
		return GitSetupReport{Skipped: "git strategy is none — the project keeps no repository"}
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return GitSetupReport{Skipped: "the project has no directory yet"}
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
	//
	// `git rev-parse --is-inside-work-tree` is TRUE FOR A SUBDIRECTORY of any repository, so a project dir that
	// sits inside a parent repo (a module under a monorepo, a folder under the operator's own checkout) would
	// look like "already a repo" — and every step below would then act on the PARENT: the bootstrap commit
	// would land in someone else's history, and `git config user.name` would rewrite their identity. Silently.
	//
	// So the question is not "is this inside a work tree" but "is this ITSELF the work tree root", and a
	// project nested inside another repository is reported rather than modified. Creating a nested repo would
	// be the other wrong answer: it would split the operator's history in two.
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

	// A BRANCH NAME IS PART OF THE STATE, not cosmetics: provisioning cuts branches from a resolved base, and a
	// repo left on an unborn or detached HEAD has nothing to resolve (see worktree_baseref.go). A repo that
	// already has a named branch is untouched.
	if _, err := gitOut(ctx, dir, "symbolic-ref", "--short", "HEAD"); err != nil {
		// Detached HEAD with a commit: name the default branch so a base ref exists. The name matches
		// baseRefCandidates' preferred entry so every existing project's behaviour is consistent.
		if err := gitRun(ctx, dir, "symbolic-ref", "HEAD", "refs/heads/develop"); err != nil {
			rep.Err = fmt.Errorf("name the default branch: %w", err)
			return rep
		}
		rep.Done = append(rep.Done, "put HEAD on a named branch (develop)")
		rep.Needed = true
	}

	if !rep.Needed {
		rep.Skipped = "the repository is already usable for git-backed runs"
	}
	return rep
}

// isRepo reports whether dir is inside a git work tree.
func isRepo(ctx context.Context, dir string) bool {
	out, err := gitOut(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// repoRoot returns the enclosing repository's work-tree root, or "" when there is none.
//
// This is the guard that makes the rest safe: see the call site for why "inside a work tree" is the wrong
// question to act on.
func repoRoot(ctx context.Context, dir string) string {
	out, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// sameDir reports whether two paths name the same directory, resolving symlinks where possible.
//
// Windows and macOS both have canonical-form traps here (a drive letter's case, /private/var vs /var,
// /tmp as a symlink), and a false "not the same" would make the nested-repo guard skip a repository we DO own.
// A resolution failure on either side falls back to a cleaned absolute comparison rather than failing open.
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
//
// It asks for the commit's OWN existence rather than for a branch, because a detached-but-committed repository
// is usable and a branchless one is not — the two differ, and only the second needs a commit.
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
//
// `--allow-empty` IS THE POINT: the project directory may hold only files the operator has gitignored (or none
// at all), and `git commit` with nothing staged fails. An empty first commit is the standard, honest way to
// give an otherwise-empty repository a HEAD — and the message says so, so it is not mistaken for work.
func createBootstrapCommit(ctx context.Context, dir string) error {
	return gitRun(ctx, dir, "commit", "--allow-empty", "-m", bootstrapMessage)
}

// gitRun runs git in dir and returns a GitFailure (with git's own output) on failure.
func gitRun(ctx context.Context, dir string, args ...string) error {
	_, err := gitOut(ctx, dir, args...)
	return err
}

// gitOut runs git in dir, returning its combined output and a diagnostic error on failure.
//
// THE ERROR MUST CARRY GIT'S OUTPUT. `exit status 128` names nothing — the same lesson the worktree reconciler
// learned the expensive way (internal/scheduler/worktree_giterror.go). Here it matters just as much: the
// operator is looking at a project save that silently did not fix their repository.
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
