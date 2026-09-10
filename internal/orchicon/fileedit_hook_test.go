package orchicon

// Regression pins for the native-loop file-edit ledger gap: executions
// dispatching through the native-loop family (ollama, commandcode, …)
// produced ledger rows for NOTHING while running because no hook was
// wired into the native path. Session.executeTools is the single shared
// completion funnel every native-loop provider rides, so the hook fires
// there — once per COMPLETED registry result, with the PRE-cap output.
// These tests pin that site (invocation count + gating); the ledger-rule
// fidelity (engine-payload-first, observer fallback, virtual ignores)
// stays pinned by internal/server/fileedit_hook_test.go, which exercises
// the same production constructor wired here.

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/fileedit"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

type nativeHookFakeStore struct {
	mu   sync.Mutex
	rows []*db.FileEditLedgerRow
}

func (f *nativeHookFakeStore) Append(_ context.Context, tenantID, ownerKind, ownerID string, entries []fileedit.Entry) ([]*db.FileEditLedgerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*db.FileEditLedgerRow, 0, len(entries))
	for _, e := range entries {
		out = append(out, &db.FileEditLedgerRow{
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
		})
	}
	f.rows = append(f.rows, out...)
	return out, nil
}

func (f *nativeHookFakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

const nativeEngineWriteOutput = "write: applied 1 write(s): notes/a.txt\n" +
	`{"summary":"write: applied 1 write(s): notes/a.txt","file_edits":[` +
	`{"path":"notes/a.txt","kind":"create","unified_diff":"--- /dev/null\n+++ b/notes/a.txt\n@@ -0,0 +1,1 @@\n+hi\n",` +
	`"existed_before":false,"existed_after":true,"size_before":0,"size_after":3}]}`

const nativeEngineBatchOutput = "batch_write: applied 2 write(s): notes/a.txt notes/b.txt\n" +
	`{"summary":"batch_write: applied 2 write(s)","file_edits":[` +
	`{"path":"notes/a.txt","kind":"create","unified_diff":"--- /dev/null\n+++ b/notes/a.txt\n@@ -0,0 +1,1 @@\n+hi\n",` +
	`"existed_before":false,"existed_after":true,"size_before":0,"size_after":3},` +
	`{"path":"notes/b.txt","kind":"modify","unified_diff":"--- a/notes/b.txt\n+++ b/notes/b.txt\n@@ -1 +1 @@\n-hi\n+yo\n",` +
	`"existed_before":true,"existed_after":true,"size_before":3,"size_after":3}]}`

// recordingEngineHook is the test-side hook: it records every invocation
// (proving the SITE fires exactly once per completed call) and ledgers
// engine payloads through the real fileedit.Service — the same
// RecordEngineOutput call the production hook makes for write/edit/
// batch_write. Non-mutating tools record the invocation but ledger nothing
// (production parity: the hook's switch ignores them).
type hookCall struct {
	tool   string
	output string
}

func newRecordingEngineHook(svc *fileedit.Service, calls *[]hookCall, mu *sync.Mutex) opencode.FileEditHookFunc {
	return func(ctx context.Context, execID, tenantID, execDir, tool string, input map[string]any, output string) {
		mu.Lock()
		*calls = append(*calls, hookCall{tool: tool, output: output})
		mu.Unlock()
		switch tool {
		case "write", "edit", "batch_write":
			svc.RecordEngineOutput(ctx, tenantID, db.FileEditOwnerExecution, execID, tool, output)
		}
	}
}

func newHookTestSession(t *testing.T, tools ToolRegistry, hook opencode.FileEditHookFunc) *Session {
	t.Helper()
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_hook_test"),
		Manifest:   testManifest("orchicon/ollama/deepseek-v4-flash"),
		ProjectDir: t.TempDir(),
		Provider:   &mockProvider{},
		Tools:      tools,
		Log:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s.SetFileEditHook(hook)
	return s
}

func TestNativeHookEngineWriteOneRow(t *testing.T) {
	store := &nativeHookFakeStore{}
	svc := fileedit.NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var mu sync.Mutex
	var calls []hookCall
	tools := newMockTools()
	tools.results["write"] = nativeEngineWriteOutput
	s := newHookTestSession(t, tools, newRecordingEngineHook(svc, &calls, &mu))
	cb := &recordedCallback{}
	results := s.executeTools(context.Background(), cb, []ToolCall{
		{Index: 0, ToolCallID: "call_1", Name: "write", ArgsJSON: `{"path":"notes/a.txt","content":"hi\n"}`},
	})
	if len(results) != 1 || results[0].Err != "" {
		t.Fatalf("executeTools = %+v, want one success", results)
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 1 || calls[0].tool != "write" {
		t.Fatalf("hook calls = %+v, want exactly one write invocation", calls)
	}
	if got := store.count(); got != 1 {
		t.Fatalf("ledger rows = %d, want 1", got)
	}
}

func TestNativeHookBatchWriteNRows(t *testing.T) {
	store := &nativeHookFakeStore{}
	svc := fileedit.NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var mu sync.Mutex
	var calls []hookCall
	tools := newMockTools()
	tools.results["batch_write"] = nativeEngineBatchOutput
	s := newHookTestSession(t, tools, newRecordingEngineHook(svc, &calls, &mu))
	cb := &recordedCallback{}
	results := s.executeTools(context.Background(), cb, []ToolCall{
		{Index: 0, ToolCallID: "call_1", Name: "batch_write", ArgsJSON: `{"ops":[]}`},
	})
	if len(results) != 1 || results[0].Err != "" {
		t.Fatalf("executeTools = %+v, want one success", results)
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("hook calls = %d, want exactly one batch_write invocation", n)
	}
	if got := store.count(); got != 2 {
		t.Fatalf("ledger rows = %d, want 2", got)
	}
}

func TestNativeHookFailedCallZeroRows(t *testing.T) {
	store := &nativeHookFakeStore{}
	svc := fileedit.NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var mu sync.Mutex
	var calls []hookCall
	tools := newMockTools()
	tools.errs["write"] = "boom: disk full"
	s := newHookTestSession(t, tools, newRecordingEngineHook(svc, &calls, &mu))
	cb := &recordedCallback{}
	results := s.executeTools(context.Background(), cb, []ToolCall{
		{Index: 0, ToolCallID: "call_1", Name: "write", ArgsJSON: `{"path":"notes/a.txt","content":"hi\n"}`},
	})
	if len(results) != 1 || results[0].Err == "" {
		t.Fatalf("executeTools = %+v, want one failure", results)
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("hook calls = %d on a failed call, want 0 (completed-only gating)", n)
	}
	if got := store.count(); got != 0 {
		t.Fatalf("ledger rows = %d on a failed call, want 0", got)
	}
}

func TestNativeHookNilHookNoop(t *testing.T) {
	tools := newMockTools()
	tools.results["write"] = nativeEngineWriteOutput
	s := newHookTestSession(t, tools, nil)
	cb := &recordedCallback{}
	results := s.executeTools(context.Background(), cb, []ToolCall{
		{Index: 0, ToolCallID: "call_1", Name: "write", ArgsJSON: `{"path":"notes/a.txt","content":"hi\n"}`},
	})
	if len(results) != 1 || results[0].Err != "" {
		t.Fatalf("executeTools = %+v, want one success (nil hook must not break tools)", results)
	}
}

func TestNativeHookNonMutatingToolZeroRows(t *testing.T) {
	store := &nativeHookFakeStore{}
	svc := fileedit.NewService(store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var mu sync.Mutex
	var calls []hookCall
	tools := newMockTools()
	tools.results["read"] = "file contents here"
	s := newHookTestSession(t, tools, newRecordingEngineHook(svc, &calls, &mu))
	cb := &recordedCallback{}
	results := s.executeTools(context.Background(), cb, []ToolCall{
		{Index: 0, ToolCallID: "call_1", Name: "read", ArgsJSON: `{"path":"notes/a.txt"}`},
	})
	if len(results) != 1 || results[0].Err != "" {
		t.Fatalf("executeTools = %+v, want one success", results)
	}
	if got := store.count(); got != 0 {
		t.Fatalf("ledger rows = %d for a read, want 0", got)
	}
}
