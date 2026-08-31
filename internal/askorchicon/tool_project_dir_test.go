package askorchicon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// These tests exercise the project-directory tools (list_project_dir /
// read_project_file) against a real Postgres with real directories on
// disk. They are skipped unless ORCHICON_TEST_DSN is set (same pattern as
// tool_workitems_test.go):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run TestProjectDirTools -v
//
// The path-traversal safety itself is unit-tested without a DB in
// internal/contextfiles (TestResolveWithin*); these tests prove the tools
// wire that resolver in end-to-end for projects found via the MCP surface.

func createProjectDirForTest(t *testing.T, ctx context.Context, pool *db.Pool, name, projectDir string) string {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant,
		Name: name, Slug: "dir-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"), ProjectDir: projectDir,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit project: %v", err)
	}
	return proj.ID
}

func TestProjectDirListAndReadDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)

	root := t.TempDir()
	mustWriteTestFile(t, filepath.Join(root, "README.md"), "# My Project\n")
	mustWriteTestFile(t, filepath.Join(root, "src", "main.go"), "package main\n")
	mustWriteTestFile(t, filepath.Join(root, "src", "deep", "util.txt"), "nested\n")
	mustWriteTestFile(t, filepath.Join(root, ".git", "config"), "should be skipped\n")
	projectID := createProjectDirForTest(t, ctx, pool, "Dir List Read", root)

	// List the root: README.md + src present, .git noise skipped.
	res, err := callProjectDirTool(t, ctx, pool, "list_project_dir", map[string]any{"project_id": projectID})
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	var listing listProjectDirResult
	if err := json.Unmarshal(res, &listing); err != nil {
		t.Fatalf("unmarshal listing: %v", err)
	}
	if listing.Path != root {
		t.Errorf("listing path = %q, want %q", listing.Path, root)
	}
	if listing.Truncated {
		t.Error("listing unexpectedly truncated")
	}
	wantNames := map[string]string{"README.md": "file", "src": "dir"}
	if len(listing.Entries) != len(wantNames) {
		t.Fatalf("root listing entries = %v, want %v", listing.Entries, wantNames)
	}
	for _, e := range listing.Entries {
		if wantType, ok := wantNames[e.Name]; !ok {
			t.Errorf("unexpected entry %q (type %q)", e.Name, e.Type)
		} else if e.Type != wantType {
			t.Errorf("entry %q type = %q, want %q", e.Name, e.Type, wantType)
		}
	}
	// File sizes are populated for regular files.
	for _, e := range listing.Entries {
		if e.Name == "README.md" && e.Size != int64(len("# My Project\n")) {
			t.Errorf("README.md size = %d, want %d", e.Size, len("# My Project\n"))
		}
		if e.Name == "src" && e.Size != 0 {
			t.Errorf("src dir size = %d, want 0", e.Size)
		}
	}

	// List a subdirectory via a relative subpath.
	res, err = callProjectDirTool(t, ctx, pool, "list_project_dir", map[string]any{"project_id": projectID, "path": "src"})
	if err != nil {
		t.Fatalf("list src: %v", err)
	}
	if err := json.Unmarshal(res, &listing); err != nil {
		t.Fatalf("unmarshal src listing: %v", err)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("src entries = %v, want main.go + deep", listing.Entries)
	}

	// List via an absolute path inside the root.
	res, err = callProjectDirTool(t, ctx, pool, "list_project_dir", map[string]any{"project_id": projectID, "path": filepath.Join(root, "src", "deep")})
	if err != nil {
		t.Fatalf("list absolute subdir: %v", err)
	}
	if err := json.Unmarshal(res, &listing); err != nil {
		t.Fatalf("unmarshal deep listing: %v", err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "util.txt" {
		t.Fatalf("deep entries = %v, want util.txt", listing.Entries)
	}

	// Read a file (relative path).
	res, err = callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{"project_id": projectID, "path": "src/main.go"})
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var read readProjectFileResult
	if err := json.Unmarshal(res, &read); err != nil {
		t.Fatalf("unmarshal read result: %v", err)
	}
	if read.Content != "package main\n" || read.Truncated || read.Bytes != len("package main\n") {
		t.Errorf("read result = %+v, want full package main content", read)
	}
	if read.Path != filepath.Join(root, "src", "main.go") {
		t.Errorf("read path = %q, want %q", read.Path, filepath.Join(root, "src", "main.go"))
	}

	// Read the same file via an absolute path inside the root.
	res, err = callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{"project_id": projectID, "path": filepath.Join(root, "README.md")})
	if err != nil {
		t.Fatalf("read README absolute: %v", err)
	}
	if err := json.Unmarshal(res, &read); err != nil {
		t.Fatalf("unmarshal README result: %v", err)
	}
	if read.Content != "# My Project\n" {
		t.Errorf("README content = %q", read.Content)
	}
}

func TestProjectDirReadBoundsAndErrorsDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)

	root := t.TempDir()
	content := strings.Repeat("abcdef", 1000) // 6000 bytes
	mustWriteTestFile(t, filepath.Join(root, "big.txt"), content)
	projectID := createProjectDirForTest(t, ctx, pool, "Dir Read Errors", root)

	// max_bytes bounds the read and flags truncation.
	res, err := callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{
		"project_id": projectID, "path": "big.txt", "max_bytes": 10,
	})
	if err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	var read readProjectFileResult
	if err := json.Unmarshal(res, &read); err != nil {
		t.Fatalf("unmarshal bounded read: %v", err)
	}
	if !read.Truncated || read.Content != content[:10] || read.Bytes != len(content) {
		t.Errorf("bounded read = %+v, want truncated first 10 of %d bytes", read, len(content))
	}

	// Negative max_bytes clamps to 1.
	res, err = callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{
		"project_id": projectID, "path": "big.txt", "max_bytes": -5,
	})
	if err != nil {
		t.Fatalf("clamped read: %v", err)
	}
	if err := json.Unmarshal(res, &read); err != nil {
		t.Fatalf("unmarshal clamped read: %v", err)
	}
	if read.Content != content[:1] {
		t.Errorf("clamped read content = %q, want first byte", read.Content)
	}

	// Reading a missing file reports a not-found error.
	if _, err := callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{"project_id": projectID, "path": "nope.txt"}); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("read missing file: err = %v, want 'no such file'", err)
	}
	// Reading a directory errors with a list hint.
	if _, err := callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{"project_id": projectID, "path": "big.txt"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The directory hint needs an actual directory on disk.
	if err := os.MkdirAll(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if _, err := callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{"project_id": projectID, "path": "subdir"}); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("read directory: err = %v, want 'is a directory' hint", err)
	}
	// Listing a file errors with a read hint.
	if _, err := callProjectDirTool(t, ctx, pool, "list_project_dir", map[string]any{"project_id": projectID, "path": "big.txt"}); err == nil || !strings.Contains(err.Error(), "is a file") {
		t.Errorf("list a file: err = %v, want 'is a file' hint", err)
	}
}

func TestProjectDirTraversalRejectedEndToEndDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	mustWriteTestFile(t, secret, "top secret\n")

	root := t.TempDir()
	mustWriteTestFile(t, filepath.Join(root, "ok.txt"), "ok\n")
	if err := os.Symlink(secret, filepath.Join(root, "leak")); err == nil {
		defer os.Remove(filepath.Join(root, "leak"))
	}
	projectID := createProjectDirForTest(t, ctx, pool, "Dir Traversal", root)

	// Every traversal attempt must be rejected with a descriptive error.
	for _, tc := range []struct {
		name   string
		tool   string
		path   string
		errSub string
	}{
		{"list .. escape", "list_project_dir", "../etc/passwd", "escapes"},
		{"list deep ..", "list_project_dir", "sub/../../etc/passwd", "escapes"},
		{"list absolute outside", "list_project_dir", "/etc/passwd", "escapes"},
		{"list sibling project", "list_project_dir", filepath.Join(filepath.Dir(root), "other"), "escapes"},
		{"read .. escape", "read_project_file", "../etc/passwd", "escapes"},
		{"read absolute outside", "read_project_file", "/etc/passwd", "escapes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := callProjectDirTool(t, ctx, pool, tc.tool, map[string]any{"project_id": projectID, "path": tc.path}); err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("%s %q error = %v, want containing %q", tc.tool, tc.path, err, tc.errSub)
			}
		})
	}

	// Symlink escape (file and directory) rejected.
	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err == nil {
		defer os.Remove(filepath.Join(root, "escape-dir"))
	}
	for _, p := range []string{"leak", "escape-dir", filepath.Join("escape-dir", "secret.txt")} {
		if _, err := callProjectDirTool(t, ctx, pool, "list_project_dir", map[string]any{"project_id": projectID, "path": p}); err == nil {
			t.Errorf("list path %q should reject the symlink escape", p)
		}
		if _, err := callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{"project_id": projectID, "path": p}); err == nil {
			t.Errorf("read path %q should reject the symlink escape", p)
		}
	}

	// A legit in-root symlink still lists (as a symlink, target not followed).
	if err := os.Symlink(filepath.Join(root, "ok.txt"), filepath.Join(root, "inlink")); err == nil {
		defer os.Remove(filepath.Join(root, "inlink"))
		res, err := callProjectDirTool(t, ctx, pool, "list_project_dir", map[string]any{"project_id": projectID})
		if err != nil {
			t.Fatalf("list root with inlink: %v", err)
		}
		var listing listProjectDirResult
		if err := json.Unmarshal(res, &listing); err != nil {
			t.Fatalf("unmarshal listing: %v", err)
		}
		found := false
		for _, e := range listing.Entries {
			if e.Name == "inlink" {
				found = true
				if e.Type != "symlink" {
					t.Errorf("inlink type = %q, want symlink", e.Type)
				}
			}
		}
		if !found {
			t.Error("inlink missing from listing")
		}
		// Reading through the in-root symlink succeeds (ResolveWithin proved
		// it stays inside the root).
		res, err = callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{"project_id": projectID, "path": "inlink"})
		if err != nil {
			t.Fatalf("read in-root symlink: %v", err)
		}
		var read readProjectFileResult
		if err := json.Unmarshal(res, &read); err != nil {
			t.Fatalf("unmarshal inlink read: %v", err)
		}
		if read.Content != "ok\n" {
			t.Errorf("inlink content = %q, want ok", read.Content)
		}
	}
}

func TestProjectDirMissingProjectAndNoDirDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)

	// Project without a project_dir.
	noDirID := createProjectDirForTest(t, ctx, pool, "No Dir", "")
	for _, tool := range []string{"list_project_dir", "read_project_file"} {
		args := map[string]any{"project_id": noDirID}
		if tool == "read_project_file" {
			args["path"] = "x.txt"
		}
		if _, err := callProjectDirTool(t, ctx, pool, tool, args); err == nil || !strings.Contains(err.Error(), "no project_dir") {
			t.Errorf("%s on project without project_dir: err = %v, want 'no project_dir'", tool, err)
		}
	}

	// Unknown project.
	for _, tool := range []string{"list_project_dir", "read_project_file"} {
		args := map[string]any{"project_id": "does-not-exist"}
		if tool == "read_project_file" {
			args["path"] = "x.txt"
		}
		if _, err := callProjectDirTool(t, ctx, pool, tool, args); err == nil {
			t.Errorf("%s on unknown project should fail", tool)
		}
	}

	// Missing required args.
	if _, err := callProjectDirTool(t, ctx, pool, "list_project_dir", map[string]any{}); err == nil || !strings.Contains(err.Error(), "project_id is required") {
		t.Errorf("list without project_id: err = %v, want 'project_id is required'", err)
	}
	if _, err := callProjectDirTool(t, ctx, pool, "read_project_file", map[string]any{"project_id": noDirID}); err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Errorf("read without path: err = %v, want 'path is required'", err)
	}
}

// TestProjectDirToolsRegistered verifies the two project-directory tools are
// registered read-only with MCP input schemas that match what the Fn actually
// parses (AGENTS.md "Ask Orchicon — keep it in sync"): Properties/Required on
// each ToolDefinition must match the tool's json.Unmarshal struct. This runs
// without a DB — the registry only needs to enumerate allTools.
func TestProjectDirToolsRegistered(t *testing.T) {
	reg := NewToolRegistry(nil, slog.Default(), nil)

	for _, name := range []string{"list_project_dir", "read_project_file"} {
		td, ok := reg.Get(name)
		if !ok {
			t.Fatalf("tool %q not registered in allTools()", name)
		}
		if td.Mutating {
			t.Errorf("%q must be read-only (Mutating false)", name)
		}
		if td.Fn == nil {
			t.Errorf("%q has no Fn", name)
		}
		if td.Description == "" {
			t.Errorf("%q has no description", name)
		}
	}

	list, _ := reg.Get("list_project_dir")
	if list.Properties == nil {
		t.Fatal("list_project_dir has no Properties schema")
	}
	for _, field := range []string{"project_id", "path"} {
		if _, ok := list.Properties[field]; !ok {
			t.Errorf("list_project_dir schema missing %q", field)
		}
	}
	if len(list.Required) != 1 || list.Required[0] != "project_id" {
		t.Errorf("list_project_dir Required = %v, want [project_id]", list.Required)
	}

	read, _ := reg.Get("read_project_file")
	for _, field := range []string{"project_id", "path", "max_bytes"} {
		if _, ok := read.Properties[field]; !ok {
			t.Errorf("read_project_file schema missing %q", field)
		}
	}
	if len(read.Required) != 2 || read.Required[0] != "project_id" || read.Required[1] != "path" {
		t.Errorf("read_project_file Required = %v, want [project_id path]", read.Required)
	}
}

// callProjectDirTool invokes the named project-dir tool by its registered
// name, mirroring how the MCP server would execute it.
func callProjectDirTool(t *testing.T, ctx context.Context, pool *db.Pool, name string, args map[string]any) (json.RawMessage, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	switch name {
	case "list_project_dir":
		return toolListProjectDir(ctx, pool, raw)
	case "read_project_file":
		return toolReadProjectFile(ctx, pool, raw)
	default:
		t.Fatalf("unknown tool %q", name)
		return nil, nil
	}
}

func mustWriteTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
