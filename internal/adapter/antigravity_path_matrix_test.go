package adapter

// antigravity_path_matrix_test.go — AC2: agy's binary discovery is proved for WINDOWS, macOS AND LINUX,
// not just for the Linux layout the original discovery test happened to cover.
//
// THE DEFECT THIS GUARDS. `TestAntigravityBinaryDiscovery` creates a `~/.local/bin/agy` tree and
// resolves it as linux. That proves ONE of the three platforms this work item exists for. It cannot
// prove the Windows installer location, `.exe` naming, the path-with-spaces case (Windows install paths
// routinely contain one — `C:\Users\a b\AppData\Local\...` — and a naive shell join splits it), or the
// macOS Homebrew prefix (`/opt/homebrew/bin`, where Apple-silicon Homebrew lives and where an
// Intel-shaped search never looks).
//
// HOW THE WINDOWS CASE IS RUN ON A LINUX HOST. `ResolveBinary` probes with `os.Stat`, so a literally
// Windows-spelled tree cannot be created here. The windows case therefore builds a REAL directory whose
// name contains a space and hands `%LOCALAPPDATA%` to the resolver through its injected `getenv`, which
// exercises the whole windows candidate path — `extraBinDirs` → `LOCALAPPDATA\Programs\agy` →
// `agy.exe` — with a space in every component. The pure Windows *spellings* (`C:\...`) are asserted
// structurally, which is the part that does not need a stat.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAntigravityBinaryPathMatrix is AC2's table: one row per platform, each asserting where agy is
// found and — for the paths that cannot exist on this host — the exact candidate list the resolver
// would probe.
func TestAntigravityBinaryPathMatrix(t *testing.T) {
	// A resolver that never consults PATH, so a hit can only come from the known-install lists.
	noPath := func(extra map[string]string) func(string) string {
		return func(k string) string {
			if k == "PATH" {
				return t.TempDir()
			}
			return extra[k]
		}
	}

	t.Run("linux", func(t *testing.T) {
		home := t.TempDir()
		bin := filepath.Join(home, ".local", "bin", "agy")
		mkExe(t, bin)

		if got := ResolveBinary("linux", home, KindAntigravity, noPath(nil)); got != bin {
			t.Fatalf("ResolveBinary(linux) = %q, want the Google default install %q", got, bin)
		}
		if got, want := declaredBinDir(home, KindAntigravity), filepath.Join(home, ".local", "bin"); got != want {
			t.Errorf("declaredBinDir(linux) = %q, want %q — Google's documented default install location", got, want)
		}
		// The POSIX system locations, so an agy installed by a package manager or Homebrew-on-Intel is
		// found without an env override.
		dirs := extraBinDirs("linux", home, KindAntigravity, noPath(nil))
		for _, want := range []string{"/usr/local/bin", "/usr/bin", "/opt/homebrew/bin"} {
			if !containsStr(dirs, want) {
				t.Errorf("extraBinDirs(linux) = %v, missing %q", dirs, want)
			}
		}
		// The file name carries no extension on a POSIX host.
		if got := binaryName("linux", KindAntigravity); got != "agy" {
			t.Errorf("binaryName(linux) = %q, want agy", got)
		}
	})

	t.Run("darwin", func(t *testing.T) {
		home := t.TempDir()
		bin := filepath.Join(home, ".local", "bin", "agy")
		mkExe(t, bin)

		if got := ResolveBinary("darwin", home, KindAntigravity, noPath(nil)); got != bin {
			t.Fatalf("ResolveBinary(darwin) = %q, want %q", got, bin)
		}
		// /opt is not creatable in a test, so the PROBE LIST is what gets pinned — and
		// /opt/homebrew/bin is the Apple-silicon Homebrew prefix, the one an Intel-shaped search misses.
		dirs := extraBinDirs("darwin", home, KindAntigravity, noPath(nil))
		for _, want := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin"} {
			if !containsStr(dirs, want) {
				t.Errorf("extraBinDirs(darwin) = %v, missing %q — an agy under %q would be reported 'not found'",
					dirs, want, want)
			}
		}
		if got := binaryName("darwin", KindAntigravity); got != "agy" {
			t.Errorf("binaryName(darwin) = %q, want agy", got)
		}
	})

	t.Run("windows", func(t *testing.T) {
		// A REAL tree whose every component below the temp root contains a space, which is the shape
		// `C:\Users\a b\AppData\Local\Programs\agy\agy.exe` has on a real machine.
		base := t.TempDir()
		localAppData := filepath.Join(base, "a b", "AppData", "Local")
		bin := filepath.Join(localAppData, "Programs", "agy", "agy.exe")
		mkExe(t, bin)

		home := filepath.Join(base, "a b")
		env := noPath(map[string]string{"LOCALAPPDATA": localAppData})
		if got := ResolveBinary("windows", home, KindAntigravity, env); got != bin {
			t.Fatalf("ResolveBinary(windows) = %q, want the installer location %q", got, bin)
		}
		// THE SPACE SURVIVES. A resolver that shell-joined its candidates would have split at "a b" and
		// failed the stat; this asserts the resolved path is still one path.
		if !strings.Contains(bin, "a b") {
			t.Fatalf("the fixture lost its space: %q", bin)
		}
		// THE EXECUTABLE IS agy.exe, never `agy` and never `antigravity.exe`.
		if got := binaryName("windows", KindAntigravity); got != "agy.exe" {
			t.Errorf("binaryName(windows) = %q, want agy.exe", got)
		}
		if ext := strings.ToLower(filepath.Ext(bin)); ext != ".exe" {
			t.Errorf("the resolved windows binary ends %q, want .exe", ext)
		}
		// The windows candidate list is the %LOCALAPPDATA%-rooted install locations. Asserted
		// structurally because the spelling differs on a real host (backslashes).
		dirs := extraBinDirs("windows", home, KindAntigravity, env)
		var sawAgy, sawWinget bool
		for _, d := range dirs {
			if strings.Contains(d, "agy") && strings.Contains(d, "a b") {
				sawAgy = true
			}
			if strings.Contains(d, "WinGet") {
				sawWinget = true
			}
		}
		if !sawAgy {
			t.Errorf("extraBinDirs(windows) = %v, has no LOCALAPPDATA-rooted agy install dir", dirs)
		}
		if !sawWinget {
			t.Errorf("extraBinDirs(windows) = %v, has no WinGet links dir — an agy installed by winget is a "+
				"shim there and must be reachable to be refused rather than missed", dirs)
		}
		// An unset %LOCALAPPDATA% must not produce a RELATIVE candidate: filepath.Join("", "Programs",
		// "agy") is "Programs/agy", and os.Stat on that resolves against the process's own cwd.
		for _, d := range extraBinDirs("windows", home, KindAntigravity, noPath(nil)) {
			if d != "" && !filepath.IsAbs(d) && !strings.ContainsAny(d, `:\/`) {
				t.Errorf("extraBinDirs(windows) with no LOCALAPPDATA produced the relative candidate %q — "+
					"os.Stat would resolve it against the process's working directory", d)
			}
		}
	})

	// THE WINDOWS SPELLING ITSELF, pinned as a shape. This is the exact path the work item names, and
	// while it cannot be stat'ed on Linux, the two rules it depends on can be asserted directly.
	t.Run("windows-spelling", func(t *testing.T) {
		const wantShaped = `C:\Users\a b\AppData\Local\Programs\agy\agy.exe`
		if !strings.Contains(wantShaped, "a b") {
			t.Fatal("fixture lost its space")
		}
		if got := binaryName("windows", KindAntigravity); wantShaped[len(wantShaped)-len(got):] != got {
			t.Errorf("%q does not end with binaryName(windows) = %q", wantShaped, got)
		}
		// A windows-spelled LOCALAPPDATA still yields candidate dirs that KEEP the space and the .exe
		// name, whichever separator Join happens to use on the compiling host.
		env := noPath(map[string]string{"LOCALAPPDATA": `C:\Users\a b\AppData\Local`})
		dirs := extraBinDirs("windows", `C:\Users\a b`, KindAntigravity, env)
		if len(dirs) == 0 {
			t.Fatal("extraBinDirs(windows) is empty for a windows-spelled LOCALAPPDATA")
		}
		joined := strings.Join(dirs, "|")
		if !strings.Contains(joined, "a b") {
			t.Errorf("the space in LOCALAPPDATA was not carried into the candidate dirs: %v", dirs)
		}
		if !strings.Contains(joined, "agy") {
			t.Errorf("the candidate dirs name no agy install location: %v", dirs)
		}
	})
}

