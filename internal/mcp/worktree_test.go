package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/worktree"
)

// allToolNames is the full worktree tool suite: 3 batch tools, 4 single-op
// wrappers, `list`, and `todoread` (D1/D2).
var allToolNames = []string{
	"batch_read", "batch_grep", "batch_write",
	"read", "grep", "write", "edit",
	"list", "todoread",
}

func TestWorktreeRegistryExposesFullToolSuite(t *testing.T) {
	r := NewWorktreeRegistry(t.TempDir(), "")
	defs := r.List()
	if len(defs) != len(allToolNames) {
		t.Fatalf("expected %d tools, got %d", len(allToolNames), len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, n := range allToolNames {
		if !names[n] {
			t.Fatalf("missing tool %s", n)
		}
	}
	// The batch tools must be present in the registry; the single-op
	// wrappers must be Mutating=false/true as appropriate.
	mutating := map[string]bool{"batch_write": true, "write": true, "edit": true}
	for _, d := range defs {
		if d.Mutating != mutating[d.Name] {
			t.Fatalf("tool %s: mutating=%v want %v", d.Name, d.Mutating, mutating[d.Name])
		}
	}
}

func TestWorktreeRegistryExecuteBatchRead(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewWorktreeRegistry(base, "")
	args, _ := json.Marshal(map[string]any{"paths": []string{"a.txt"}})
	res, err := r.Execute(context.Background(), nil, "batch_read", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !strings.Contains(string(res), "hello") {
		t.Fatalf("batch_read result missing content: %s", res)
	}
}

func TestWorktreeRegistrySingleOpWrappers(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewWorktreeRegistry(base, "")
	ctx := context.Background()

	// read wrapper
	args, _ := json.Marshal(map[string]any{"path": "a.txt"})
	res, err := r.Execute(ctx, nil, "read", args)
	if err != nil || !strings.Contains(string(res), "hello world") {
		t.Fatalf("read: err=%v res=%s", err, res)
	}
	// grep wrapper
	args, _ = json.Marshal(map[string]any{"pattern": "world"})
	res, err = r.Execute(ctx, nil, "grep", args)
	if err != nil || !strings.Contains(string(res), "a.txt") {
		t.Fatalf("grep: err=%v res=%s", err, res)
	}
	// write wrapper
	args, _ = json.Marshal(map[string]any{"filePath": "b.txt", "content": "new content"})
	if _, err := r.Execute(ctx, nil, "write", args); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(base, "b.txt")); string(got) != "new content" {
		t.Fatalf("write did not persist: %q", got)
	}
	// edit wrapper
	args, _ = json.Marshal(map[string]any{"filePath": "b.txt", "oldString": "new", "newString": "edited"})
	if _, err := r.Execute(ctx, nil, "edit", args); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(base, "b.txt")); string(got) != "edited content" {
		t.Fatalf("edit did not apply: %q", got)
	}
}

func TestWorktreeRegistryList(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(base, "a.txt"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(base, "sub", "b.txt"), []byte("hi"), 0o644)
	r := NewWorktreeRegistry(base, "")
	args, _ := json.Marshal(map[string]any{})
	res, err := r.Execute(context.Background(), nil, "list", args)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := string(res)
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "sub/") {
		t.Fatalf("list missing entries: %s", out)
	}
}

