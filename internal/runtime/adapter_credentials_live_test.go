package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// registryExemptProviders are ids this map deliberately does NOT take from
// models.dev's index, so a registry disagreement on them is expected rather than
// drift. Each carries the reason it is exempt.
//
// THE DISTINCTION IS THE WHOLE POINT, and it is why the check is an exemption
// rather than a loosened comparison. `commandcode` NOW appears in models.json
// carrying env [CMD_API_KEY] — it did not when providerAPIKeyEnv was written.
// But the registry entry describes models.dev's own provider for that id, while
// this host serves it through a THIRD-PARTY opencode provider package
// (commandcode-go-opencode-provider), which reads COMMANDCODE_API_KEY in its own
// src/auth.ts and sets that same name in plugin.ts. Following the registry here
// would hand the package a variable it never reads — precisely the SILENT
// failure this test exists to catch — so the map stays as it is and the
// exemption is stated out loud instead of being papered over.
var registryExemptProviders = map[string]string{
	"commandcode": "served by the commandcode-go-opencode-provider package, whose own src/auth.ts reads COMMANDCODE_API_KEY",
	"command-code": "Orchicon-store duplicate of commandcode; not a models.dev entry at all",
}

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
// third-party provider packages rather than models.dev entries. That intent is
// ENFORCED by registryExemptProviders below, not merely described here: an id
// whose registry entry must not be followed is exempted by name and reason.
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

// TestRegistryExemptProvidersAreReal guards the exemption list itself, so the
// list cannot rot into a blanket permission.
//
// AN EXEMPTION FOR AN ID THIS MAP DOES NOT CARRY IS STALE, and it is not merely
// untidy: it would sit on file and silently exempt a FUTURE entry of the same
// name, which is how an exemption stops being a deliberate decision and becomes
// a hole. Asserting the list is a subset of the map is what keeps every
// exemption accountable to a real provider.
func TestRegistryExemptProvidersAreReal(t *testing.T) {
	for id := range registryExemptProviders {
		if _, ok := providerAPIKeyEnv[id]; !ok {
			t.Errorf("registryExemptProviders carries %q, which providerAPIKeyEnv does not — "+
				"the exemption is stale and exempts nothing", id)
		}
	}
}
