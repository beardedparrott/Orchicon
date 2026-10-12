package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// openCodeAuthPath is the operator's opencode credential store, relative to the
// host home. Every opencode install writes it here, and it is the same file the
// pool fingerprint hashes (hostfp.go) so a rotated key already invalidates a
// warm container.
func openCodeAuthPath(home string) string {
	return filepath.Join(home, ".local", "share", "opencode", "auth.json")
}

// providerAPIKeyEnv maps an opencode PROVIDER ID to the environment variable
// that provider's credential is read from.
//
// THE SOURCE IS opencode's OWN PROVIDER REGISTRY — its cached models.dev index
// (models.json), where every provider carries the `env` list it is keyed by.
// That is the authority, and it is used rather than guessed because a WRONG name
// fails SILENTLY: opencode ignores an environment variable it does not know, so
// a typo here would look exactly like a credential the operator never set.
// adapter_credentials_live_test.go re-checks every entry below against the
// registry on any host that has it.
//
// Two ids the registry does not carry are mapped from Orchicon's own provider
// registry instead (internal/providers builtinSecretNames, which agrees with the
// models.dev index on every id the two share): commandcode and command-code are
// third-party provider PACKAGES rather than registry entries. `ollama` and
// `ollama-cloud` share OLLAMA_API_KEY deliberately — models.dev keys Ollama
// Cloud by OLLAMA_API_KEY, and the local server needs no token at all.
var providerAPIKeyEnv = map[string]string{
	"anthropic":    "ANTHROPIC_API_KEY",
	"openai":       "OPENAI_API_KEY",
	"openrouter":   "OPENROUTER_API_KEY",
	"deepseek":     "DEEPSEEK_API_KEY",
	"ollama":       "OLLAMA_API_KEY",
	"ollama-cloud": "OLLAMA_API_KEY",
	"opencode":     "OPENCODE_API_KEY",
	"opencode-go":  "OPENCODE_API_KEY",
	"commandcode":  "COMMANDCODE_API_KEY",
}

// openCodeAuthEntry is one credential in the operator's store. Only the api-key
// shape can be delivered through the environment; an oauth entry holds access
// and refresh tokens with their own lifetime, so translating it into a static
// variable would hand the CLI a value that expires — it is reported instead.
type openCodeAuthEntry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// adapterCredentialEnv returns the KEY=VALUE pairs that carry the operator's
// provider credentials into a demanded adapter's container environment, plus
// the provider ids that could NOT be delivered this way (so the caller can say
// so out loud rather than leave a silent gap).
//
// WHY THE ENVIRONMENT, AND WHY ONLY FOR THE DEFAULT ADAPTER. opencode 2.x does
// NOT read auth.json: it keeps credentials in its SQLite store and imports
// auth.json only once, while migrating a legacy database. A runtime container
// has a FRESH data home by construction (the serve's private XDG directory), so
// on 2.x it would have no credentials at all. Measured against the real
// binaries:
//
//	1.18.34, auth.json present   -> 6 integrations,  549 models
//	2.0.26,  auth.json present   -> "No authenticated integrations",  23 models
//	2.0.26,  two keys in the env -> 404 models
//	1.18.34, NO auth.json at all -> 496 models, from the env alone
//
// So the environment is the ONE channel that authenticates BOTH versions and
// needs no version branch — which is the whole reason this is env vars and not
// a seeded credential file. It is also the channel the daemon already uses for
// run secrets (req.Secrets) and for GH_TOKEN, and the daemon never logs its
// argv, so a value passed here reaches exactly one place.
//
// Returns (nil, nil) when no opencode demand, no store, or an unreadable store:
// a host with no opencode credentials is not an error, it is the native-only
// case. It never guesses an env name — an unmapped provider is REPORTED.
func adapterCredentialEnv(home string, kinds ...string) (env []string, unusable []string) {
	if home == "" {
		return nil, nil
	}
	demanded := false
	for _, k := range kinds {
		if normalizedKind(k) == adapter.DefaultAdapterKind {
			demanded = true
			break
		}
	}
	if !demanded {
		return nil, nil
	}
	b, err := os.ReadFile(openCodeAuthPath(home))
	if err != nil {
		return nil, nil
	}
	var entries map[string]openCodeAuthEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, nil
	}
	for id, e := range entries {
		name, known := providerAPIKeyEnv[strings.ToLower(strings.TrimSpace(id))]
		if !known || !strings.EqualFold(strings.TrimSpace(e.Type), "api") || strings.TrimSpace(e.Key) == "" {
			unusable = append(unusable, id)
			continue
		}
		env = append(env, name+"="+e.Key)
	}
	// SORTED so the emitted argv is deterministic: the pool key and any test
	// asserting the create arguments must not depend on Go's map order.
	sort.Strings(env)
	sort.Strings(unusable)
	return env, unusable
}
