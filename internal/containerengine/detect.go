// Package containerengine is the ONE truth for "is there a container engine on this
// host, what is it, is it new enough, and is it actually working?".
//
// WHY A LIBRARY PACKAGE AND NOT cmd/orchicon: the installer (cmd/orchicon, package main)
// and the control plane (internal/api) must answer that question with the SAME rules —
// the UI reports availability for the carrier the install actually produced, so two
// implementations of the probe would eventually disagree and the UI would describe an
// engine that is not there. package main cannot be imported, so the rules live here and
// both consumers call in.
//
// THE PROBE IS INJECTED. Every side effect (PATH lookup, process execution, stat, GOOS,
// environment) goes through Probe, because the fixture posture in this repo is a fake
// binary in a t.TempDir() on PATH (cmd/orchicon/install_host_test.go) — and because
// detection must never touch the network or a real engine in CI.
package containerengine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// MinimumVersion is the oldest Podman this product supports, in ONE place.
//
// 4.0.0 is the floor because it is the first release with the `podman machine list
// --format json` shape and `podman machine init --now` this package drives, and it is
// where the `--format {{json .NetworkSettings.Ports}}` / `--filter label=` parity with
// the daemon's docker argv (internal/runtime/daemon.go) holds. A too-old Podman is
// REPORTED, never attempted: the failure mode we must avoid is a 3.x binary that half
// works and fails mid-run.
const MinimumVersion = "4.0.0"

const (
	// EnginePinEnv pins the engine by name, bypassing the search order. It also makes a
	// provisioning failure FATAL (see cmd/orchicon/install.go): an operator who named the
	// engine asked for it, so silently installing in local mode would ignore them.
	EnginePinEnv = "ORCHICON_CONTAINER_ENGINE"
	// MinVersionEnv overrides MinimumVersion so a test can drive the too-old path without
	// a 3.x binary on the runner.
	MinVersionEnv = "ORCHICON_PODMAN_MIN_VERSION"
	// ProvisionEnv is the opt-in gate for installing an engine: 1/0/ask (default ask).
	ProvisionEnv = "ORCHICON_PROVISION_ENGINE"
)

// Name is the engine family. "none" is an explicit, supported mode — not an error.
type Name string

const (
	EnginePodman Name = "podman"
	EngineDocker Name = "docker"
	EngineNone   Name = "none"
)

// Availability states served by /enginez and rendered in the UI (AC5).
const (
	StateReady        = "ready"
	StateProvisioning = "provisioning"
	StateUnavailable  = "unavailable"
)

// Engine is a detected engine: what it is, where it is, and (when asked) its version.
type Engine struct {
	Name    Name   `json:"name"`
	Bin     string `json:"bin,omitempty"`
	Version string `json:"version,omitempty"`
}

// Probe is every side effect detection needs. The zero value is NOT usable — call
// DefaultProbe(); tests build one field by field so nothing real is executed.
type Probe struct {
	LookPath func(string) (string, error)
	// Run executes bin with args and returns combined output. A non-zero exit comes back
	// as an error carrying that output.
	Run  func(ctx context.Context, bin string, args ...string) (string, error)
	Stat func(string) (os.FileInfo, error)
	// GOOS/Getenv default to runtime.GOOS/os.Getenv when nil.
	GOOS   string
	Getenv func(string) string
	// IsRoot reports whether the installer may run a root-requiring package manager
	// without escalation. Default: os.Geteuid() == 0.
	IsRoot func() bool
	// Sleep is the poll sleep for the machine reachability wait (injected so tests do not
	// wait). Default time.Sleep.
	Sleep func(d time.Duration)
	// MachineTimeout / PollInterval bound the reachability poll (0 = defaults).
	MachineTimeout time.Duration
	PollInterval   time.Duration
}

