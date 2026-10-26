package runtime

// opencode_store_platform_test.go — WHERE THE OPERATOR'S opencode STORE IS, PER PLATFORM.
//
// REPORTED ON WINDOWS: "Model unavailable: opencode-go/…" for every opencode-go model under the
// opencode adapter, with the same key working under the orchicon adapter. The same report carried
// opencode's own message for the free model — "Connect a provider or subscribe to OpenCode Go" —
// which is opencode saying it does not consider the provider connected at all.
//
// The cause was here: the store path was the XDG layout on EVERY platform
// (`<home>/.local/share/opencode`), and Windows does not use it. The resolver therefore read a
// directory that does not exist, delivered no credentials, and reported nothing undeliverable
// because it had read nothing to judge — so the serve started with an empty data dir and every
// opencode-go turn was unauthenticated.
//
// These tests name the layouts explicitly rather than relying on the host they run on, because the
// broken platform is the one CI does not run on.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeEnv is a getenv for one platform's environment.
func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// mkStore creates a store dir holding the named file, so storeWrittenAt sees it.
func mkStore(t *testing.T, dir, file string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", p, err)
	}
}

// TestWindowsStoreIsFoundUnderLocalAppData is THE regression test for the report: on Windows the
// operator's store lives under LOCALAPPDATA, and the XDG path beside it does not exist.
func TestWindowsStoreIsFoundUnderLocalAppData(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, "AppData", "Local")
	store := filepath.Join(local, "opencode", "Data")
	mkStore(t, store, "opencode.db", time.Minute)

	env := fakeEnv(map[string]string{"LOCALAPPDATA": local})
	got := openCodeStoreDir("windows", home, env)
	if got != store {
		t.Fatalf("store dir = %q, want %q — this is the miss that left every opencode-go model unauthenticated", got, store)
	}
}

// TestWindowsStoreWithoutDataComponent covers the other Windows layout opencode's bundle allows.
func TestWindowsStoreWithoutDataComponent(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, "AppData", "Local")
	store := filepath.Join(local, "opencode")
	mkStore(t, store, "auth.json", time.Minute)

	env := fakeEnv(map[string]string{"LOCALAPPDATA": local})
	if got := openCodeStoreDir("windows", home, env); got != store {
		t.Fatalf("store dir = %q, want %q", got, store)
	}
}

// TestWindowsFallsBackToHomeWhenNoEnvIsSet: a bare environment still reaches a real store, and never
// invents the Linux-only path and stops there.
func TestWindowsFallsBackToHomeWhenNoEnvIsSet(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, "AppData", "Local", "opencode", "Data")
	mkStore(t, store, "opencode.db", time.Minute)

	cands := openCodeStoreCandidates("windows", home, fakeEnv(nil))
	var sawHome bool
	for _, c := range cands {
		if c == store {
			sawHome = true
		}
	}
	if !sawHome {
		t.Fatalf("candidates %v omit the %%USERPROFILE%%\\AppData\\Local layout", cands)
	}
	if got := openCodeStoreDir("windows", home, fakeEnv(nil)); got != store {
		t.Fatalf("store dir = %q, want %q", got, store)
	}
}

// TestLinuxKeepsTheXDGStore: the platform that worked must keep resolving exactly where it did.
func TestLinuxKeepsTheXDGStore(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, ".local", "share", "opencode")
	mkStore(t, store, "opencode.db", time.Minute)

	if got := openCodeStoreDir("linux", home, fakeEnv(nil)); got != store {
		t.Fatalf("store dir = %q, want %q", got, store)
	}
}

// TestXDGDataHomeWinsOnLinux: an operator who relocated the store must still be found.
func TestXDGDataHomeWinsOnLinux(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "elsewhere")
	store := filepath.Join(xdg, "opencode")
	mkStore(t, store, "opencode.db", time.Minute)

	env := fakeEnv(map[string]string{"XDG_DATA_HOME": xdg})
	if got := openCodeStoreDir("linux", home, env); got != store {
		t.Fatalf("store dir = %q, want %q", got, store)
	}
}

// TestTheNewestStoreWins: a host holding both layouts resolves to the one just written by a sign-in,
// because a stale copy of the other would hand the serve a dead credential.
func TestTheNewestStoreWins(t *testing.T) {
	home := t.TempDir()
	old := filepath.Join(home, ".local", "share", "opencode")
	mkStore(t, old, "auth.json", 48*time.Hour)

	local := filepath.Join(home, "AppData", "Local")
	fresh := filepath.Join(local, "opencode", "Data")
	mkStore(t, fresh, "opencode.db", time.Minute)

	env := fakeEnv(map[string]string{"LOCALAPPDATA": local})
	if got := openCodeStoreDir("windows", home, env); got != fresh {
		t.Fatalf("store dir = %q, want the freshly written %q", got, fresh)
	}
}

// TestNoStoreReturnsThePathItLookedAt: with nothing on disk the planner still reports a location,
// which is what the serve logs when it warns that an operator's store was never found.
func TestNoStoreReturnsThePathItLookedAt(t *testing.T) {
	home := t.TempDir()
	got := openCodeStoreDir("windows", home, fakeEnv(nil))
	if got == "" {
		t.Fatal("store dir is empty — the warning would name nothing, which is the silence this whole change exists to end")
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("store dir %q is not absolute", got)
	}
}

// TestAnUnknownStoreYieldsNoPathAtAll: filepath.Join("", "auth.json") is the RELATIVE "auth.json",
// which would resolve against the process working directory. Both readers must see "" instead.
func TestAnUnknownStoreYieldsNoPathAtAll(t *testing.T) {
	if got := underStoreDir("", "auth.json"); got != "" {
		t.Fatalf("underStoreDir with no dir = %q, want \"\" — a relative path here reads the CWD", got)
	}
	if got := underStoreDir(t.TempDir(), "auth.json"); got == "" || !filepath.IsAbs(got) {
		t.Fatalf("underStoreDir with a dir = %q, want an absolute path", got)
	}
}
