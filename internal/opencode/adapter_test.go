package opencode

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

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
			hasBatch := strings.Contains(got, "batch_read")
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