func TestWorktreeRegistryTodoRead(t *testing.T) {
	base := t.TempDir()
	r := NewWorktreeRegistry(base, "")
	// No snapshot yet — must return a graceful empty message, not an error.
	res, err := r.Execute(context.Background(), nil, "todoread", nil)
	if err != nil {
		t.Fatalf("todoread empty: %v", err)
	}
	if !strings.Contains(string(res), "no todo list") {
		t.Fatalf("todoread empty message: %s", res)
	}
	// Write a snapshot the way the adapter would, then read it back.
	snap := `[{"content":"first item","status":"in_progress","priority":"high"}]`
	if err := os.MkdirAll(filepath.Join(base, ".orchicon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".orchicon", "todos.json"), []byte(snap), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = r.Execute(context.Background(), nil, "todoread", nil)
	if err != nil {
		t.Fatalf("todoread: %v", err)
	}
	if !strings.Contains(string(res), "first item") || !strings.Contains(string(res), "in_progress") {
		t.Fatalf("todoread snapshot: %s", res)
	}
}

func TestWorktreeRegistryExecuteRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	r := NewWorktreeRegistry(base, "")
	args, _ := json.Marshal(map[string]any{"paths": []string{"../etc/passwd"}})
	if _, err := r.Execute(context.Background(), nil, "batch_read", args); err == nil {
		t.Fatal("expected a path-traversal error")
	}
	// Single-op wrappers must inherit the same confinement.
	args, _ = json.Marshal(map[string]any{"path": "../../etc/passwd"})
	if _, err := r.Execute(context.Background(), nil, "read", args); err == nil {
		t.Fatal("expected a path-traversal error for read")
	}
}

func TestWorktreeRegistryExecuteUnknownTool(t *testing.T) {
	r := NewWorktreeRegistry(t.TempDir(), "")
	if _, err := r.Execute(context.Background(), nil, "nope", nil); err == nil {
		t.Fatal("expected an unknown-tool error")
	}
}

// TestWorktreeRegistryExecuteProjectRootRead pins AC1 at the registry level
// with a NON-EMPTY project root (the exact wiring RuntimeServeConfig injects:
// worktree under <proj>/.orchicon-worktrees/<run>, project root = <proj>). A
// worker must be able to batch_read a run-state .orchicon/<run>/ file by both
// a `..`-relative path and an absolute project-root path — without the tools
// it would shell out (or re-derive facts). This closes the coverage gap the
// PR step noted (the worktree unit tests cover the mechanism, not the wiring).
func TestWorktreeRegistryExecuteProjectRootRead(t *testing.T) {
	proj := t.TempDir()
	run := "run-qa"
	wt := filepath.Join(proj, ".orchicon-worktrees", run)
	if err := os.MkdirAll(filepath.Join(proj, ".orchicon", run), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "FACTS LEARNED: the project root is readable.\n"
	if err := os.WriteFile(filepath.Join(proj, ".orchicon", run, "facts_learned"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewWorktreeRegistry(wt, proj)
	ctx := context.Background()

	// `..`-relative project-root path.
	args, _ := json.Marshal(map[string]any{"paths": []string{"../../.orchicon/" + run + "/facts_learned"}})
	res, err := r.Execute(ctx, nil, "batch_read", args)
	if err != nil {
		t.Fatalf("relative project-root read should be allowed: %v", err)
	}
	if !strings.Contains(string(res), content) {
		t.Fatalf("relative read missing run-file content: %s", res)
	}

	// Absolute project-root path.
	abs := filepath.Join(proj, ".orchicon", run, "facts_learned")
	argsAbs, _ := json.Marshal(map[string]any{"paths": []string{abs}})
	resAbs, err := r.Execute(ctx, nil, "batch_read", argsAbs)
	if err != nil {
		t.Fatalf("absolute project-root read should be allowed: %v", err)
	}
	if !strings.Contains(string(resAbs), content) {
		t.Fatalf("absolute read missing run-file content: %s", resAbs)
	}
}

// TestWorktreeRegistryExecuteScratchWrite pins AC2 at the registry level: a
// batch_write (and its read-back) to the sanctioned scratch dir /tmp/orchicon
// must be permitted — the tools are not confined to the worktree for scratch.
func TestWorktreeRegistryExecuteScratchWrite(t *testing.T) {
	if err := os.MkdirAll(worktree.DefaultScratchDir, 0o755); err != nil {
		t.Skipf("cannot create scratch dir %s: %v", worktree.DefaultScratchDir, err)
	}
	base := t.TempDir()
	r := NewWorktreeRegistry(base, "")
	target := filepath.Join(worktree.DefaultScratchDir, "reg-scratch-"+filepath.Base(t.TempDir())+".txt")
	defer os.Remove(target)

	args, _ := json.Marshal(map[string]any{"writes": []map[string]any{{"path": target, "mode": "create", "content": "scratch-ok"}}})
	if _, err := r.Execute(context.Background(), nil, "batch_write", args); err != nil {
		t.Fatalf("scratch write should be allowed: %v", err)
	}
	got, rerr := os.ReadFile(target)
	if rerr != nil || string(got) != "scratch-ok" {
		t.Fatalf("scratch write did not persist: %q err=%v", got, rerr)
	}
}
