package runtime

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// writeCredentialDB creates a minimal opencode 2.x credential store.
// credDBPathFor resolves the store path for a test's OWN temp home, and REFUSES to hand back a path
// that is not inside it.
//
// THE GUARD IS THE POINT. This file once called credDBPathFor(t, home) and got the AMBIENT store back
// whenever its `home` candidate was not the most recently written one — so CREATE TABLE and, in the
// "corrupt database" case, os.WriteFile("this is not a sqlite database") landed on the operator's live
// 39 MB database. Every opencode serve has failed since with "database disk image is malformed".
// A test that writes to a path may not trust a resolver that consults the environment.
func credDBPathFor(t *testing.T, home string) string {
	t.Helper()
	p := openCodeDBPathForTest(home)
	if !storePathUnderStateRoot(p, home) {
		t.Fatalf("refusing to write to %q: it is not inside this test's home %q. A resolver that consults "+
			"the ambient environment must never be used for a path a test will write to.", p, home)
	}
	return p
}

// openCodeDBPathForTest resolves the store under EXACTLY `home`, ignoring XDG_DATA_HOME and friends.
func openCodeDBPathForTest(home string) string { return openCodeDBPathForHome(home) }

func writeCredentialDB(t *testing.T, path string, rows map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE credential (id text primary key, integration_id text, label text, value text)`); err != nil {
		t.Fatal(err)
	}
	for id, key := range rows {
		if _, err := db.Exec(`INSERT INTO credential (id, integration_id, label, value) VALUES (?,?,?,?)`,
			id, id, "API key", `{"type":"api","key":"`+key+`"}`); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCredentialDBOverridesStaleAuthJSON is the regression guard for a bug that shipped: reading
// ONLY auth.json hands a worker a DEAD credential.
func TestCredentialDBOverridesStaleAuthJSON(t *testing.T) {
	home := t.TempDir()
	writeAuthStore(t, home, `{"ollama-cloud":{"type":"api","key":"STALE-FROM-AUTH-JSON"}}`)
	writeCredentialDB(t, credDBPathFor(t, home), map[string]string{"ollama-cloud": "CURRENT-FROM-DB"})

	env, _ := adapterCredentialEnv(home, "opencode")
	if !contains(env, "OLLAMA_API_KEY=CURRENT-FROM-DB") {
		t.Errorf("the DATABASE is the live store where the two disagree; got %v", env)
	}
	for _, e := range env {
		if strings.Contains(e, "STALE-FROM-AUTH-JSON") {
			t.Errorf("the stale auth.json value was used: %v", env)
		}
	}
}

// TestCredentialFallbacksAreBestEffort pins the shapes that must still work, and the one that must
// degrade quietly rather than fail container creation:
func TestCredentialFallbacksAreBestEffort(t *testing.T) {
	t.Run("1.x: auth.json only", func(t *testing.T) {
		home := t.TempDir()
		writeAuthStore(t, home, `{"deepseek":{"type":"api","key":"FROM-AUTH-JSON"}}`)
		env, _ := adapterCredentialEnv(home, "opencode")
		if !contains(env, "DEEPSEEK_API_KEY=FROM-AUTH-JSON") {
			t.Errorf("a 1.x install must still resolve from auth.json; got %v", env)
		}
	})

	t.Run("2.x: database present but no credential table", func(t *testing.T) {
		home := t.TempDir()
		writeAuthStore(t, home, `{"deepseek":{"type":"api","key":"FROM-AUTH-JSON"}}`)
		p := credDBPathFor(t, home)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", "file:"+p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE kv (key text primary key, value text)`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		env, _ := adapterCredentialEnv(home, "opencode")
		if !contains(env, "DEEPSEEK_API_KEY=FROM-AUTH-JSON") {
			t.Errorf("a database without the table must fall back to auth.json; got %v", env)
		}
	})

	t.Run("corrupt database", func(t *testing.T) {
		home := t.TempDir()
		writeAuthStore(t, home, `{"deepseek":{"type":"api","key":"FROM-AUTH-JSON"}}`)
		p := credDBPathFor(t, home)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("this is not a sqlite database"), 0o600); err != nil {
			t.Fatal(err)
		}
		env, _ := adapterCredentialEnv(home, "opencode")
		if !contains(env, "DEEPSEEK_API_KEY=FROM-AUTH-JSON") {
			t.Errorf("a corrupt database must degrade to auth.json, not to nothing; got %v", env)
		}
	})
}

// TestCredentialDBIgnoresUnusableRows pins what the database read must NOT pass through: a value
// with no `key`, and a row whose value is not JSON at all.
func TestCredentialDBIgnoresUnusableRows(t *testing.T) {
	home := t.TempDir()
	p := credDBPathFor(t, home)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE credential (id text primary key, integration_id text, label text, value text)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ id, val string }{
		{"no-key", `{"type":"api"}`},
		{"not-json", `garbage`},
		{"empty-key", `{"type":"api","key":""}`},
		{"good", `{"type":"api","key":"GOODVAL"}`},
	} {
		if _, err := db.Exec(`INSERT INTO credential (id, integration_id, label, value) VALUES (?,?,?,?)`,
			r.id, r.id, "l", r.val); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	// `good` is not a mapped provider id, so nothing is emitted — but the point is
	// that the UNUSABLE rows are not emitted as empty values either.
	env, _ := adapterCredentialEnv(home, "opencode")
	for _, e := range env {
		if strings.HasSuffix(e, "=") {
			t.Errorf("an empty credential must never be emitted: %q", e)
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
