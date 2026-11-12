package adapter

// adapterdiscover.go — ASK THE OS WHERE THE BINARY IS, INSTEAD OF LISTING INSTALLS WE KNOW.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FreshPathDirs returns the directories the OS says are on PATH, read from the SOURCE rather than
// from this process's copy.
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

// DiscoverKind returns every USABLE binary for a kind that the OS's PATH knows about, deduplicated,
// in search order.
func DiscoverKind(goos, home, kind string, getenv func(string) string) []string {
	base := canonicalKind(kind)
	if base == "" {
		return nil
	}
	// THE NATIVE ADAPTER IS ORCHICON ITSELF, and it must never be discovered.
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

// Candidates returns every place a kind's binary can be found, WITH provenance, so the caller can
// show the operator what exists and let them choose.
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

// Ambiguous reports whether the candidate set needs a human decision: more than one distinct
// binary. TWO BINARIES IS THE CASE THAT MUST NOT BE RESOLVED SILENTLY.
func Ambiguous(cands []Candidate) bool { return len(cands) > 1 }
