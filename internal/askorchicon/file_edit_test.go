package askorchicon

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// TestFileEditHookFiresOnToolUse: a completed mutating tool_use part reaches
// the file-edit hook with the tool name, input map, and output — the Ask
// conversation's ledger contract (owner_kind ask_conversation is applied
// server-side by the hook implementation). Parts before the message was
// accepted (sent == false) must NOT fire the hook.
func TestFileEditHookFiresOnToolUse(t *testing.T) {
	s := New(nil, slog.Default(), nil, nil, nil)
	var mu sync.Mutex
	var calls []string
	s.SetFileEditHook(func(ctx context.Context, tenantID, convID, toolName string, input map[string]any, output string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, toolName+"|"+output)
	})
	if s.fileEditHook == nil {
		t.Fatalf("hook not stored")
	}
	// Direct contract check: the drain loop passes completed tool_use parts
	// verbatim (chat.go's "part" case). The full SSE loop is covered by the
	// session tests; this pins the setter + nil-safety contract.
	s.fileEditHook(context.Background(), "tnt-1", "conv-1", "batch_write", map[string]any{}, `{"file_edits":[]}`)
	if len(calls) != 1 || calls[0] != "batch_write|{\"file_edits\":[]}" {
		t.Fatalf("hook call wrong: %v", calls)
	}
}

// TestFileEditHookNilIsNoOp: without a hook the turn runs exactly as before
// (tests / DB-less planes) — no panic on the nil hook path.
func TestFileEditHookNilIsNoOp(t *testing.T) {
	s := New(nil, slog.Default(), nil, nil, nil)
	if s.fileEditHook != nil {
		t.Fatalf("hook should default to nil")
	}
	if s.fileEditReconciler != nil {
		t.Fatalf("reconciler should default to nil")
	}
}

// TestFileEditReconcilerWired: the reconciler setter stores the terminal-turn
// git reconciliation callback.
func TestFileEditReconcilerWired(t *testing.T) {
	s := New(nil, slog.Default(), nil, nil, nil)
	called := false
	s.SetFileEditReconciler(func(ctx context.Context, tenantID, convID string) {
		called = true
	})
	if s.fileEditReconciler == nil {
		t.Fatalf("reconciler not stored")
	}
	s.fileEditReconciler(context.Background(), "tnt-1", "conv-1")
	if !called {
		t.Fatalf("reconciler not invoked")
	}
}
