package worktree

// Tests for the interactive path's EXTRA default roots (a per-session git worktree alongside the
// project dir). The property under test is narrow and load-bearing: a relative path must still resolve
// to the PROJECT file when it exists there, and must resolve into a worktree only when it does not.

import (
	"os"
	"path/filepath"
	"testing"
)

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func interactiveBase(projectDir, worktreeDir string) Base {
	return Base{
		Worktree:     projectDir,
		ScratchDir:   DefaultScratchDir,
		AllowAnyPath: true,
		ExtraRoots:   []string{projectDir, worktreeDir},
	}
}

func TestExtraRootsProjectWinsOnAMbiguousRelativePath(t *testing.T) {
	// THE RULE THAT PREVENTS THE VERY BUG THIS FEATURE FIXES, in reverse: an extra root must never
	// shadow a path that exists under the project. `read src/app.tsx` means the project's file, full
	// stop — otherwise adding a worktree would change the meaning of every relative path in the session.
	project := t.TempDir()
	wt := t.TempDir()
	mkfile(t, filepath.Join(project, "src", "app.tsx"), "PROJECT")
	mkfile(t, filepath.Join(wt, "src", "app.tsx"), "WORKTREE")

	got, err := ResolvePath(interactiveBase(project, wt), "src/app.tsx", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := filepath.Join(project, "src", "app.tsx"); got != want {
		t.Fatalf("ambiguous relative path resolved to %q, want the project file %q", got, want)
	}
}

func TestExtraRootsResolvesAPathThatOnlyExistsInTheWorktree(t *testing.T) {
	project := t.TempDir()
	wt := t.TempDir()
	mkfile(t, filepath.Join(wt, "only-here.txt"), "WORKTREE")

	got, err := ResolvePath(interactiveBase(project, wt), "only-here.txt", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := filepath.Join(wt, "only-here.txt"); got != want {
		t.Fatalf("got %q, want the worktree file %q", got, want)
	}
}

func TestExtraRootsFallsBackToProjectWhenNeitherHasIt(t *testing.T) {
	// A path that exists NOWHERE must resolve under the project (the anchor), not error and not
	// silently pick a worktree — that keeps write-a-new-file behaviour identical to before.
	project := t.TempDir()
	wt := t.TempDir()
	got, err := ResolvePath(interactiveBase(project, wt), "new/dir/file.txt", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := filepath.Join(project, "new", "dir", "file.txt"); got != want {
		t.Fatalf("got %q, want the project path %q", got, want)
	}
}

func TestExtraRootsAreInertWithoutAllowAnyPath(t *testing.T) {
	// THE "BREAK NOTHING" GUARANTEE. The confined worker suite (NewHostTools) sets no AllowAnyPath;
	// ExtraRoots must be IGNORED there, so its resolution behaviour is byte-for-byte unchanged even if
	// a caller populates the field.
	project := t.TempDir()
	scratch := t.TempDir()
	wt := t.TempDir()
	mkfile(t, filepath.Join(wt, "only-here.txt"), "WORKTREE")

	confined := Base{
		Worktree:     project,
		ScratchDir:   scratch,
		AllowAnyPath: false,
		ExtraRoots:   []string{wt},
	}
	// A confined base resolves the relative path INSIDE its own worktree — it must never be redirected
	// into the extra root. (Whether that path exists is the caller's observation, not the resolver's:
	// a confined base resolves within its roots and lets the read report a missing file.)
	got, err := ResolvePath(confined, "only-here.txt", false)
	if err != nil {
		t.Fatalf("confined resolve: %v", err)
	}
	if want := filepath.Join(project, "only-here.txt"); got != want {
		t.Fatalf("a confined base resolved %q, want %q — ExtraRoots must be consulted ONLY on the "+
			"interactive (AllowAnyPath) path", got, want)
	}
	// And an absolute path into the extra root is still refused by the confined base.
	if _, err := ResolvePath(confined, filepath.Join(wt, "only-here.txt"), false); err == nil {
		t.Fatal("a confined base accepted an absolute path into an ExtraRoot")
	}
}

func TestExtraRootsCannotBeEscapedThroughASymlink(t *testing.T) {
	// A link placed inside a root must not let a relative path read somewhere else entirely.
	project := t.TempDir()
	wt := t.TempDir()
	outside := t.TempDir()
	mkfile(t, filepath.Join(outside, "secret.txt"), "SECRET")
	if err := os.Symlink(outside, filepath.Join(wt, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The escape attempt must not be HONOURED: it resolves, but to the project anchor (the fallback),
	// never through the link into the outside directory.
	got, err := ResolvePath(interactiveBase(project, wt), "escape/secret.txt", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if leaked := filepath.Join(outside, "secret.txt"); got == leaked {
		t.Fatalf("a symlink escaped its ExtraRoot: resolved to the outside file %q", got)
	}
	if want := filepath.Join(project, "escape", "secret.txt"); got != want {
		t.Fatalf("got %q, want the project fallback %q", got, want)
	}
}

func TestAbsolutePathsAreUnaffectedByExtraRoots(t *testing.T) {
	project := t.TempDir()
	wt := t.TempDir()
	mkfile(t, filepath.Join(wt, "x.txt"), "X")
	abs := filepath.Join(wt, "x.txt")

	got, err := ResolvePath(interactiveBase(project, wt), abs, false)
	if err != nil {
		t.Fatalf("resolve absolute: %v", err)
	}
	if got != abs {
		t.Fatalf("absolute path resolved to %q, want %q", got, abs)
	}
}
