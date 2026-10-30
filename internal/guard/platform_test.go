package guard

// platform_test.go — the guard must never DISABLE the feature it protects.
//
// REPORTED on Windows:
//
//	create conversation session: host opencode serve unavailable — Ask chat transport is disabled:
//	host serve guard: guard: symlink sudo: symlink guard
//	C:\Users\operator\AppData\Local\Temp\orchicon-guard-706398431\sudo: A required privilege is not held
//	by the client.
//
// "Why is there a sudo in windows?" — there is none; it is a SYMLINK named `sudo`, the guard's
// interception mechanism. It cannot exist on Windows for three independent reasons (symlink privilege,
// a #!/bin/bash shim, and PATHEXT not resolving an extensionless name — see platform_windows.go), and
// the reporting machine proved the first one is subtle: an ELEVATED shell creates the symlink happily,
// which is why testing it from an admin prompt hides the bug.
//
// THE DEFECT WAS THE SEVERITY. Every other caller warns and continues; the host serve returned the
// error, so a defense-in-depth backstop being unavailable disabled Ask chat entirely. These assert
// the corrected contract on WHICHEVER platform runs them, which is deliberate: the POSIX half proves
// the enabled path is untouched by the gate, and the disabled half is exercised through the same
// accessors Windows uses, so the logic is checked on the Linux gate that actually runs.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAGuardIsEitherEnabledOrSaysWhyNot — there is no third state, and "cannot exist here" must be
// distinguishable from "failed to build", because one is a warning and the other was fatal.
func TestAGuardIsEitherEnabledOrSaysWhyNot(t *testing.T) {
	g, err := NewExecutionGuard("")
	if err != nil {
		t.Fatalf("NewExecutionGuard returned an error (%v). An unsupported platform must report itself "+
			"through Enabled/DisabledReason with a nil error — returning an error here is what let the "+
			"host serve turn 'no OS guard' into 'no Ask chat'.", err)
	}
	if g == nil {
		t.Fatal("NewExecutionGuard returned (nil, nil) — every caller dereferences the result")
	}
	if g.Enabled() {
		if g.DisabledReason() != "" {
			t.Errorf("an enabled guard still carries a reason: %q", g.DisabledReason())
		}
		return
	}
	if g.DisabledReason() == "" {
		t.Error("a disabled guard gives no reason. The operator is left with a mystery, which is the " +
			"reported experience ('Why is there a sudo in windows?')")
	}
}

// TestADisabledGuardLeavesTheEnvironmentAlone — and returns a COPY.
//
// Prepending an empty PATH element would put the process's working directory on the worker's search
// path, so a bare `guard` or `orchicon` could resolve to whatever happens to sit in the project dir.
// That is strictly worse than running unguarded.
func TestADisabledGuardLeavesTheEnvironmentAlone(t *testing.T) {
	g := &Guard{disabled: true, reason: "test"}
	env := []string{"PATH=/usr/bin:/bin", "HOME=/home/x"}
	got := g.Apply(env)

	if strings.Join(got, "\n") != strings.Join(env, "\n") {
		t.Errorf("a disabled guard changed the environment:\n got %v\nwant %v", got, env)
	}
	if len(got) > 0 && got[0] == "PATH=" {
		t.Error("a disabled guard prepended an EMPTY PATH element — that adds the working directory " +
			"to the search path, which is worse than no guard")
	}
	// A COPY, so a caller appending to the result cannot corrupt its own environment slice.
	got[0] = "PATH=/corrupted"
	if env[0] != "PATH=/usr/bin:/bin" {
		t.Error("Apply returned the caller's own slice; mutating it corrupted the input")
	}
}

// TestMakeGuardReportsNoGuardRatherThanAnError pins the same severity rule for the container path.
//
// MakeGuard returns a DIRECTORY that callers prepend to PATH. On an unsupported platform it must say
// "no guard" as an empty string with a nil error, so the four supervisor call sites can skip cleanly.
// Returning an error would log a scary line on every single execution for a condition that is
// permanent and expected.
func TestMakeGuardReportsNoGuardRatherThanAnError(t *testing.T) {
	dir, err := MakeGuard(t.TempDir(), "")
	if err != nil {
		t.Fatalf("MakeGuard returned an error on an unsupported platform: %v", err)
	}
	if !supported() {
		if dir != "" {
			t.Errorf("MakeGuard returned %q on an unsupported platform, want \"\" — a non-empty path "+
				"would be prepended to a worker's PATH and there is nothing behind it", dir)
		}
		return
	}
	if dir == "" {
		t.Fatal("MakeGuard returned no directory on a SUPPORTED platform")
	}
	defer os.RemoveAll(dir)
	if _, statErr := os.Stat(filepath.Join(dir, "guard")); statErr != nil {
		t.Errorf("the guard script is missing from %q: %v", dir, statErr)
	}
}
