package runtime

// adapter_credentials_dedupe_test.go — two provider ids, ONE environment variable, and the right one wins.
//
// opencode's console issues SEPARATE integrations for `opencode` and `opencode-go`, and BOTH map to
// OPENCODE_API_KEY. The emitter used to append one pair per provider id, so that variable appeared TWICE. The
// child takes the last assignment and the pairs are sorted as strings, so the winner was decided by the
// credential's own TEXT — deterministic, arbitrary, and invisible.
//
// MEASURED on the reporting host: `opencode` still held a key from a September auth.json (sha 66b335081f) and
// `opencode-go` held the key rotated in October (sha 8f28a2cf64). Both were emitted, and the serve could run on
// the expired one — which is exactly the reported "This API key has expired" failure.
//
// The most recently WRITTEN credential is the one the operator just refreshed, so it wins.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStores fabricates an operator home: an auth.json (the older, 1.x-era source) and a 2.x credential
// database. Credential values are opaque markers so an assertion can name the winner.
func writeStores(t *testing.T, authKeys map[string]string, dbKeys map[string]credEntry) string {
	t.Helper()
	home := t.TempDir()
	share := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if authKeys != nil {
		entries := map[string]map[string]string{}
		for id, k := range authKeys {
			entries[id] = map[string]string{"type": "api", "key": k}
		}
		b, _ := json.Marshal(entries)
		if err := os.WriteFile(filepath.Join(share, "auth.json"), b, 0o600); err != nil {
			t.Fatalf("write auth.json: %v", err)
		}
	}
	if dbKeys != nil {
		dbPath := filepath.Join(share, "opencode.db")
		db, err := sql.Open("sqlite", "file:"+dbPath)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TABLE credential (integration_id TEXT, value TEXT, time_updated INTEGER)`); err != nil {
			t.Fatalf("create table: %v", err)
		}
		for id, e := range dbKeys {
			v, _ := json.Marshal(map[string]string{"type": "key", "key": e.key})
			if _, err := db.Exec(`INSERT INTO credential VALUES (?,?,?)`, id, string(v), e.updated); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	return home
}

// envValue returns the single value emitted for name, and fails if it was emitted more than once.
func singleEnvValue(t *testing.T, env []string, name string) string {
	t.Helper()
	var got []string
	for _, p := range env {
		if k, v, ok := strings.Cut(p, "="); ok && k == name {
			got = append(got, v)
		}
	}
	if len(got) > 1 {
		t.Fatalf("%s emitted %d times (%v) — with a duplicate, the winner is decided by the value's own text "+
			"and the serve may run on the expired credential", name, len(got), got)
	}
	if len(got) == 0 {
		return ""
	}
	return got[0]
}

// TestTheMostRecentlyRotatedCredentialWins is the reported case: a stale `opencode` and a fresh `opencode-go`.
func TestTheMostRecentlyRotatedCredentialWins(t *testing.T) {
	home := writeStores(t,
		// auth.json (September-era): the stale key for BOTH ids.
		map[string]string{"opencode": "stale-from-authjson", "opencode-go": "stale-from-authjson"},
		map[string]credEntry{
			// The 2.x store: `opencode` last written Oct 12, `opencode-go` rotated Oct 24.
			"opencode":    {key: "old-db-key", updated: 1791834000000}, // 2026-10-12
			"opencode-go": {key: "FRESH-ROTATED", updated: 1792862340000}, // 2026-10-24
		})

	pairs, _ := AdapterCredentialEnv(home, "opencode")
	if got := singleEnvValue(t, pairs, "OPENCODE_API_KEY"); got != "FRESH-ROTATED" {
		t.Fatalf("OPENCODE_API_KEY = %q, want the most recently written credential (FRESH-ROTATED). Anything "+
			"else hands the serve a key the operator has already replaced — the reported expired-key failure", got)
	}
}

// TestTheDatabaseSupersedesAuthJSON: on 2.x auth.json is a fossil, so even the OLDEST database row beats it.
func TestTheDatabaseSupersedesAuthJSON(t *testing.T) {
	home := writeStores(t,
		map[string]string{"opencode-go": "stale-from-authjson"},
		map[string]credEntry{"opencode-go": {key: "from-db", updated: 1}}) // ancient, but still newer than auth.json's 0

	pairs, _ := AdapterCredentialEnv(home, "opencode")
	if got := singleEnvValue(t, pairs, "OPENCODE_API_KEY"); got != "from-db" {
		t.Errorf("OPENCODE_API_KEY = %q, want the database value: auth.json is only imported once by 2.x and "+
			"never updated, so it is the older source by construction", got)
	}
}

// TestOnlyAuthJSONStillWorks: a 1.x install has no database at all.
func TestOnlyAuthJSONStillWorks(t *testing.T) {
	home := writeStores(t, map[string]string{"opencode-go": "only-authjson"}, nil)
	pairs, _ := AdapterCredentialEnv(home, "opencode")
	if got := singleEnvValue(t, pairs, "OPENCODE_API_KEY"); got != "only-authjson" {
		t.Errorf("OPENCODE_API_KEY = %q, want the auth.json value for a 1.x install", got)
	}
}

// TestTiesBreakDeterministically: equal timestamps must not depend on map iteration order.
func TestTiesBreakDeterministically(t *testing.T) {
	for i := 0; i < 8; i++ { // map order varies run to run; repeat to catch it
		home := writeStores(t, nil, map[string]credEntry{
			"opencode":    {key: "from-opencode", updated: 500},
			"opencode-go": {key: "from-opencode-go", updated: 500},
		})
		pairs, _ := AdapterCredentialEnv(home, "opencode")
		got := singleEnvValue(t, pairs, "OPENCODE_API_KEY")
		if got == "" {
			t.Fatal("no OPENCODE_API_KEY emitted")
		}
		if i > 0 && got != firstTieWinner {
			t.Fatalf("tie-break is not deterministic: %q then %q", firstTieWinner, got)
		}
		firstTieWinner = got
	}
}

var firstTieWinner string
