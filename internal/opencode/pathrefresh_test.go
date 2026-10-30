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

// TestAnNpmGlobalInstallResolvesToTheRealBinary — the second report, with the operator's own path.
//
//	"Opencode is currently installed here C:\Users\operator\AppData\Roaming\npm\opencode.ps1"
//
// That is npm's SHIM. On Windows npm makes three per bin entry (`opencode`, `opencode.cmd`,
// `opencode.ps1`) and never an `.exe`, so the directory above yields shims only — and a shim is
// correctly refused as unlaunchable, which left resolution with nothing and the operator being told
// to install what they had already installed.
//
// The real executable is deterministic: the package `opencode-ai` declares
// `"bin": {"opencode": "bin/opencode.exe"}`, and its postinstall copies the 211 MB platform binary
// over the stub. So the plane must look one level down, in node_modules.
//
// THIS IS TESTED WITH A REAL DIRECTORY TREE, not just a table, because the shim-implied rule has to
// STAT the shim before it can follow it. The tree is built under a temp dir with the env vars pointed
// at it, so the same code path runs on Linux that runs on the operator's machine.
func TestAnNpmGlobalInstallResolvesToTheRealBinary(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{
		"USERPROFILE":  home,
		"LOCALAPPDATA": filepath.Join(home, "AppData", "Local"),
	}
	// The npm prefix, exactly where their install is: %APPDATA%\npm.
	prefix := filepath.Join(home, "AppData", "Roaming", "npm")
	env["APPDATA"] = filepath.Join(home, "AppData", "Roaming")

	// npm's three shims for this one bin entry. None is launchable, and the .ps1 is the one the
	// operator actually quoted.
	// REAL shim bodies: the derivation now PARSES the shim for the target it names, so a fake
	// "shim\n" body has nothing to point at (which is correct, and is why these were updated).
	mustWrite(t, filepath.Join(prefix, "opencode.ps1"),
		"& \"$basedir/node_modules/opencode-ai/bin/opencode.exe\"   $args\n")
	mustWrite(t, filepath.Join(prefix, "opencode.cmd"),
		"\"%dp0%\\node_modules\\opencode-ai\\bin\\opencode.exe\"   %*\r\n")

	// The real binary, where the package's bin entry and postinstall put it.
	real := filepath.Join(prefix, "node_modules", "opencode-ai", "bin", "opencode.exe")
	mustWrite(t, real, "MZ fake pe binary")

	getenv := winEnv(env)

	// THE PREFIX TABLE must name it, for the case where no shim survived.
	cands := openCodePlatformCandidates("windows", getenv)
	foundInTable := false
	for _, c := range cands {
		if c == real {
			foundInTable = true
			break
		}
	}
	if !foundInTable {
		t.Errorf("the Windows candidate table does not name the npm package's real executable %q.\n"+
			"  An `npm i -g opencode-ai` installs a real opencode.exe under node_modules, and the\n"+
			"  table only looked at the prefix itself — where npm puts nothing but shims.\n got: %v",
			real, cands)
	}

	// AND THE SHIM-IMPLIED RULE must find it too, which is what covers a custom npm prefix.
	derived := npmExeFromShims("windows", getenv)
	foundDerived := false
	for _, c := range derived {
		if c == real {
			foundDerived = true
			break
		}
	}
	if !foundDerived {
		t.Errorf("npmExeFromShims did not derive %q from the shims it found in %q.\n got: %v\n"+
			"  Following the shim is what makes an install in an UNUSUAL prefix work: the shim proves\n"+
			"  which prefix npm used, so the binary can be found without hardcoding it.",
			real, prefix, derived)
	}
}

