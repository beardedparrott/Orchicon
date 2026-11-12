package opencode

// seed_auth_shape_test.go — the auth.json copy is GATED ON THE STORE SHAPE, not deleted.
//
// THE THREE QUESTIONS THIS ANSWERS, in the operator's words:
//
//	"Do the serves get cleaned up by anything? Are we leaving dead opencode serves out there?"
//	"If we aren't using the copy of the fossil auth.json should we stop copying it?"
//	"Opencode 1.x would use that still correct?"
//
// The third is the reason this is a gate and not a deletion. On 1.x auth.json IS the credential store, and this
// serve's data dir is PRIVATE by construction — so a 1.x serve starts with no credentials at all unless one is
// copied in. On 2.x the same copy is what poisons it: the store imports the file once, at migration, and then
// prefers the result for ever, which is how a September key ended up authenticating an October serve.
//
// So: copy for 1.x (no SQLite store), skip for 2.x (SQLite store present).

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// operatorHome builds a fake operator data home. withDB controls whether it looks like a 2.x install.
func operatorHome(t *testing.T, withDB bool, authBody string) string {
	t.Helper()
	home := t.TempDir()
	share := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if authBody != "" {
		if err := os.WriteFile(filepath.Join(share, "auth.json"), []byte(authBody), 0o600); err != nil {
			t.Fatalf("write auth.json: %v", err)
		}
	}
	if withDB {
		if err := os.WriteFile(filepath.Join(share, "opencode.db"), []byte("SQLite format 3\x00"), 0o600); err != nil {
			t.Fatalf("write opencode.db: %v", err)
		}
	}
	return home
}

// TestThe2xFossilIsNotCopied is question 2: on 2.x the copy is a fossil, so it stops.
func TestThe2xFossilIsNotCopied(t *testing.T) {
	home := operatorHome(t, true, `{"opencode-go":{"type":"api","key":"stale-key"}}`)
	dataDir := t.TempDir()

	h := NewHostServe(slog.Default(), dataDir, home)
	if err := h.seedAuth(); err != nil {
		t.Fatalf("seedAuth must be a clean no-op on 2.x, not an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "opencode", "auth.json")); err == nil {
		t.Fatal("auth.json was copied into a 2.x serve's data dir. 2.x imports that file ONCE at migration and " +
			"prefers the result for ever, so a stale copy means \"This API key has expired\" with no way to " +
			"recover by signing in again")
	}
}

// TestThe1xFossilIsStillCopied is question 3: 1.x has no SQLite store, so the copy is its credential store.
func TestThe1xFossilIsStillCopied(t *testing.T) {
	body := `{"opencode-go":{"type":"api","key":"only-key"},"oauth-thing":{"type":"oauth","refresh":"r"}}`
	home := operatorHome(t, false, body)
	dataDir := t.TempDir()

	h := NewHostServe(slog.Default(), dataDir, home)
	if err := h.seedAuth(); err != nil {
		t.Fatalf("seedAuth: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "opencode", "auth.json"))
	if err != nil {
		t.Fatalf("1.x was NOT seeded, so a 1.x serve starts with no credentials at all — removing the copy "+
			"outright would take opencode away from every 1.x operator: %v", err)
	}
	// A RAW BYTE COPY: this package models only api entries, and rewriting the file would drop the oauth ones,
	// which 1.x needs.
	if string(got) != body {
		t.Errorf("the seeded file is not a byte copy — oauth entries this package does not model would be "+
			"lost. got %q", string(got))
	}
}

// TestTheShapeSignalIsTheOperatorsOwnStore pins the signal itself.
func TestTheShapeSignalIsTheOperatorsOwnStore(t *testing.T) {
	// The signal is the OPERATOR'S opencode DATA DIR — the same one HostServe.authSource() returns.
	dataDirOf := func(home string) string { return filepath.Join(home, ".local", "share", "opencode") }
	if operatorUsesSQLiteStore(dataDirOf(operatorHome(t, false, "{}"))) {
		t.Error("a 1.x home (auth.json only) was read as 2.x, so a 1.x operator would stop being seeded")
	}
	if !operatorUsesSQLiteStore(dataDirOf(operatorHome(t, true, "{}"))) {
		t.Error("a 2.x home (opencode.db present) was not detected, so the fossil would be copied and imported")
	}
	if operatorUsesSQLiteStore(filepath.Join(t.TempDir(), "absent")) {
		t.Error("an absent data dir was read as 2.x")
	}
}

// TestNoAuthJsonAndNoStoreIsNotAnError: nothing to copy is the native-only case, not a failure.
func TestNoAuthJsonAndNoStoreIsNotAnError(t *testing.T) {
	home := operatorHome(t, false, "")
	h := NewHostServe(slog.Default(), t.TempDir(), home)
	if err := h.seedAuth(); err != nil {
		t.Fatalf("seedAuth with no auth.json must not error: %v", err)
	}
}
