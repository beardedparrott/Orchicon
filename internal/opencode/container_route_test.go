package opencode

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestRuntimeContainerRouteEnabled pins the always-container routing gate
// (acceptance C + the local-mode contract): an opencode session dispatches
// into the run's container only when a daemon client is wired AND the run
// is workflow-bound AND the run is NOT in local execution mode. A local
// run has no container (EnsureForRun skipped) — it must use the host serve
// (in-process), never create a fresh container.
func TestRuntimeContainerRouteEnabled(t *testing.T) {
	cases := []struct {
		name      string
		hasClient bool
		manifest  scheduler.ExecutionManifest
		want      bool
	}{
		{
			name:      "runtime mode, workflow-bound, daemon → container",
			hasClient: true,
			manifest:  scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1", ExecutionMode: db.ExecutionModeRuntime},
			want:      true,
		},
		{
			name:      "local mode, workflow-bound, daemon → host serve (in-process)",
			hasClient: true,
			manifest:  scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1", ExecutionMode: db.ExecutionModeLocal},
			want:      false,
		},
		{
			name:      "local mode, no daemon → host serve",
			hasClient: false,
			manifest:  scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1", ExecutionMode: db.ExecutionModeLocal},
			want:      false,
		},
		{
			name:      "empty execution mode defaults to runtime → container",
			hasClient: true,
			manifest:  scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1"},
			want:      true,
		},
		{
			name:      "standalone task (no run) → host serve",
			hasClient: true,
			manifest:  scheduler.ExecutionManifest{ExecutionMode: db.ExecutionModeRuntime},
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtimeContainerRouteEnabled(tc.hasClient, tc.manifest); got != tc.want {
				t.Errorf("runtimeContainerRouteEnabled(%v, %+v) = %v, want %v", tc.hasClient, tc.manifest, got, tc.want)
			}
		})
	}
}
