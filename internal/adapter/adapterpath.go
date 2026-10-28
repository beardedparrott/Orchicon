package adapter

// adapterpath.go — WHERE AN ADAPTER CLI IS, ON EVERY PLATFORM, WITHOUT A CYCLE.
//
// WHY THIS IS NOT IN internal/opencode OR internal/claude. Both of those import internal/runtime (they drive
// it), so internal/runtime cannot import them back to ask "where is your binary?". The mount code that needs
// the answer therefore has no way to call either resolver — which is exactly how the container mounts ended up
// HARDCODED to `~/.opencode/bin` and `~/.local/bin` while each adapter's own host resolver grew a proper
// Windows candidate list beside it.
//
// So the resolution lives HERE: a package nobody depends on in the wrong direction, which:
//   - internal/runtime reads, to derive its mounts and its PATH (it already imports adapter);
//   - internal/opencode and internal/claude read, so the host-side resolution and the container-side mount
//     are computed from the SAME candidate list and cannot disagree.
//
// THAT LAST POINT IS THE FIX. The reported symptom — "opencode is installed but the plane says it is not" —
// was a host resolver that had learned about npm's `node_modules\opencode-ai\bin` while the mount still
// pointed at `~/.opencode/bin`. Two lists, two answers.
//
// AND THE SECOND TRAP IS THE ONE THAT MAKES CONTAINERS FAIL SILENTLY. What resolves on Windows is a PE
// executable. A Debian container cannot run it. Resolving "the adapter" and mounting it is therefore not
// enough for a container: the binary must be executable BY THE CONTAINER'S KERNEL. That is why every entry
// here is checked, and why an unusable one produces an ACTIONABLE error rather than a container that starts
// and then cannot find its adapter.

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// canonicalKind lowercases a kind so a caller spelling it "OpenCode" or "CLAUDE" resolves the same entry.
//
// The kind vocabulary is two constants (KindOpencode / KindClaude) and the model-ref parser is LEFT-GREEDY
// on case already, so this is a normalisation rather than a new concept — it exists so the three switches in
// this file cannot disagree about spelling.
func canonicalKind(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}

// BinEnv names the environment override for a kind, or "" when the kind has none.
//
// An explicit operator choice always wins in resolution, and it is the documented remedy in every error
// message — so it lives here rather than being rediscovered per package.
func BinEnv(kind string) string {
	switch canonicalKind(kind) {
	case DefaultAdapterKind:
		return "ORCHICON_OPENCODE_BIN"
	case KindClaude:
		return "ORCHICON_CLAUDE_BIN"
	}
	return ""
}

// binaryName is the executable's file name for a kind on a platform.
func binaryName(goos, kind string) string {
	base := canonicalKind(kind)
	if goos == "windows" {
		return base + ".exe"
	}
	return base
}

// declaredBinDir is where the MOUNT table has always assumed the CLI lives, kept only as the last
// candidate: an operator whose install matches it should not pay for the search.
func declaredBinDir(home, kind string) string {
	switch canonicalKind(kind) {
	case DefaultAdapterKind:
		return filepath.Join(home, ".opencode", "bin")
	case KindClaude:
		return filepath.Join(home, ".local", "bin")
	}
	return ""
}

// extraBinDirs are the kind-specific directories worth probing beyond the declared one.
//
// EACH ENTRY IS EVIDENCE, not a guess. The opencode entries come from that resolver's own history (the
// operator's `C:\...\AppData\Roaming\npm\opencode.ps1`, and the real `node_modules\opencode-ai\bin\opencode.exe`
// one level down); the claude entry is the WinGet package directory from the operator's `Get-Command claude`.
//
// A CYCLE IS NOT INTRODUCED BY DUPLICATING THESE SMALL LISTS — the alternative was leaving the mount
// hardcoded, which is what broke. When one of these moves, the resolver's own tests still catch it.
func extraBinDirs(goos, home, kind string, getenv func(string) string) []string {
	if goos != "windows" {
		return nil
	}
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	var dirs []string
	add := func(parts ...string) {
		if parts[0] == "" {
			return
		}
		dirs = append(dirs, filepath.Join(parts...))
	}
	switch canonicalKind(kind) {
	case DefaultAdapterKind:
		// THE NPM GLOBAL PREFIXES, and the two package names in use. The shim lives at the prefix root;
		// the executable one or two levels down.
		// npm's global prefix is %APPDATA%\npm; pnpm's is %LOCALAPPDATA%\pnpm; an all-users npm lives
		// under the node install. Built through `add` so an EMPTY base cannot produce a rooted `\npm`
		// that then passes the IsAbs check and resolves against the drive root.
		add(env("APPDATA"), "npm", "node_modules", "opencode-ai", "bin")
		add(env("APPDATA"), "npm", "node_modules", "@opencode", "cli", "bin")
		add(env("LOCALAPPDATA"), "pnpm", "node_modules", "opencode-ai", "bin")
		add(env("ProgramFiles"), "nodejs", "node_modules", "opencode-ai", "bin")
		// The npm prefix ROOT itself, where a package that ships a real .exe puts it.
		add(env("APPDATA"), "npm")
		add(env("USERPROFILE"), ".bun", "bin")
		add(env("LOCALAPPDATA"), "Programs", "opencode")
		add(env("LOCALAPPDATA"), "Microsoft", "WinGet", "Links")
		add(env("ProgramFiles"), "opencode")
	case KindClaude:
		add(env("LOCALAPPDATA"), "Microsoft", "WinGet", "Links")
		add(env("LOCALAPPDATA"), "Microsoft", "WindowsApps")
		add(env("LOCALAPPDATA"), "Programs", "claude")
		add(env("ProgramFiles"), "Claude")
		add(env("APPDATA"), "npm")
		add(env("USERPROFILE"), "scoop", "shims")
	}
	return dirs
}

