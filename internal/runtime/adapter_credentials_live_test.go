package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderAPIKeyEnvMatchesOpenCodeRegistry is the DRIFT DETECTOR for
// providerAPIKeyEnv. That map is only as good as opencode's own provider
// registry, and a wrong entry fails SILENTLY at run time (opencode ignores an
// environment variable it does not know), so the map is re-checked here against
// the registry opencode itself caches on the host.
//
// It SKIPS rather than fails when the registry is not present (a CI host or a
// machine that has never run opencode): the check is evidence about THIS host's
// opencode, not a hermetic property of the repository. Ids the registry does not
// carry are reported, not failed — two of them (commandcode, command-code) are
// deliberately sourced from Orchicon's own provider registry because they are
// third-party provider packages rather than models.dev entries.
func TestProviderAPIKeyEnvMatchesOpenCodeRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("live-registry check skipped in -short mode")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	p := filepath.Join(home, ".cache", "opencode", "models.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("opencode registry not present (%s) — nothing to cross-check against", p)
	}
	// The registry is either a bare {id: provider} map or wraps one under
	// "providers". Accept both rather than assume a shape this test cannot verify.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	providers := top
	if raw, ok := top["providers"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err == nil {
			providers = nested
		}
	}
	if len(providers) == 0 {
		t.Skipf("registry at %s carries no providers", p)
	}

	absent := []string{}
	checked := 0
	for id, want := range providerAPIKeyEnv {
		raw, ok := providers[id]
		if !ok {
			absent = append(absent, id)
			continue
		}
		var meta struct {
			Env []string `json:"env"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Errorf("provider %q: %v", id, err)
			continue
		}
		if len(meta.Env) == 0 {
			continue
		}
		found := false
		for _, e := range meta.Env {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("provider %q: the registry reads %v but providerAPIKeyEnv says %q — a wrong name "+
				"is SILENT (opencode ignores an env var it does not know)", id, meta.Env, want)
		}
		checked++
	}
	t.Logf("cross-checked %d provider(s) against %s; %d not in the registry: %s",
		checked, p, len(absent), strings.Join(absent, ", "))
}
