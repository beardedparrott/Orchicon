package providers

// secret_sharing_test.go — ONE SECRET, SEVERAL PROVIDERS.
//
// THE BUG THIS PINS, measured on the dev instance:
//
//	18:35:04Z  audit: provider.token_set  target=opencode        (the operator saved the new key)
//	18:36:14Z  log:   ask orchicon session error "This API key has expired…"
//
// The save had WORKED. The session was running model_ref
// `orchicon/opencode-go/deepseek-v4.1-flash` — and opencode-go is a DIFFERENT
// provider id that authenticates with the SAME OPENCODE_API_KEY (distinct base
// URLs, one credential — ADR D4). SetProviderToken invalidated only the provider
// the operator clicked, so opencode-go's cached client kept the old, expired key
// and every turn 401'd while the store held the new one.
//
// The mapping is asserted here as a pure function, so it cannot drift from the
// invalidation that depends on it.

import (
	"os"
	"strings"
	"testing"
)

func TestBuiltinProvidersSharingSecretCoversSiblings(t *testing.T) {
	got := builtinProvidersSharingSecret("OPENCODE_API_KEY")
	want := map[string]bool{"opencode": true, "opencode-go": true}

	if len(got) != len(want) {
		t.Fatalf("OPENCODE_API_KEY is read by %v, want exactly %d providers (opencode and "+
			"opencode-go). Invalidating a subset leaves the other one authenticating with the "+
			"rotated key's predecessor.", got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected provider %q shares OPENCODE_API_KEY", id)
		}
	}

	// ONLY the opencode family is shared: a provider with its own key must not be
	// swept into another's invalidation.
	if ids := builtinProvidersSharingSecret("ANTHROPIC_API_KEY"); len(ids) != 1 || ids[0] != "anthropic" {
		t.Errorf("ANTHROPIC_API_KEY maps to %v, want exactly [anthropic]", ids)
	}
	if ids := builtinProvidersSharingSecret("CUSTOM_NOT_A_BUILTIN_API_KEY"); len(ids) != 0 {
		t.Errorf("an unknown secret name mapped to %v, want none", ids)
	}
	if ids := builtinProvidersSharingSecret("  "); len(ids) != 0 {
		t.Errorf("a blank name mapped to %v, want none", ids)
	}
}

// TestEveryBuiltinSecretNameResolvesBackToItsProvider is the reverse direction:
// a name in the table that resolves to nothing would mean a provider whose
// credential write invalidates no cache at all.
func TestEveryBuiltinSecretNameResolvesBackToItsProvider(t *testing.T) {
	for id, name := range builtinSecretNames {
		found := false
		for _, candidate := range builtinProvidersSharingSecret(name) {
			if candidate == id {
				found = true
			}
		}
		if !found {
			t.Errorf("provider %q uses secret %q, but that name does not resolve back to it — a "+
				"credential write for %q would invalidate nothing", id, name, id)
		}
	}
}

// TestTokenWritesInvalidateSiblings — the CALL SITE, not just the mapping.
//
// The mapping test above passes as long as builtinProvidersSharingSecret is
// correct, even if SetProviderToken stops calling it — which is exactly the bug
// (it called s.invalidate(tenantID, providerID): one id). Asserted at the source,
// the repo's idiom for wiring contracts, because the effect is otherwise
// invisible: the registry cache is unexported and its staleness only shows up as
// a 401 on the OTHER provider.
func TestTokenWritesInvalidateSiblings(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	text := string(src)

	for _, fn := range []string{
		"func (s *Service) SetProviderToken",
		"func (s *Service) ClearProviderToken",
	} {
		start := strings.Index(text, fn)
		if start < 0 {
			t.Fatalf("%s not found", fn)
		}
		end := strings.Index(text[start+1:], "\nfunc ")
		if end < 0 {
			end = len(text) - start - 1
		}
		body := text[start : start+1+end]

		if !strings.Contains(body, "s.InvalidateSecret(") {
			t.Errorf("%s does not call InvalidateSecret.\n"+
				"Invalidating only the provider the operator clicked leaves a SIBLING that shares the "+
				"same credential (opencode / opencode-go both read OPENCODE_API_KEY) authenticating "+
				"with the replaced key — measured on dev: token_set succeeded at 18:35:04Z and the "+
				"opencode-go session still returned api_key_expired at 18:36:14Z.", fn)
		}
		if strings.Contains(body, "s.invalidate(tenantID, providerID)") {
			t.Errorf("%s still invalidates ONLY providerID — that is the single-provider bug", fn)
		}
	}
}