// winGetPackageExes finds an executable inside a WinGet PACKAGE directory.
//
// WinGet's package directories are CONTENT-ADDRESSED (`publisher.package_source_hash`), so they cannot be
// spelled out — they are enumerated. This is the shape the operator's own `Get-Command claude` reported.
func winGetPackageExes(goos, home, kind string, getenv func(string) string) []string {
	if goos != "windows" {
		return nil
	}
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	root := env("LOCALAPPDATA")
	if root == "" {
		return nil
	}
	prefix := winGetPackagePrefix(kind)
	if prefix == "" {
		return nil
	}
	packages := filepath.Join(root, "Microsoft", "WinGet", "Packages")
	entries, err := os.ReadDir(packages)
	if err != nil {
		return nil
	}
	name := binaryName("windows", kind)
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		base := filepath.Join(packages, e.Name())
		out = append(out, filepath.Join(base, name))
		inner, ierr := os.ReadDir(base)
		if ierr != nil {
			continue
		}
		for _, sub := range inner {
			if sub.IsDir() {
				out = append(out, filepath.Join(base, sub.Name(), name))
			}
		}
	}
	sort.Strings(out)
	return out
}

// winGetPackagePrefix is the directory-name prefix WinGet gives a kind's package, or "" when it has none.
//
// MEASURED from the operator's own output: `Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe`.
// The prefix (publisher.package) is stable; the suffix is the source and a hash.
func winGetPackagePrefix(kind string) string {
	switch canonicalKind(kind) {
	case KindClaude:
		return "Anthropic.ClaudeCode"
	}
	return ""
}

