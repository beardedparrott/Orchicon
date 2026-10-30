package scheduler

// worktree_baseref.go — RESOLVING A REPO'S BASE REF WITHOUT GUESSING.
//
// THE DEFECT. Provisioning cut every new branch from a base ref resolved by a two-step guess: try
// `develop`/`origin/develop`, then `symbolic-ref refs/remotes/origin/HEAD`, then `HEAD`. On a repository
// where none of those held — a hand-built repo with a dangling `origin/HEAD` and only a `develop` remote
// branch — the resolver returned "" and provisioning asked git to start the branch at HEAD, which in an
// unreadable state is `fatal: invalid reference`. The recorded failure was `exit status 128` (see
// worktree_giterror.go), so neither the base ref nor the reason reached the operator.
//
// TWO TRAPS THIS FILE EXISTS TO AVOID, both measured on real repositories:
//
//  1. `git rev-parse --abbrev-ref origin/HEAD` ECHOES ITS INPUT BACK when the remote-HEAD symref is missing.
//     It does not fail. So a resolver built on it returns the literal string "origin/HEAD", hands that to
//     `git worktree add`, and gets `fatal: invalid reference: origin/HEAD` — a failure that looks like
//     corruption and is actually a missing symref. Orphaned bare repositories (`init --bare` + push, no
//     `remote set-head`) have no symref at all; `git clone` always creates one. Reading the symref DIRECTLY
//     with `symbolic-ref refs/remotes/origin/HEAD` is what makes the absence observable.
//
//  2. A hand-built repository can have `origin/HEAD` pointing at a branch that does not exist (`HEAD -> master`
//     with only `develop` pushed). The symref RESOLVES, so any resolver that stops there returns an invalid
//     ref. Hence the final guard below: nothing is returned unless `rev-parse --verify <ref>^{commit}` has
//     confirmed it points at a commit.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// baseRefCandidates is the CONFIGURED DEFAULT step of the chain: the branch names this platform treats as a
// repository's integration base, in order, checked on the local refs and then on origin.
//
// It is the same convention the git rules injected into every git-backed worker's prompt assume ("work on a
// branch created off `develop`"), so keeping it in the chain preserves the behaviour every existing project
// already relies on. It is named here rather than buried so the "configured default" step of the resolution
// order has one home.
var baseRefCandidates = []string{"develop", "main", "master"}

// resolveBaseRefWithProvenance resolves the ref a new run branch is cut from, and reports HOW.
//
// It returns "", nil for exactly one legitimate case: the repository is on a DETACHED HEAD and HEAD itself
// resolves to a commit. Empty then means "start at HEAD", which is what every caller already does with an
// empty base — and it is a real state (a detached checkout), not a failure to resolve.
//
// A non-nil error means NO candidate could be confirmed, and it names every attempt so the operator (and the
// run record) says what was tried rather than just `exit status 128`.
func (r *WorktreeReconciler) resolveBaseRefWithProvenance(ctx context.Context, projectDir string) (string, string, error) {
	var attempts []string
	fail := func(why string) string {
		attempts = append(attempts, why)
		return why
	}

	// STEP 1 — the remote's own default, read as a SYMREF.
	//
	// symbolic-ref, never rev-parse: rev-parse echoes `origin/HEAD` back when the symref is absent, which is
	// the trap that produced "invalid reference: origin/HEAD" on a hand-built repository.
	if out, err := runGit(ctx, projectDir, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "refs/remotes/"))
		if ref != "" {
			if ok, _ := r.refIsCommit(ctx, projectDir, ref); ok {
				return ref, "origin/HEAD", nil
			}
			// The symref EXISTS but points at a branch that does not — a hand-built repo whose origin/HEAD
			// says `master` while only `develop` was ever pushed. Recorded, then treated as absent.
			fail("origin/HEAD -> " + ref + " (symref present but that ref is not a commit)")
		}
	} else {
		fail("origin/HEAD (no remote-HEAD symref — `git remote set-head origin -a` would create one)")
	}

	// STEP 2 — ask the remote. This is the step that answers for a bare origin whose HEAD is dangling:
	// `remote show` reports the branch the REMOTE would check out, independent of any local symref.
	if ref := r.remoteHeadBranch(ctx, projectDir); ref != "" {
		if ok, _ := r.refIsCommit(ctx, projectDir, "origin/"+ref); ok {
			return "origin/" + ref, "remote-show", nil
		}
		if ok, _ := r.refIsCommit(ctx, projectDir, ref); ok {
			return ref, "remote-show", nil
		}
		fail("remote show origin -> " + ref + " (not a commit locally or on origin)")
	} else {
		fail("remote show origin (no HEAD branch reported)")
	}

	// STEP 3 — the configured default convention (see baseRefCandidates), local then origin.
	for _, name := range baseRefCandidates {
		for _, ref := range []string{name, "origin/" + name} {
			if ok, _ := r.refIsCommit(ctx, projectDir, ref); ok {
				return ref, "configured-default", nil
			}
		}
	}
	fail("configured defaults " + strings.Join(baseRefCandidates, "/") + " (none is a commit)")

	// STEP 4 — whatever is currently checked out.
	if out, err := runGit(ctx, projectDir, "symbolic-ref", "--short", "HEAD"); err == nil {
		if ref := strings.TrimSpace(out); ref != "" {
			if ok, _ := r.refIsCommit(ctx, projectDir, ref); ok {
				return ref, "HEAD", nil
			}
			fail("HEAD -> " + ref + " (not a commit)")
		}
	} else if ok, _ := r.refIsCommit(ctx, projectDir, "HEAD"); ok {
		// DETACHED HEAD — a legitimate state, not an unresolved base. "" tells the caller to start at HEAD.
		return "", "detached-HEAD", nil
	} else {
		fail("HEAD (unborn, detached without a commit, or unreadable)")
	}

	return "", "", fmt.Errorf("could not resolve a base ref to create the branch from in %s — tried: %s",
		projectDir, strings.Join(attempts, "; "))
}

