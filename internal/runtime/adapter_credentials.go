package runtime

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
	_ "modernc.org/sqlite"
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
	// The hyphenated spelling appears in the operator store as a DUPLICATE of
	// `commandcode` (same value, both ids present) and is not a models.dev entry
	// either — mapped to the same variable so the duplicate is not reported as an
	// undeliverable provider every run.
	"command-code": "COMMANDCODE_API_KEY",
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
	// BOTH STORES, MERGED WITH THE DATABASE WINNING — see openCodeDBPath for why
	// auth.json alone is not enough on a 2.x install.
	keys := map[string]string{}
	bad := map[string]struct{}{}
	if b, err := os.ReadFile(openCodeAuthPath(home)); err == nil {
		var entries map[string]openCodeAuthEntry
		if json.Unmarshal(b, &entries) == nil {
			for id, e := range entries {
				norm := strings.ToLower(strings.TrimSpace(id))
				if !strings.EqualFold(strings.TrimSpace(e.Type), "api") || strings.TrimSpace(e.Key) == "" {
					bad[norm] = struct{}{}
					continue
				}
				keys[norm] = e.Key
			}
		}
	}
	for id, key := range credentialTableKeys(openCodeDBPath(home)) {
		norm := strings.ToLower(strings.TrimSpace(id))
		if strings.TrimSpace(key) == "" {
			continue
		}
		keys[norm] = key
		delete(bad, norm)
	}
	for id, key := range keys {
		name, known := providerAPIKeyEnv[id]
		if !known {
			// A credential we cannot DELIVER: opencode would be handed a variable it
			// does not read, which fails exactly like no credential at all — so it is
			// reported rather than passed off as handled.
			bad[id] = struct{}{}
			continue
		}
		env = append(env, name+"="+key)
	}
	// `bad` is exactly the right set by now: an auth.json entry that was unusable
	// (removed above when the database superseded it), plus any id carrying a
	// credential with no environment variable to put it in.
	for id := range bad {
		unusable = append(unusable, id)
	}
	sort.Strings(env)
	sort.Strings(unusable)
	return env, unusable
}

// openCodeDBPath is opencode 2.x's own credential store: a SQLite database, whose
// `credential` table holds the integrations the operator signed in to.
//
// WHY IT IS READ AT ALL, GIVEN auth.json ALREADY IS. On 1.x auth.json IS the
// credential store. 2.x imports it ONCE (a migration) and keeps credentials in
// this database thereafter — so on a 2.x install auth.json is a STALE COPY of
// whatever was imported, and any provider signed in or rotated AFTER the upgrade
// differs between the two. Measured on the development host, where one provider
// had been re-authenticated under 2.x:
//
//	ollama-cloud   auth.json sha8=d354ab9e  !=  db sha8=b66edf7e   (DIFFERENT)
//	openrouter     auth.json sha8=98cd644b  ==  db sha8=98cd644b   (identical)
//	deepseek       auth.json sha8=7ad66b9e  ==  db sha8=7ad66b9e   (identical)
//
// Reading only auth.json therefore hands a worker a DEAD CREDENTIAL for exactly
// the providers the operator most recently configured — and it fails at the model
// call as an Unauthorized, far from the cause. Hence: read both, and let the
// database win, because where the two disagree the database is the live store.
func openCodeDBPath(home string) string {
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

// credentialTableKeys reads the 2.x credential store as integration id -> secret.
//
// BEST-EFFORT BY CONTRACT: every failure (no database, an unexpected schema, a
// locked or mid-write file, an unparseable value) returns what it has and no
// error, because the caller has a working fallback in auth.json and container
// creation must never fail over credentials it can read another way.
//
// Opened read-only and WITHOUT `immutable`, which matters: this is a live
// database with a write-ahead log, so `immutable` would let it read a torn state
// and a plain open could take a write lock on someone else's store.
func credentialTableKeys(dbPath string) map[string]string {
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT integration_id, value FROM credential`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			continue
		}
		var v struct {
			Key string `json:"key"`
		}
		if json.Unmarshal([]byte(raw), &v) != nil || strings.TrimSpace(v.Key) == "" {
			continue
		}
		out[id] = v.Key
	}
	return out
}
