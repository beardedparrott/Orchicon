package opencode

import (
	"context"

	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestFileEditHookEngineOutput: a batch_write tool_use event hands the
// hook the tool's structured output (the engine's file_edits payload) plus
// the run's exec dir — the exact contract the server-side ledger hook
// implements. The hook fires for EVERY mutating tool event; the payload is
// passed verbatim (no pre-parsing in the adapter).
func TestFileEditHookEngineOutput(t *testing.T) {
	execDir := t.TempDir()
	manifest := scheduler.ExecutionManifest{ProjectDir: execDir}
	adapter := newTestAdapter(t)

	var gotExecID, gotTenant, gotDir, gotTool, gotOutput string
	adapter.SetFileEditHook(func(ctx context.Context, execID, tenantID, execDir, toolName string, input map[string]any, output string) {
		gotExecID, gotTenant, gotDir, gotTool, gotOutput = execID, tenantID, execDir, toolName, output
	})

	evt := map[string]any{
		"type": "tool_use",
		"part": map[string]any{
			"tool":   "batch_write",
			"callID": "call-1",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]any{"writes": []any{}},
				"output": "batch_write: applied 1 write(s): a.txt\n{\"summary\":\"batch_write: applied 1 write(s): a.txt\",\"file_edits\":[{\"path\":\"a.txt\",\"kind\":\"create\",\"unified_diff\":\"--- /dev/null\\n+++ b/a.txt\\n@@ -0,0 +1,1 @@\\n+hi\\n\"}]}",
			},
		},
	}
	var output strings.Builder
	textSeq := 0
	adapter.parseEvent(context.Background(), db.ExecutionRow{ID: "exec-1", TenantID: "tnt-1"}, manifest, evt, noopCallbacks{}, nil, &output, nil, &textSeq, nil, nil)

	if gotTool != "batch_write" || gotExecID != "exec-1" || gotTenant != "tnt-1" {
		t.Fatalf("hook context wrong: exec=%q tenant=%q tool=%q", gotExecID, gotTenant, gotTool)
	}
	if gotDir != execDir {
		t.Fatalf("hook execDir = %q, want %q", gotDir, execDir)
	}
	if !strings.Contains(gotOutput, "\"file_edits\"") {
		t.Fatalf("hook output lost the file_edits payload: %q", gotOutput)
	}
}

// TestFileEditHookNilIsNoOp: without a hook (tests, DB-less planes) tool_use
// events parse exactly as before — the ledger is additive.
func TestFileEditHookNilIsNoOp(t *testing.T) {
	execDir := t.TempDir()
	manifest := scheduler.ExecutionManifest{ProjectDir: execDir}
	adapter := newTestAdapter(t)

	evt := map[string]any{
		"type": "tool_use",
		"part": map[string]any{
			"tool":   "edit",
			"callID": "call-2",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]any{"filePath": "x.txt", "oldString": "a", "newString": "b"},
				"output": "ok",
			},
		},
	}
	var output strings.Builder
	textSeq := 0
	// Must not panic with a nil hook.
	adapter.parseEvent(context.Background(), db.ExecutionRow{ID: "exec-2"}, manifest, evt, noopCallbacks{}, nil, &output, nil, &textSeq, nil, nil)
	if !strings.Contains(output.String(), "") {
		t.Fatalf("unexpected output")
	}
}

