package server

// Regression test for the live-run file-edit ledger gap: since the
// composite-tools rollout the tool names "write"/"edit" denote BOTH the
// worktree engine's single-op wrappers (ground truth in the structured
// `file_edits` output) AND the opencode built-in file tools (plane-side
// after-snapshot). The old hook routed write/edit exclusively down the
// observer path, silently dropping every successful single-op engine edit.
// The fixed hook (newFileEditHook) tries the engine payload first and falls
// back to the observer only for genuine built-in usage.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/fileedit"
)

type fakeFileEditStore struct {
	mu   sync.Mutex
	rows []*db.FileEditLedgerRow
}

func (f *fakeFileEditStore) Append(_ context.Context, tenantID, ownerKind, ownerID string, entries []fileedit.Entry) ([]*db.FileEditLedgerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*db.FileEditLedgerRow, 0, len(entries))
	for _, e := range entries {
		r := &db.FileEditLedgerRow{
			ID:           db.NewID(),
			TenantID:     tenantID,
			OwnerKind:    ownerKind,
			OwnerID:      ownerID,
			Seq:          int64(len(f.rows) + len(out) + 1),
			Path:         e.Path,
			Kind:         e.Kind,
			UnifiedDiff:  e.UnifiedDiff,
			BeforeSize:   e.BeforeSize,
			AfterSize:    e.AfterSize,
			BeforeSHA256: e.BeforeSHA,
			AfterSHA256:  e.AfterSHA,
			Tool:         e.Tool,
		}
		out = append(out, r)
	}
	f.rows = append(f.rows, out...)
	return out, nil
}

func (f *fakeFileEditStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeFileEditStore) tools() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tools := make([]string, 0, len(f.rows))
	for _, r := range f.rows {
		tools = append(tools, r.Tool)
	}
	return tools
}

const engineWriteOutput = "write: applied 1 write(s): notes/a.txt\n" +
	`{"summary":"write: applied 1 write(s): notes/a.txt","file_edits":[` +
	`{"path":"notes/a.txt","kind":"create","unified_diff":"--- /dev/null\n+++ b/notes/a.txt\n@@ -0,0 +1,1 @@\n+hi\n",` +
	`"existed_before":false,"existed_after":true,"size_before":0,"size_after":3}]}`

const engineEditOutput = "edit: applied 1 edit(s): notes/a.txt\n" +
	`{"summary":"edit: applied 1 edit(s): notes/a.txt","file_edits":[` +
	`{"path":"notes/a.txt","kind":"modify","unified_diff":"--- a/notes/a.txt\n+++ b/notes/a.txt\n@@ -1 +1 @@\n-hi\n+yo\n",` +
	`"existed_before":true,"existed_after":true,"size_before":3,"size_after":3}]}`

// TestFileEditHookEngineWriteEdit: engine single-op write/edit outputs with a
// file_edits payload yield exactly one ledger row each, tagged with the tool
// name — the payload is tried BEFORE the observer, so no plane-side read is
// needed (execDir points at an empty dir to prove the observer never fires:
// any observer fallback would find no file and record nothing, or record a
// wrong-base row).
func TestFileEditHookEngineWriteEdit(t *testing.T) {
	store := &fakeFileEditStore{}
	svc := fileedit.NewService(store, slog.Default())
	hook := newFileEditHook(svc, slog.Default())

	ctx := context.Background()
	emptyDir := t.TempDir()
	hook(ctx, "exec-1", "tnt-1", emptyDir, "write", map[string]any{"filePath": "notes/a.txt"}, engineWriteOutput)
	hook(ctx, "exec-1", "tnt-1", emptyDir, "edit", map[string]any{"filePath": "notes/a.txt"}, engineEditOutput)

	if got := store.count(); got != 2 {
		t.Fatalf("engine write+edit yielded %d rows, want 2", got)
	}
	tools := store.tools()
	if tools[0] != "write" || tools[1] != "edit" {
		t.Fatalf("row tools = %v, want [write edit]", tools)
	}
}

// TestFileEditHookBuiltinFallback: a genuine built-in-shaped write (no
// file_edits payload, content input) falls back to the plane-side
// after-snapshot and records one opencode:write row from real file state.
func TestFileEditHookBuiltinFallback(t *testing.T) {
	store := &fakeFileEditStore{}
	svc := fileedit.NewService(store, slog.Default())
	hook := newFileEditHook(svc, slog.Default())

	ctx := context.Background()
	execDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(execDir, "note.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	hook(ctx, "exec-2", "tnt-1", execDir, "write",
		map[string]any{"filePath": "note.md", "content": "hello"}, "wrote note.md")

	if got := store.count(); got != 1 {
		t.Fatalf("built-in write yielded %d rows, want 1", got)
	}
	if tools := store.tools(); tools[0] != fileedit.ToolOpenCodeWrite {
		t.Fatalf("row tool = %v, want %v", tools, fileedit.ToolOpenCodeWrite)
	}
}

// TestFileEditHookFailedOutputSilent: a failed edit call's output (no payload,
// error text) yields zero rows — the tolerant design: failed tools stay
// silent, never phantom rows.
func TestFileEditHookFailedOutputSilent(t *testing.T) {
	store := &fakeFileEditStore{}
	svc := fileedit.NewService(store, slog.Default())
	hook := newFileEditHook(svc, slog.Default())

	ctx := context.Background()
	execDir := t.TempDir()
	hook(ctx, "exec-3", "tnt-1", execDir, "edit",
		map[string]any{"filePath": "missing.txt"}, "edit: oldString not found in content")
	hook(ctx, "exec-3", "tnt-1", execDir, "batch_write",
		map[string]any{}, "")
	hook(ctx, "exec-3", "tnt-1", execDir, "write_artifact",
		map[string]any{}, "whatever")

	if got := store.count(); got != 0 {
		t.Fatalf("failed/empty outputs yielded %d rows, want 0", got)
	}
}

// TestFileEditHookBatchWrite: the batch_write path is unchanged — structured
// output records one row per op.
func TestFileEditHookBatchWrite(t *testing.T) {
	store := &fakeFileEditStore{}
	svc := fileedit.NewService(store, slog.Default())
	hook := newFileEditHook(svc, slog.Default())

	ctx := context.Background()
	out := "batch_write: applied 2 write(s): a.txt, b.md\n" +
		`{"summary":"x","file_edits":[` +
		`{"path":"a.txt","kind":"create","unified_diff":"--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1,1 @@\n+a\n","existed_before":false,"existed_after":true,"size_before":0,"size_after":2},` +
		`{"path":"b.md","kind":"create","unified_diff":"--- /dev/null\n+++ b/b.md\n@@ -0,0 +1,1 @@\n+b\n","existed_before":false,"existed_after":true,"size_before":0,"size_after":2}]}`
	hook(ctx, "exec-4", "tnt-1", t.TempDir(), "batch_write", map[string]any{}, out)

	if got := store.count(); got != 2 {
		t.Fatalf("batch_write yielded %d rows, want 2", got)
	}
}
