package claude

// guard_leak_test.go — THE PIN for a leak no assertion could see.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shimDirPrefix is the name internal/guard passes to MkdirTemp.
const shimDirPrefix = "orchicon-guard-"

func TestMain(m *testing.M) {
	// A PRIVATE $TMPDIR for this package's run, so every guard shim these tests build lands in a
	// directory whose contents are entirely ours.
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

// TestCloseAskRemovesTheGuardShimDir pins the invariant the harness cleanup leans on: a live Ask
// session OWNS its shim directory, and CloseAsk.
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

// askShimDir returns the guard shim directory an Ask session puts first on its child's PATH — the
// guard's own report of where it lives, so the test never has to guess at /tmp naming.
func askShimDir(t *testing.T, s *askSession) string {
	t.Helper()
	path := envValue(s.childEnv(), "PATH")
	first := strings.SplitN(path, string(os.PathListSeparator), 2)[0]
	if !strings.Contains(filepath.Base(first), shimDirPrefix) {
		t.Fatalf("the Ask child's PATH does not begin with a guard shim dir: %q", path)
	}
	return first
}