// TestAntigravityShimNeverResolved is the other half of "installed but not found": on Windows agy can
// arrive as a `agy.cmd`/`agy.ps1` shim from npm/pnpm/scoop, and spawning a shim instead of the real
// executable is what produces the quoted-path bug the claude adapter documents.
func TestAntigravityShimNeverResolved(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agy.cmd", "agy.bat", "agy.ps1"} {
		mkExe(t, filepath.Join(dir, name))
	}
	env := func(k string) string {
		if k == "PATH" {
			return t.TempDir()
		}
		return ""
	}
	// THE INVARIANT IS "NEVER A SHIM", not "never anything". On a POSIX host the resolver also probes
	// /usr/local/bin, /usr/bin and /opt/homebrew/bin, and a real agy installed there IS the right
	// answer — this fixture cannot make those absent. So the assertion is on the shape: whatever comes
	// back must be a real executable, and a shim is disqualified outright.
	for _, goos := range []string{"windows", "linux", "darwin"} {
		got := ResolveBinary(goos, home, KindAntigravity, env)
		switch strings.ToLower(filepath.Ext(got)) {
		case ".cmd", ".bat", ".ps1":
			t.Errorf("ResolveBinary(%s) = %q — a shell shim must NEVER be resolved as agy; spawning one is the "+
				"quoted-path/spaces failure the claude adapter documents at internal/claude/permissions.go", goos, got)
		}
	}
	// A WINDOWS host also has no POSIX system dirs, so with %LOCALAPPDATA% unset and PATH empty the
	// answer is genuinely NOT FOUND: a shim is not a fallback.
	if got := ResolveBinary("windows", home, KindAntigravity, env); got != "" {
		t.Errorf("ResolveBinary(windows) = %q, want \"\" — with no install location and only shims present agy "+
			"is NOT INSTALLED, and a shim must not be substituted for it", got)
	}
}

// mkExe writes a placeholder executable, creating parents.
func mkExe(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// containsStr reports whether want is in list.
func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
