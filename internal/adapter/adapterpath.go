package adapter

// adapterpath.go — WHERE AN ADAPTER CLI IS, ON EVERY PLATFORM, WITHOUT A CYCLE.

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// canonicalKind lowercases a kind so a caller spelling it "OpenCode" or "CLAUDE" resolves the same
// entry.
func canonicalKind(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}

// BinEnv names the environment override for a kind, or "" when the kind has none.
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
		// THE NPM GLOBAL PREFIXES, and the two package names in use.
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

// winGetPackagePrefix is the directory-name prefix WinGet gives a kind's package, or "" when it has
// none.
func winGetPackagePrefix(kind string) string {
	switch canonicalKind(kind) {
	case KindClaude:
		return "Anthropic.ClaudeCode"
	}
	return ""
}

// ResolveBinary returns the adapter executable for a kind, or "" when none was found. IT NEVER
// RETURNS A SHIM.
func ResolveBinary(goos, home, kind string, getenv func(string) string) string {
	// THE FULL ORDER: an explicit override, then what the OS's PATH reports, then the known-install
	// lists.
	if p := ResolveBinaryDeclared(goos, home, kind, getenv); p != "" {
		return p
	}
	if home == "" {
		return ""
	}
	for _, p := range DiscoverKind(goos, home, kind, getenv) {
		return p
	}
	return ""
}

// ResolveBinaryDeclared is the OVERRIDE-plus-known-lists resolver, without discovery.
func ResolveBinaryDeclared(goos, home, kind string, getenv func(string) string) string {
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
	p, _ := resolveKnownInner(goos, home, kind, getenv)
	return p
}

// resolveKnown is the known-install probe, ignoring the override (which the candidate set reports separately so
// its provenance is its own).
func resolveKnown(home, kind, goos string, getenv func(string) string) string {
	p, _ := resolveKnownInner(goos, home, kind, getenv)
	// If the override named the same file, it is not a separate "known" hit.
	if e := BinEnv(kind); e != "" && getenv != nil {
		if v := strings.TrimSpace(getenv(e)); v != "" && v == p {
			return ""
		}
	}
	return p
}

// resolveKnownInner is the candidate-list search: declared dir, conventional dirs, WinGet packages.
func resolveKnownInner(goos, home, kind string, getenv func(string) string) (string, bool) {
	// NO HOME MEANS NO ADAPTER, not "whatever this process happens to reach".
	if strings.TrimSpace(home) == "" {
		return "", false
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
			return cand, true
		}
	}
	return "", false
}

// exesIn returns the executables in dir whose stem is the kind or the kind followed by "-".
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

// ShimHint reports a shell shim that WOULD have been resolved, naming it and the override that
// fixes it.
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

// ContainerUsable reports whether a resolved adapter path can execute INSIDE a runtime container,
// and why not when it cannot.
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

// THE GATES HERE ARE THE ONLY PLACE THIS IS DECIDED, and every caller reads THEM rather than
// re-deriving them.
func PreferHostAdapterInContainer(hostGOOS, hostBin string) (bool, string) {
	if strings.TrimSpace(hostBin) == "" {
		return false, "no host install was resolved, so the image's CLI is the only route"
	}
	if ok, why := ContainerUsable(hostBin, "linux"); !ok {
		return false, why
	}
	if goos := strings.TrimSpace(hostGOOS); goos != "" && goos != "linux" {
		return false, "the host install is a " + goos + " binary and runtime containers are Linux"
	}
	return true, ""
}

// HostGOOS and HostGetenv are here so a caller with an injected platform (a test) and a caller without one
// (production) read the same defaults through a single pair of names.
func HostGOOS() string { return runtime.GOOS }
