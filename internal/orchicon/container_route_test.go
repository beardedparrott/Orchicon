package orchicon

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestNativeContainerRouteEnabled pins the always-container routing gate
// (acceptance C + the local-mode contract): native `bash` routes into the
// run's container only when a daemon client is wired AND the run is
// workflow-bound AND the run is NOT in local execution mode. A local run
// has no container lease — it must stay in-process even with a daemon.
func TestNativeContainerRouteEnabled(t *testing.T) {
	cases := []struct {
		name       string
		hasClient  bool
		manifest   scheduler.ExecutionManifest
		want       bool
	}{
		{
			name:      "runtime mode, workflow-bound, daemon → route",
			hasClient: true,
			manifest:  scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1", ExecutionMode: db.ExecutionModeRuntime},
			want:      true,
		},
		{
			name:      "local mode, workflow-bound, daemon → in-process",
			hasClient: true,
			manifest:  scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1", ExecutionMode: db.ExecutionModeLocal},
			want:      false,
		},
		{
			name:      "local mode, no daemon → in-process",
			hasClient: false,
			manifest:  scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1", ExecutionMode: db.ExecutionModeLocal},
			want:      false,
		},
		{
			name:      "empty execution mode defaults to runtime → route",
			hasClient: true,
			manifest:  scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1"},
			want:      true,
		},
		{
			name:      "standalone task (no run) → in-process",
			hasClient: true,
			manifest:  scheduler.ExecutionManifest{ExecutionMode: db.ExecutionModeRuntime},
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeContainerRouteEnabled(tc.hasClient, tc.manifest); got != tc.want {
				t.Errorf("nativeContainerRouteEnabled(%v, %+v) = %v, want %v", tc.hasClient, tc.manifest, got, tc.want)
			}
		})
	}
}
