package runtime

// store_confinement_test.go — A TEST THAT WRITES A STORE MUST NOT BE ABLE TO REACH THE REAL ONE.
//
// THE INCIDENT. Four opencode stores on the operator's machine were rotated on 2026-11-23 because one
// of them had been overwritten with 29 bytes:
//
//	this is not a sqlite database
//
// That string is written by THIS PACKAGE's own test (the "corrupt database" case of
// TestCredentialFallbacksAreBestEffort). It reached a live 39 MB database because the test asked
// openCodeDBPath(home) for the path — and openCodeStoreCandidates consults the AMBIENT XDG_DATA_HOME
// and openCodeStoreDir returns whichever store was MOST RECENTLY WRITTEN. The test built a perfectly
// good t.TempDir() home and still got the production store back, then CREATE TABLE'd into it and, in
// one subtest, wrote the stub over it.
//
// This is the third instance of one root cause in a single day — GIT_DIR leaking into `git -C`, and
// ORCHICON_CLAUDE_PROJECT_DIR leaking into a boundary test were the other two. In each, a path or a
// root was resolved from the process environment inside a test that then WRITES. The confinement below
// is the general shape of the fix.

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// THE RESOLVER A WRITER USES IGNORES THE ENVIRONMENT. With XDG_DATA_HOME pointing at a real-looking
// store, openCodeStoreDirFor must still answer for the home it was given.
func TestTheWritingResolverIgnoresTheAmbientStoreDir(t *testing.T) {
	ambient := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ambient, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A newer ambient store is exactly what used to win.
	if err := os.WriteFile(filepath.Join(ambient, "opencode", "opencode.db"), []byte("ambient"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", ambient)

	home := t.TempDir()
	got := openCodeStoreDirFor(goruntime.GOOS, home)
	want := filepath.Join(home, ".local", "share", "opencode")
	if got != want {
		t.Fatalf("openCodeStoreDirFor = %q, want %q. A resolver that consults XDG_DATA_HOME hands a WRITER "+
			"the operator's real store — that is how a live 39 MB database was overwritten with a 29-byte stub.",
			got, want)
	}
	if storePathUnderStateRoot(got, ambient) {
		t.Fatal("the writing resolver answered with the ambient store")
	}
}

// AND THE CONFINEMENT ACTUALLY REJECTS AN ESCAPE. A guard that says yes to everything is not a guard.
func TestStorePathConfinementRejectsAnEscape(t *testing.T) {
	home := t.TempDir()
	inside := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	for _, tc := range []struct {
		path   string
		inside bool
		why    string
	}{
		{inside, true, "the store under this home"},
		{home, true, "the home itself"},
		{filepath.Join(home, "sibling", "opencode.db"), true, "still under home"},
		{"/somewhere/else/entirely/opencode.db", false, "an absolute escape"},
		{filepath.Join(home, "..", "sneaky", "opencode.db"), false, "a .. escape that cleans outside"},
	} {
		if got := storePathUnderStateRoot(tc.path, home); got != tc.inside {
			t.Errorf("storePathUnderStateRoot(%q) = %v, want %v — %s", tc.path, got, tc.inside, tc.why)
		}
	}
	if storePathUnderStateRoot(inside, "") {
		t.Error("an empty root must confine nothing")
	}
}

// THE STUB WRITER CANNOT REACH A REAL STORE ANY MORE. This runs the exact subtest that did the damage,
// with XDG_DATA_HOME aimed at a decoy store, and asserts the decoy is untouched.
func TestTheCorruptDatabaseFixtureCannotTouchTheAmbientStore(t *testing.T) {
	decoyDir := t.TempDir()
	decoy := filepath.Join(decoyDir, "opencode", "opencode.db")
	if err := os.MkdirAll(filepath.Dir(decoy), 0o755); err != nil {
		t.Fatal(err)
	}
	const real = "GENUINE-DATA-MUST-SURVIVE"
	if err := os.WriteFile(decoy, []byte(real), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", decoyDir)

	// The fixture: exactly what the "corrupt database" subtest writes, against a test home.
	home := t.TempDir()
	p := credDBPathFor(t, home)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The decoy — standing in for the operator's real store — is intact.
	b, err := os.ReadFile(decoy)
	if err != nil {
		t.Fatalf("the ambient store was removed: %v", err)
	}
	if strings.TrimSpace(string(b)) != real {
		t.Fatalf("the ambient store was OVERWRITTEN with %q. This is the exact write that corrupted four "+
			"real stores.", string(b))
	}
	if !storePathUnderStateRoot(p, home) {
		t.Fatalf("the fixture wrote to %q, outside its own home %q", p, home)
	}
}
