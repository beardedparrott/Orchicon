package scheduler

// ref_attribution_test.go — EVERY REF MOVE MUST SAY WHO MOVED IT.
//
// MEASURED, from the operator's own repository:
//
//	$ git reflog show develop
//	066279b (develop) develop@{0}:
//	cb3af04           develop@{1}:
//	69845f3           develop@{2}:
//	f421a7c           develop@{3}:
//	558d52e (HEAD -> master) develop@{4}:
//
// Five moves, EVERY MESSAGE EMPTY. That is the exact output of `git update-ref` without `-m`, which is what
// setBranchRef did. The consequence was not cosmetic: nothing in the repository could say whether a human, a
// worker, or the platform moved the branch, or which run did it — so the investigation had to be reconstructed
// from mount tables and path mappers, and a "the branch was force-set back" theory survived several rounds
// because the log could neither confirm nor refute it.
//
// `-m` costs one argument.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE REPORTED OUTPUT, prevented: a move made by this platform names itself in the reflog.
func TestSetBranchRefWritesAnAttributedReflogEntry(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "x@orchicon.dev")
	gitRun(t, repo, "config", "user.name", "X")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "first")
	// A descendant commit to move to.
	gitRun(t, repo, "checkout", "-b", "run-1")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "run work")
	tip := strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))
	gitRun(t, repo, "checkout", "develop")

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	const why = "orchicon: local integration fast-forward of develop to run 01MTEST"
	if err := r.setBranchRef(context.Background(), repo, "develop", tip, why); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}

	entry := strings.TrimSpace(gitRun(t, repo, "reflog", "show", "--format=%gs", "-n", "1", "develop"))
	if entry == "" {
		t.Fatalf("the reflog entry for the move is EMPTY — this is the reported output: an operator cannot tell "+
			"who moved %q, and an investigation into it has nothing to read", "develop")
	}
	if entry != why {
		t.Fatalf("reflog message = %q, want %q", entry, why)
	}
	// And it must name the platform, which is the part that survives being read months later.
	if !strings.HasPrefix(entry, "orchicon:") {
		t.Errorf("reflog message does not identify the platform: %q", entry)
	}
}

// The two integration paths must pass DISTINCT, self-describing reasons — a generic "moved" tells a reader
// nothing about which operation ran.
func TestIntegrationRefMovesCarryDistinctReasons(t *testing.T) {
	src, err := os.ReadFile("local_integrate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		"local integration fast-forward of",
		"local integration merge of run",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("no ref-move reason containing %q — the reflog would not distinguish a fast-forward from a "+
				"merge", want)
		}
	}
	// A bare update-ref in this file would be the silent mover returning.
	if strings.Contains(s, `"update-ref", "refs/heads/"`) {
		t.Error("an update-ref without -m is back — that is what produced the blank reflog entries")
	}
}
