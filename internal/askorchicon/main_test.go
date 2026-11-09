package askorchicon

// main_test.go — the process-wide Ask guard is a resource the test BINARY must hand back, not one a
// test can.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	CloseAskGuard()
	os.Exit(code)
}

// TestCloseAskGuardRemovesTheSingletonShimDir pins the invariant the daemon's shutdown seam now
// depends on: CloseAskGuard takes the shim DIRECTORY with it.
func TestCloseAskGuardRemovesTheSingletonShimDir(t *testing.T) {
	if _, err := askGuardForExec(); err != nil {
		t.Skip("the execution guard could not be built in this environment")
	}
	shimDir := askGuardShimDir(t)
	if _, err := os.Stat(shimDir); err != nil {
		t.Fatalf("the singleton's shim dir is not present while the guard is live: %v", err)
	}

	CloseAskGuard()

	if _, err := os.Stat(shimDir); !os.IsNotExist(err) {
		t.Fatalf("the shim dir survived CloseAskGuard (%v) — the daemon-shutdown seam in internal/server would leak it on every plane start", err)
	}
}

// askGuardShimDir reports where the live Ask guard keeps its shim, taken from
// the environment the guard itself builds rather than from /tmp naming.
func askGuardShimDir(t *testing.T) string {
	t.Helper()
	env := AskGuardEnvironFor(filepath.Join(t.TempDir(), "permission-policy.yaml"), "", nil, nil, false)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			continue
		}
		first := strings.SplitN(strings.TrimPrefix(kv, "PATH="), string(os.PathListSeparator), 2)[0]
		if strings.Contains(filepath.Base(first), "orchicon-guard-") {
			return first
		}
	}
	t.Fatal("AskGuardEnvironFor prepended no guard shim dir to PATH")
	return ""
}
