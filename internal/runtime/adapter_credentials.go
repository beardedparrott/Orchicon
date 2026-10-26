package runtime

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	goruntime "runtime"

	"github.com/beardedparrott/orchicon/internal/adapter"
	_ "modernc.org/sqlite"
)

// openCodeAuthPath is the operator's opencode credential store, relative to the
// host home. Every opencode install writes it here, and it is the same file the
// pool fingerprint hashes (hostfp.go) so a rotated key already invalidates a
// warm container.
func openCodeAuthPath(home string) string {
	return underStoreDir(OpenCodeStoreDir(home), "auth.json")
}

// underStoreDir joins a file onto a store dir, and returns "" when the dir is unknown.
//
// THE EMPTY CASE IS LOAD-BEARING: filepath.Join("", "auth.json") is the RELATIVE path
// "auth.json", so a missing store would silently resolve against the PROCESS's working
// directory instead. Every caller treats "" as "no store", which is the truth.
func underStoreDir(dir, name string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

// OpenCodeStoreDir is the directory the OPERATOR's own opencode keeps its credential
// store in — resolved for this host, once, so that every reader agrees about it.
//
// IT IS NOT A CONSTANT, AND THAT WAS A WINDOWS BUG. This used to be
// filepath.Join(home, ".local", "share", "opencode") on every platform: the XDG layout,
// which is what opencode uses on Linux and macOS. On Windows the operator's own install
// puts its store somewhere else entirely, so the resolver read a directory that does not
// exist, delivered NO credentials (and reported nothing undeliverable, because it had
// read nothing to judge), and the serve started with an empty data dir — leaving every
// `opencode-go` model unauthenticated and every turn on it failing with "Model
// unavailable" while the same key worked under the orchicon adapter.
func OpenCodeStoreDir(home string) string {
	return openCodeStoreDir(goruntime.GOOS, home, os.Getenv)
}

// openCodeStoreDir is the platform rule, free of process state so it can be tested for an
// OS this build is not running on.
func openCodeStoreDir(goos, home string, getenv func(string) string) string {
	candidates := openCodeStoreCandidates(goos, home, getenv)
	if len(candidates) == 0 {
		return ""
	}
	// THE MOST RECENTLY WRITTEN STORE WINS. A host can hold more than one (a user who has
	// used opencode under both layouts), and the one with the newest credential file is the
	// one their sign-in just wrote — which is the same reasoning that decides between two
	// provider ids sharing one variable.
	best, bestAt := "", int64(-1)
	for _, dir := range candidates {
		if at, ok := storeWrittenAt(dir); ok && at > bestAt {
			best, bestAt = dir, at
		}
	}
	if best != "" {
		return best
	}
	// NONE EXISTS: return the primary candidate so callers can still REPORT where they
	// looked (the serve logs this path when it warns that a store was never found).
	return candidates[0]
}

// openCodeStoreCandidates is every directory an opencode install may have used for its
// data dir, most conventional first for the OS.
//
// THE WINDOWS ENTRIES ARE MEASURED FROM opencode'S OWN BUNDLE, which carries both
// conventions: an xdg helper (`XDG_DATA_HOME || <home>/.local/share`) and the node
// `env-paths` win32 branch (`%LOCALAPPDATA%\<name>\Data`, falling back to
// `<home>\AppData\Local`). Guessing one of them is what produced the silent miss, so all
// of them are probed and the store that actually exists decides.
func openCodeStoreCandidates(goos, home string, getenv func(string) string) []string {
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		for _, seen := range out {
			if strings.EqualFold(seen, p) {
				return
			}
		}
		out = append(out, p)
	}
	if xdg := getenv("XDG_DATA_HOME"); xdg != "" {
		add(filepath.Join(xdg, "opencode"))
	}
	if home != "" {
		add(filepath.Join(home, ".local", "share", "opencode"))
	}
	if goos == "windows" {
		bases := []string{getenv("LOCALAPPDATA"), getenv("APPDATA")}
		if home != "" {
			bases = append(bases, filepath.Join(home, "AppData", "Local"))
		}
		for _, base := range bases {
			if base == "" {
				continue
			}
			// env-paths puts a Data component on win32; older layouts did not.
			add(filepath.Join(base, "opencode", "Data"))
			add(filepath.Join(base, "opencode"))
		}
	}
	return out
}

