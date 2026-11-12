package opencode

// binary.go — resolving the opencode CLI to spawn, in ONE place.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	goruntime "runtime"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// OpenCodeBinEnv overrides the resolved opencode binary.
const OpenCodeBinEnv = "ORCHICON_OPENCODE_BIN"

// THE FIRST ONE IS THE ONLY ONE THAT WORKS EVERYWHERE, and that is worth stating plainly because
// the difference is invisible at the call site.
var openCodeRelCandidates = []string{
	filepath.Join(".opencode", "bin", "opencode"),
	filepath.Join(".local", "bin", "opencode"),
	filepath.Join(".npm-global", "bin", "opencode"),
	filepath.Join(".bun", "bin", "opencode"),
}

// systemWideOpenCode are the locations a SYSTEM-WIDE install lands in — the npm global prefix under
// sudo, or a distro package. THEY ARE DIAGNOSTIC ONLY and are never returned as a resolution.
var systemWideOpenCode = []string{
	filepath.Join("/usr", "local", "bin", "opencode"),
	filepath.Join("/usr", "bin", "opencode"),
}

// openCodePlatformCandidates returns ABSOLUTE candidate paths for the install locations a given
// platform puts opencode in that the $HOME list above does not cover — EMPTY on Unix.
func openCodePlatformCandidates(goos string, getenv func(string) string) []string {
	if goos != "windows" {
		return nil
	}
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	var out []string
	// add takes the BASE SEPARATELY, because filepath.Join("", ".opencode", "bin") is ".opencode/bin" —
	// a RELATIVE path, which os.Stat resolves against the process's own working directory.
	add := func(base string, parts ...string) {
		if base == "" {
			return
		}
		out = append(out, filepath.Join(append([]string{base}, parts...)...)+string(os.PathSeparator)+"opencode.exe")
	}
	// The per-user installs first: they need no elevation, so they are what a desktop machine has.
	add(env("USERPROFILE"), ".opencode", "bin")
	add(env("LOCALAPPDATA"), "Programs", "opencode")
	add(env("USERPROFILE"), "scoop", "shims")
	add(env("LOCALAPPDATA"), "Microsoft", "WinGet", "Links")
	// NPM AND BUN GLOBALS ON WINDOWS, which the list above misses and which are the two most likely
	// places a `npm i -g opencode-cli` or `bun add -g` lands.
	add(env("APPDATA"), "npm")
	add(env("USERPROFILE"), ".bun", "bin")

	// WHERE AN NPM GLOBAL INSTALL ACTUALLY PUTS THE EXECUTABLE. This is the case that was still
	// failing after the first pass, reported with the operator's own install path:
	//
	//	"Opencode is currently installed here C:\Users\operator\AppData\Roaming\npm\opencode.ps1"
	//
	// THAT PATH IS A SHIM, NOT A BINARY, and on Windows npm makes three of them (`opencode`,
	// `opencode.cmd`, `opencode.ps1`) — never an `.exe`. So the directory above yielded shims only,
	// the shim is correctly refused as unlaunchable, and resolution fell through to nothing. The
	// operator was told to install something they had installed.
	//
	// THE REAL BINARY IS ONE LEVEL DEEPER AND ENTIRELY DETERMINISTIC. The npm package `opencode-ai`
	// declares `"bin": {"opencode": "bin/opencode.exe"}` and its postinstall copies the 211 MB
	// platform binary from `opencode-windows-x64` over the 479-byte stub that ships in the tarball.
	// So a working global install has a real executable at:
	//
	//	<npm prefix>\node_modules\opencode-ai\bin\opencode.exe
	//
	// npm's global prefix on Windows is %APPDATA%\npm per-user, or the node install directory
	// (%ProgramFiles%\nodejs) for an all-users install. pnpm's is %LOCALAPPDATA%\pnpm.
	//
	// A POSTINSTALL THAT NEVER RAN LEAVES THE 479-BYTE STUB in that same place. Resolving it would be
	// worse than not resolving at all — it prints an error and exits — which is why the stub's size is
	// not what we test for (a future version could differ) but why the not-found message names the
	// `--ignore-scripts` cause explicitly. See npmBinaryHint.
	// THE PUBLISHED NAMES, for the case where NO shim survives (deleted by hand, or a PATH stripped
	// before npm ran). Two, because the package has been renamed in the wild and both are in use:
	//   opencode-ai   the unscoped name
	//   @opencode/cli the scoped name the reporting operator actually has (246 MB binary at
	//                 node_modules\@opencode\cli\bin\opencode.exe)
	// The shim parse above is the general answer; these are the fallback for when there is no shim to
	// read, so the list has to be kept in step with reality by hand.
	for _, prefix := range npmGlobalPrefixes(env) {
		add(prefix, "node_modules", "opencode-ai", "bin")
		add(prefix, "node_modules", "@opencode", "cli", "bin")
	}

	// Then the machine-wide ones, for an all-users install.
	add(env("ProgramFiles"), "opencode")
	add(env("ProgramFiles(x86)"), "opencode")
	return out
}

