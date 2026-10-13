package claude

// guard_leak_test.go — THE PIN for a leak no assertion could see.
//
// The OS-level execution guard is a temp directory: guard.NewExecutionGuard
// calls os.MkdirTemp("", "orchicon-guard-*"), which lands it in $TMPDIR
// (internal/guard/guard.go). Its lifetime is owned by whoever built it — a
// worker session hands the cleanup back with the child environment
// (session.childEnv, deferred at the spawn site) and an Ask session closes its
// own in teardown() (ask.go) — so a caller that merely ABANDONS a session
// leaves the directory behind for the life of the machine, and NOTHING FAILS.
// Measured before this file existed: 11 tests in ask_test.go leaked exactly one
// shim dir each on every run of this package.
//
// A snapshot of the real temp dir cannot see that reliably: a long-lived Ask
// session, an opencode host serve and a running worker all legitimately own a
// shim, and none of those is a leak. The one process that can see precisely the
// directories ITS OWN tests created is the test binary, so the check lives in
// TestMain — point $TMPDIR at a directory this package owns for the duration of
// the run, and assert it holds no shim dirs once every test has finished. A
// dropped cleanup then FAILS the suite, naming the leftovers, instead of
// quietly littering the host.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shimDirPrefix is the name internal/guard passes to MkdirTemp. Spelled here as
// a literal rather than exported from the guard package: this file checks an
// OBSERVABLE (a directory name in $TMPDIR), and exporting the prefix would make
// the guard package own a test's vocabulary.
const shimDirPrefix = "orchicon-guard-"

func TestMain(m *testing.M) {
	// A PRIVATE $TMPDIR for this package's run, so every guard shim these tests
	// build lands in a directory whose contents are entirely ours. That closes
	// the set, which is what makes "still present at the end" mean exactly
	// "leaked" — no liveness scan, no heuristic, no flake.
	base := os.Getenv("TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	tmp, err := os.MkdirTemp(base, "orchicon-claude-tmp-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "guard-leak check: private $TMPDIR: %v\n", err)
		os.Exit(1)
	}
	prev, hadPrev := os.LookupEnv("TMPDIR")
	if err := os.Setenv("TMPDIR", tmp); err != nil {
		fmt.Fprintf(os.Stderr, "guard-leak check: set $TMPDIR: %v\n", err)
		os.Exit(1)
	}

	// The check is only worth anything if the runtime honours a changed $TMPDIR.
	// Verified per-call on go1.26 (even after os.TempDir was read once). If a
	// future runtime caches it, the shims would go to the real temp dir, this
	// directory would stay empty, and the check would pass VACUOUSLY — so say so
	// loudly rather than report a clean run that proves nothing.
	probe, perr := os.MkdirTemp("", "probe-*")
	mechanismOK := perr == nil && strings.HasPrefix(probe, tmp+string(os.PathSeparator))
	if perr == nil {
		_ = os.RemoveAll(probe)
	}
	if !mechanismOK {
		fmt.Fprintln(os.Stderr, "GUARD LEAK CHECK DISABLED: this runtime ignores a changed $TMPDIR, so this package's guard shim dirs cannot be isolated and counted.")
	}

	code := m.Run()

	var left []string
	if mechanismOK {
		left = leakedShimDirs(tmp)
	}
	// The shims are the evidence and they are already named below, so the wrapper
	// goes either way — leaving it would be its own small litter.
	if err := os.RemoveAll(tmp); err != nil {
		fmt.Fprintf(os.Stderr, "guard-leak check: remove %s: %v\n", tmp, err)
	}
	if hadPrev {
		_ = os.Setenv("TMPDIR", prev)
	} else {
		_ = os.Unsetenv("TMPDIR")
	}

	if len(left) > 0 {
		fmt.Fprintf(os.Stderr, "\nGUARD LEAK: %d execution-guard shim dir(s) outlived every test in this package:\n", len(left))
		for _, d := range left {
			fmt.Fprintln(os.Stderr, "  "+d)
		}
		fmt.Fprintln(os.Stderr,
			"A session built a guard and was then abandoned instead of retired. The owned\n"+
				"cleanup is session.childEnv's second return value (a worker session) or\n"+
				"askSession.teardown() / Bridge.CloseAsk() (an Ask session). newHarness and\n"+
				"askHarness now retire what they build via t.Cleanup, so a session built\n"+
				"OUTSIDE them — newAskSession/newSession directly — must be torn down by the\n"+
				"test itself.")
		os.Exit(1)
	}
	os.Exit(code)
}

// leakedShimDirs lists the guard shim directories still present in dir.
func leakedShimDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), shimDirPrefix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// TestCloseAskRemovesTheGuardShimDir pins the invariant the harness cleanup
// leans on: a live Ask session OWNS its shim directory, and CloseAsk — the
// shutdown seam the server calls on a plane restart — takes it with it.
//
// The leak this closes is invisible to a behavioural assertion: the session
// keeps working perfectly while its shim outlives it, which is how eleven tests
// did it unnoticed. So the assertion is about the DIRECTORY, not the session.
// It is also the Ask half of TestChildEnvCleanupRemovesTheShimDir, which pins
// the same ownership for the worker transport.
func TestCloseAskRemovesTheGuardShimDir(t *testing.T) {
	h, _ := askHarness(t)
	s := h.b.ensureAskSession("conv-guard-close")
	s.ensureGuard()
	if s.guard == nil {
		t.Skip("the execution guard could not be built in this environment")
	}
	shimDir := askShimDir(t, s)
	if _, err := os.Stat(shimDir); err != nil {
		t.Fatalf("the shim directory is not present while the session is live: %v", err)
	}

	h.b.CloseAsk()

	if _, err := os.Stat(shimDir); !os.IsNotExist(err) {
		t.Fatalf("the shim directory survived CloseAsk (%v) — a retired Ask session must not keep a shim guarding a path nothing owns", err)
	}
}

// askShimDir returns the guard shim directory an Ask session puts first on its
// child's PATH — the guard's own report of where it lives, so the test never
// has to guess at /tmp naming.
func askShimDir(t *testing.T, s *askSession) string {
	t.Helper()
	path := envValue(s.childEnv(), "PATH")
	first := strings.SplitN(path, string(os.PathListSeparator), 2)[0]
	if !strings.Contains(filepath.Base(first), shimDirPrefix) {
		t.Fatalf("the Ask child's PATH does not begin with a guard shim dir: %q", path)
	}
	return first
}