// storeWrittenAt reports whether dir holds an opencode credential store, and when its
// newest credential file was written. Either layout counts: the 2.x database, the 1.x
// auth.json, or both.
func storeWrittenAt(dir string) (int64, bool) {
	found, newest := false, int64(-1)
	for _, name := range []string{"opencode.db", "auth.json"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
			found = true
			if at := st.ModTime().UnixMilli(); at > newest {
				newest = at
			}
		}
	}
	return newest, found
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
// Two ids are mapped from Orchicon's own provider registry instead
// (internal/providers builtinSecretNames, which agrees with the models.dev index
// on every id the two share): commandcode and command-code are served by a
// third-party opencode provider PACKAGE rather than by models.dev's provider of
// the same name.
//
// `commandcode` HAS since appeared in models.json, carrying [CMD_API_KEY] — it
// did not when this map was written. The map still does NOT follow it, and that
// is deliberate: the registry entry describes models.dev's own provider for that
// id, while a host that installs the package serves it through
// commandcode-go-opencode-provider, whose src/auth.ts reads COMMANDCODE_API_KEY
// and whose plugin.ts sets that name on the provider. Adopting CMD_API_KEY here
// would hand the package a variable it never reads, which fails exactly like no
// credential at all. The divergence is asserted, not left to prose —
// adapter_credentials_live_test.go's registryExemptProviders carries these ids
// with this reason.
//
// `command-code` is not a models.dev entry at all. `ollama` and `ollama-cloud`
// share OLLAMA_API_KEY deliberately — models.dev keys Ollama Cloud by
// OLLAMA_API_KEY, and the local server needs no token at all.
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
// AdapterCredentialEnv is adapterCredentialEnv for callers outside this package — specifically the HOST
// opencode serve, which needs the same credentials the container path is given.
//
// WHY IT HAS TO EXIST. adapterCredentialEnv already read BOTH stores with the database winning, because
// "auth.json alone is not enough on a 2.x install" — but its only caller was the container spawn, so the HOST
// serve was left seeding auth.json and nothing else. Reported: a dev instance whose opencode-go credential came
// from an auth.json last written in September while the operator had rotated the key six weeks later; every Ask
// turn through the opencode adapter failed with "This API key has expired … Signing in again will not renew this
// key", while the operator's own `opencode` worked, because it reads the SQLite store this function prefers.
//
// MEASURED, and it is what makes env the right delivery channel rather than a guess: with the SAME stale store,
// prompting opencode-go fails (the reported error), and with the resolved key in OPENCODE_API_KEY it succeeds.
// The environment OUTRANKS the stored credential on 2.x, so no store surgery, no re-import and no deletion is
// needed — the serve simply has to be told the credential.
func AdapterCredentialEnv(home string, kinds ...string) (env []string, unusable []string) {
	return adapterCredentialEnv(home, kinds...)
}

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
	keys := map[string]credEntry{}
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
				// NO TIMESTAMP IN auth.json: this file is the OLDER source by construction on a 2.x install (2.x
				// imports it once and never rewrites it), so a database copy of the same provider always wins.
				keys[norm] = credEntry{key: e.Key, updated: 0}
			}
		}
	}
	for id, e := range credentialTableEntries(openCodeDBPath(home)) {
		norm := strings.ToLower(strings.TrimSpace(id))
		if strings.TrimSpace(e.key) == "" {
			continue
		}
		keys[norm] = e
		delete(bad, norm)
	}

	// ONE VALUE PER ENVIRONMENT VARIABLE.
	//
	// opencode's console issues SEPARATE integrations for `opencode` and `opencode-go`, and both map to
	// OPENCODE_API_KEY — so the loop below used to emit that variable TWICE. The child takes the last
	// assignment, and the pairs are sorted as strings, so the winner was decided by the credential's own TEXT:
	// deterministic, arbitrary, and invisible. Measured on the reporting host, where `opencode` still held a key
	// from a September auth.json and `opencode-go` held the key rotated in October, BOTH were emitted and either
	// could win.
	//
	// The most recently WRITTEN credential is the one the operator just refreshed, so it wins: that is what
	// "rotated the key" means. Provider ids break ties so the result never depends on map iteration order.
	type chosen struct {
		id      string
		key     string
		updated int64
	}
	byEnv := map[string]chosen{}
	for id, e := range keys {
		name, known := providerAPIKeyEnv[id]
		if !known {
			// A credential we cannot DELIVER: opencode would be handed a variable it
			// does not read, which fails exactly like no credential at all — so it is
			// reported rather than passed off as handled.
			bad[id] = struct{}{}
			continue
		}
		if cur, ok := byEnv[name]; !ok || e.updated > cur.updated || (e.updated == cur.updated && id < cur.id) {
			byEnv[name] = chosen{id: id, key: e.key, updated: e.updated}
		}
	}
	for name, c := range byEnv {
		env = append(env, name+"="+c.key)
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
	return underStoreDir(OpenCodeStoreDir(home), "opencode.db")
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
	full := credentialTableEntries(dbPath)
	out := make(map[string]string, len(full))
	for id, e := range full {
		out[id] = e.key
	}
	return out
}