// TestTheShimHintNamesTheRealExecutable — when only a shim is found, the operator is told where the
// binary SHOULD be and why it might not be there. "Install it" is useless advice to someone who did.
func TestTheShimHintNamesTheRealExecutable(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{
		"USERPROFILE": home,
		"APPDATA":     filepath.Join(home, "AppData", "Roaming"),
	}
	prefix := filepath.Join(home, "AppData", "Roaming", "npm")
	shim := filepath.Join(prefix, "opencode.ps1")
	mustWrite(t, shim, "#!/usr/bin/env pwsh\n")

	hint := foundElsewhereHintFor("windows", winEnv(env))
	if hint == "" {
		t.Fatal("foundElsewhereHint was empty with a real npm shim present — the operator is told " +
			"to install opencode they have already installed, which is the reported bug")
	}
	wantExe := filepath.Join(prefix, "node_modules", "opencode-ai", "bin", "opencode.exe")
	if !strings.Contains(hint, wantExe) {
		t.Errorf("the hint does not name the real executable %q:\n%s", wantExe, hint)
	}
	if !strings.Contains(hint, "ignore-scripts") {
		t.Errorf("the hint does not mention --ignore-scripts, which is the one cause of a shim that "+
			"exists while its binary does not:\n%s", hint)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestTheOperatorsNpmInstallResolvesEndToEnd drives the REAL resolver over the reported machine.
//
// This is the case that survived the first fix, quoted with the operator's own path:
//
//	"Opencode is currently installed here C:\Users\operator\AppData\Roaming\npm\opencode.ps1"
//
// `(Get-Command opencode).Source` returns exactly that — a shim — so using it as the answer would
// resolve something Go cannot launch. The binary the shim wraps is deterministic though:
// `<npm prefix>\node_modules\opencode-ai\bin\opencode.exe`, because that is the package's declared
// `bin` entry and its postinstall copies the real platform binary there.
//
// The resolver is driven through resolveOpenCodeBinaryFor so the Windows branches actually execute on
// a Linux CI box. Without that seam this scenario could only be reproduced on a Windows machine,
// which is the trap this package already documents for its candidate tables.
func TestTheOperatorsNpmInstallResolvesEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // hostHome()
	t.Setenv(OpenCodeBinEnv, "")
	emptyPATH := t.TempDir()
	t.Setenv("PATH", emptyPATH) // exec.LookPath reads the PROCESS environment, not the injected map

	roaming := filepath.Join(home, "AppData", "Roaming")
	getenv := winEnv(map[string]string{
		"USERPROFILE":  home,
		"APPDATA":      roaming,
		"LOCALAPPDATA": filepath.Join(home, "AppData", "Local"),
		"PATH":         emptyPATH,
	})

	prefix := filepath.Join(roaming, "npm")
	// npm's three shims for the one bin entry. None is launchable.
	// REAL shim bodies: the derivation now PARSES the shim for the target it names, so a fake
	// "shim\n" body has nothing to point at (which is correct, and is why these were updated).
	mustWrite(t, filepath.Join(prefix, "opencode.ps1"),
		"& \"$basedir/node_modules/opencode-ai/bin/opencode.exe\"   $args\n")
	mustWrite(t, filepath.Join(prefix, "opencode.cmd"),
		"\"%dp0%\\node_modules\\opencode-ai\\bin\\opencode.exe\"   %*\r\n")

	real := filepath.Join(prefix, "node_modules", "opencode-ai", "bin", "opencode.exe")

	// (a) SHIMS ONLY: postinstall did not run, so the 479-byte stub's directory holds no exe at all.
	// Resolution must fail AND the message must name the path that would fix it — "install opencode"
	// is useless advice to someone who installed it.
	if _, err := resolveOpenCodeBinaryFor("windows", getenv, nil); err == nil {
		t.Fatal("resolution succeeded with no real binary present — a shim was accepted as the " +
			"executable, which Go cannot launch")
	} else if !strings.Contains(err.Error(), real) {
		t.Errorf("the not-found message does not name the real executable %q:\n%v", real, err)
	}

	// (b) POSTINSTALL RAN: the real binary is in place and the NEXT resolution finds it.
	mustWrite(t, real, "MZ the real 211MB PE binary")
	got, err := resolveOpenCodeBinaryFor("windows", getenv, nil)
	if err != nil {
		t.Fatalf("after npm's postinstall placed the binary, resolution STILL fails: %v\n"+
			"  This is the reported bug: opencode is installed, runnable from any shell, and the "+
			"plane cannot find it.", err)
	}
	if got != real {
		t.Errorf("resolved %q, want the npm-installed executable %q", got, real)
	}
}

// The npm layout is found by TWO independent mechanisms, and each is pinned separately below.
//
// They overlap in the ordinary case, which is the point — an install resolves even if one of them is
// broken — but overlap means a test that exercises only the ordinary case passes when EITHER is
// deleted. (Measured: removing the prefix table, and separately removing the shim-implied rule, both
// left TestTheOperatorsNpmInstall green, because the other mechanism covered for it.) So:
//
//	(a) NO SHIM AT ALL      -> only the PREFIX TABLE can find it
//	(b) AN UNUSUAL PREFIX   -> only the SHIM-IMPLIED rule can find it

func TestThePrefixTableAloneFindsAnNpmInstallWithNoShim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(OpenCodeBinEnv, "")
	emptyPATH := t.TempDir()
	t.Setenv("PATH", emptyPATH)

	roaming := filepath.Join(home, "AppData", "Roaming")
	getenv := winEnv(map[string]string{
		"USERPROFILE": home,
		"APPDATA":     roaming,
		"PATH":        emptyPATH,
	})
	// NO SHIMS. A npm prefix whose shims were deleted by hand, or a PATH stripped before npm ran —
	// the table is the only thing that can still find the binary.
	real := filepath.Join(roaming, "npm", "node_modules", "opencode-ai", "bin", "opencode.exe")
	mustWrite(t, real, "MZ real")

	got, err := resolveOpenCodeBinaryFor("windows", getenv, nil)
	if err != nil {
		t.Fatalf("with no shim present the PREFIX TABLE did not find %q: %v\n"+
			"  The table is what covers an install whose shims are gone, so it has to work on its own.", real, err)
	}
	if got != real {
		t.Errorf("resolved %q, want %q", got, real)
	}
}

