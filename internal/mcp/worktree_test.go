package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeRegistryExposesThreeTools(t *testing.T) {
	r := NewWorktreeRegistry(t.TempDir())
	defs := r.List()
	if len(defs) != 3 {
		t.Fatalf("expected 3 batch tools, got %d", len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, n := range []string{"batch_read", "batch_grep", "batch_write"} {
		if !names[n] {
			t.Fatalf("missing tool %s", n)
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

func TestWorktreeRegistryExecuteRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	r := NewWorktreeRegistry(base)
	args, _ := json.Marshal(map[string]any{"paths": []string{"../etc/passwd"}})
	if _, err := r.Execute(context.Background(), nil, "batch_read", args); err == nil {
		t.Fatal("expected a path-traversal error")
	}
}

func TestWorktreeRegistryExecuteUnknownTool(t *testing.T) {
	r := NewWorktreeRegistry(t.TempDir())
	if _, err := r.Execute(context.Background(), nil, "nope", nil); err == nil {
		t.Fatal("expected an unknown-tool error")
	}
}
