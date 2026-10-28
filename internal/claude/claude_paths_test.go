package claude

// claude_paths_test.go — CLAUDE IS WHEREVER IT IS, NOT ONLY AT ~/.local/bin.
//
// The reported case, from the operator's own `Get-Command claude`:
//
//	C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\
//	    Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe\claude.exe
//
// A real executable in a WinGet PACKAGE directory. The old resolver probed `$HOME/.local/bin/claude` and
// then a bare `claude`, so it missed this install entirely — and the bare fallback could not rescue it
// either, because the plane's PATH is the one it was started with.
//
// THE WINIT DIRECTORY IS CONTENT-ADDRESSED, so these tests build the real shape in a temp dir rather than
// asserting a spelled-out path: that is the only form that proves the enumeration works.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeWindowsEnv builds a getenv over a temp tree shaped like a Windows profile.
type fakeWindowsEnv struct {
	root string
	vars map[string]string
}

func newFakeWindowsEnv(t *testing.T) *fakeWindowsEnv {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "claude-winenv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	localAppData := filepath.Join(root, "AppData", "Local")
	roaming := filepath.Join(root, "AppData", "Roaming")
	for _, d := range []string{localAppData, roaming, filepath.Join(root, "Programs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeWindowsEnv{
		root: root,
		vars: map[string]string{
			"LOCALAPPDATA": localAppData,
			"APPDATA":      roaming,
			"USERPROFILE":  root,
			"HOME":         "",
		},
	}
}

func (f *fakeWindowsEnv) getenv(k string) string { return f.vars[k] }

// writeFile creates a file (with parents) and returns its path.
func (f *fakeWindowsEnv) writeFile(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{f.root}, parts...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestClaudeResolvesInAWingetPackageDirectory is the reported path, exactly.
func TestClaudeResolvesInAWingetPackageDirectory(t *testing.T) {
	env := newFakeWindowsEnv(t)
	want := env.writeFile(t, "AppData", "Local", "Microsoft", "WinGet", "Packages",
		"Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe", "claude.exe")

	if got := claudeBinaryPathFor("windows", env.getenv); got != want {
		t.Fatalf("ClaudeBinaryPath = %q, want the WinGet package binary %q", got, want)
	}
}

// TestClaudeResolvesWhenThePackageNestsTheExe covers a package that lays itself out one level in.
func TestClaudeResolvesWhenThePackageNestsTheExe(t *testing.T) {
	env := newFakeWindowsEnv(t)
	want := env.writeFile(t, "AppData", "Local", "Microsoft", "WinGet", "Packages",
		"Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe", "bin", "claude.exe")

	if got := claudeBinaryPathFor("windows", env.getenv); got != want {
		t.Fatalf("ClaudeBinaryPath = %q, want the nested package binary %q", got, want)
	}
}

// TestClaudeNeverResolvesAShim is the trap that makes a discovery succeed and a dispatch die: Go cannot
// execute a .cmd/.bat/.ps1, so resolving one is worse than not resolving at all.
func TestClaudeNeverResolvesAShim(t *testing.T) {
	env := newFakeWindowsEnv(t)
	// An npm-style install: shims only, no executable anywhere.
	if err := os.MkdirAll(filepath.Join(env.root, "AppData", "Roaming", "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude.cmd", "claude.ps1", "claude"} {
		p := filepath.Join(env.root, "AppData", "Roaming", "npm", name)
		if err := os.WriteFile(p, []byte("@echo off"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := claudeBinaryPathFor("windows", env.getenv)
	if got != "claude" {
		t.Fatalf("resolved %q — a shim must never be resolved as the binary (Go cannot execute it), the "+
			"fallback is the bare name with a hint", got)
	}
	// AND THE HINT NAMES IT, so "not found" is not a lie about an install the operator can see.
	hint := claudeShimHint("windows", env.getenv)
	if hint == "" {
		t.Fatal("a shim was present and the hint said nothing — the operator is told to install what " +
			"they already installed")
	}
	for _, wantPart := range []string{"claude.cmd", "SHIM", ClaudeBinEnv} {
		if !strings.Contains(hint, wantPart) {
			t.Errorf("the hint must mention %q; got: %s", wantPart, hint)
		}
	}
}

// TestClaudeOverrideWins keeps the escape hatch first in the order.
func TestClaudeOverrideWins(t *testing.T) {
	env := newFakeWindowsEnv(t)
	env.writeFile(t, "AppData", "Local", "Microsoft", "WinGet", "Packages",
		"Anthropic.ClaudeCode_X", "claude.exe")
	env.vars[ClaudeBinEnv] = `D:\tools\claude.exe`
	if got := claudeBinaryPathFor("windows", env.getenv); got != `D:\tools\claude.exe` {
		t.Errorf("override ignored: got %q", got)
	}
}

// TestPosixBehaviourUnchanged is the regression guard: on Linux and macOS the candidate list is the single
// $HOME/.local/bin/claude it always was, and Windows locations contribute nothing.
func TestPosixBehaviourUnchanged(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	if got := claudeBinaryPathFor("linux", env); got != p {
		t.Errorf("linux resolver = %q, want %q", got, p)
	}
	// And the Windows candidate list is EMPTY on POSIX — no stray probes of C:\ paths on a Linux host.
	if got := claudeWindowsCandidates("linux", env); len(got) != 0 {
		t.Errorf("linux asked for Windows candidates: %v", got)
	}
	if got := claudeShimHint("linux", env); got != "" {
		t.Errorf("linux produced a Windows shim hint: %q", got)
	}
}

// TestClaudePathIsAbsoluteWhenResolved pins that no candidate can be a RELATIVE path, which os.Stat would
// resolve against the process's working directory — the same class of bug opencode documents ("found"
// relative to wherever the app happens to be running).
func TestClaudePathIsAbsoluteWhenResolved(t *testing.T) {
	env := newFakeWindowsEnv(t)
	want := env.writeFile(t, "AppData", "Local", "Microsoft", "WinGet", "Packages",
		"Anthropic.ClaudeCode_X", "claude.exe")
	got := claudeBinaryPathFor("windows", env.getenv)
	if !filepath.IsAbs(got) {
		t.Fatalf("resolved a RELATIVE path %q — it must be absolute", got)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	_ = runtime.GOOS
}

// TestClaudeResolvesAnAliasLauncher covers the seam the operator named: "no matter where they are
// instead of hardcoding stuff".
//
// An MSIX/alias install puts its launcher in an alias directory under a name we cannot enumerate ahead of
// time — the operator's own resolution was `…\WinGet\Packages\Anthropic.ClaudeCode_…\claude.exe`, one
// directory shape away from `WinGet\Links\claude-<something>.exe`. The scan tolerates that; a fixed name
// probe cannot.
func TestClaudeResolvesAnAliasLauncher(t *testing.T) {
	env := newFakeWindowsEnv(t)
	links := filepath.Join(env.root, "AppData", "Local", "Microsoft", "WinGet", "Links")
	if err := os.MkdirAll(links, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(links, "claude-code.exe")
	if err := os.WriteFile(want, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A decoy in the same directory that must NOT be chosen: wrong prefix.
	if err := os.WriteFile(filepath.Join(links, "claudette.exe"), []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := claudeBinaryPathFor("windows", env.getenv); got != want {
		t.Fatalf("ClaudeBinaryPath = %q, want the alias launcher %q", got, want)
	}
}

// TestTheAliasScanIgnoresNonExecutables pins the bound: the scan is a name-set filter, not "take the first
// file in the directory".
func TestTheAliasScanIgnoresNonExecutables(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"claude.txt", "claude.cmd", "claude.ps1", "notes.exe", "CLAUDE-REAL.EXE"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := claudeExesIn(dir)
	if len(got) != 1 || !strings.HasSuffix(strings.ToLower(got[0]), "claude-real.exe") {
		t.Fatalf("claudeExesIn = %v, want exactly the claude-*.exe (case-insensitive, .exe only)", got)
	}
}
