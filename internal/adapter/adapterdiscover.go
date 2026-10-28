package adapter

// adapterdiscover.go — ASK THE OS WHERE THE BINARY IS, INSTEAD OF LISTING INSTALLS WE KNOW.
//
// THE PRINCIPLE. Orchicon is a tool that CALLS other tools. It should meet them where they are, not require
// them to be where we guessed. Every candidate list in this package is evidence about what SOME machine
// looked like: the operator's npm global, their WinGet package. That is a treadmill — Chocolatey, nix,
// mise, a portable zip, a corporate image each need a new entry, and the entry is always added AFTER
// somebody reports the miss.
//
// So this file does the two things the lists cannot:
//
//  1. IT ASKS THE OS. `exec.LookPath` is the `which` algorithm, implemented in-process — the same one the
//     shell uses, with the right per-platform rules (PATHEXT on Windows, the exec bit on POSIX). Shelling
//     out to `which` would be worse in three ways: it is not always installed (minimal images, NixOS), it is
//     a separate process with its own environment so it can resolve a different binary than we execute, and
//     on some systems it is a script with its own quirks.
//
//  2. IT USES A PATH THAT IS NOT A SNAPSHOT. This is the part that actually fixes the reported bug. LookPath
//     reads the PROCESS's PATH, and a long-lived plane's PATH is whatever the process that started it had —
//     so an operator who installs opencode afterwards has a shell that finds it and a plane that does not.
//     "I reinstalled and it still says not found" is that, exactly. The fix is to read the PATH from its
//     SOURCE (see FreshPathDirs) and search that alongside our own.
//
// AND IT STILL REFUSES SHIMS, because LookPath does not. On Windows LookPath will happily return
// `opencode.cmd` — a file Go cannot execute — so a discovery built on LookPath alone would find the adapter
// and then fail at the first dispatch. Every hit is checked before it is offered.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FreshPathDirs returns the directories the OS says are on PATH, read from the SOURCE rather than from this
// process's copy.
//
// WINDOWS reads HKCU\Environment and the machine-wide Path (see freshpath_windows.go): that list IS the
// source, and a process keeps looking in its old one for as long as it lives. POSIX has no equivalent single
// store — a login shell composes PATH from profile files this process has already sourced — so the closest
// honest answer is the system configuration files plus our own PATH (see freshpath_other.go).
//
// THE RESULT IS APPENDED TO the process PATH, never a replacement: a process whose PATH carries something the
// system store does not (an injected dev shell, a CI harness, a test) keeps working.
func FreshPathDirs() []string { return freshPathDirsHook() }

// freshPathDirsHook is the platform seam, and a VARIABLE so a test can drive it from any host: a path table
// that can only be checked on the OS it describes is a table that will not be checked.
var freshPathDirsHook = freshPathDirs

// searchPath is the PATH a discovery search uses: our own first (fastest, and what the process was started
// with), then whatever the system store adds.
func searchPath(getenv func(string) string) string {
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	sep := string(os.PathListSeparator)
	seen := map[string]struct{}{}
	var dirs []string
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		key := strings.ToLower(d)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		dirs = append(dirs, d)
	}
	for _, d := range strings.Split(env("PATH"), sep) {
		add(d)
	}
	for _, d := range FreshPathDirs() {
		add(d)
	}
	return strings.Join(dirs, sep)
}

// execUsable reports whether a path is a file this process could actually START.
//
// THE SHIM CHECK IS THE POINT. exec.LookPath returns `.cmd`/`.bat`/`.ps1` on Windows because the CMD shell
// can run them; Go cannot (CreateProcess refuses, it needs cmd.exe). Offering one would be a discovery that
// succeeds and a dispatch that dies later, where the operator has least context.
func execUsable(p string) bool {
	if p == "" {
		return false
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".cmd", ".bat", ".ps1":
		return false
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	return true
}

// DiscoverKind returns every USABLE binary for a kind that the OS's PATH knows about, deduplicated, in search
// order.
//
// IT LOOKS FOR BOTH SPELLINGS. The bare name is what `which claude` answers, and the platform's own extension
// form (`claude.exe`) is what the OS store usually holds. LookPath handles the extension rules per platform,
// so both are asked for rather than one assumed.
//
// AN EMPTY RESULT IS NORMAL AND NOT AN ERROR: plenty of hosts have no adapter installed, and the caller decides
// whether that matters (a workflow whose sessions need no adapter does not care).
func DiscoverKind(goos, home, kind string, getenv func(string) string) []string {
	base := canonicalKind(kind)
	if base == "" {
		return nil
	}
	// THE NATIVE ADAPTER IS ORCHICON ITSELF, and it must never be discovered.
	//
	// Its binary IS this product — `/home/op/.local/bin/orchicon`, `%LOCALAPPDATA%\Programs\Orchicon\orchicon.exe`
	// — and it is bind-mounted into containers by the DAEMON from its own path (ContainerExe), not as an adapter
	// install. Discovering it would put the product binary on the container's adapter PATH and derive an
	// "adapter mount" for it, which is both wrong and noisy: found by TestAdapterPathPrefixPerKind, where the
	// native kind suddenly contributed ~/.local/bin.
	if base == "orchicon" {
		return nil
	}
	// THE OVERRIDE IS NOT A DISCOVERY. An explicit ORCHICON_*_BIN is a decision, reported by ResolveBinary,
	// and mixing it in here would make "what did the OS find" unanswerable.
	path := searchPath(getenv)
	var out []string
	seen := map[string]struct{}{}
	for _, name := range []string{base, binaryName(goos, kind)} {
		hit, err := lookPathIn(path, name)
		if err != nil || !execUsable(hit) {
			continue
		}
		key := strings.ToLower(filepath.Clean(hit))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, hit)
	}
	return out
}

