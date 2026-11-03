package scheduler

// worktree_pointer_test.go — A WORKTREE OF OURS MUST BE RECOGNISED WHEREVER ITS POINTER WAS WRITTEN.
//
// REPORTED, FOUR RUNS RUNNING:
//
//	path C:\projects\weatherapp\.orchicon-worktrees\01M6HKWFDAE7E5VFDQ911ZEANQ is occupied by a non-worktree
//	directory
//
// ...on a directory that was a COMPLETE worktree — `.git`, `.gitattributes`, `index.html`, `package.json`,
// `src/`, `styles.css`. Not debris; a checkout.
//
// Three links, two of them ours:
//
//  1. the container's worker created the worktree itself (the arbitration gap — still open);
//  2. #906 rewrote its `.git` to the GUEST form so the container's git could follow it — and that file is
//     BIND-MOUNTED, so the host reads `/mnt/c/...` too, a path it cannot resolve;
//  3. this branch's ownership test compared with `filepath.Separator`, so on Windows `.git\worktrees` never
//     matched the file's `.git/worktrees`, the directory was judged foreign, and provisioning refused.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeWorktree creates a fake worktree directory with a `.git` pointer and optional content.
func writeWorktree(t *testing.T, dir, pointer string, files ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+pointer+"\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
}

// THE REPORTED POINTER, verbatim: a CONTAINER-written gitdir, forward slashes, /mnt/c.
func TestContainerWrittenPointerIsRecognised(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "weatherapp")
	runID := "01M6HKWFDAE7E5VFDQ911ZEANQ"
	dir := filepath.Join(projectDir, ".orchicon-worktrees", runID)

	writeWorktree(t, dir, "/mnt/c/projects/weatherapp/.git/worktrees/"+runID,
		"index.html", "package.json", "src/app.js")

	r := &WorktreeReconciler{log: quietSchedulerLogger()}

	if got := r.worktreePointerTarget(dir); got == "" {
		t.Fatal("a forward-slash gitdir pointer was NOT recognised as a worktree pointer. On Windows the old " +
			"comparison used filepath.Separator and never matched — that is the reported misclassification")
	}
	if !r.isOrchiconWorktreeArtifact(dir) {
		t.Fatal("a worktree whose .git names .git/worktrees is not recognised as ours")
	}
	if !r.isForeignWorktreeOfOurs(dir, projectDir) {
		t.Fatal("a CONTAINER-created worktree of this run was judged foreign — provisioning then refuses with " +
			"'occupied by a non-worktree directory', which is the reported failure")
	}
}

// A BACKSLASH pointer (host-written) must still work — the fix must not trade one form for the other.
func TestHostWrittenPointerStillRecognised(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "weatherapp")
	runID := "01RUN"
	dir := filepath.Join(projectDir, ".orchicon-worktrees", runID)

	writeWorktree(t, dir, projectDir+string(filepath.Separator)+".git"+string(filepath.Separator)+"worktrees"+string(filepath.Separator)+runID, "index.html")
	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	if !r.isForeignWorktreeOfOurs(dir, projectDir) {
		t.Fatal("a host-written pointer is no longer recognised")
	}
}

// GENUINELY FOREIGN CONTENT IS STILL REFUSED — this is the one unrecoverable mistake available.
func TestForeignDirectoryIsNotReclaimed(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "weatherapp")
	r := &WorktreeReconciler{log: quietSchedulerLogger()}

	// (a) real content, no .git at all.
	foreign := filepath.Join(projectDir, ".orchicon-worktrees", "01RUN")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "notes.txt"), []byte("operator data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if r.isForeignWorktreeOfOurs(foreign, projectDir) || r.isReclaimableRunDir(foreign) {
		t.Fatal("a directory with real content and no worktree pointer was reclaimable — provisioning would " +
			"DELETE operator data")
	}

	// (b) a worktree pointer into SOMEBODY ELSE'S repository.
	other := filepath.Join(projectDir, ".orchicon-worktrees", "01OTHER")
	writeWorktree(t, other, "/home/someone/elsewhere/.git/worktrees/01OTHER", "file.txt")
	if r.isForeignWorktreeOfOurs(other, projectDir) {
		t.Fatal("a worktree of ANOTHER repository was claimed as ours")
	}

	// (c) a real .git DIRECTORY is a repository, never a worktree of ours.
	repo := filepath.Join(projectDir, ".orchicon-worktrees", "01REPO")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if r.isForeignWorktreeOfOurs(repo, projectDir) {
		t.Fatal("a directory holding a real .git DIRECTORY was treated as a worktree")
	}
}

// The reclaim must be separator-agnostic BOTH ways, so it cannot silently pass on one platform only.
func TestPointerTargetFoldsSeparators(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "wt")
	writeWorktree(t, dir, `C:\projects\app\.git\worktrees\x`)
	target := (&WorktreeReconciler{}).worktreePointerTarget(dir)
	if target == "" {
		t.Fatal("backslash pointer not recognised")
	}
	if !strings.Contains(strings.ReplaceAll(target, "\\", "/"), ".git/worktrees") {
		t.Fatalf("pointer target %q does not fold to a worktrees path", target)
	}
}
