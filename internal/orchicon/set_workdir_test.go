package orchicon

// Tests for the set_workdir tool: rebinding the relative-path anchor and bash cwd to a per-session
// git worktree, additively.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// worktreeProject builds a project dir containing .orchicon-worktrees/<id>/ and returns both.
func worktreeProject(t *testing.T) (project, wt string) {
	t.Helper()
	project = t.TempDir()
	wt = filepath.Join(project, ".orchicon-worktrees", "conv-1")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	return project, wt
}

func runSetWorkdir(t *testing.T, h *HostTools, args string) map[string]any {
	t.Helper()
	out, err := h.Execute(context.Background(), "set_workdir", args)
	if err != nil {
		t.Fatalf("set_workdir(%s): %v", args, err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("set_workdir returned non-JSON: %v (%s)", err, out)
	}
	return env
}

func TestSetWorkdirRebindsRelativePathsAndBashCwd(t *testing.T) {
	project, wt := worktreeProject(t)
	h := NewHostToolsUnrestricted(project)
	h.SetExtraRoots([]string{project, wt})

	// BEFORE: a new relative path lands in the project dir (the reported behaviour).
	before, err := h.resolveDir("new-file.txt")
	if err != nil {
		t.Fatalf("resolve before: %v", err)
	}
	if want := filepath.Join(project, "new-file.txt"); before != want {
		t.Fatalf("anchor before rebind = %q, want %q", before, want)
	}

	env := runSetWorkdir(t, h, `{"path":"`+wt+`"}`)
	if env["anchor"] != wt {
		t.Fatalf("reported anchor = %v, want %q", env["anchor"], wt)
	}

	// AFTER: the same relative path lands in the worktree.
	after, err := h.resolveDir("new-file.txt")
	if err != nil {
		t.Fatalf("resolve after: %v", err)
	}
	if want := filepath.Join(wt, "new-file.txt"); after != want {
		t.Fatalf("anchor after rebind = %q, want %q", after, want)
	}
}

func TestSetWorkdirBashRunsInTheWorktree(t *testing.T) {
	// The cwd is what made bare bash commands edit the wrong tree, so assert it directly.
	project, wt := worktreeProject(t)
	h := NewHostToolsUnrestricted(project)
	h.SetExtraRoots([]string{project, wt})
	runSetWorkdir(t, h, `{"path":"`+wt+`"}`)

	out, err := h.Execute(context.Background(), "bash", `{"command":"pwd"}`)
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	// macOS /tmp is a symlink; compare the resolved forms.
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(out))
	want, _ := filepath.EvalSymlinks(wt)
	if got != want {
		t.Fatalf("bash cwd = %q, want the worktree %q", got, want)
	}
}

func TestSetWorkdirCanRestoreTheProjectDir(t *testing.T) {
	project, wt := worktreeProject(t)
	h := NewHostToolsUnrestricted(project)
	h.SetExtraRoots([]string{project, wt})

	runSetWorkdir(t, h, `{"path":"`+wt+`"}`)
	runSetWorkdir(t, h, `{"use_project_dir":true}`)

	got, err := h.resolveDir("x.txt")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := filepath.Join(project, "x.txt"); got != want {
		t.Fatalf("after restore anchor = %q, want the project dir %q", got, want)
	}
}

func TestSetWorkdirRefusesAPathOutsideItsRoots(t *testing.T) {
	// THE SAFETY PROPERTY: this tool rebinds the anchor, it does NOT widen the session's scope. A
	// directory outside the project (and outside its worktrees) is refused, so it cannot be used to
	// walk a conversation into another project's tree.
	project, _ := worktreeProject(t)
	outside := t.TempDir()
	h := NewHostToolsUnrestricted(project)
	h.SetExtraRoots([]string{project})

	out, err := h.Execute(context.Background(), "set_workdir", `{"path":"`+outside+`"}`)
	if err == nil {
		t.Fatalf("set_workdir accepted a path outside its roots and reported %s", out)
	}
	if !strings.Contains(err.Error(), "outside the allowed roots") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The anchor is unchanged.
	if h.Anchor() != project {
		t.Fatalf("a refused rebind changed the anchor to %q", h.Anchor())
	}
}

func TestSetWorkdirRefusesANonDirectory(t *testing.T) {
	project := t.TempDir()
	file := filepath.Join(project, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHostToolsUnrestricted(project)
	h.SetExtraRoots([]string{project})
	if _, err := h.Execute(context.Background(), "set_workdir", `{"path":"`+file+`"}`); err == nil {
		t.Fatal("set_workdir accepted a file as the working directory")
	}
}

func TestSetWorkdirNoArgsReportsState(t *testing.T) {
	project, wt := worktreeProject(t)
	h := NewHostToolsUnrestricted(project)
	h.SetExtraRoots([]string{project, wt})

	env := runSetWorkdir(t, h, `{}`)
	if env["anchor"] != project {
		t.Fatalf("anchor = %v, want the project dir", env["anchor"])
	}
	avail, ok := env["available"].([]any)
	if !ok || len(avail) < 2 {
		t.Fatalf("available roots = %v, want the project dir and the worktree", env["available"])
	}
}

func TestSetWorkdirDoesNotNarrowWritableReach(t *testing.T) {
	// ADDITIVE, NEVER NARROWING: after a rebind the project directory must still be writable. If the
	// rebind narrowed reach, every existing project-relative write would start failing.
	project, wt := worktreeProject(t)
	h := NewHostToolsUnrestricted(project)
	h.SetExtraRoots([]string{project, wt})
	runSetWorkdir(t, h, `{"path":"`+wt+`"}`)

	abs := filepath.Join(project, "still-writable.txt")
	if _, err := h.Execute(context.Background(), "write",
		`{"filePath":"`+abs+`","content":"ok"}`); err != nil {
		t.Fatalf("write to the project dir after a rebind: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("project file was not written: %v", err)
	}
	// And the worktree is writable by relative path now.
	if _, err := h.Execute(context.Background(), "write",
		`{"filePath":"in-wt.txt","content":"ok"}`); err != nil {
		t.Fatalf("relative write into the worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "in-wt.txt")); err != nil {
		t.Fatalf("worktree file was not written: %v", err)
	}
}