// DefaultProbe is the real-world Probe.
func DefaultProbe() Probe {
	return Probe{
		LookPath: exec.LookPath,
		Run: func(ctx context.Context, bin string, args ...string) (string, error) {
			// quietCommandContext: the probe runs from a DETACHED install (no console), so a bare
			// exec.CommandContext here puts a window on the operator's screen on Windows.
			out, err := procwin.CommandContext(ctx, bin, args...).CombinedOutput()
			if err != nil {
				return strings.TrimSpace(string(out)), fmt.Errorf("%s %s: %w: %s",
					bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return strings.TrimSpace(string(out)), nil
		},
		Stat:   os.Stat,
		GOOS:   runtime.GOOS,
		Getenv: os.Getenv,
		IsRoot: func() bool { return os.Geteuid() == 0 },
		Sleep:  time.Sleep,
	}
}

func (p Probe) goos() string {
	if p.GOOS != "" {
		return p.GOOS
	}
	return runtime.GOOS
}

func (p Probe) getenv(k string) string {
	if p.Getenv != nil {
		return p.Getenv(k)
	}
	return os.Getenv(k)
}

// sleep is the Probe's injected wait, so a poll is testable without real time passing. Falls back to
// time.Sleep when the caller did not inject one, which is the production shape.
func (p Probe) sleep(d time.Duration) {
	if p.Sleep != nil {
		p.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (p Probe) lookPath(name string) (string, error) {
	if p.LookPath != nil {
		return p.LookPath(name)
	}
	return exec.LookPath(name)
}

func (p Probe) stat(path string) (os.FileInfo, error) {
	if p.Stat != nil {
		return p.Stat(path)
	}
	return os.Stat(path)
}

func (p Probe) isRoot() bool {
	if p.IsRoot != nil {
		return p.IsRoot()
	}
	return os.Geteuid() == 0
}

// conventionalPaths are the install locations a package manager or a GUI app uses that
// a non-login PATH may miss (a double-clicked app bundle inherits none of the shell's
// profile). Checked with Stat only, so detection stays a probe and never runs a binary.
//
// IT TAKES THE PROBE'S ENVIRONMENT, NOT THIS PROCESS'S. It used os.Getenv directly, which had two
// consequences: the locations could not be exercised by a test (a Probe carries Getenv precisely so the OS
// boundary is replaceable — every other resolver in this package honours it), and with an EMPTY environment it
// produced a RELATIVE candidate (`AppData\Local/Programs\Podman\podman.exe`, joined from an empty
// %USERPROFILE%), which then resolved against the working directory. Measured while adding the installer
// fallback; the empty-environment case is a probe-only artifact, and the untestability was the real cost.
func conventionalPaths(p Probe) map[Name][]string {
	progFiles := p.getenv("ProgramFiles")
	if progFiles == "" {
		progFiles = `C:\Program Files`
	}
	localAppData := p.getenv("LOCALAPPDATA")
	if localAppData == "" {
		localAppData = p.getenv("USERPROFILE")
		if localAppData == "" {
			// NO RELATIVE PATH, EVER. An unresolvable %LOCALAPPDATA% must yield NO candidate rather than one
			// that silently means "somewhere under the current directory".
			return conventionalPathsWithoutLocalAppData(p.goos(), progFiles)
		}
		localAppData = filepath.Join(localAppData, `AppData\Local`)
	}
	switch p.goos() {
	case "darwin":
		return map[Name][]string{
			EnginePodman: {
				"/opt/homebrew/bin/podman",
				"/usr/local/bin/podman",
				"/Applications/Podman Desktop.app/Contents/Resources/bin/podman",
			},
			EngineDocker: {"/usr/local/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker"},
		}
	case "windows":
		return map[Name][]string{
			EnginePodman: {
				filepath.Join(progFiles, `RedHat\Podman\podman.exe`),
				filepath.Join(localAppData, `Programs\Podman\podman.exe`),
			},
			EngineDocker: {filepath.Join(progFiles, `Docker\Docker\resources\bin\docker.exe`)},
		}
	default:
		return map[Name][]string{
			EnginePodman: {"/usr/bin/podman", "/usr/local/bin/podman"},
			EngineDocker: {"/usr/bin/docker", "/usr/local/bin/docker"},
		}
	}
}

// conventionalPathsWithoutLocalAppData is the table MINUS the entries that need %LOCALAPPDATA%.
//
// It exists so an unresolvable %LOCALAPPDATA% yields FEWER candidates instead of a relative one. The entries
// that survive are all built from %ProgramFiles% or an absolute POSIX path, so they cannot depend on where the
// process happens to be.
func conventionalPathsWithoutLocalAppData(goos, progFiles string) map[Name][]string {
	switch goos {
	case "darwin":
		return map[Name][]string{
			EnginePodman: {"/opt/homebrew/bin/podman", "/usr/local/bin/podman"},
			EngineDocker: {"/usr/local/bin/docker"},
		}
	case "windows":
		return map[Name][]string{
			EnginePodman: {filepath.Join(progFiles, `RedHat\Podman\podman.exe`)},
			EngineDocker: {filepath.Join(progFiles, `Docker\Docker\resources\bin\docker.exe`)},
		}
	default:
		return map[Name][]string{
			EnginePodman: {"/usr/bin/podman", "/usr/local/bin/podman"},
			EngineDocker: {"/usr/bin/docker", "/usr/local/bin/docker"},
		}
	}
}

// packageManagerPaths are the places each installer program lives when PATH does not say so.
//
// IT IS THE SAME IDEA AS conventionalPaths, FOR THE OTHER HALF OF THE INSTALL. The engine half already worked
// this way; the installer half did not, and that asymmetry is the reported bug (see locateFile). A Windows
// `winget` is an App Execution Alias under %LOCALAPPDATA%\Microsoft\WindowsApps — a directory that IS on a
// normal user's PATH but is NOT on the PATH of a process launched by an installer custom action.
//
// KEPT SEPARATE FROM conventionalPaths ON PURPOSE: that map is keyed by engine Name and feeds Detect, whose
// contract is "is an ENGINE available" and which must never learn about package managers. These are not engines.
func packageManagerPaths(p Probe) map[string][]string {
	progFiles := p.getenv("ProgramFiles")
	if progFiles == "" {
		progFiles = `C:\Program Files`
	}
	localAppData := p.getenv("LOCALAPPDATA")
	if localAppData == "" {
		localAppData = p.getenv("USERPROFILE")
		if localAppData != "" {
			localAppData = filepath.Join(localAppData, `AppData\Local`)
		}
	}
	switch p.goos() {
	case "darwin":
		return map[string][]string{
			"brew": {"/opt/homebrew/bin/brew", "/usr/local/bin/brew"},
		}
	case "windows":
		winget := []string{
			filepath.Join(progFiles, `WindowsApps\winget.exe`),
			filepath.Join(progFiles, `WindowsApps\Microsoft.DesktopAppInstaller_8wekyb3d8bbwe\winget.exe`),
		}
		// USER FIRST: a per-user winget is what an unelevated install gets. PREPENDED ONLY WHEN %LOCALAPPDATA%
		// RESOLVED — joining onto an empty base yields a RELATIVE path, which would resolve against the working
		// directory and could match anything. A candidate that might be wrong is worse than one fewer candidate:
		// the machine-wide entries below still cover the ordinary case.
		if localAppData != "" {
			winget = append([]string{filepath.Join(localAppData, `Microsoft\WindowsApps\winget.exe`)}, winget...)
		}
		return map[string][]string{"winget": winget}
	default:
		return map[string][]string{
			"dnf":  {"/usr/bin/dnf"},
			// BOTH NAMES, because planInstall asks for "apt-get" and a host may have only "apt" (or a busybox-ish
			// image may have neither). Keyed by the name the PLAN uses, with the other as a candidate for it.
			"apt-get": {"/usr/bin/apt-get", "/usr/bin/apt"},
			"apt":     {"/usr/bin/apt", "/usr/bin/apt-get"},
			"yum":  {"/usr/bin/yum"},
			"zypper": {"/usr/bin/zypper"},
			"pacman": {"/usr/bin/pacman"},
			"apk":    {"/sbin/apk", "/usr/bin/apk"},
		}
	}
}

// locatePackageManager is locateFile for an INSTALLER program, with its own location table.
func locatePackageManager(p Probe, name string) (string, bool) {
	return locateFile(p, name, packageManagerPaths(p)[name])
}

// Detect resolves the engine. Order: explicit pin → podman on PATH → podman in the
// platform's conventional location → docker on PATH → docker in its conventional
// location. Podman first because it is the engine we can legally provision and it is
// rootless by default; docker second so an existing Docker user keeps working unchanged.
//
// ABSENCE IS A MODE, NOT AN ERROR: no engine returns Engine{Name: EngineNone} and a nil
// error, which is the contract cmd/orchicon/install_host.go documents — a host must be
// able to answer "no engine" without a failure.
func Detect(p Probe) (Engine, error) {
	pin := strings.ToLower(strings.TrimSpace(p.getenv(EnginePinEnv)))
	if pin != "" {
		if pin == string(EngineNone) || pin == "0" || pin == "off" {
			return Engine{Name: EngineNone}, nil
		}
		name := Name(pin)
		if name != EnginePodman && name != EngineDocker {
			return Engine{Name: EngineNone}, fmt.Errorf("%s=%q is not a known engine (podman, docker, none)", EnginePinEnv, pin)
		}
		// A PIN IS A REQUIREMENT, so a pin that is not installed is an error rather than a
		// silent fall-through to the other engine — that would make the pin meaningless.
		bin, ok := find(p, name)
		if !ok {
			return Engine{Name: EngineNone}, fmt.Errorf("%s=%s but no %s binary was found", EnginePinEnv, pin, pin)
		}
		return Engine{Name: name, Bin: bin}, nil
	}

	for _, name := range []Name{EnginePodman, EngineDocker} {
		if bin, ok := find(p, name); ok {
			return Engine{Name: name, Bin: bin}, nil
		}
	}
	return Engine{Name: EngineNone}, nil
}

// locateFile answers "where is this program, REALLY" — PATH first, then the places the platform puts it that a
// process's inherited PATH may not carry.
//
// THIS EXISTS BECAUSE ENGINES AND INSTALLERS HAD TWO DIFFERENT ANSWERS. find() (the engine resolver) already
// searched conventional locations; planInstall (the winget/brew/dnf resolver) asked lookPath and NOTHING else.
// So on a Windows box where winget is installed but its directory is not in the PATH this process inherited, an
// install concluded "winget is not available, so podman cannot be installed automatically" — while the very same
// run had just found the opencode adapter, because internal/adapter's resolver knows about
// %LOCALAPPDATA%\Microsoft\WindowsApps and %APPDATA%\npm.
//
// MEASURED, and the reason this file changed: launched by the MSI's custom action, `orchicon install` reported
// "winget is not available" and SKIPPED the podman install entirely. Run by hand in PowerShell it installed
// podman every time — because PowerShell resolves App Execution Aliases itself and this process cannot. A
// capability that depends on WHO LAUNCHED US is a bug, not a configuration.
//
// PATH is still searched FIRST, so an operator's own choice of binary wins, exactly as it does today.
func locateFile(p Probe, name string, conventional []string) (string, bool) {
	if bin, err := p.lookPath(name); err == nil && bin != "" {
		return bin, true
	}
	for _, cand := range conventional {
		// A DIRECTORY IS NOT A PROGRAM. On Windows this is load-bearing: an App Execution Alias is a
		// ZERO-BYTE reparse point, and an interrupted install can leave a directory of that name behind.
		if fi, err := p.stat(cand); err == nil && !fi.IsDir() {
			return cand, true
		}
	}
	return "", false
}

// find looks up one engine on PATH, then in its conventional locations.
func find(p Probe, name Name) (string, bool) {
	return locateFile(p, string(name), conventionalPaths(p)[name])
}

// Version attaches the engine's version, and it is a SEPARATE call on purpose.
//
// Detect must stay an EXECUTION-FREE probe: it answers "is an engine installed?" from PATH
// and stat alone, which is what lets a host with no engine (or a `docker` CLI whose daemon is
// down) answer "no" without a doomed invocation — the invariant
// cmd/orchicon/install_host_test.go pins. Reading the version needs one client call
// (`<bin> --version`, which needs no daemon), so only the paths that need the version — the
// minimum check, the status surface — pay for it. A failed `--version` does not hide an
// engine that exists: the error is returned with the version empty.
func Version(p Probe, e Engine) (Engine, error) {
	if e.Name == EngineNone || e.Bin == "" {
		return e, fmt.Errorf("no container engine to query")
	}
	out, err := p.Run(context.Background(), e.Bin, "--version")
	if err != nil {
		return e, err
	}
	e.Version = ParseVersion(out)
	return e, nil
}

// ParseVersion extracts the version token from `--version` output. Both engines print a
// sentence ("podman version 5.2.1", "Docker version 27.1.1, build ..."), so the first
// token that starts with a digit wins.
func ParseVersion(out string) string {
	for _, tok := range strings.Fields(out) {
		tok = strings.TrimSuffix(strings.TrimPrefix(tok, "v"), ",")
		if tok == "" || tok[0] < '0' || tok[0] > '9' {
			continue
		}
		if strings.Count(tok, ".") >= 1 {
			// Drop a -dev / +meta suffix ("4.9.4-dev" → "4.9.4").
			tok = strings.TrimRight(strings.FieldsFunc(tok, func(r rune) bool {
				return r == '-' || r == '+'
			})[0], ".")
			return tok
		}
	}
	return ""
}

// CompareVersions compares dot-separated numeric versions: -1 if a<b, 0 if equal, 1 if
// a>b. Missing components are zero ("5.2" == "5.2.0"), non-numeric components compare
// as 0 so a garbled version cannot panic or look newer than the minimum.
func CompareVersions(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(as) {
			ai, _ = strconv.Atoi(strings.TrimSpace(as[i]))
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(strings.TrimSpace(bs[i]))
		}
		switch {
		case ai < bi:
			return -1
		case ai > bi:
			return 1
		}
	}
	return 0
}

// MinimumFor returns the effective minimum (const, or the env override).
func MinimumFor(p Probe) string {
	if v := strings.TrimSpace(p.getenv(MinVersionEnv)); v != "" {
		return v
	}
	return MinimumVersion
}

// CheckMinimum reports whether the engine is new enough to drive. Docker has no minimum
// of its own here (the daemon's argv has been stable across the releases we support);
// Podman's floor is MinimumVersion, and the error carries the UPGRADE COMMAND so the
// operator is told what to do, not just what is wrong (AC1).
func CheckMinimum(p Probe, e Engine) error {
	if e.Name != EnginePodman || e.Version == "" {
		return nil
	}
	min := MinimumFor(p)
	if CompareVersions(e.Version, min) >= 0 {
		return nil
	}
	return fmt.Errorf("podman %s is older than the minimum %s — Orchicon needs the flags 4.0 introduced; upgrade with %q (or set %s to accept an older engine)",
		e.Version, min, "brew upgrade podman", MinVersionEnv)
}

// Status is the availability report served by GET /enginez and rendered in the UI where
// execution mode is chosen (AC5).
type Status struct {
	Engine    Name         `json:"engine"`
	Bin       string       `json:"bin,omitempty"`
	Version   string       `json:"version,omitempty"`
	Minimum   string       `json:"minimum"`
	Available bool         `json:"available"`
	State     string       `json:"state"`
	Reason    string       `json:"reason,omitempty"`
	Machine   MachineState `json:"machine,omitempty"`
}

// Inspect builds the full Status: detect → minimum → machine reachability, never running
// a container (that is Verify, which the installer calls; a status poll must stay cheap).
func Inspect(ctx context.Context, p Probe) Status {
	min := MinimumFor(p)
	e, err := Detect(p)
	st := Status{Engine: EngineNone, Minimum: min, State: StateUnavailable}
	if e.Name != EngineNone {
		st.Engine, st.Bin, st.Version = e.Name, e.Bin, e.Version
	}
	if IsProvisioning(p) {
		st.State = StateProvisioning
		st.Reason = "podman is being installed"
		return st
	}
	if err != nil {
		st.Reason = err.Error()
		return st
	}
	if e.Name == EngineNone {
		st.Reason = "no container engine found — runs execute locally"
		return st
	}
	if v, verr := Version(p, e); verr == nil {
		e = v
		st.Version = v.Version
	}
	if err := CheckMinimum(p, e); err != nil {
		st.Reason = err.Error()
		return st
	}
	ms, reason, merr := MachineStatus(ctx, p, e)
	st.Machine = ms
	switch ms {
	case MachineAbsent, MachineStopped:
		st.Reason = reason
		return st
	case MachineFailed:
		st.Reason = reason
		return st
	}
	if merr != nil {
		st.Reason = merr.Error()
		return st
	}
	st.Available = true
	st.State = StateReady
	return st
}

// WaitForBinary re-probes for an engine that may have just been INSTALLED.
//
// IT EXISTS BECAUSE AN INSTALL DOES NOT UPDATE THIS PROCESS'S PATH. winget, brew and npm write their result to
// the registry / shell profile; the environment block of the process that invoked them is untouched, and a child
// inherits its parent's. Measured shape of the failure: `winget install RedHat.Podman` succeeds, `podman --version`
// works in a NEW shell, and the installer — which is still the old process — reports "no container engine" and
// therefore starts no runtime daemon. The operator sees a successful install and a container fetch that fails on a
// socket that will never exist.
//
// The probe is deliberately NARROW: a short window, and a hit only when `podman` resolves through PATH *and* sits
// at one of its conventional locations. A winget App Execution Alias, or a stub that briefly appears on PATH while
// the real install is still committing, therefore cannot be mistaken for a usable engine.
func (e Engine) WaitForBinary(p Probe) (Engine, bool) {
	if e.Name != EngineNone {
		return e, false
	}
	return e.WaitForBinaryWithin(p, engineProbeWindow)
}

// WaitForBinaryWithin is WaitForBinary with an explicit budget, so a caller (and a test) can bound it without
// depending on the package's default window.
func (e Engine) WaitForBinaryWithin(p Probe, window time.Duration) (Engine, bool) {
	if e.Name != EngineNone {
		return e, false
	}
	deadline := time.Now().Add(window)
	for {
		if bin, ok := find(p, EnginePodman); ok {
			if conventional := isConventionalInstall(p, EnginePodman, bin); conventional {
				return Engine{Name: EnginePodman, Bin: bin}, true
			}
		}
		if time.Now().After(deadline) {
			return Engine{Name: EngineNone}, false
		}
		p.sleep(engineProbeInterval)
	}
}

const (
	// engineProbeWindow bounds the wait. Long enough for a package manager to finish committing, short enough
	// that a genuinely absent engine costs the operator nothing noticeable.
	engineProbeWindow = 45 * time.Second
	// engineProbeInterval is how often the probe looks.
	engineProbeInterval = 1500 * time.Millisecond
)

// isConventionalInstall reports whether bin is one of the engine's own conventional paths.
//
// `podman` ON PATH IS NOT EVIDENCE ENOUGH: winget installs App Execution Aliases that live in
// %LOCALAPPDATA%\Microsoft\WindowsApps and forward elsewhere, and a half-finished install can leave a name
// resolvable before the binary works. Requiring the conventional location as well keeps the probe honest.
func isConventionalInstall(p Probe, name Name, bin string) bool {
	cleanBin := filepath.Clean(bin)
	for _, cand := range conventionalPaths(p)[name] {
		if filepath.Clean(cand) == cleanBin {
			return true
		}
	}
	return false
}
