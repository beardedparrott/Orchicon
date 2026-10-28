package claude

// claude_paths.go — WHERE CLAUDE CODE ACTUALLY IS, PER PLATFORM.
//
// WHY THIS FILE EXISTS. The resolver beside it probes ONE path — `$HOME/.local/bin/claude` — and then
// falls back to a bare `claude` on PATH. That is a POSIX install shape. Reported from a Windows host,
// with the operator's own `Get-Command claude` output:
//
//	Source
//	------
//	C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe\claude.exe
//
// A real executable, in a WinGet package directory — a path the POSIX probe never looks at. And the bare
// `claude` fallback cannot rescue it either for the same reason opencode documents at length: the plane is
// a long-lived detached process, so its PATH is the one it was STARTED with, and an install that happened
// afterwards is invisible to it.
//
// opencode already solved this exact class of problem (internal/opencode/binary.go), including the two
// Windows-specific traps this mirrors: probe CONVENTIONAL INSTALL LOCATIONS directly, and never resolve a
// shell shim (`.cmd`/`.bat`/`.ps1`) because Go cannot execute one — that is a discovery that succeeds and
// a dispatch that dies somewhere else.
//
// IT NEVER RESOLVES A SHIM, AND THAT IS DELIBERATE. An npm-style install of a Node-based CLI lands
// `claude.cmd`/`claude.ps1` shims rather than an executable. Reporting "found" for one of those would move
// the failure to the first execution, where the operator has far less context. They are DIAGNOSED instead
// (claudeShimHint), naming the real binary beside them and why it may be missing.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// claudeWindowsCandidates lists the places a Windows claude CODE CLI lands, per-user first because that
// needs no elevation and is what a desktop machine has.
//
// EACH ENTRY IS EVIDENCE, not a guess: the WinGet shape is the operator's own reported path (with the
// package-directory suffix that WinGet appends), %LOCALAPPDATA%\Programs is where a per-user installer
// puts things, and %ProgramFiles% covers an all-users install. The bare home candidates mirror the POSIX
// list so a Windows machine that uses a Unix-style layout still resolves.
func claudeWindowsCandidates(goos string, getenv func(string) string) []string {
	if goos != "windows" {
		return nil
	}
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	join := func(base string, parts ...string) string {
		if base == "" {
			return ""
		}
		return filepath.Join(append([]string{base}, parts...)...) + string(os.PathSeparator) + "claude.exe"
	}
	var out []string
	// WINGET. The package directory name embeds a source and a hash, so it is found by WALKING the WinGet
	// Packages root for a directory whose name starts with the publisher prefix — the exact shape in the
	// operator's report. The non-package spellings are also probed: WinGet's "Links" shim directory holds
	// an alias, and a hand-installed copy may sit directly under the WinGet root.
	out = append(out, claudeWinGetCandidates(env)...)
	out = append(out, join(env("LOCALAPPDATA"), "Microsoft", "WinGet", "Links"))
	out = append(out, join(env("LOCALAPPDATA"), "Programs", "claude"))
	out = append(out, join(env("ProgramFiles"), "Claude"))
	out = append(out, join(env("ProgramFiles(x86)"), "Claude"))
	// DIRECTORIES WHOSE CONTENTS ARE SCANNED, not probed for one fixed name. A fixed name assumes the
	// install layout we guessed — which is exactly the assumption that produced "installed but not
	// found". An alias/MSIX-style install puts its launcher in one of these with a name we cannot
	// enumerate ahead of time, so the scan is bounded by an explicit name set instead.
	for _, dir := range claudeAliasDirs(env) {
		out = append(out, claudeExesIn(dir)...)
	}
	// A Unix-style layout on Windows still resolves (WSL users, a portable install).
	if home := env("USERPROFILE"); home != "" {
		out = append(out, filepath.Join(home, ".local", "bin", "claude"))
		out = append(out, filepath.Join(home, ".claude", "local", "claude"))
	}
	// Drop the empties so a missing variable cannot contribute a relative path.
	kept := out[:0]
	for _, p := range out {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return kept
}

// claudeWinGetPackagePrefix is the directory-name prefix WinGet gives the Claude Code package.
//
// MEASURED from the operator's own output: `Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe`.
// The prefix is stable (publisher.package); the suffix is the source and a hash, which is why the match is
// a prefix rather than an equality.
const claudeWinGetPackagePrefix = "Anthropic.ClaudeCode"

// claudeWinGetCandidates finds the executable inside a WinGet PACKAGE directory.
//
// The package directory is content-addressed, so it cannot be spelled out — it is enumerated. A missing
// WinGet root is not an error: it simply contributes nothing, and the caller falls through to the other
// locations.
func claudeWinGetCandidates(env func(string) string) []string {
	root := env("LOCALAPPDATA")
	if root == "" {
		return nil
	}
	packages := filepath.Join(root, "Microsoft", "WinGet", "Packages")
	entries, err := os.ReadDir(packages)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), claudeWinGetPackagePrefix) {
			continue
		}
		// The executable can be at the package root or one level in, depending on how the package lays
		// itself out. Two stats per package is cheap and covers both.
		base := filepath.Join(packages, e.Name())
		out = append(out, filepath.Join(base, "claude.exe"))
		inner, ierr := os.ReadDir(base)
		if ierr != nil {
			continue
		}
		for _, sub := range inner {
			if sub.IsDir() {
				out = append(out, filepath.Join(base, sub.Name(), "claude.exe"))
			}
		}
	}
	return out
}

