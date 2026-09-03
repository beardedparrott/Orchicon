package opencode

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

// newTestAdapter returns an Adapter with a discard logger for tests.
func newTestAdapter(t *testing.T) *Adapter {
	t.Helper()
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// noopCallbacks is a no-op ExecutionCallbacks for tests that only exercise
// parseEvent's side effects (todo snapshot) without a live transport.
type noopCallbacks struct{}

func (noopCallbacks) OnArtifact(context.Context, string, string, string, string) {}
func (noopCallbacks) OnToolCall(context.Context, string, string, []byte, []byte) {}
func (noopCallbacks) OnStepFinish(context.Context, string)                       {}
func (noopCallbacks) OnSessionEvent(context.Context, string, string)             {}
func (noopCallbacks) OnEvent(context.Context, string, string)                    {}
func (noopCallbacks) OnStall(context.Context, string, string, bool)              {}
func (noopCallbacks) OnProviderError(context.Context, string, string, string)    {}
func (noopCallbacks) OnCheckpoint(context.Context, string, string, []byte)       {}
func (noopCallbacks) OnCost(context.Context, string, float64)                    {}
func (noopCallbacks) OnUsage(context.Context, string, string, string, int64, int64, int64, int64, int64, int64) {
}
func (noopCallbacks) OnStarted(context.Context, string)                      {}
func (noopCallbacks) OnWrittenFiles(context.Context, string, []string)       {}
func (noopCallbacks) OnHealth(context.Context, string, string)               {}
func (noopCallbacks) OnRecovered(context.Context, string, string)            {}
func (noopCallbacks) OnResult(context.Context, string, bool, string, string) {}
func (noopCallbacks) OnText(context.Context, string, string)                 {}

// TestExecutionDir verifies the execution working-directory resolution: the
// provisioned worktree path wins when set, otherwise the project dir. This is
// the single resolution the session directory (the worker's cwd) and the
// safety-lint write location share.
func TestExecutionDir(t *testing.T) {
	cases := []struct {
		name     string
		manifest scheduler.ExecutionManifest
		want     string
	}{
		{
			name: "worktree path wins",
			manifest: scheduler.ExecutionManifest{
				ProjectDir:   "/srv/proj",
				WorktreePath: "/srv/proj/.orchicon-worktrees/run1",
			},
			want: "/srv/proj/.orchicon-worktrees/run1",
		},
		{
			name: "empty worktree falls back to project dir",
			manifest: scheduler.ExecutionManifest{
				ProjectDir: "/srv/proj",
			},
			want: "/srv/proj",
		},
		{
			name: "non-worktree run keeps project dir even when worktree set empty",
			manifest: scheduler.ExecutionManifest{
				ProjectDir:   "/srv/proj",
				WorktreePath: "",
			},
			want: "/srv/proj",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := executionDir(tc.manifest); got != tc.want {
				t.Errorf("executionDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExecutionSystemPrompt verifies the run-prelude notes are appended only
// under the right conditions: the worktree-path discipline note only when the
// run has a provisioned worktree (WorktreePath != ""), and the batch-tool
// discipline only for a runtime-container run with a project dir. It also
// checks the base system prompt is always preserved.
func TestExecutionSystemPrompt(t *testing.T) {
	base := "You are an autonomous coding agent."
	cases := []struct {
		name         string
		manifest     scheduler.ExecutionManifest
		wantWorktree bool // true = worktree-path discipline note present
		wantBatch    bool // true = batch-tool discipline present
	}{
		{
			name:         "runtime container + project dir",
			manifest:     scheduler.ExecutionManifest{SystemPrompt: base, RuntimeWorkflowID: "run-1", ProjectDir: "/p"},
			wantWorktree: false, // no provisioned worktree on this manifest
			wantBatch:    true,
		},
		{
			name:         "runtime container, no project dir",
			manifest:     scheduler.ExecutionManifest{SystemPrompt: base, RuntimeWorkflowID: "run-1"},
			wantWorktree: false,
			wantBatch:    false,
		},
		{
			name:         "host serve (no runtime container)",
			manifest:     scheduler.ExecutionManifest{SystemPrompt: base, ProjectDir: "/p"},
			wantWorktree: false,
			wantBatch:    false,
		},
		{
			name:         "worktree run (provisioned worktree, no runtime container)",
			manifest:     scheduler.ExecutionManifest{SystemPrompt: base, WorktreePath: "/srv/proj/.orchicon-worktrees/run1"},
			wantWorktree: true,
			wantBatch:    false,
		},
		{
			name:         "worktree run + runtime container",
			manifest:     scheduler.ExecutionManifest{SystemPrompt: base, WorktreePath: "/srv/proj/.orchicon-worktrees/run1", RuntimeWorkflowID: "run-1", ProjectDir: "/p"},
			wantWorktree: true,
			wantBatch:    true,
		},
		{
			name:         "empty manifest (no worktree, no runtime)",
			manifest:     scheduler.ExecutionManifest{SystemPrompt: base},
			wantWorktree: false,
			wantBatch:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := executionSystemPrompt(tc.manifest)
			// The worktree-path discipline note is present only under a
			// provisioned worktree. Reference the const directly so the test
			// and the source cannot drift apart.
			if hasWorktree := strings.Contains(got, worktreePathDiscipline); hasWorktree != tc.wantWorktree {
				t.Errorf("executionSystemPrompt() worktree note present = %v, want %v; prompt=%q", hasWorktree, tc.wantWorktree, got)
			}
			hasBatch := strings.Contains(got, batchToolsDiscipline)
			if hasBatch != tc.wantBatch {
				t.Errorf("executionSystemPrompt() batch discipline present = %v, want %v; prompt=%q", hasBatch, tc.wantBatch, got)
			}
			// The base system prompt must always be preserved.
			if !strings.Contains(got, base) {
				t.Errorf("executionSystemPrompt() dropped base prompt: %q", got)
			}
		})
	}
}

// TestAbortExecutionNoLiveSession verifies AbortExecution is a safe no-op for
// an unknown/already-finished execution (no live session on the transport).
// Cancelling a non-running execution must never panic or error.
func TestAbortExecutionNoLiveSession(t *testing.T) {
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := a.AbortExecution(context.Background(), "exec-nonexistent", "test-cancel"); err != nil {
		t.Fatalf("AbortExecution for unknown exec should be a no-op, got error: %v", err)
	}
}

// TestParseTodoItems verifies the todowrite → sidecar-snapshot parsing: the
// full replacement array shape, malformed-item dropping, empty-content
// skipping, and status defaulting. This is the parsing layer that feeds
// `todoread` (and the native todo surface) for DB-less sessions.
func TestParseTodoItems(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []worktree.TodoItem
	}{
		{
			name: "full array with statuses and priorities",
			in: map[string]any{"todos": []any{
				map[string]any{"content": "do a", "status": "in_progress", "priority": "high"},
				map[string]any{"content": "do b", "status": "completed", "priority": "medium"},
			}},
			want: []worktree.TodoItem{
				{Content: "do a", Status: "in_progress", Priority: "high"},
				{Content: "do b", Status: "completed", Priority: "medium"},
			},
		},
		{
			name: "missing status defaults to pending",
			in:   map[string]any{"todos": []any{map[string]any{"content": "do c"}}},
			want: []worktree.TodoItem{{Content: "do c", Status: "pending", Priority: ""}},
		},
		{
			name: "empty content dropped, malformed item dropped",
			in: map[string]any{"todos": []any{
				map[string]any{"content": ""},
				"not-a-map",
				map[string]any{"content": "do d"},
			}},
			want: []worktree.TodoItem{{Content: "do d", Status: "pending", Priority: ""}},
		},
		{
			name: "not a map returns nil",
			in:   "junk",
			want: nil,
		},
		{
			name: "missing todos key returns nil",
			in:   map[string]any{"other": 1},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTodoItems(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseTodoItems() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseTodoItems()[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestTodoWriteSnapshotSidecar verifies the full wiring: a todowrite tool_use
// event flows through parseEvent and persists the snapshot to
// .orchicon/todos.json under the execution dir, which `todoread` then
// returns. This is the native todo surface working end-to-end without any
// parsing layer for DB-less sessions.
func TestTodoWriteSnapshotSidecar(t *testing.T) {
	execDir := t.TempDir()
	manifest := scheduler.ExecutionManifest{ProjectDir: execDir}
	adapter := newTestAdapter(t)

	evt := map[string]any{
		"type": "tool_use",
		"part": map[string]any{
			"tool":   "todowrite",
			"callID": "call-1",
			"state": map[string]any{
				"status": "completed",
				"input": map[string]any{
					"todos": []any{
						map[string]any{"content": "first", "status": "in_progress", "priority": "high"},
						map[string]any{"content": "second", "status": "pending", "priority": "low"},
					},
				},
				"output": "ok",
			},
		},
	}
	callbacks := noopCallbacks{}
	var output strings.Builder
	textSeq := 0
	adapter.parseEvent(context.Background(), db.ExecutionRow{ID: "exec-1"}, manifest, evt, callbacks, nil, &output, nil, &textSeq, nil, nil)

	// todoread must now return the snapshot.
	out, err := worktree.TodoRead(worktree.BaseFor(execDir), worktree.TodoReadArgs{})
	if err != nil {
		t.Fatalf("TodoRead err: %v", err)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Fatalf("todoread did not return the snapshot items:\n%s", out)
	}
	if !strings.Contains(out, "in_progress") {
		t.Fatalf("todoread should surface statuses:\n%s", out)
	}
}
