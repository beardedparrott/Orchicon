package opencode

// credentials_changed_test.go — A SIGN-IN THAT LANDS AFTER THE SERVE STARTED.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAStoreWrittenAfterTheServeStartedIsANewerStore is the incident, as a predicate.
func TestAStoreWrittenAfterTheServeStartedIsANewerStore(t *testing.T) {
	started := time.Date(2026, 10, 26, 14, 25, 0, 0, time.UTC)
	signIn := time.Date(2026, 10, 26, 14, 43, 0, 0, time.UTC)
	if !credentialStoreIsNewer(signIn, started) {
		t.Fatal("a sign-in 18 minutes after the serve started must recycle it — this is the reported bug")
	}
}

// TestAnOlderStoreDoesNotRecycle: the ordinary case must not restart a healthy serve on every turn.
func TestAnOlderStoreDoesNotRecycle(t *testing.T) {
	started := time.Date(2026, 10, 26, 14, 43, 0, 0, time.UTC)
	if credentialStoreIsNewer(started.Add(-time.Hour), started) {
		t.Fatal("an older store must not recycle the serve")
	}
}

// TestASignInInTheSameMomentDoesNotRecycleForever: without an allowance the serve would restart on
// every single demand.
func TestASignInInTheSameMomentDoesNotRecycleForever(t *testing.T) {
	started := time.Date(2026, 10, 26, 14, 43, 0, 0, time.UTC)
	if credentialStoreIsNewer(started.Add(time.Second), started) {
		t.Fatal("a write within the allowance must not recycle — otherwise every demand restarts the serve")
	}
	if !credentialStoreIsNewer(started.Add(3*time.Second), started) {
		t.Fatal("a write clearly after the start must still recycle")
	}
}

// TestNoStoreMeansNothingChanged: the native-only host has no store, so nothing to notice.
func TestNoStoreMeansNothingChanged(t *testing.T) {
	home := t.TempDir()
	if _, ok := credentialStoreWrittenAt(home); ok {
		t.Fatal("a home with no opencode store must report no change")
	}
}

// TestTheStoreClockIsTheNewestFileInIt: either layout counts (2.x db, 1.x auth.json), and the newest
// is the one a sign-in just wrote.
func TestTheStoreClockIsTheNewestFileInIt(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now().Add(-time.Minute)
	for name, at := range map[string]time.Time{"auth.json": old, "opencode.db": fresh} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := credentialStoreWrittenAt(home)
	if !ok {
		t.Fatal("the store exists but was not found")
	}
	if !got.Equal(fresh.Round(time.Second)) && got.Sub(fresh).Abs() > time.Second {
		t.Fatalf("store clock = %v, want the newest file (%v)", got, fresh)
	}
}

// TestEnsureStartedRecyclesBeforeReportingReady is the WIRING guard.
func TestEnsureStartedRecyclesBeforeReportingReady(t *testing.T) {
	src, err := os.ReadFile("servehost.go")
	if err != nil {
		t.Fatalf("read servehost.go: %v", err)
	}
	body := string(src)
	at := strIndex(body, "func (h *HostServe) EnsureStarted(")
	if at < 0 {
		t.Fatal("EnsureStarted not found — this guard is stale, fix it rather than deleting it")
	}
	rest := body[at:]
	end := strIndex(rest, "\nfunc ")
	if end > 0 {
		rest = rest[:end]
	}
	if strIndex(rest, "recycleIfCredentialsChanged(ctx)") < 0 {
		t.Fatal("EnsureStarted does not recycle on a credential change — a sign-in after the serve started will be ignored, which is the reported bug")
	}
	if strIndex(rest, "recycleIfCredentialsChanged(ctx)") > strIndex(rest, "if h.ready()") {
		t.Fatal("the recycle must happen BEFORE the ready() short-circuit, or a live serve is never reconsidered")
	}
}

// strIndex is a plain substring search. The package already has an indexOf with a different
// signature (prompt_body_test.go), so this one is named for what it does.
func strIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
