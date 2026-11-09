package scheduler

// provision_base_test.go — a run must never be provisioned an EMPTY worktree.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE REPORTED SHAPE: HEAD is an empty root commit while a real branch exists.
func TestProvisioningRefusesAnEmptyBaseTree(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "x@orchicon.dev")
	gitRun(t, repo, "config", "user.name", "X")
	// An empty root commit as HEAD — `git commit --allow-empty`, which is exactly how a repo reaches this state.
	gitRun(t, repo, "commit", "--allow-empty", "-m", "empty root")
	// ...and a real branch that HAS content, so the repo is not simply empty.
	gitRun(t, repo, "checkout", "-b", "real")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "real work")
	gitRun(t, repo, "checkout", "develop")

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	ref, ok, reason := r.resolveBaseRefForProvisioning(context.Background(), repo)
	if ok {
		t.Fatalf("provisioning accepted base %q, whose tree is EMPTY — the run would get a worktree with no "+
			"files and the worker would have to reset it by hand", ref)
	}
	if !strings.Contains(strings.ToLower(reason), "empty") {
		t.Fatalf("refusal does not say the tree is empty: %q", reason)
	}
}

// A base WITH content is accepted and named.
func TestProvisioningAcceptsABaseWithContent(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "x@orchicon.dev")
	gitRun(t, repo, "config", "user.name", "X")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "base")

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	ref, ok, reason := r.resolveBaseRefForProvisioning(context.Background(), repo)
	if !ok {
		t.Fatalf("a repo with content was refused: %s", reason)
	}
	if ref != "develop" {
		t.Fatalf("base = %q, want develop", ref)
	}
}

// A repository with NO commits at all — an unborn HEAD — is refused rather than provisioned empty.
func TestProvisioningRefusesAnUnbornRepository(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "develop")

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	if _, ok, reason := r.resolveBaseRefForProvisioning(context.Background(), repo); ok {
		t.Fatalf("an unborn repository was accepted as a base (%q)", reason)
	}
}

// baseTreeIsEmpty answers for both an explicit ref and the ""-means-HEAD convention addWorktree uses.
func TestBaseTreeIsEmptyHandlesTheDetachedConvention(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main")
	gitRun(t, repo, "config", "user.email", "x@orchicon.dev")
	gitRun(t, repo, "config", "user.name", "X")
	gitRun(t, repo, "commit", "--allow-empty", "-m", "empty")
	r := &WorktreeReconciler{log: quietSchedulerLogger()}

	if empty, err := r.baseTreeIsEmpty(context.Background(), repo, ""); err != nil || !empty {
		t.Fatalf("baseTreeIsEmpty(\"\") = %v, %v; want empty via the HEAD convention", empty, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "content")
	if empty, err := r.baseTreeIsEmpty(context.Background(), repo, "main"); err != nil || empty {
		t.Fatalf("baseTreeIsEmpty(main) = %v, %v; want not-empty", empty, err)
	}
}