// claudeShimHint reports a shim that WOULD have been resolved and explains where the real binary is.
//
// It exists for the same reason opencode's foundElsewhereHint does: "not found" is a lie when the operator
// can see `claude` in their shell. Naming the shim, and the executable it stands for, turns an
// install-something-you-already-installed message into an actionable one.
func claudeShimHint(goos string, getenv func(string) string) string {
	if goos != "windows" {
		return ""
	}
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	var dirs []string
	// The three places a Node-based CLI lands its shims on Windows, per-user only: an all-users install
	// would already have resolved as a real executable.
	for _, spec := range [][]string{
		{env("APPDATA"), "npm"},
		{env("LOCALAPPDATA"), "pnpm"},
		{env("USERPROFILE"), "scoop", "shims"},
	} {
		if spec[0] == "" {
			continue
		}
		dirs = append(dirs, filepath.Join(spec...))
	}
	for _, dir := range dirs {
		for _, name := range []string{"claude.cmd", "claude.bat", "claude.ps1", "claude"} {
			p := filepath.Join(dir, name)
			st, err := os.Stat(p)
			if err != nil || st.IsDir() {
				continue
			}
			if strings.EqualFold(filepath.Ext(p), ".exe") {
				continue // a real executable here would have resolved
			}
			return " — NOTE: claude IS installed at " + p + ", but that is a shell SHIM and the plane " +
				"cannot execute it directly (Go cannot launch a .cmd/.bat/.ps1). Point " + ClaudeBinEnv +
				" at the real executable, or install the CLI where it puts one."
		}
	}
	return ""
}

// claudeAliasDirs are the directories whose CONTENTS are scanned for an executable, rather than probed
// for one fixed file name.
//
// WHY A SCAN. A fixed file name assumes the install is the layout we guessed, and that assumption is what
// produced "installed but not found" in the first place. An MSIX/alias-style install (which is what a
// `Get-Command claude` resolving into an AppData package path looks like) can name its launcher something
// we cannot enumerate ahead of time. The scan is BOUNDED — a small explicit directory list and an explicit
// name set — so it stays deterministic rather than hunting the filesystem.
func claudeAliasDirs(env func(string) string) []string {
	var dirs []string
	if root := env("LOCALAPPDATA"); root != "" {
		dirs = append(dirs,
			filepath.Join(root, "Microsoft", "WinGet", "Links"),
			filepath.Join(root, "Microsoft", "WindowsApps"),
		)
	}
	if pf := env("ProgramFiles"); pf != "" {
		dirs = append(dirs, filepath.Join(pf, "Claude"))
	}
	if pf := env("ProgramFiles(x86)"); pf != "" {
		dirs = append(dirs, filepath.Join(pf, "Claude"))
	}
	return dirs
}

// claudeExesIn returns the executables in dir named claude or claude-<something>.
//
// ONLY .exe: a shim is never a candidate (the file header explains why Go cannot run one). SORTED, so a
// machine with two installs resolves the same one every time instead of depending on directory order.
func claudeExesIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if !strings.HasSuffix(name, ".exe") {
			continue
		}
		stem := strings.TrimSuffix(name, ".exe")
		if stem != "claude" && !strings.HasPrefix(stem, "claude-") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}