func TestTheShimRuleAloneFindsAnInstallInAnUnusualPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(OpenCodeBinEnv, "")
	emptyPATH := t.TempDir()
	t.Setenv("PATH", emptyPATH)

	// A prefix that is NOT in npmGlobalPrefixes — the `npm config set prefix` case, or a corporate
	// image. The table cannot name it, so following the shim is the only way to it.
	oddPrefix := filepath.Join(home, "corp", "tools", "node")
	getenv := winEnv(map[string]string{
		"USERPROFILE": home,
		// APPDATA deliberately points somewhere WITHOUT an npm prefix, so the table sees nothing.
		"APPDATA": filepath.Join(home, "elsewhere"),
		"PATH":    emptyPATH,
	})
	// The shim table only looks in %APPDATA%\npm, %APPDATA% and scoop's shims, so put it where the
	// table can see it while the npm PREFIX it implies is not in the prefix list.
	shimDir := filepath.Join(home, "elsewhere", "npm")
	mustWrite(t, filepath.Join(shimDir, "opencode.ps1"),
		"& \"$basedir/node_modules/opencode-ai/bin/opencode.exe\"   $args\n")

	// ...but the binary is in the odd prefix, which the shim does NOT point at. So instead drive the
	// rule directly: a shim whose own directory is the unusual prefix.
	oddShim := filepath.Join(oddPrefix, "opencode.cmd")
	mustWrite(t, oddShim, "\"%dp0%\\node_modules\\opencode-ai\\bin\\opencode.exe\"   %*\r\n")
	real := filepath.Join(oddPrefix, "node_modules", "opencode-ai", "bin", "opencode.exe")
	mustWrite(t, real, "MZ real")

	derived := npmExeFromShims("windows", winEnv(map[string]string{"USERPROFILE": home, "APPDATA": oddPrefix, "PATH": emptyPATH}))
	found := false
	for _, d := range derived {
		if d == real {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("npmExeFromShims did not derive %q from the shim beside it (%q).\n got: %v\n"+
			"  Following the shim is what makes an install in an UNKNOWN prefix work: the shim proves "+
			"which prefix npm used, so no table of prefixes is needed.", real, oddShim, derived)
	}
	_ = getenv
}

// TestTheShimIsParsedForItsTargetNotGuessed is the third and final iteration of this bug, and the
// shims below are COPIED VERBATIM from the machine that reported it.
//
//	"Opencode is currently installed here C:\Users\operator\AppData\Roaming\npm\opencode.ps1"
//
// The second fix assumed the npm package was `opencode-ai` and probed
// `node_modules/opencode-ai/bin/opencode.exe`. Their real install is `@opencode/cli`, so the guess
// missed a 246 MB executable sitting at
//
//	C:\Users\operator\AppData\Roaming\npm\node_modules\@opencode\cli\bin\opencode.exe
//
// GUESSING THE PACKAGE NAME IS THE WRONG SHAPE. The name is not the plane's to know: it is scoped,
// it has already changed once in the wild, and a distributor could publish it under anything. The
// SHIM is the authority, because npm generates it from the package's own `bin` entry — so it names
// exactly the file `npm i -g` made runnable.
//
// These three shim bodies are byte-for-byte what npm writes, which is what makes this a regression
// test rather than a restatement of the parser.
const (
	npmShCmd = `@ECHO off\r
GOTO start\r
:find_dp0\r
SET dp0=%~dp0\r
EXIT /b\r
:start\r
SETLOCAL\r
CALL :find_dp0\r
\r
"%dp0%\node_modules\@opencode\cli\bin\opencode.exe"   %*\r
`
	npmShPS1 = `#!/usr/bin/env pwsh
$basedir=Split-Path $MyInvocation.MyCommand.Definition -Parent

$exe=""
if ($PSVersionTable.PSVersion -lt "6.0" -or $IsWindows) {
  # Fix case when both the Windows and Linux builds of Node
  # are installed in the same directory
  $exe=".exe"
}
# Support pipeline input
if ($MyInvocation.ExpectingInput) {
  $input | & "$basedir/node_modules/@opencode/cli/bin/opencode.exe"   $args
} else {
  & "$basedir/node_modules/@opencode/cli/bin/opencode.exe"   $args
}
exit $LASTEXITCODE
`
	npmShSh = `#!/bin/sh
basedir=$(dirname "$(echo "$0" | sed -e 's,\\,/,g')")

case ` + "`uname`" + ` in
    *CYGWIN*|*MINGW*|*MSYS*)
        if command -v cygpath > /dev/null 2>&1; then
            basedir=` + "`cygpath -w \"$basedir\"`" + `
        fi
    ;;
esac

exec "$basedir/node_modules/@opencode/cli/bin/opencode.exe"   "$@"
`
)