// resolveBaseRef keeps the original single-value shape for callers that treat an unresolvable base as "start
// at HEAD". It is now a thin wrapper so there is ONE resolution implementation, and it logs the failure
// rather than silently collapsing it — the diagnosis that was missing before.
func (r *WorktreeReconciler) resolveBaseRef(ctx context.Context, projectDir string) string {
	ref, _, err := r.resolveBaseRefWithProvenance(ctx, projectDir)
	if err != nil {
		if r.log != nil {
			r.log.Warn("worktree: no base ref could be resolved", "project_dir", projectDir, "error", err)
		}
		return ""
	}
	return ref
}

// refIsCommit is the GUARD the resolver puts in front of every candidate: a ref is only usable as a base if
// it resolves to a commit. `--verify` is what makes a name that merely LOOKS like a ref (the echoed
// `origin/HEAD`, a symref to a deleted branch) fail here instead of inside `git worktree add`.
func (r *WorktreeReconciler) refIsCommit(ctx context.Context, projectDir, ref string) (bool, error) {
	if strings.TrimSpace(ref) == "" {
		return false, errors.New("empty ref")
	}
	if _, err := runGit(ctx, projectDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return false, err
	}
	return true, nil
}

// remoteHeadBranch parses `git remote show origin` for the branch the remote would check out.
//
// The output is human-readable and localised by git's own settings, so this reads the ONE stable shape —
// a `HEAD branch: <name>` line — and treats anything else (including git's "(unknown)") as no answer. A
// failure to reach the remote (offline, no credentials) is not an error here: it is simply the next step of
// the chain's turn, and the attempts list records it.
func (r *WorktreeReconciler) remoteHeadBranch(ctx context.Context, projectDir string) string {
	// ASKING A REMOTE THAT DOES NOT EXIST IS A WASTED ROUND TRIP. `remote show` reaches the remote when
	// origin is configured, and this resolver runs on EVERY provision — so on a local-only repository it was
	// a doomed attempt per run. A cheap local pre-check first.
	if !r.hasOriginRemote(ctx, projectDir) {
		return ""
	}
	out, err := runGit(ctx, projectDir, "remote", "show", "origin")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(key), "HEAD branch") {
			continue
		}
		name := strings.TrimSpace(value)
		if name == "" || name == "(unknown)" {
			return ""
		}
		return name
	}
	return ""
}
