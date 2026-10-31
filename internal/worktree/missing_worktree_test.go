package worktree

// missing_worktree_test.go — THE FILE TOOLS AND THE SHELL MUST AGREE ABOUT WHERE "HERE" IS.
//
// Reported in container mode: every `bash` call died
//
//	runtime exec: chdir <worktree>: no such file or directory
//
// while batch_read / batch_write / glob resolved against the READ-ONLY extra root — the real project checkout —
// and worked. A worker could therefore read (and, through the run-state paths, believe it was inspecting) the
// shared tree it was supposed to be isolated from, with only a bash error as the signal that its own tree was
// gone. A less careful worker would have written feature code into the shared checkout.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAMissingWorktreeIsRefusedForReadsAndWrites(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, ".orchicon-worktrees", "01M69SPRKBJ5NM10W7M3V86SMH")

	base := Base{Worktree: missing, ProjectRoot: project}

	// A READ that would previously have been served from the project root.
	if _, err := ResolvePath(base, "app.go", false); err == nil {
		t.Fatal("a relative read resolved while the run's worktree was missing — it was served from the " +
			"project root, so the worker reads the shared checkout it is meant to be isolated from")
	} else if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the refusal must say the directory does not exist; got: %v", err)
	}
	// And the ABSOLUTE form of the same project-root path.
	if _, err := ResolvePath(base, filepath.Join(project, "app.go"), false); err == nil {
		t.Fatal("an absolute project-root read resolved while the worktree was missing")
	}
	// Writes, which the boundary already refused but must refuse for the SAME reason now.
	if _, err := ResolvePath(base, "out.txt", true); err == nil {
		t.Fatal("a write resolved while the worktree was missing")
	}
}

func TestAPresentWorktreeStillResolves(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "run")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	base := Base{Worktree: wt, ProjectRoot: filepath.Join(root, "project")}
	got, err := ResolvePath(base, "app.go", false)
	if err != nil {
		t.Fatalf("a present worktree was refused: %v", err)
	}
	if want := filepath.Join(wt, "app.go"); got != want {
		t.Errorf("resolved %q, want %q", got, want)
	}
}