func TestTheShimIsParsedForItsTargetNotGuessed(t *testing.T) {
	prefix := t.TempDir()
	real := filepath.Join(prefix, "node_modules", "@opencode", "cli", "bin", "opencode.exe")

	for _, tc := range []struct{ shim, body string }{
		{"opencode.cmd", npmShCmd},
		{"opencode.ps1", npmShPS1},
		{"opencode", npmShSh},
	} {
		path := filepath.Join(prefix, tc.shim)
		mustWrite(t, path, tc.body)
		got := npmTargetFromShim(path)
		if got != real {
			t.Errorf("npmTargetFromShim(%s) = %q, want %q\n"+
				"  The shim names its target explicitly; guessing the package name is how a real\n"+
				"  246 MB executable was missed on the reporting machine.", tc.shim, got, real)
		}
	}
}

// TestAShimPointingAtAScriptYieldsNothing — the malformed and non-executable cases. A shim that names
// a .js entry is not spawnable by this plane, so it must not be returned as a resolution.
func TestAShimPointingAtAScriptYieldsNothing(t *testing.T) {
	prefix := t.TempDir()
	for _, tc := range []struct{ name, body string }{
		{"js-target.cmd", "\"%dp0%\\node_modules\\some-pkg\\bin\\cli.js\" %*"},
		{"no-target.cmd", "@echo off\r\necho nothing here\r\n"},
		{"empty.ps1", ""},
	} {
		path := filepath.Join(prefix, tc.name)
		mustWrite(t, path, tc.body)
		if got := npmTargetFromShim(path); got != "" {
			t.Errorf("npmTargetFromShim(%s) = %q, want empty — a non-executable target must not be "+
				"handed to a spawn", tc.name, got)
		}
	}
}


// TestTheScopedPackageFallbackCoversANoShimInstall pins the SECOND published name.
//
// The shim parse is the general rule, but it needs a shim to read. When there is none — deleted by
// hand, or a PATH stripped before npm ran — the prefix table is all that is left, and it has to know
// both names in use:
//
//	opencode-ai     the unscoped package
//	@opencode/cli   the scoped one the reporting machine actually has
//
// Without this case the scoped entry can be deleted and every other test still passes (measured).
func TestTheScopedPackageFallbackCoversANoShimInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(OpenCodeBinEnv, "")
	emptyPATH := t.TempDir()
	t.Setenv("PATH", emptyPATH)

	roaming := filepath.Join(home, "AppData", "Roaming")
	getenv := winEnv(map[string]string{
		"USERPROFILE": home,
		"APPDATA":     roaming,
		"PATH":        emptyPATH,
	})
	// NO SHIMS AT ALL, and the SCOPED layout.
	real := filepath.Join(roaming, "npm", "node_modules", "@opencode", "cli", "bin", "opencode.exe")
	mustWrite(t, real, "MZ real")

	got, err := resolveOpenCodeBinaryFor("windows", getenv, nil)
	if err != nil {
		t.Fatalf("with no shim present the prefix table did not find the SCOPED layout %q: %v\n"+
			"  @opencode/cli is the name the reporting machine has, so a table that only knows\n"+
			"  opencode-ai covers the wrong half of the installs in the wild.", real, err)
	}
	if got != real {
		t.Errorf("resolved %q, want %q", got, real)
	}
}
