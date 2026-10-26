package opencode

// servehost_credentials_store_test.go — the END-TO-END guard for the expired-key report.
//
// Every previous test in this package replaced credentialResolver with a stub, so they pinned that the
// resolver is CALLED and with roughly what — never that the real one, given the real paths, finds the
// operator's real store. That is precisely the gap the bug lived in: the resolver was called with the
// opencode DATA DIRECTORY instead of HOME, so it composed
//
//	<data>/.local/share/opencode/opencode.db
//
// which cannot exist, read nothing, and delivered nothing — silently, because an empty store yields no
// `unusable` ids to warn about either. A stub cannot fail that way, so no stubbed test could see it.
//
// REPORTED: "When using the opencode adapter and then using opencode-go as the provider, I get … This API
// key has expired … That same API key works just fine using the orchicon adapter."
//
// The store below is the reporting host's, in miniature: a STALE `opencode` integration and a FRESH
// `opencode-go` one, both of which map to OPENCODE_API_KEY. Measured on that host: `opencode` answers 401
// api_key_expired, `opencode-go` answers 200.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// writeOpencodeStore builds a 2.x credential store under home, as opencode lays it out.
func writeOpencodeStore(t *testing.T, home string, rows map[string]string, updated map[string]int64) {
	t.Helper()
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE credential (id text primary key, integration_id text, label text, value text, time_updated integer)`); err != nil {
		t.Fatal(err)
	}
	for id, key := range rows {
		if _, err := db.Exec(
			`INSERT INTO credential (id, integration_id, label, value, time_updated) VALUES (?,?,?,?,?)`,
			id, id, "API key", `{"type":"api","key":"`+key+`"}`, updated[id]); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheHostServeDeliversTheFRESHCredentialFromARealStore is the reported incident end to end.
func TestTheHostServeDeliversTheFRESHCredentialFromARealStore(t *testing.T) {
	home := t.TempDir()
	// The reporting host, in miniature: `opencode` was last refreshed on Oct 12 and has since expired;
	// `opencode-go` was rotated on Oct 26 and works. Both map to the SAME environment variable.
	writeOpencodeStore(t, home,
		map[string]string{"opencode": "STALE-EXPIRED-KEY", "opencode-go": "FRESH-WORKING-KEY"},
		map[string]int64{"opencode": 1760300000, "opencode-go": 1793030000},
	)

	h := NewHostServe(nil, t.TempDir(), home)
	pairs, unusable := h.credentialEnv()

	if len(pairs) == 0 {
		t.Fatalf("the host serve got NO credentials at all for a store that has two — this is the reported "+
			"failure exactly: the serve falls back to opencode's own stale key and every turn dies with "+
			"api_key_expired. unusable=%v", unusable)
	}
	joined := strings.Join(pairs, " ")
	if strings.Contains(joined, "STALE-EXPIRED-KEY") {
		t.Fatalf("delivered the EXPIRED key: %v\n"+
			"  The most-recently-written credential is the one the operator rotated — that is what "+
			"\"rotated the key\" means, and it is the rule this resolver exists to apply.", pairs)
	}
	if !strings.Contains(joined, "OPENCODE_API_KEY=FRESH-WORKING-KEY") {
		t.Fatalf("OPENCODE_API_KEY is missing or wrong: %v", pairs)
	}
	// ONE assignment per variable. Both integrations map to OPENCODE_API_KEY, so a loop that emitted both
	// would leave the winner to the child's last-assignment rule — decided by nothing the operator can see.
	n := 0
	for _, p := range pairs {
		if strings.HasPrefix(p, "OPENCODE_API_KEY=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("OPENCODE_API_KEY emitted %d times — duplicate assignments are settled by string order, "+
			"so which key wins becomes arbitrary and invisible: %v", n, pairs)
	}
}

// TestTheHostServeFindsTheStoreFromHomeNotFromTheDataDir is the one-line cause, pinned directly.
//
// The resolver takes HOME and appends `.local/share/opencode`. Passing the data dir instead doubles the
// suffix, so the store is never found. This asserts the real resolver against a real store, which is the
// only shape of test that can tell the two apart.
func TestTheHostServeFindsTheStoreFromHomeNotFromTheDataDir(t *testing.T) {
	home := t.TempDir()
	writeOpencodeStore(t, home, map[string]string{"deepseek": "DS-KEY"}, map[string]int64{"deepseek": 1})

	fromHome := NewHostServe(nil, t.TempDir(), home)
	if pairs, _ := fromHome.credentialEnv(); !strings.Contains(strings.Join(pairs, " "), "DEEPSEEK_API_KEY=DS-KEY") {
		t.Fatalf("resolving from HOME found nothing (%v) — the store is at "+
			"$HOME/.local/share/opencode/opencode.db and the resolver appends that suffix itself", pairs)
	}

	// The wrong argument, to show the failure is SILENT rather than merely different: pointed at the data
	// dir, the same store yields nothing and reports nothing.
	dataDir := filepath.Join(home, ".local", "share", "opencode")
	if pairs, unusable := credentialEnvFor(dataDir); len(pairs) != 0 {
		t.Fatalf("pointing the resolver at the DATA dir produced %v; the whole bug was that it produces "+
			"NOTHING, quietly", pairs)
	} else if len(unusable) != 0 {
		t.Fatalf("and it must report nothing as undeliverable (%v) — an empty store yields no bad ids, "+
			"which is why the plane logged no warning at all", unusable)
	}
}

// credentialEnvFor calls the same resolver the host serve calls, with an explicit base dir — so the test
// exercises the production resolver rather than a stub.
func credentialEnvFor(base string) ([]string, []string) {
	return credentialResolver(base, "opencode")
}

// TestAnEmptyResolutionAgainstAPresentStoreIsLoud pins the diagnosability half of the incident.
//
// The reported failure was SILENT: the resolver read a doubled path, found nothing, and reported nothing
// undeliverable — because there was nothing to judge. From the outside that is indistinguishable from a host
// that never signed in to opencode, so the only symptom was an expired-key error at the model call and no line
// anywhere naming credentials. The operator found the cause, not the logs.
//
// The decision is a pure function so the three cases stay distinguishable: "nothing to deliver" must stay
// quiet, "there IS a store and we resolved nothing from it" must be loud, and a partial delivery is already
// covered by its own warning.
func TestAnEmptyResolutionAgainstAPresentStoreIsLoud(t *testing.T) {
	cases := []struct {
		name         string
		pairs        int
		unusable     int
		storePresent bool
		wantReport   bool
		because      string
	}{
		{"no opencode at all", 0, 0, false, false,
			"a native-only host has no store and nothing to deliver — warning here would train the reader to ignore the line"},
		{"store present, nothing resolved", 0, 0, true, true,
			"this is the reported fingerprint: a path bug reads nothing and reports nothing"},
		{"delivered", 3, 0, true, false,
			"a normal delivery is not a gap"},
		{"undeliverable only", 0, 2, true, false,
			"a signed-in provider with no env var has its own warning, which names the providers"},
	}
	for _, c := range cases {
		if got := credentialGapIsReportable(c.pairs, c.unusable, c.storePresent); got != c.wantReport {
			t.Errorf("%s: reportable = %v, want %v — %s", c.name, got, c.wantReport, c.because)
		}
	}
}

// TestTheStoreProbeSeesBothShapes — the "is there something to find" probe must not itself be able to miss a
// store, or the loud line above would be silent in exactly the case it exists for.
func TestTheStoreProbeSeesBothShapes(t *testing.T) {
	// 2.x: SQLite only.
	sqliteHome := t.TempDir()
	writeOpencodeStore(t, sqliteHome, map[string]string{"deepseek": "K"}, map[string]int64{"deepseek": 1})
	if !NewHostServe(nil, t.TempDir(), sqliteHome).operatorHasCredentialStore() {
		t.Error("a 2.x SQLite store was not seen — the gap warning could never fire on the version that " +
			"actually reported this")
	}

	// 1.x: auth.json only.
	fileHome := t.TempDir()
	dir := filepath.Join(fileHome, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"deepseek":{"type":"api","key":"K"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !NewHostServe(nil, t.TempDir(), fileHome).operatorHasCredentialStore() {
		t.Error("a 1.x auth.json store was not seen")
	}

	// Nothing installed.
	if NewHostServe(nil, t.TempDir(), t.TempDir()).operatorHasCredentialStore() {
		t.Error("an empty home reported a store — the warning would fire on every native-only plane")
	}
}
