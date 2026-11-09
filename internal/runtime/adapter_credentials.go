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

// openCodeAuthPath is the operator's opencode credential store, relative to the host home.
func openCodeAuthPath(home string) string {
	return underStoreDir(OpenCodeStoreDir(home), "auth.json")
}

// underStoreDir joins a file onto a store dir, and returns "" when the dir is unknown.
func underStoreDir(dir, name string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

// OpenCodeStoreDir is the directory the OPERATOR's own opencode keeps its credential store in —
// resolved for this host, once, so that every reader agrees about it.
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
	// THE MOST RECENTLY WRITTEN STORE WINS.
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

// openCodeStoreCandidates is every directory an opencode install may have used for its data dir,
// most conventional first for the OS.
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

// storeWrittenAt reports whether dir holds an opencode credential store, and when its newest
// credential file was written.
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

// providerAPIKeyEnv maps an opencode PROVIDER ID to the environment variable that provider's
// credential is read from.
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
	// The hyphenated spelling appears in the operator store as a DUPLICATE of `commandcode` (same
	// value, both ids present) and is not a models.dev entry either.
	"command-code": "COMMANDCODE_API_KEY",
}

// openCodeAuthEntry is one credential in the operator's store.
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
	type chosen struct {
		id      string
		key     string
		updated int64
	}
	byEnv := map[string]chosen{}
	for id, e := range keys {
		name, known := providerAPIKeyEnv[id]
		if !known {
			// A credential we cannot DELIVER: opencode would be handed a variable it does not read, which fails
			// exactly like no credential at all — so it is reported rather than passed off as handled.
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
	// `bad` is exactly the right set by now: an auth.json entry that was unusable (removed above when
	// the database superseded it).
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
func credentialTableEntries(dbPath string) map[string]credEntry {
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil
	}
	defer db.Close()

	// ONE QUERY, WITH A FALLBACK — not a Columns() probe.
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