// lookPathIn runs LookPath against an EXPLICIT PATH rather than the process's own.
//
// `exec.LookPath` is specified against os.Getenv("PATH"), so the mechanism is borrowed by setting PATH for
// the duration of the call. That is deliberate rather than clever: it means the per-platform extension rules
// (PATHEXT, the exec bit, the weird `foo.bat.exe` case) are Go's, not ours — the exact behaviours a
// hand-rolled search gets wrong.
//
// IT IS NOT SAFE FOR CONCURRENT USE, and does not need to be: resolution happens at process start and per
// session spawn, not on a hot path. A caller that ever needs concurrency should take the dirs and do its own
// join instead.
func lookPathIn(path, name string) (string, error) {
	prev, had := os.LookupEnv("PATH")
	if err := os.Setenv("PATH", path); err != nil {
		return "", err
	}
	defer func() {
		if had {
			_ = os.Setenv("PATH", prev)
			return
		}
		_ = os.Unsetenv("PATH")
	}()
	return exec.LookPath(name)
}

// Candidate describes one place an adapter was found, for the explicit surface.
type Candidate struct {
	// Path is the executable.
	Path string
	// Source says HOW it was found: "override", "discovered" (the OS's PATH), or "known" (a candidate list
	// entry). The operator needs this to judge between two hits.
	Source string
	// ContainerOK is false for a binary the runtime containers cannot execute (a Windows .exe in a Linux
	// container), with Why carrying the remedy.
	ContainerOK bool
	Why         string
}

// Candidates returns every place a kind's binary can be found, WITH provenance, so the caller can show the
// operator what exists and let them choose.
//
// ORDER IS BY CONFIDENCE, not by discovery: an explicit override, then what the OS reports, then the
// known-install list. A single candidate is unambiguous; two are exactly the case that must be SURFACED
// rather than silently resolved, because picking one for the operator is how "it works on my machine"
// happens.
func Candidates(goos, home, kind string, getenv func(string) string) []Candidate {
	var out []Candidate
	seen := map[string]struct{}{}
	add := func(path, source string) {
		if path == "" {
			return
		}
		key := strings.ToLower(filepath.Clean(path))
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		ok, why := ContainerUsable(path, "linux")
		out = append(out, Candidate{Path: path, Source: source, ContainerOK: ok, Why: why})
	}

	// 1. The override — an explicit decision.
	if e := BinEnv(kind); e != "" {
		if v := strings.TrimSpace(func() string {
			if getenv == nil {
				return ""
			}
			return getenv(e)
		}()); v != "" {
			add(v, "override")
		}
	}
	// 2. The OS's PATH — the agnostic answer.
	for _, p := range DiscoverKind(goos, home, kind, getenv) {
		add(p, "discovered")
	}
	// 3. The known-install lists — a fast path for the common shapes, and the only route where the OS store
	//    does not carry the install (a hand-placed binary in a directory nobody added to PATH).
	if home != "" {
		if p := resolveKnown(home, kind, goos, getenv); p != "" {
			add(p, "known")
		}
	}
	return out
}

// Ambiguous reports whether the candidate set needs a human decision: more than one distinct binary.
//
// TWO BINARIES IS THE CASE THAT MUST NOT BE RESOLVED SILENTLY. `claude` on PATH plus a different `claude.exe`
// in a WinGet package is two installs with different versions and different credentials, and picking one for
// the operator is how a session signs in as the wrong account.
func Ambiguous(cands []Candidate) bool { return len(cands) > 1 }
