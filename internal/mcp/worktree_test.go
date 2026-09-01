package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allToolNames is the full worktree tool suite: 3 batch tools, 4 single-op
// wrappers, `list`, and `todoread` (D1/D2).
var allToolNames = []string{
	"batch_read", "batch_grep", "batch_write",
	"read", "grep", "write", "edit",
	"list", "todoread",
}

func TestWorktreeRegistryExposesFullToolSuite(t *testing.T) {
	r := NewWorktreeRegistry(t.TempDir())
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
	r := NewWorktreeRegistry(base)
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
	r := NewWorktreeRegistry(base)
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
	r := NewWorktreeRegistry(base)
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
	r := NewWorktreeRegistry(base)
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
	r := NewWorktreeRegistry(base)
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
	r := NewWorktreeRegistry(t.TempDir())
	if _, err := r.Execute(context.Background(), nil, "nope", nil); err == nil {
		t.Fatal("expected an unknown-tool error")
	}
}
