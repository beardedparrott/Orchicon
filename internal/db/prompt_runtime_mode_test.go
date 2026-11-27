package db

import (
	"strings"
	"testing"
)

// TestRuntimeEnvironmentBlockBranches pins acceptance D: the runtime branch renders the container
// claim with the resolved image name.
func TestRuntimeEnvironmentBlockBranches(t *testing.T) {
	rt := RuntimeEnvironmentBlock("orchicon-runtime:orchicon-dev", ExecutionModeRuntime)
	if !strings.Contains(rt, "orchicon-runtime:orchicon-dev") {
		t.Errorf("runtime branch must name the resolved image, got:\n%s", rt)
	}
	if !strings.Contains(strings.ToLower(rt), "container") {
		t.Errorf("runtime branch must claim container execution, got:\n%s", rt)
	}
	// Legacy empty mode degrades to the container branch (fail-open toward
	// the container, never toward silent host exec).
	legacy := RuntimeEnvironmentBlock("orchicon-runtime:orchicon-dev", "")
	if legacy != rt {
		t.Error("empty mode must render the runtime container branch")
	}

	local := RuntimeEnvironmentBlock("orchicon-runtime:orchicon-dev", ExecutionModeLocal)
	if strings.Contains(local, "You are running inside") {
		t.Errorf("local branch must NOT claim container execution, got:\n%s", local)
	}
	// The local branch must name BOTH planes by their real ports: prod (8091/5432) as untouchable
	// and dev (8080) as the sanctioned target. Asserting only that 5432 appears would let the block
	// regress to "everything loopback is LIVE", which is what told QA to stand down.
	// The local branch names the PLANES by port (8091 prod, 8080 dev) and must NOT name a database
	// port: the DB a plane is wired to is environment-specific, and asserting "5432" here is what
	// locked in the wrong mapping that told workers live data was their sandbox.
	for _, want := range []string{"IN-PROCESS", "8091", "8080", "/tmp/orchicon"} {
		if !strings.Contains(local, want) {
			t.Errorf("local branch missing %q, got:\n%s", want, local)
		}
	}

	pRuntime := StablePromptPrefix("img", ExecutionModeRuntime)
	pLocal := StablePromptPrefix("img", ExecutionModeLocal)
	if pRuntime == pLocal {
		t.Error("prefix must split across modes (no shared cache bytes between truths)")
	}
	if q := StablePromptPrefix("img", ExecutionModeLocal); q != pLocal {
		t.Error("prefix must be byte-identical within a mode (cache-safe)")
	}
}