// credEntry is one stored credential and WHEN it was last written, which is how two provider ids that share an
// environment variable are resolved to one value (see adapterCredentialEnv's dedupe).
type credEntry struct {
	key     string
	updated int64 // unix millis; 0 when the source carries no timestamp (auth.json)
}

// credentialTableEntries reads the 2.x store as id -> (key, last-updated).
//
// The timestamp is the deciding fact for a dedupe that would otherwise be arbitrary: opencode's console issues
// SEPARATE integrations for `opencode` and `opencode-go`, and BOTH map to OPENCODE_API_KEY. When an operator
// rotates one of them, the environment can carry only one value, and the most recently written credential is
// the one they just refreshed. Without this, the winner was decided by the credential's own TEXT (the pairs were
// sorted as strings and the child takes the last assignment) — deterministic, but arbitrary and invisible.
func credentialTableEntries(dbPath string) map[string]credEntry {
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil
	}
	defer db.Close()

	// ONE QUERY, WITH A FALLBACK — not a Columns() probe. The probe version this replaces asked the driver for
	// column names, decided the timestamp was absent, and returned NOTHING, which silently dropped the whole
	// database half of the merge: the resolver fell back to auth.json alone and handed out exactly the stale
	// credential this function exists to correct. A read that fails here now degrades to "no timestamps" rather
	// than to "no credentials".
	if out, ok := readCredentialRows(db, `SELECT integration_id, value, time_updated FROM credential`); ok {
		return out
	}
	return readCredentialRowsTwoColumn(db)
}

// readCredentialRows runs q (three columns: id, value, updated) and returns id -> credEntry.
// ok is false when the query itself failed, so the caller can try the older shape.
func readCredentialRows(db *sql.DB, q string) (map[string]credEntry, bool) {
	rows, err := db.Query(q)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := map[string]credEntry{}
	for rows.Next() {
		var id, raw string
		var updated int64
		if err := rows.Scan(&id, &raw, &updated); err != nil {
			continue
		}
		if e, ok := parseCredentialValue(raw); ok {
			e.updated = updated
			out[id] = e
		}
	}
	return out, true
}

// readCredentialRowsTwoColumn is the shape without a timestamp (an older or unexpected schema). Credentials are
// still delivered; only the dedupe ordering loses its tie-break.
func readCredentialRowsTwoColumn(db *sql.DB) map[string]credEntry {
	out := map[string]credEntry{}
	rows, err := db.Query(`SELECT integration_id, value FROM credential`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			continue
		}
		if e, ok := parseCredentialValue(raw); ok {
			out[id] = e
		}
	}
	return out
}

// parseCredentialValue decodes one stored credential's JSON ({type, key}). ok is false when the value is not
// that shape or carries no key.
func parseCredentialValue(raw string) (credEntry, bool) {
	var v struct {
		Key string `json:"key"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil || strings.TrimSpace(v.Key) == "" {
		return credEntry{}, false
	}
	return credEntry{key: v.Key}, true
}
