package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// registryExemptProviders are ids this map deliberately does NOT take from models.dev's index, so a
// registry disagreement on them is expected rather than drift.
var registryExemptProviders = map[string]string{
	"commandcode": "served by the commandcode-go-opencode-provider package, whose own src/auth.ts reads COMMANDCODE_API_KEY",
	"command-code": "Orchicon-store duplicate of commandcode; not a models.dev entry at all",
}

// TestProviderAPIKeyEnvMatchesOpenCodeRegistry is the DRIFT DETECTOR for providerAPIKeyEnv.
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
	exempted := []string{}
	checked := 0
	for id, want := range providerAPIKeyEnv {
		if _, exempt := registryExemptProviders[id]; exempt {
			exempted = append(exempted, id)
			continue
		}
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
	t.Logf("cross-checked %d provider(s) against %s; %d not in the registry: %s; %d exempted by design: %s",
		checked, p, len(absent), strings.Join(absent, ", "), len(exempted), strings.Join(exempted, ", "))
}

// TestRegistryExemptProvidersAreReal guards the exemption list itself, so the list cannot rot into
// a blanket permission.
func TestRegistryExemptProvidersAreReal(t *testing.T) {
	for id := range registryExemptProviders {
		if _, ok := providerAPIKeyEnv[id]; !ok {
			t.Errorf("registryExemptProviders carries %q, which providerAPIKeyEnv does not — "+
				"the exemption is stale and exempts nothing", id)
		}
	}
}
