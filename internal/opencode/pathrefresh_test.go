package opencode

// pathrefresh_test.go — the persisted-PATH reader, tested from any host.
//
// NOT NAMED pathrefresh_windows_test.go: a `_windows` filename suffix is a GOOS BUILD CONSTRAINT, so
// the file would compile only on Windows and these cases would silently not run anywhere else. The
// same trap binary_platform_test.go documents, and it was measured there.
//
// WHAT THIS GUARDS. The plane's PATH is a snapshot of the process that started it
// (internal/planecontrol builds the child env from os.Environ()). On Windows the authoritative list
// lives in the REGISTRY and is rebuilt at LOGON, so a plane started before opencode was installed
// keeps looking in the old places for its whole life — which is exactly the report:
//
//	"I reinstalled on windows and I am still getting this error ... opencode CLI should be installed
//	 on PATH now. I can run it from anywhere."
//
// Both statements were true. Re-reading the registry is how discovery becomes self-healing without a
// restart. The registry call itself is Windows-only and cannot run here, so the LOGIC — splitting a
// persisted value, expanding its %VARS%, dropping what cannot be a directory — is a pure function and
// is pinned on every platform.
//
// The parsing rules are not cosmetic; each case below is a way a real registry value differs from a
// naive strings.Split.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envList turns a map into the "K=V" form os.Environ returns, which is what SplitPersistedPath walks.
func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func TestSplitPersistedPathExpandsAndFilters(t *testing.T) {
	// A realistic HKCU\Environment\Path: quoted entries, %VAR% references, an empty segment from a
	// trailing semicolon, and one reference to a variable that does not exist.
	const val = `C:\Users\me\AppData\Local\Microsoft\WindowsApps;` +
		`"%USERPROFILE%\AppData\Roaming\npm";` +
		`%LOCALAPPDATA%\Programs\opencode;` +
		`%NOT_A_REAL_VARIABLE%\bin;` +
		`C:\Program Files\opencode;`

	got := SplitPersistedPath(val, envList(map[string]string{
		"USERPROFILE":  `C:\Users\me`,
		"LOCALAPPDATA": `C:\Users\me\AppData\Local`,
	}))

	want := []string{
		`C:\Users\me\AppData\Local\Microsoft\WindowsApps`,
		`"C:\Users\me\AppData\Roaming\npm"`,
		`C:\Users\me\AppData\Local\Programs\opencode`,
		`C:\Program Files\opencode`,
	}
	if len(got) != len(want) {
		t.Fatalf("SplitPersistedPath = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}

	// THE UNEXPANDED REFERENCE IS DROPPED, and that is deliberate rather than incidental. A literal
	// directory named "%NOT_A_REAL_VARIABLE%\bin" cannot hold opencode, so probing it is a wasted
	// stat on every resolution forever. The alternative — keeping it — would also make the failure
	// message claim it looked somewhere it could never have found anything.
	for _, dir := range got {
		if strings.Contains(dir, "%") {
			t.Errorf("an unexpanded reference survived into the probe list: %q", dir)
		}
	}
}

func TestSplitPersistedPathIsCaseInsensitiveOnVariableNames(t *testing.T) {
	// WINDOWS ENVIRONMENT NAMES ARE CASE-INSENSITIVE, and the registry routinely disagrees with the
	// process about spelling: a persisted Path says %SystemRoot% while the process exports
	// "SYSTEMROOT". A case-sensitive lookup expands nothing, drops the directory, and silently stops
	// finding a real install. Measured failure mode, not a hypothetical.
	got := SplitPersistedPath(`%SystemRoot%\system32;%windir%`, envList(map[string]string{
		"SYSTEMROOT": `C:\Windows`,
		"WinDir":     `C:\Windows`,
	}))
	if len(got) != 2 {
		t.Fatalf("SplitPersistedPath = %#v, want two expanded entries", got)
	}
	if got[0] != `C:\Windows\system32` || got[1] != `C:\Windows` {
		t.Errorf("case-insensitive expansion failed: %#v", got)
	}
}

func TestSplitPersistedPathHandlesEmptyAndMalformed(t *testing.T) {
	for _, val := range []string{"", "   ", ";;;", "%", "%%", "%UNCLOSED"} {
		if got := SplitPersistedPath(val, nil); len(got) != 0 {
			t.Errorf("SplitPersistedPath(%q) = %#v, want no directories — a malformed value must "+
				"yield nothing rather than a bogus path that gets probed", val, got)
		}
	}
	// A nil environment must not panic, and a value with no references needs none.
	if got := SplitPersistedPath(`C:\x`, nil); len(got) != 1 {
		t.Errorf("SplitPersistedPath(value with no references, nil env) = %#v, want the literal kept", got)
	}
}

// TestTheWindowsCandidateTablesCoverTheGlobalInstalls pins the two locations the reported operator's
// install most likely landed in, and the shim diagnostic's directory.
func TestTheWindowsCandidateTablesCoverTheGlobalInstalls(t *testing.T) {
	env := winEnv(map[string]string{
		"USERPROFILE":  `C:\Users\me`,
		"APPDATA":      `C:\Users\me\AppData\Roaming`,
		"LOCALAPPDATA": `C:\Users\me\AppData\Local`,
	})

	// EXPECTATIONS ARE BUILT WITH filepath.Join, never as backslash literals: this test runs on
	// Linux, where filepath.Join separates with "/", and a literal would fail for a reason that has
	// nothing to do with the table. Same convention as binary_platform_test.go.
	cands := openCodePlatformCandidates("windows", env)
	for _, want := range []struct{ what, path string }{
		// npm's global prefix on Windows. It normally holds a .cmd shim, but a package that ships the
		// real binary beside it resolves here and nowhere else.
		{"the npm global prefix", filepath.Join(`C:\Users\me\AppData\Roaming`, "npm", "opencode.exe")},
		// bun's Windows global bin, which had no counterpart in the POSIX-shaped $HOME list at all.
		{"the bun global bin", filepath.Join(`C:\Users\me`, ".bun", "bin", "opencode.exe")},
		// The official installer's spot, and the only non-sudo system-ish place an executable lands.
		{"the official install location", filepath.Join(`C:\Users\me`, ".opencode", "bin", "opencode.exe")},
	} {
		found := false
		for _, c := range cands {
			if c == want.path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the Windows candidate table is missing %s (%q) — an install there resolves "+
				"nowhere and the operator is told to install something they already have.\n got: %v",
				want.what, want.path, cands)
		}
	}

	shims := openCodeShimCandidates("windows", env)
	shimJoined := strings.Join(shims, "\n")
	// %APPDATA%\npm IS the directory npm puts opencode.cmd in. This list used to name %APPDATA%
	// itself — one level too high — so the hint that exists for exactly this operator never fired.
	if !strings.Contains(shimJoined, filepath.Join(`C:\Users\me\AppData\Roaming`, "npm", "opencode.cmd")) {
		t.Errorf("the shim table does not look in %%APPDATA%%\\npm, which is where npm puts "+
			"opencode.cmd on Windows — the 'I installed it but the plane cannot launch the shim' "+
			"hint would stay silent.\n got:\n%s", shimJoined)
	}
}

// TestAShimOnPathDoesNotCountAsAResolvedBinary — the hole the candidate tables could not close.
//
// internal/opencode's own rule is that a Windows npm install's `opencode.cmd` must NOT be resolved:
// Go cannot CreateProcess a .cmd, so resolving it means discovery starts offering models and the
// first execution dies somewhere unrelated. The candidate tables enforce that by naming .exe only.
//
// BUT LookPath RUNS FIRST and has no such opinion. A PATH carrying the npm prefix returns the shim,
// which sailed past the policy entirely — the rule only held for paths the tables happened to find.
// That is the operator's shape of install ("I can run it from anywhere"), so it is worth closing.
func TestAShimOnPathDoesNotCountAsAResolvedBinary(t *testing.T) {
	for _, p := range []string{`C:\Users\me\AppData\Roaming\npm\opencode.cmd`, `C:\x\opencode.BAT`, `C:\x\opencode.ps1`} {
		if launchableResolved("windows", p) {
			t.Errorf("launchableResolved(%q) = true — a package-manager SHIM is being resolved as "+
				"the executable. Go cannot CreateProcess a .cmd/.bat/.ps1, so discovery would offer "+
				"models and the first dispatch would fail for a reason that looks unrelated.", p)
		}
	}
	for _, p := range []string{`C:\Users\me\.opencode\bin\opencode.exe`, `C:\Users\me\scoop\shims\opencode.exe`} {
		if !launchableResolved("windows", p) {
			t.Errorf("launchableResolved(%q) = false — a real executable must be accepted", p)
		}
	}
	// UNIX IS UNTOUCHED. A shell script IS executable there, and an extension allow-list would be
	// wrong for the same reason it would be wrong to name ".exe" as the only valid Unix suffix: it
	// is a normal directory name. So the filter is Windows-only and the Unix answer never changes.
	for _, p := range []string{"/usr/local/bin/opencode", "/home/me/.opencode/bin/opencode.cmd"} {
		if !launchableResolved("linux", p) {
			t.Errorf("launchableResolved(linux, %q) = false — the shim filter must not change Unix "+
				"resolution at all", p)
		}
	}
}

// TestResolutionRecoversWithoutARestartAfterAnInstall is the operator's flow, end to end: the plane
// cannot resolve opencode, they install it, and the NEXT LOOK finds it.
//
// THE SECOND HALF IS THE FIX. Before this cycle, resolution happened once at plane boot, so the only
// thing a retry could do was return the same answer — which is why the message told the operator to
// restart. The resolver itself was already re-callable; what changed is that the plane now calls it
// again (internal/aigateway's lazy discoverer). This pins the resolver half so the two cannot drift:
// if resolution ever becomes cache-on-first-failure again, this test says so at the layer where the
// mistake would be made.
func TestResolutionRecoversWithoutARestartAfterAnInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(OpenCodeBinEnv, "")
	// PATH is EMPTIED for this simulation: the machine running the test may genuinely have opencode
	// installed, which would short-circuit the "nothing installed yet" step and make the test pass
	// without exercising anything.
	t.Setenv("PATH", t.TempDir())

	if _, err := resolveOpenCodeBinary(); err == nil {
		t.Fatal("resolution succeeded with nothing installed — the test's premise is broken, so the " +
			"recovery it is meant to prove proves nothing")
	}

	dir := filepath.Join(home, ".opencode", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "opencode")
	if err := os.WriteFile(want, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveOpenCodeBinary()
	if err != nil {
		t.Fatalf("after installing opencode at %s, resolution STILL fails: %v — a retry has to "+
			"find a binary that appeared after the first attempt, or the operator is back to "+
			"restarting the plane", want, err)
	}
	if got != want {
		t.Errorf("resolved %q, want %q", got, want)
	}
}
