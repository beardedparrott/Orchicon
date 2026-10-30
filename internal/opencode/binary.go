package opencode

// binary.go — resolving the opencode CLI to spawn, in ONE place.
//
// WHY THIS EXISTS. The same probe was written THREE times — the adapter's spawn
// path (adapter.go), the in-process host serve (servehost.go) and the plane's
// discovery wiring (internal/server/server.go) — and the three did NOT agree.
// Only the discovery copy honoured ORCHICON_OPENCODE_BIN, which is the variable
// the plane's OWN error message tells the operator to set:
//
//\tmodel discovery is not configured (set ORCHICON_OPENCODE_BIN or install opencode on PATH)
//
// So an operator who followed that instruction fixed DISCOVERY and left
// EXECUTION on a different binary — or on none at all. The failure is the
// expensive kind: nothing errors, the model picker starts offering models, and
// the first dispatch fails somewhere else for a reason that looks unrelated.
// One resolver, one candidate list, all three call sites.
//
// THE CANDIDATES ARE CONTAINER-VISIBLE ON PURPOSE, and that is a design
// constraint rather than a coincidence. opencode is spawned in two places, and
// only one of them can see the host's PATH:
//
//   - the HOST plane (the default residency) spawns it with the host PATH, and
//   - a RUNTIME CONTAINER sees ONLY what internal/runtime's adapter declaration
//     bind-mounts for the kind — for opencode that is ~/.opencode, plus
//     ~/.config/opencode and ~/.local/share/opencode as read-only data.
//
// A system-wide install (`sudo npm i -g opencode` lands in /usr/local/bin) is on
// the operator's PATH and so is resolvable by the host plane, but NO container
// mounts it — the container has its own /usr/local/bin. Resolving it there would
// hand the plane a binary its own workers cannot execute, so a PATH hit that is
// invisible from inside a container is accepted (it is a legitimate host
// install) and REPORTED rather than silently trusted; see ContainerVisible, and
// the misplacement hint the not-found error carries.

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

// OpenCodeBinEnv overrides the resolved opencode binary. It is the escape hatch
// the plane's discovery error has always advertised, and (before this file) the
// only one of the three probes that read it.
//
// It mirrors ORCHICON_CLAUDE_BIN (internal/claude/binary.go): an explicit
// operator or deployment choice wins over every probe.
const OpenCodeBinEnv = "ORCHICON_OPENCODE_BIN"

// openCodeRelCandidates are the per-$HOME install locations, in probe order.
//
// THE FIRST ONE IS THE ONLY ONE THAT WORKS EVERYWHERE, and that is worth stating
// plainly because the difference is invisible at the call site. ~/.opencode/bin is
// the official installer's location AND the path internal/runtime declares as a
// container mount (bootprofile.go), so a resolution there is usable by the host
// plane AND by every runtime container.
//
// THE OTHER THREE WIDEN HOST RESOLUTION ONLY. They are where a NON-SUDO
// npm/bun/manual install lands, and a runtime container does NOT mount them: its
// mount set is the adapter declaration, the git identity mounts and the project
// directory — never $HOME wholesale. So a hit there is real and useful (the plane
// discovers the models and the host serve can spawn the binary) and
// ContainerVisible reports it as host-only, which is what the boot warning in
// internal/server is for. Widening the CONTAINER mounts to cover them is a
// deliberate change to a guarded list and is not done here: a `-v` of ~/.local/bin
// exposes every other local binary to the sandbox, and an npm global's bin entry is
// a symlink into node_modules that would DANGLE if the link alone were mounted
// (the same trap internal/runtime's claude declaration documents).
var openCodeRelCandidates = []string{
	filepath.Join(".opencode", "bin", "opencode"),
	filepath.Join(".local", "bin", "opencode"),
	filepath.Join(".npm-global", "bin", "opencode"),
	filepath.Join(".bun", "bin", "opencode"),
}