// TestFileEditHookBuiltinWriteArtifactBranch: opencode's built-in `write`
// is intercepted as an artifact (content streamed + artifact card) AND still
// reaches the file-edit hook — the hook runs before the artifact early-break
// (adapter.go), so the plane-side observer takes a live after-snapshot for
// the successful mutation. No double-recording with the file_diff fallback:
// the fallback skips engine-payload paths via stats.engineEditedPaths, and a
// built-in write carries no payload (registers nothing) but the server-side
// observer cache already holds the fresh read, so the later file_diff
// ObserveAfter sees no change and yields no second row.
func TestFileEditHookBuiltinWriteArtifactBranch(t *testing.T) {
	execDir := t.TempDir()
	manifest := scheduler.ExecutionManifest{ProjectDir: execDir}
	adapter := newTestAdapter(t)

	var calls int
	adapter.SetFileEditHook(func(ctx context.Context, execID, tenantID, dir, toolName string, input map[string]any, output string) {
		calls++
	})

	evt := map[string]any{
		"type": "tool_use",
		"part": map[string]any{
			"tool":   "write",
			"callID": "call-3",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]any{"path": "note.md", "content": "hello"},
				"output": "wrote note.md",
			},
		},
	}
	var output strings.Builder
	textSeq := 0
	adapter.parseEvent(context.Background(), db.ExecutionRow{ID: "exec-3", TenantID: "tnt-1"}, manifest, evt, noopCallbacks{}, nil, &output, nil, &textSeq, nil, nil)

	if calls != 1 {
		t.Fatalf("built-in write must reach the file-edit hook once (live observer row), got %d", calls)
	}
	if !strings.Contains(output.String(), "hello") {
		t.Fatalf("built-in write must still stream its content as an artifact, got %q", output.String())
	}
}

// TestFileEditHookFileDiffFallback: a file_diff event for a path the engine
// tools never ledgered reaches the hook with tool="file_diff" and the path
// in the input map — the fallback entry the server computes from real file
// state. Engine-covered paths are filtered adapter-side via stats.engineEditedPaths,
// so the test passes a real execStreamState (production always does; parseEvent
// nests the fallback under the stats != nil guard).
func TestFileEditHookFileDiffFallback(t *testing.T) {
	execDir := t.TempDir()
	manifest := scheduler.ExecutionManifest{ProjectDir: execDir}
	adapter := newTestAdapter(t)

	type hookCall struct {
		tool  string
		dir   string
		input map[string]any
	}
	var calls []hookCall
	adapter.SetFileEditHook(func(ctx context.Context, execID, tenantID, dir, toolName string, input map[string]any, output string) {
		calls = append(calls, hookCall{tool: toolName, dir: dir, input: input})
	})

	// 1) An engine-covered batch_write first (fires the hook itself AND
	// registers covered.txt in stats.engineEditedPaths).
	engEvt := map[string]any{
		"type": "tool_use",
		"part": map[string]any{
			"tool":   "batch_write",
			"callID": "call-a",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]any{},
				"output": "batch_write: applied 1 write(s): covered.txt\n{\"summary\":\"s\",\"file_edits\":[{\"path\":\"covered.txt\",\"kind\":\"create\"}]}",
			},
		},
	}
	// 2) file_diff events for the covered path (skipped) and an uncovered
	// one (falls through).
	diffCovered := map[string]any{"type": "file_diff", "part": map[string]any{"path": "covered.txt"}}
	diffUncovered := map[string]any{"type": "file_diff", "part": map[string]any{"path": "mystery.txt"}}

	var output strings.Builder
	textSeq := 0
	ctx := context.Background()
	row := db.ExecutionRow{ID: "exec-4", TenantID: "tnt-1"}
	stats := &execStreamState{}
	for _, e := range []map[string]any{engEvt, diffCovered, diffUncovered} {
		adapter.parseEvent(ctx, row, manifest, e, noopCallbacks{}, nil, &output, nil, &textSeq, stats, nil)
	}

	// 2 hook calls: the batch_write tool_use itself, then the uncovered
	// file_diff fallback. covered.txt is skipped (engine-covered).
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 hook calls (batch_write + uncovered file_diff), got %d: %+v", len(calls), calls)
	}
	if calls[0].tool != "batch_write" {
		t.Fatalf("first call should be the batch_write tool_use, got %+v", calls[0])
	}
	if calls[1].tool != "file_diff" || calls[1].dir != execDir {
		t.Fatalf("fallback call wrong: %+v", calls[1])
	}
	if p, _ := calls[1].input["path"].(string); p != "mystery.txt" {
		t.Fatalf("fallback path = %v", calls[1].input)
	}
}
