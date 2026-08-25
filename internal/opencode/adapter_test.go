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

// TestExecutionSystemPrompt verifies the batch-tool discipline is appended
// only when the execution runs in a runtime container with a project dir
// (composite worktree tools active), and is absent on the host-serve path.
func TestExecutionSystemPrompt(t *testing.T) {
	base := "You are an autonomous coding agent."
	containers := []struct {
		name     string
		manifest scheduler.ExecutionManifest
		want     bool // true = batch discipline should be present
	}{
		{name: "runtime container + project dir", manifest: scheduler.ExecutionManifest{SystemPrompt: base, RuntimeWorkflowID: "run-1", ProjectDir: "/p"}, want: true},
		{name: "runtime container, no project dir", manifest: scheduler.ExecutionManifest{SystemPrompt: base, RuntimeWorkflowID: "run-1"}, want: false},
		{name: "host serve (no runtime container)", manifest: scheduler.ExecutionManifest{SystemPrompt: base, ProjectDir: "/p"}, want: false},
	}
	for _, tc := range containers {
		t.Run(tc.name, func(t *testing.T) {
			got := executionSystemPrompt(tc.manifest)
			has := strings.Contains(got, "batch_read")
			if has != tc.want {
				t.Errorf("executionSystemPrompt() batch discipline present = %v, want %v; prompt=%q", has, tc.want, got)
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
