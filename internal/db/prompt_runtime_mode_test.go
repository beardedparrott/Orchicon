package db

import (
	"strings"
	"testing"
)

// TestRuntimeEnvironmentBlockBranches pins acceptance D: the runtime branch
// renders the container claim with the resolved image name; the local
// branch renders the HONEST in-process block (no container, loopback is
// LIVE). StablePromptPrefix threads the mode through and splits the KV
// cache across modes while sharing it within a mode.
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
	for _, want := range []string{"IN-PROCESS", "127.0.0.1:5432", "localhost:5432", "/tmp/orchicon"} {
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