// ResolveBinary returns the adapter executable for a kind, or "" when none was found.
//
// ORDER, each step load-bearing:
//
//  1. The kind's ORCHICON_*_BIN override — an explicit operator or deployment choice.
//  2. The DECLARED directory, so the common POSIX install pays for one stat.
//  3. The platform's conventional install directories, then the WinGet package directories.
//
// IT NEVER RETURNS A SHIM. Go cannot execute a .cmd/.bat/.ps1, so resolving one converts a clean "not found"
// into a dispatch that dies elsewhere — the failure that resolver's own comments describe. Shims are
// reported separately by ShimHint so "not found" is never a lie about an install the operator can see.
//
// IT RETURNS THE HOST PATH. Whether that path is usable INSIDE a container is a different question, answered
// by ContainerUsable — a Windows .exe is a perfectly good host adapter and cannot run in a Debian container.
func ResolveBinary(goos, home, kind string, getenv func(string) string) string {
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	if e := BinEnv(kind); e != "" {
		if v := env(e); v != "" {
			return v
		}
	}
	// NO HOME MEANS NO ADAPTER, not "whatever this process happens to reach".
	//
	// Every candidate below is derived from the caller's home, and an empty one would make the ORCHICON_*_BIN
	// override and the ambient environment decide the answer INSTEAD of the container's own view. That is
	// wrong in the direction that matters: the daemon asks this question about the home it mounts, and a
	// daemon with no home configured mounts no adapter — so an adapter found here would be mounted nowhere
	// and reported as present.
	if strings.TrimSpace(home) == "" {
		return ""
	}
	var candidates []string
	if dir := declaredBinDir(home, kind); dir != "" {
		candidates = append(candidates, filepath.Join(dir, binaryName(goos, kind)))
		// The declared dir is also scanned for a differently-named launcher, because a fixed name per
		// directory is the assumption that produced "installed but not found" in the first place.
		candidates = append(candidates, exesIn(dir, canonicalKind(kind))...)
	}
	for _, dir := range extraBinDirs(goos, home, kind, getenv) {
		candidates = append(candidates, filepath.Join(dir, binaryName(goos, kind)))
		candidates = append(candidates, exesIn(dir, canonicalKind(kind))...)
	}
	candidates = append(candidates, winGetPackageExes(goos, home, kind, getenv)...)

	for _, cand := range candidates {
		if cand == "" {
			continue
		}
		// NEVER A SHIM: the extension check is the guard, and it is not cosmetic.
		switch strings.ToLower(filepath.Ext(cand)) {
		case ".cmd", ".bat", ".ps1":
			continue
		}
		// ABSOLUTE, ALWAYS. os.Stat on a relative candidate resolves against the process's working
		// directory, which is the "found wherever the app happens to be running" bug.
		if !filepath.IsAbs(cand) {
			continue
		}
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

// exesIn returns the executables in dir whose stem is the kind or the kind followed by "-".
//
// BOUNDED (an explicit name set, .exe only) and SORTED, so two installs resolve the same one every time
// rather than by directory order.
func exesIn(dir, kind string) []string {
	if dir == "" {
		return nil
	}
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
		if stem != kind && !strings.HasPrefix(stem, kind+"-") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// ShimHint reports a shell shim that WOULD have been resolved, naming it and the override that fixes it.
//
// Empty when there is nothing to add. The purpose is narrow and worth stating: "claude was not found" is a
// lie when `claude` answers in the operator's shell, and the honest answer names the shim and why Go cannot
// launch it.
func ShimHint(goos, home, kind string, getenv func(string) string) string {
	if goos != "windows" {
		return ""
	}
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	// A directory is usable only if its BASE variable was set: filepath.Join("", "npm") is "npm", a
	// RELATIVE path that os.Stat would resolve against the process's own working directory.
	type dirSpec struct {
		base string
		sub  []string
	}
	specs := []dirSpec{
		{env("APPDATA"), []string{"npm"}},
		{env("LOCALAPPDATA"), []string{"pnpm"}},
		{env("USERPROFILE"), []string{"scoop", "shims"}},
	}
	base := canonicalKind(kind)
	for _, spec := range specs {
		if spec.base == "" {
			continue
		}
		dir := filepath.Join(append([]string{spec.base}, spec.sub...)...)
		for _, ext := range []string{".cmd", ".bat", ".ps1"} {
			p := filepath.Join(dir, base+ext)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return " — NOTE: " + base + " IS installed at " + p + ", but that is a shell SHIM and the " +
					"plane cannot execute it directly (Go cannot launch a .cmd/.bat/.ps1). Point " +
					BinEnv(kind) + " at the real executable."
			}
		}
	}
	return ""
}

// ContainerUsable reports whether a resolved adapter path can execute INSIDE a runtime container, and why
// not when it cannot.
//
// THIS IS THE CHECK THAT STOPS A SILENT CONTAINER FAILURE. A runtime container is Linux; a Windows `.exe`
// resolves perfectly on the host and is unrunnable inside. Without this, the mount succeeds, the PATH entry
// points at it, and the adapter dies at spawn — inside the container, far from the operator's command.
//
// The remedy is the operator's, and it is stated: install the adapter's LINUX build and point the kind's
// ORCHICON_*_BIN at it. Orchicon deliberately does not download or redistribute an adapter CLI (licensing:
// Claude Code's terms prohibit bundling), so this is a documented requirement rather than a missing feature.
func ContainerUsable(hostPath, containerGOOS string) (bool, string) {
	if hostPath == "" {
		return false, "no adapter binary was resolved on this host"
	}
	// The CONTAINER's kernel decides what runs, and the runtime image is Linux.
	_ = containerGOOS
	if strings.EqualFold(filepath.Ext(hostPath), ".exe") {
		return false, "the resolved adapter is a Windows executable (" + hostPath + "); runtime containers " +
			"are Linux and cannot execute it. Install the adapter's Linux build and point " +
			"ORCHICON_OPENCODE_BIN / ORCHICON_CLAUDE_BIN at it — Orchicon does not download or redistribute " +
			"an adapter CLI for licensing reasons"
	}
	return true, ""
}

// HostGOOS and HostGetenv are here so a caller with an injected platform (a test) and a caller without one
// (production) read the same defaults through a single pair of names.
func HostGOOS() string { return runtime.GOOS }
