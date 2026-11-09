package scheduler

// worktree_baseref.go — RESOLVING A REPO'S BASE REF WITHOUT GUESSING.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// baseRefCandidates is the CONFIGURED DEFAULT step of the chain.
var baseRefCandidates = []string{"develop", "main", "master"}

// resolveBaseRefWithProvenance resolves the ref a new run branch is cut from, and reports HOW.
func (r *WorktreeReconciler) resolveBaseRefWithProvenance(ctx context.Context, projectDir string) (string, string, error) {
	var attempts []string
	fail := func(why string) string {
		attempts = append(attempts, why)
		return why
	}

	// STEP 1 — the remote's own default, read as a SYMREF.
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

// resolveBaseRef keeps the original single-value shape for callers that treat an unresolvable base
// as "start at HEAD".
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

// refIsCommit is the GUARD the resolver puts in front of every candidate: a ref is only usable as a
// base if it resolves to a commit.
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
func (r *WorktreeReconciler) remoteHeadBranch(ctx context.Context, projectDir string) string {
	// ASKING A REMOTE THAT DOES NOT EXIST IS A WASTED ROUND TRIP.
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

// resolveBaseRefForProvisioning resolves the base a run branch is cut from, and REFUSES to hand
// back a base that would give the worker an EMPTY worktree.
func (r *WorktreeReconciler) resolveBaseRefForProvisioning(ctx context.Context, projectDir string) (string, bool, string) {
	ref, how, err := r.resolveBaseRefWithProvenance(ctx, projectDir)
	if err != nil {
		return "", false, err.Error()
	}
	// "" means detached HEAD: legitimate, and the caller starts the branch at HEAD.
	if empty, terr := r.baseTreeIsEmpty(ctx, projectDir, ref); terr != nil {
		return "", false, fmt.Sprintf("could not read the tree of base %q: %v", ref, terr)
	} else if empty {
		label := ref
		if label == "" {
			label = "HEAD (detached)"
		}
		return "", false, fmt.Sprintf("the resolved base %q (from %s) has an EMPTY tree — a worktree cut from it "+
			"would contain no files, so the run is refused rather than provisioned empty", label, how)
	}
	return ref, true, ""
}

// baseTreeIsEmpty reports whether the commit a base ref names has no entries.
func (r *WorktreeReconciler) baseTreeIsEmpty(ctx context.Context, projectDir, ref string) (bool, error) {
	target := ref
	if strings.TrimSpace(target) == "" {
		target = "HEAD"
	}
	out, err := runGit(ctx, projectDir, "ls-tree", "-z", target)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}