// npmGlobalPrefixes names the directories npm (or pnpm) uses as its GLOBAL prefix on Windows,
// per-user first, because that is what needs no elevation and is what a desktop machine has.
func npmGlobalPrefixes(env func(string) string) []string {
	var out []string
	for _, p := range []string{
		joinIfBase(env("APPDATA"), "npm"),
		joinIfBase(env("USERPROFILE"), "AppData", "Roaming", "npm"),
		joinIfBase(env("ProgramFiles"), "nodejs"),
		joinIfBase(env("ProgramFiles(x86)"), "nodejs"),
		joinIfBase(env("LOCALAPPDATA"), "pnpm"),
	} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// joinIfBase joins parts under base, returning "" when base is empty so a missing environment variable can
// never produce a relative path.
func joinIfBase(base string, parts ...string) string {
	if base == "" {
		return ""
	}
	return filepath.Join(append([]string{base}, parts...)...)
}

// openCodeShimCandidates names the package-manager SHIMS the plane can find but cannot launch, so the error
// message can point at the file the operator actually installed rather than repeating "install opencode".
func openCodeShimCandidates(goos string, getenv func(string) string) []string {
	if goos != "windows" {
		return nil
	}
	env := func(k string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(k))
	}
	var out []string
	// %APPDATA%\npm IS THE NPM GLOBAL PREFIX ON WINDOWS, and this list used to name %APPDATA% itself —
	// one directory too high.
	for _, dir := range []string{
		joinIfBase(env("APPDATA"), "npm"),
		env("APPDATA"),
		joinIfBase(env("USERPROFILE"), "scoop", "shims"),
	} {
		if dir == "" {
			continue
		}
		for _, name := range []string{"opencode.cmd", "opencode.bat", "opencode.ps1", "opencode"} {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// lookupEnvFold reads an environment variable case-INSENSITIVELY, which is how Windows names them.
func lookupEnvFold(getenv func(string) string, name string) string {
	if getenv == nil || name == "" {
		return ""
	}
	if v := getenv(name); v != "" {
		return v
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// npmExeFromShims returns the real executables implied by npm shims that ACTUALLY EXIST on this machine.
//
// IT PARSES THE SHIM'S TARGET rather than guessing a package name, and that is the correction the
// operator's own machine forced. The first version assumed the package was `opencode-ai` and probed
// `node_modules/opencode-ai/bin/opencode.exe`. Their real install is `@opencode/cli`, so the guess
// missed a 246 MB executable that was sitting right there:
//
//	C:\Users\operator\AppData\Roaming\npm\node_modules\@opencode\cli\bin\opencode.exe
//
// GUESSING A PACKAGE NAME IS THE WRONG SHAPE OF ANSWER, because the name is not this plane's to know:
// it has already changed once, it is scoped, and a distributor could publish it under anything. The
// SHIM is the authority — npm generates it from the package's own `bin` entry, so it names the exact
// file that `npm i -g` intended to be runnable:
//
//	opencode.cmd   "%dp0%\node_modules\@opencode\cli\bin\opencode.exe"  %*
//	opencode.ps1   & "$basedir/node_modules/@opencode/cli/bin/opencode.exe" $args
//	opencode       exec "$basedir/node_modules/@opencode/cli/bin/opencode.exe" "$@"
//
// All three spell the same path, so ONE parser covers .cmd, .ps1 and the POSIX shim.
//
// Empty when no shim exists or none names an executable, so it costs one small read per known shim
// location and nothing else.
func npmExeFromShims(goos string, getenv func(string) string) []string {
	if goos != "windows" {
		return nil
	}
	var out []string
	for _, shim := range openCodeShimCandidates(goos, getenv) {
		st, err := os.Stat(shim)
		if err != nil || st.IsDir() {
			continue
		}
		if target := npmTargetFromShim(shim); target != "" {
			out = append(out, target)
		}
	}
	return out
}

// npmTargetFromShim reads an npm shim and returns the executable it points at, or "" when it names
// none. ONLY AN EXECUTABLE IS RETURNED.
func npmTargetFromShim(shim string) string {
	b, err := os.ReadFile(shim)
	if err != nil || len(b) == 0 || len(b) > 64<<10 {
		return "" // unreadable, or not plausibly a shim
	}
	text := string(b)
	i := strings.Index(text, "node_modules")
	if i < 0 {
		return ""
	}
	rest := text[i:]
	// The path ends at the closing quote, at whitespace, or at end of line — whichever comes first.
	if j := strings.IndexAny(rest, "\"'\r\n\t "); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return ""
	}
	// THE SHIMS DISAGREE ABOUT SEPARATORS, so both are normalised before the join.
	target := filepath.Join(filepath.Dir(shim), filepath.FromSlash(strings.ReplaceAll(rest, `\`, "/")))
	if !strings.EqualFold(filepath.Ext(target), ".exe") {
		return ""
	}
	return target
}

// A CANDIDATE MUST EXIST AND BE A REGULAR FILE; anything else falls through rather than handing a
// spawn a path that cannot run.
func resolveOpenCodeBinary() (string, error) {
	return resolveOpenCodeBinaryFor(runtimeGOOS(), os.Getenv, nil)
}

// resolveOpenCodeBinaryFor is the resolver with the OS, the environment and the persisted-PATH
// directories INJECTABLE, and it is the real implementation.
func resolveOpenCodeBinaryFor(goos string, getenv func(string) string, registryDirs []string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := strings.TrimSpace(getenv(OpenCodeBinEnv)); v != "" {
		if st, err := os.Stat(v); err != nil || st.IsDir() {
			return "", fmt.Errorf("%s is set to %q, which is not an existing file — fix or unset it%s", OpenCodeBinEnv, v, misplacedOpenCodeHintFor(goos, getenv))
		}
		return v, nil
	}
	if p, err := exec.LookPath("opencode"); err == nil && launchableResolved(goos, p) {
		return p, nil
	}
	if home := hostHome(); home != "" {
		for _, rel := range openCodeRelCandidates {
			cand := filepath.Join(home, rel)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand, nil
			}
		}
	}
	// THE PLATFORM PASS: where THIS OS installs opencode beyond the POSIX $HOME shapes above. Empty on Unix.
	for _, cand := range openCodePlatformCandidates(goos, getenv) {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	// THE NPM INSTALL IMPLIED BY A SHIM THAT IS REALLY THERE, before the persisted PATH: this is a
	// known package layout rather than a directory that happens to be on PATH.
	for _, cand := range npmExeFromShims(goos, getenv) {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	// THE PERSISTED PATH, LAST.
	if registryDirs == nil {
		registryDirs = registryPathDirs()
	}
	for _, dir := range registryDirs {
		cand := filepath.Join(dir, "opencode"+executableExt())
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	// THE AGNOSTIC LAST RESORT: ask the OS's own PATH — from its SOURCE, which is what the loop above
	// reads on Windows — and let Go's LookPath apply the platform's extension rules.
	if home, herr := os.UserHomeDir(); herr == nil {
		if hits := adapter.DiscoverKind(goos, home, adapter.DefaultAdapterKind, getenv); len(hits) > 0 {
			return hits[0], nil
		}
	}
	return "", fmt.Errorf("opencode binary not found on PATH or in any known install location (%s)%s%s (install it as your user, never with sudo: curl -fsSL https://opencode.ai/install | bash; set %s to point at an existing install; set ORCHICON_SIMULATE_ADAPTER=1 only for offline dev)",
		strings.Join(openCodeRelCandidates, ", "), misplacedOpenCodeHintFor(goos, getenv), foundElsewhereHintFor(goos, getenv), OpenCodeBinEnv)
}

// OpenCodeBinDirs returns the host directories the resolver probes for opencode, in probe order, as
// ABSOLUTE paths for the given home.
func OpenCodeBinDirs(home string) []string {
	if strings.TrimSpace(home) == "" {
		return nil
	}
	seen := make(map[string]struct{}, len(openCodeRelCandidates))
	out := make([]string, 0, len(openCodeRelCandidates))
	for _, rel := range openCodeRelCandidates {
		dir := filepath.Dir(filepath.Join(home, rel))
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		out = append(out, dir)
	}
	return out
}

// ResolveBinaryPath is the exported resolver, for callers OUTSIDE the adapter — the plane's
// discovery wiring, which must resolve the same binary the spawn path will.
func ResolveBinaryPath() (string, error) { return resolveOpenCodeBinary() }

// ContainerVisible reports whether a resolved opencode path can be reached from inside a runtime
// container.
func ContainerVisible(path string) bool {
	home := hostHome()
	if home == "" {
		return true
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	for _, root := range runtime.AdapterHostMountRoots(home, adapter.DefaultAdapterKind) {
		if abs == root || strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// misplacedOpenCodeHint names an opencode install that EXISTS but is not usable by the plane's
// runtime containers, and says what to do about it.
func misplacedOpenCodeHint() string { return misplacedOpenCodeHintFor(runtimeGOOS(), os.Getenv) }

// misplacedOpenCodeHintFor is misplacedOpenCodeHint with the OS and environment injectable, matching
// the rest of this file's seams.
func misplacedOpenCodeHintFor(goos string, getenv func(string) string) string {
	for _, p := range systemWideOpenCode {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		return fmt.Sprintf(" — NOTE: opencode IS installed at %s, but the plane's runtime containers mount only opencode's paths under $HOME, so workflow runs could never execute it. A `sudo npm i -g opencode` (which writes to the npm prefix under /usr/local) is the usual cause; install it as your user instead: curl -fsSL https://opencode.ai/install | bash", p)
	}
	return ""
}

// foundElsewhereHint names an opencode the plane FOUND but could not use, in terms the operator can
// act on.
func foundElsewhereHint() string {
	return foundElsewhereHintFor(runtimeGOOS(), os.Getenv)
}

// foundElsewhereHintFor is foundElsewhereHint with the OS and environment injectable, for the same
// reason openCodePlatformCandidates takes them.
func foundElsewhereHintFor(goos string, getenv func(string) string) string {
	for _, p := range openCodeShimCandidates(goos, getenv) {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(p), ".exe") {
			continue // a real executable here would have resolved; this is for the shims
		}
		// NAME THE REAL EXECUTABLE, not just the problem.
		real := filepath.Join(filepath.Dir(p), "node_modules", "opencode-ai", "bin", "opencode.exe")
		return fmt.Sprintf(" — NOTE: opencode IS installed at %s, but that is an npm SHIM and the plane "+
			"cannot launch one directly: Windows needs cmd.exe for a .cmd/.ps1, and the real executable "+
			"is what the shim wraps. The plane looks for %q as well. If that file is absent, npm's "+
			"postinstall never ran — it is what copies the platform binary over the 479-byte stub — so "+
			"reinstall without --ignore-scripts: npm i -g opencode-ai",
			p, real)
	}
	return ""
}

// launchableResolved reports whether a path LookPath returned can actually be EXECUTED here, rather
// than merely found.
func launchableResolved(goos, p string) bool {
	if goos != "windows" {
		return true
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".cmd", ".bat", ".ps1":
		// Findable, not runnable. Falling through lets the candidate tables and the registry pass
		// run, and foundElsewhereHint names the shim with the remedy that works.
		return false
	}
	return true
}

// executableExt is the suffix a spawned executable needs on this platform: ".exe" on Windows, and
// EMPTY everywhere else, where an extension is not part of the name.
func executableExt() string {
	if runtimeGOOS() == "windows" {
		return ".exe"
	}
	return ""
}

// runtimeGOOS is a seam so the platform tables can be tested for Windows from any host: the tables
// are pure functions of the OS name.
var runtimeGOOS = func() string { return goruntime.GOOS }

// hostHome is os.UserHomeDir with the trim/emptiness handling every caller here
// needs. An empty home means 'no scoped candidates and no visibility check'.
func hostHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(home)
}