// systemWideOpenCode are the locations a SYSTEM-WIDE install lands in — the npm
// global prefix under sudo, or a distro package.
//
// THEY ARE DIAGNOSTIC ONLY and are never returned as a resolution. See the file
// comment: the runtime containers mount nothing outside $HOME, so resolving one
// of these would let the plane discover a binary its own workers cannot execute.
// They exist so the not-found error can name the install the operator ACTUALLY
// has, instead of repeating 'install opencode' at someone who already did.
var systemWideOpenCode = []string{
	filepath.Join("/usr", "local", "bin", "opencode"),
	filepath.Join("/usr", "bin", "opencode"),
}

// openCodePlatformCandidates returns ABSOLUTE candidate paths for the install locations a given platform
// puts opencode in that the $HOME list above does not cover — EMPTY on Unix, where the $HOME list plus PATH is
// the whole story and the system prefixes are diagnostic only (see systemWideOpenCode).
//
// WHY THIS EXISTS. REPORTED on Windows: "I installed opencode desktop and opencode command line on the
// windows box and when I tried to select opencode models it claimed opencode was not on PATH."
//
// The $HOME candidates above are POSIX-SHAPED — they have no file extension — and `os.Stat` never appends one
// (only the command interpreter does). So on Windows every one of them missed a real install, whatever the
// operator did, while the plane's error told them to install something they had already installed.
//
// AND PATH ALONE IS NOT ENOUGH even when the file IS on it: the plane is a long-lived detached process, so its
// PATH is the one it was STARTED with. Installing opencode after the plane started leaves the plane looking
// somewhere it no longer exists — which is why the error says "restart the plane". Probing the KNOWN install
// directories directly means the common case does not require that restart at all.
//
// .EXE ONLY, DELIBERATELY NOT .CMD/.BAT. An npm global install on Windows lands a `opencode.cmd` shim, and Go
// cannot execute a .cmd directly (CreateProcess refuses it; it needs cmd.exe). Resolving the shim would be the
// exact failure this file exists to prevent — discovery starts offering models and the first dispatch dies
// somewhere else. The shim IS reported instead, by foundElsewhereHint, with the two remedies that work.
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
	// add takes the BASE SEPARATELY, because filepath.Join("", ".opencode", "bin") is ".opencode/bin" — a
	// RELATIVE path, which os.Stat resolves against the process's own working directory. An install that is
	// not there would then be "found" relative to wherever the app happens to be running, which is the same
	// class of bug as launching from the wrong directory: quietly correct on one machine and wrong on another.
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
	//
	// %APPDATA%\npm is the npm global prefix on Windows (%APPDATA%\Roaming\npm). It normally holds a
	// `.cmd` shim rather than an executable — that is what openCodeShimCandidates reports — but some
	// packages ship the real binary beside it, so it is probed as well as diagnosed. Probing a
	// directory that turns out to hold only a shim costs one failed os.Stat.
	//
	// %USERPROFILE%\.bun\bin mirrors the POSIX list's .bun/bin, which had no Windows counterpart at
	// all: bun's Windows install is a real .exe, so it resolves where npm's shim cannot.
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

