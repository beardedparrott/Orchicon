package askorchicon

// main_test.go — the process-wide Ask guard is a resource the test BINARY must
// hand back, not one a test can.
//
// askGuardForExec builds ONE shim dir per process ("one dir per daemon", as its
// own doc puts it) and only CloseAskGuard removes it. Production calls that at
// shutdown, from the seam in internal/server beside claudeBridge.CloseAsk and
// CloseAskMCP. Nothing in the tests did, so a test binary that execs Ask bash
// left its shim dir in $TMPDIR for the life of the host.
//
// MEASURED, and intermittent in a way that hid it: because the singleton is built
// LAZILY by whichever test execs bash FIRST, the leaked dir carried that test's
// own baked policy path. The one found was an orphan whose shim script named
// TestGuardShimAndConsentCoreReadTheSamePolicy's t.TempDir() policy — identifying
// its author exactly while saying nothing about which run produced it. Whether a
// given run leaked depended on test order (the singleton is closed only by
// TestCloseAskGuardResetsState, so any bash-executing test after it re-armed a
// guard nothing closed), which is why running this package alone never showed it.
//
// TestMain rather than an init or a per-test t.Cleanup: the resource is
// process-wide and outlives every individual test, so the binary is the only
// lifetime that can own it. With this, the package cannot leak a shim dir
// whatever order its tests run in.

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

// TestCloseAskGuardRemovesTheSingletonShimDir pins the invariant the daemon's
// shutdown seam now depends on: CloseAskGuard takes the shim DIRECTORY with it.
//
// Without this the product could drop the call from internal/server and every
// test would still pass — the guard keeps working perfectly right up until the
// process exits without removing it, which is precisely how the accumulation was
// invisible for so long. The assertion is therefore about the DIRECTORY, not
// about the guard's state. It is the Ask twin of
// TestChildEnvCleanupRemovesTheShimDir (the worker transport) and
// TestCloseAskRemovesTheGuardShimDir (claude's Ask transport).
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
