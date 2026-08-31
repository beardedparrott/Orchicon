package opencode

import (
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