// npmGlobalPrefixes names the directories npm (or pnpm) uses as its GLOBAL prefix on Windows, per-user
// first, because that is what needs no elevation and is what a desktop machine has.
//
// IT IS A LIST RATHER THAN A SINGLE GUESS because these move between installers: npm's default is
// %APPDATA%\npm, the Node MSI uses %ProgramFiles%\nodejs for all-users, a corporate image may set a
// custom prefix (which the shim-adjacent rule below still covers), and pnpm uses %LOCALAPPDATA%\pnpm.
// A missing prefix contributes nothing — joinIfBase returns "" — so an unset variable cannot produce a
// relative path that would be probed against the process's working directory.
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
	// one directory too high, so the shim it exists to find was never found and the hint stayed silent
	// for the exact operator it was written for ("but I installed it"). Both are kept: APPDATA covers a
	// hand-rolled shim at the top level, npm covers the real one.
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
//
// IT LIVES IN THE SHARED FILE, not the Windows one, for two reasons. The persisted Windows Path is
// full of %SystemRoot%-style references while the process routinely exports the upper-case spelling,
// so a case-sensitive lookup would expand nothing and silently drop real directories — that logic
// needs to be reachable from a test on any OS. And it is only ever used by name from the Windows
// path-expansion helper, so keeping it here costs the Unix build nothing.
//
// An EMPTY result means "not set": the caller treats an unexpanded reference as a dead directory.
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
// none.
//
// THE PARSE IS DELIBERATELY NARROW. It looks for the literal `node_modules` — every npm shim contains
// it and nothing else in the file resembles it — then takes everything up to the closing quote (or the
// end of the token, for a shim that does not quote). It does NOT try to understand .cmd, .ps1 or sh
// syntax: a real parser for three shell dialects, to read one string, would be a large amount of code
// whose bugs would only show up on an operator's machine.
//
// ONLY AN EXECUTABLE IS RETURNED. A shim whose target is a `.js` entry point is not something this
// plane can spawn, so it yields "" and the caller carries on to the next candidate rather than
// handing a spawn a script.
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
	// THE SHIMS DISAGREE ABOUT SEPARATORS, so both are normalised before the join. `.cmd` writes
	// backslashes (`node_modules\@opencode\cli\bin\opencode.exe`) while `.ps1` and the POSIX shim
	// write forward slashes for the SAME target. filepath.FromSlash only ever rewrites '/', so on a
	// non-Windows host a backslash path would survive as a literal and never match a real file —
	// found exactly that way: the test failed on Linux with the separators still in place.
	target := filepath.Join(filepath.Dir(shim), filepath.FromSlash(strings.ReplaceAll(rest, `\`, "/")))
	if !strings.EqualFold(filepath.Ext(target), ".exe") {
		return ""
	}
	return target
}

// resolveOpenCodeBinary is the ONE production resolver: the env override, then
// PATH, then the container-visible $HOME candidates. It is the default for the
// adapter's spawn path (SetBinaryResolver swaps it), the host serve, and the
// plane's discovery wiring.
// A CANDIDATE MUST EXIST AND BE A REGULAR FILE; anything else falls through
// rather than handing a spawn a path that cannot run. The env override is
// validated rather than trusted, because an override that does not exist is an
// explicit instruction that is wrong — and quietly ignoring it (which is what
// the discovery wiring used to do) hides a typo behind 'not found'.
func resolveOpenCodeBinary() (string, error) {
	return resolveOpenCodeBinaryFor(runtimeGOOS(), os.Getenv, nil)
}

// resolveOpenCodeBinaryFor is the resolver with the OS, the environment and the persisted-PATH
// directories INJECTABLE, and it is the real implementation — the wrapper above is the production
// entry point.
//
// WHY, IN THE FILE'S OWN TERMS. Everything Windows-specific here (the candidate tables, the npm
// layout, the shim refusal, the persisted PATH) is unreachable on a Linux CI box, so without a seam
// the reported failure can only be reproduced on a maintainer's Windows machine — which is precisely
// the "a test that could only run on Windows is a test that will not run" trap this package already
// documents for its tables. With the seam, a test can build the operator's exact layout in a temp
// directory and drive the REAL resolver over it, on any host.
//
// registryDirs is passed rather than called so the loop is testable too; production passes nil and
// reads the registry (pathrefresh_windows.go), which is a no-op off Windows.
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
	// known package layout rather than a directory that happens to be on PATH, so it is the better
	// answer when both would match.
	for _, cand := range npmExeFromShims(goos, getenv) {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	// THE PERSISTED PATH, LAST. This process's PATH is the one it was STARTED with, and on Windows that
	// snapshot is taken from the registry at logon — so a plane started before opencode was installed
	// keeps looking in the places the operator has since replaced. Re-reading the registry is what makes
	// "I installed it and I can run it from anywhere" true for the plane too, without a restart.
	//
	// AFTER the fixed candidates, deliberately: a known install location is a better answer than a
	// directory that merely happens to be on PATH today, and the fixed list is what the container
	// mounts are built from. Empty on Unix (see pathrefresh_other.go).
	if registryDirs == nil {
		registryDirs = registryPathDirs()
	}
	for _, dir := range registryDirs {
		cand := filepath.Join(dir, "opencode"+executableExt())
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	// THE AGNOSTIC LAST RESORT: ask the OS's own PATH — from its SOURCE, which is what the loop above reads on
	// Windows — and let Go's LookPath apply the platform's extension rules, because internal/adapter owns that
	// search now and both adapters read it. This covers an install by ANY means we did not anticipate
	// (Chocolatey, nix, a portable zip): the point is to meet the tool where it is rather than require it to
	// match our list.
	//
	// LAST, because everything above is a KNOWN layout and a known layout is the better answer when both match
	// — and it never yields a shim, which discovery filters itself.
	if home, herr := os.UserHomeDir(); herr == nil {
		if hits := adapter.DiscoverKind(goos, home, adapter.DefaultAdapterKind, getenv); len(hits) > 0 {
			return hits[0], nil
		}
	}
	return "", fmt.Errorf("opencode binary not found on PATH or in any known install location (%s)%s%s (install it as your user, never with sudo: curl -fsSL https://opencode.ai/install | bash; set %s to point at an existing install; set ORCHICON_SIMULATE_ADAPTER=1 only for offline dev)",
		strings.Join(openCodeRelCandidates, ", "), misplacedOpenCodeHintFor(goos, getenv), foundElsewhereHintFor(goos, getenv), OpenCodeBinEnv)
}

// OpenCodeBinDirs returns the host directories the resolver probes for opencode,
// in probe order, as ABSOLUTE paths for the given home. Existence is NOT
// filtered: a caller asking about the RULE wants the rule rather than today's
// filesystem.
//
// IT HAS NO PRODUCTION CALLER. This comment used to name one — "the host-plane PATH augmentation in
// cmd/orchicon/container.go" — and that caller no longer exists, so the sentence was describing an
// arrangement the code had moved away from. It is kept as the exported statement of the probe order (and
// binary_test.go pins it to the candidate list) because a PATH augmentation is the obvious next thing to
// want; inventing it here would be a seam nothing calls, and deleting the only description of the order
// would lose the rule.
//
// The rule it states is the one that matters: a second hand-written list is how a PATH prefix and the
// resolver drift apart — the plane prepends a directory it thinks holds opencode while the resolver looks
// somewhere else, and the symptom is a serve that reports 'not found' with the binary sitting on the PATH it
// was handed.
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

// ResolveBinaryPath is the exported resolver, for callers OUTSIDE the adapter —
// the plane's discovery wiring, which must resolve the same binary the spawn path
// will. A discovery path that resolves something different from the executor is
// worse than no discovery: the model picker starts working and the first dispatch
// fails for a reason that looks unrelated.
func ResolveBinaryPath() (string, error) { return resolveOpenCodeBinary() }

// ContainerVisible reports whether a resolved opencode path can be reached from
// inside a runtime container — i.e. whether it sits under one of the host roots
// internal/runtime declares as adapter mounts for the opencode kind.
//
// IT IS THE HALF THAT WAS MISSING. Every check that 'opencode is present' — and
// adapterCLIPresent in cmd/orchicon does exactly this — was a HOST check, so a
// system-wide install passed it while every workflow-run execution failed to find
// the binary. The pair is what the operator needs: present AND visible.
//
// An undeterminable home returns true (no warning) rather than false: this
// function only ever produces a WARNING, and a warning that fires because the
// home could not be read is noise that trains the operator to ignore it.
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

// misplacedOpenCodeHint names an opencode install that EXISTS but is not usable
// by the plane's runtime containers, and says what to do about it. Empty when
// there is nothing to report, so it can be appended to any error
// unconditionally.
//
// It is the difference between an error that is CORRECT and one that is USEFUL.
// The report this answers is an operator who installed opencode, was told it
// could not be found, installed it a second way, and was told the same thing.
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

// foundElsewhereHint names an opencode the plane FOUND but could not use, in terms the operator can act on.
//
// IT IS THE WINDOWS ANSWER TO "but I installed it". A package-manager install puts a `opencode.cmd` (or
// `.ps1`) shim on PATH; the plane cannot execute either, so resolution correctly refuses — but refusing
// silently is the report this fixes. The remedy is an absolute path to a real executable, which is exactly
// what the advertised escape hatch takes.
//
// Empty when there is nothing to report, so it can be appended unconditionally.
func foundElsewhereHint() string {
	return foundElsewhereHintFor(runtimeGOOS(), os.Getenv)
}

// foundElsewhereHintFor is foundElsewhereHint with the OS and environment injectable, for the same
// reason openCodePlatformCandidates takes them: this is a Windows-only message about Windows-only
// shims, and a message that can only be checked on Windows is a message that will not be checked by
// a gate that runs on Linux. Found by trying to test it — the hint came back empty on Linux because
// runtimeGOOS() refused the platform, which is correct in production and untestable there.
func foundElsewhereHintFor(goos string, getenv func(string) string) string {
	for _, p := range openCodeShimCandidates(goos, getenv) {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(p), ".exe") {
			continue // a real executable here would have resolved; this is for the shims
		}
		// NAME THE REAL EXECUTABLE, not just the problem. The operator supplied their own shim path
		// ("C:\Users\operator\AppData\Roaming\npm\opencode.ps1"), which means the useful answer is
		// where the binary that shim stands for lives — one level down, in node_modules — and why it
		// might be missing even though the shim is there.
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

// launchableResolved reports whether a path LookPath returned can actually be EXECUTED here,
// rather than merely found.
//
// THIS CLOSES A HOLE IN THE FILE'S OWN RULE. openCodePlatformCandidates states the policy plainly —
// a Windows npm global install lands a `opencode.cmd` shim, Go cannot CreateProcess a .cmd (it needs
// cmd.exe), so resolving the shim "would be the exact failure this file exists to prevent". But
// LookPath runs BEFORE those candidates and returns the shim happily, so a PATH that carries the npm
// prefix sailed past the policy: resolution "succeeded", discovery started offering models, and the
// first execution died somewhere unrelated. The rule only held for paths the candidate tables found.
//
// Windows only. On Unix a shell script IS executable, so LookPath's answer is always launchable and
// there is nothing to filter — gating on the OS keeps the Unix behaviour byte-for-byte unchanged
// rather than relying on an extension allow-list that would be wrong there (.exe is a normal name).
// THE OS IS A PARAMETER, not read from the host, for the same reason openCodePlatformCandidates
// takes one: the rule is a pure function of the platform and the path, and a rule that can only be
// checked on Windows is a rule that will not be checked by a gate that runs on Linux.
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
//
// IT EXISTS SO THE REGISTRY-PATH PROBE DOES NOT SPELL THE PLATFORM OUT TWICE. The Windows candidate
// table above already appends ".exe" by hand; a second literal in the probe loop is how the two lists
// drift apart, and a drifted probe is silent — it just stops matching and resolution falls back
// without anything reporting a problem.
func executableExt() string {
	if runtimeGOOS() == "windows" {
		return ".exe"
	}
	return ""
}

// runtimeGOOS is a seam so the platform tables can be tested for Windows from any host: the tables are pure
// functions of the OS name, and a test that could only run on Windows is a test that will not run.
// goruntime is the STANDARD LIBRARY's runtime, aliased: this file already imports
// internal/runtime under the plain name (it reads the adapter mount roots from it).
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
