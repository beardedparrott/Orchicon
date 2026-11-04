package runtime

// gitenv_test.go — GIT IS TOLD HOW TO READ A WORKTREE; THE WORKTREE IS NEVER ALTERED.
//
// THE DEFECT THIS REPLACES. #906 rewrote each worktree's `.git` pointer to the GUEST path so a git inside the
// container could follow it. That file is BIND-MOUNTED, so the HOST read the rewrite too — and a host that
// cannot resolve `/mnt/c/...` then failed to validate its own worktree, refused to provision
// ("occupied by a non-worktree directory"), and could not satisfy git's registration
// ("'<branch>' is already used by worktree at '/mnt/...'"). The container was given a working worktree by
// breaking the host's.
//
// The worktree is created ONCE, by the host. The container uses the same one. Nothing is rewritten, and no
// second worktree is created: each container CHILD is handed GIT_DIR/GIT_WORK_TREE, which git prefers to the
// pointer file.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE REPORTED SHAPE: a HOST-form pointer, a container that cannot resolve it.
func TestGitEnvTransposesAHostPointer(t *testing.T) {
	root := t.TempDir() // stands in for /mnt/c/projects/weatherapp
	runID := "01M6HRDZX3FB7NGGJNBR8J4CHS"
	worktree := filepath.Join(root, ".orchicon-worktrees", runID)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// What the HOST's git wrote — the form the container cannot use, and must not change.
	hostPointer := `C:\projects\weatherapp\.git\worktrees\` + runID
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+hostPointer+"\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	t.Setenv(envGitdirMaps, `C:\projects\weatherapp|`+root)

	got := gitEnvFor(worktree)
	if len(got) != 2 {
		t.Fatalf("gitEnvFor returned %v, want GIT_DIR and GIT_WORK_TREE", got)
	}
	joined := strings.Join(got, " ")
	wantGitDir := "GIT_DIR=" + root + "/.git/worktrees/" + runID
	if !strings.Contains(joined, wantGitDir) {
		t.Fatalf("gitEnvFor = %v\n  want it to contain %q — the host form must be TRANSPOSED for the container",
			got, wantGitDir)
	}
	if !strings.Contains(joined, "GIT_WORK_TREE="+worktree) {
		t.Fatalf("gitEnvFor = %v, want GIT_WORK_TREE=%s", got, worktree)
	}
	// AND THE FILE IS UNTOUCHED — this is the point of the whole change.
	raw, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "gitdir: "+hostPointer {
		t.Fatalf("the worktree's .git was MODIFIED: %q\n  the host and the container open the same bytes, so "+
			"rewriting it for one breaks the other — that was the defect", string(raw))
	}
}

// A pointer that already resolves in this namespace needs no help and must be used verbatim.
func TestGitEnvLeavesAResolvingPointerAlone(t *testing.T) {
	root := t.TempDir()
	runID := "01M6RUN"
	worktree := filepath.Join(root, ".orchicon-worktrees", runID)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitDir := filepath.Join(root, ".git", "worktrees", runID)
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir gitdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := gitEnvFor(worktree)
	if len(got) != 2 || !strings.Contains(strings.Join(got, " "), "GIT_DIR="+gitDir) {
		t.Fatalf("gitEnvFor = %v, want GIT_DIR=%s verbatim", got, gitDir)
	}
}

// NOT A WORKTREE: no pointer file, or a real .git DIRECTORY, contributes nothing. Setting GIT_DIR there would
// break an ordinary repository checkout.
func TestGitEnvIgnoresNonWorktrees(t *testing.T) {
	root := t.TempDir()

	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := gitEnvFor(plain); got != nil {
		t.Errorf("a directory with no .git produced %v", got)
	}

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := gitEnvFor(repo); got != nil {
		t.Errorf("a real .git DIRECTORY produced %v — that would point GIT_DIR at a repository's own .git", got)
	}

	if got := gitEnvFor(""); got != nil {
		t.Errorf("an empty cwd produced %v", got)
	}
}

// A pointer this container cannot transpose is LEFT TO GIT to report — a wrong GIT_DIR is worse than an honest
// error from git itself.
func TestGitEnvRefusesWhenItCannotTranspose(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: D:/elsewhere/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(envGitdirMaps, `C:\projects\weatherapp|`+root)
	if got := gitEnvFor(worktree); got != nil {
		t.Fatalf("gitEnvFor produced %v for a pointer it cannot transpose — a wrong GIT_DIR silently points git "+
			"at the wrong repository", got)
	}
}

// The mapping format: "<host>|<guest>" joined by ';' — ':' is inside a Windows path and '|' cannot be in a
// POSIX one. Malformed entries are skipped, never fatal.
func TestPathMappingFormat(t *testing.T) {
	got := parsePathMappings(`C:\projects\weatherapp|/mnt/c/projects/weatherapp;D:\work|/mnt/d/work`)
	if len(got) != 2 {
		t.Fatalf("parsed %d, want 2: %+v", len(got), got)
	}
	for _, raw := range []string{"", "nopipe", "/a|/a", "|x", "x|"} {
		if m := parsePathMappings(raw); len(m) != 0 {
			t.Errorf("parsePathMappings(%q) = %+v, want none", raw, m)
		}
	}
}
