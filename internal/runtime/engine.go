package runtime

import (
	"errors"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Engine is the container engine the runtime daemon drives: which CLI family
// it is, and the resolved binary to execute. It is ONE value with per-engine
// behaviour methods rather than an interface, so every engine-specific
// difference lives in this file, side by side with its sibling: neither the
// podman branch nor the docker branch can be edited without meeting the other.
//
// Nothing outside this file may name a container engine binary. `d.engine(...)`
// is the only entry point, and the binary comes from Engine.Bin.
type Engine struct {
	// Name is the engine's CLI family: "podman" or "docker".
	Name string
	// Bin is the resolved binary (an absolute path from exec.LookPath when the
	// engine was resolved by ResolveEngine; "" means "not resolved", in which
	// case the daemon falls back to DefaultEngineName()).
	Bin string
}

// Engine names. Podman is the default: it is rootless by default, which is the
// posture the runtime wants, and the runtime's containers were always meant to
// run as the operator rather than a root daemon.
const (
	enginePodman = "podman"
	engineDocker = "docker"
)

// EnvEngine is the environment variable that selects the engine, so an operator
// (or a docker-only host) can override the default without a rebuild.
const EnvEngine = "ORCHICON_RUNTIME_ENGINE"

// EnvEngineAlias is the engine variable the rest of the project already honours:
// scripts/lib/engine.sh, scripts/runtime.sh (image build/pull) and the Makefile
// all read ORCHICON_ENGINE, defaulting to podman. The daemon accepts it as a
// FALLBACK so one host cannot end up with images built or pulled by docker while
// containers are created by podman — `ORCHICON_ENGINE=docker scripts/runtime.sh
// daemon-start` must mean ONE engine, whichever spelling the operator used.
const EnvEngineAlias = "ORCHICON_ENGINE"

// DefaultEngineName returns the engine the daemon uses when nothing overrides
// it. Podman on EVERY platform, including macOS/Windows through a Podman
// machine.
func DefaultEngineName() string { return enginePodman }

// EngineMissingError is a container engine that is not on PATH. It exists so a
// missing engine is reported as an INSTALL instruction naming what to install
// and how, never as a bare `exec: "podman": executable file not found in $PATH`
// — which tells an operator nothing about what to do.
type EngineMissingError struct {
	Name      string // the engine that is missing: "podman" / "docker"
	Requested string // the raw configured value ("" = unset)
	Err       error  // the underlying LookPath error, if any
}

func (m *EngineMissingError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "runtime engine %q not found on PATH.\n", m.Name)
	fmt.Fprintf(&b, "  Install %s:\n", displayEngineName(m.Name))
	fmt.Fprintf(&b, "    macOS:          brew install %s && %s machine init && %s machine start\n", m.Name, m.Name, m.Name)
	switch {
	case m.Name == enginePodman:
		b.WriteString("    Fedora/RHEL:    sudo dnf install podman\n")
		b.WriteString("    Debian/Ubuntu:  sudo apt-get install podman\n")
		b.WriteString("    Windows:        winget install --id RedHat.Podman\n")
		fmt.Fprintf(&b, "  Or use Docker instead: %s=docker\n", EnvEngine)
	case m.Name == engineDocker:
		b.WriteString("    Fedora/RHEL:    sudo dnf install docker-ce && sudo systemctl enable --now docker\n")
		b.WriteString("    Debian/Ubuntu:  sudo apt-get install docker.io && sudo systemctl enable --now docker\n")
		b.WriteString("    Windows:        winget install --id Docker.DockerDesktop\n")
		fmt.Fprintf(&b, "  Or use Podman instead (the default): unset %s\n", EnvEngine)
	}
	fmt.Fprintf(&b, "  (this daemon was started with %s=%q; %s is accepted as an alias)\n",
		EnvEngine, m.Requested, EnvEngineAlias)
	b.WriteString("  Then restart the daemon: orchicon runtime-daemon")
	return b.String()
}

func (m *EngineMissingError) Unwrap() error { return m.Err }

// displayEngineName renders the engine's product name for operator-facing text.
func displayEngineName(name string) string {
	if name == enginePodman {
		return "Podman"
	}
	return "Docker"
}

// ResolveEngine resolves the configured engine to an executable. An empty
// requested name means DefaultEngineName(). An unknown name is rejected rather
// than guessed, and a missing binary yields an actionable EngineMissingError.
func ResolveEngine(requested string) (Engine, error) {
	name := strings.ToLower(strings.TrimSpace(requested))
	if name == "" {
		name = DefaultEngineName()
	}
	switch name {
	case enginePodman, engineDocker:
	default:
		return Engine{}, fmt.Errorf("unknown runtime engine %q: %s accepts %q or %q (leave it unset for %q)",
			requested, EnvEngine, enginePodman, engineDocker, DefaultEngineName())
	}
	bin, err := lookupEngine(name)
	if err != nil {
		return Engine{Name: name, Bin: name}, &EngineMissingError{Name: name, Requested: requested, Err: err}
	}
	return Engine{Name: name, Bin: bin}, nil
}

// lookupEngine finds an engine executable, and it does NOT use this process's PATH alone.
//
// THE THIRD INSTANCE OF THE SAME BUG, and this one broke the daemon. `orchicon install` runs `winget install
// RedHat.Podman`, and winget commits to the REGISTRY — not to the environment of the process that ran it, and
// not to any child it spawns afterwards. The install then starts the runtime daemon with its own (stale) PATH,
// the daemon's ResolveEngine does exec.LookPath("podman"), fails, and exits:
//
//	starting runtime daemon …
//	waiting for the runtime daemon to answer /v1/health …
//	error: runtime daemon did not become ready
//
// Which is EXACTLY the report. WaitForBinary fixed the installer's ladder; the daemon has its own resolution and
// got nothing.
//
// SO THE PATH IS REFRESHED FROM THE OS (internal/adapter reads the registry on Windows and /etc/paths on POSIX),
// and each candidate directory is checked directly. The process PATH is tried FIRST, because it is free and it
// is what a normally-started daemon has.
func lookupEngine(name string) (string, error) {
	if bin, err := exec.LookPath(name); err == nil {
		return bin, nil
	}
	exts := []string{""}
	if runtime.GOOS == "windows" {
		// PATHEXT's own default order, so a `podman.exe` (or the rarer `.cmd`, which an engine never is) resolves
		// the way the shell would. Listed explicitly rather than parsed from the environment: this runs before
		// anything else can be trusted to have a complete environment.
		exts = []string{".exe", ".cmd", ".bat", ""}
	}
	var tried []string
	for _, dir := range adapter.FreshPathDirs() {
		for _, ext := range exts {
			cand := filepath.Join(dir, name+ext)
			tried = append(tried, cand)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand, nil
			}
		}
	}
	// Report the OS-level search honestly: a NotFound that names only `$PATH` hides that we looked in the
	// refreshed list too, which is the difference between "you have not installed it" and "we cannot see it".
	return "", fmt.Errorf("not found on PATH or in the system PATH list (%d entries checked)", len(tried))
}

// ResolveEngineFromEnv resolves the engine from the environment: EnvEngine first,
// then the project-wide EnvEngineAlias, else DefaultEngineName(). One entry point,
// so `cmd/orchicon` cannot resolve the engine differently from a sibling command.
func ResolveEngineFromEnv() (Engine, error) {
	requested := strings.TrimSpace(os.Getenv(EnvEngine))
	if requested == "" {
		requested = strings.TrimSpace(os.Getenv(EnvEngineAlias))
	}
	return ResolveEngine(requested)
}

// MissingError is the actionable error for THIS engine being unavailable,
// mirroring what ResolveEngine returns. The daemon uses it on the defensive
// path where an engine value exists but its binary cannot be executed.
func (e Engine) MissingError() error {
	name := e.name()
	var cause error
	if e.Bin != "" {
		cause = fmt.Errorf("%s: %w", e.Bin, exec.ErrNotFound)
	}
	return &EngineMissingError{Name: name, Requested: e.Name, Err: cause}
}

// VersionArgs is the argv that reports the engine's CLIENT version, which both
// engines answer with the same template (`--format {{.Client.Version}}`). It is
// the VERSION half of the pool key's engine term (ADR-0013 D7.1): a podman
// upgrade can change create/removal semantics under a warm container without
// the engine NAME changing, so the version must color the key too.
func (e Engine) VersionArgs() []string {
	return []string{"version", "--format", "{{.Client.Version}}"}
}

// shapeRelabel is the create-time SELinux relabel marker this build emits for
// its bind mounts. ADR-0013 D3.2 (the `,z` relabel) lands as "z"; until then it
// is "none". It is part of Shape() precisely so that FLIPPING IT invalidates
// every warm container created under the old mount shape — a container created
// without a relabel cannot be relabelled in place (ADR-0013 D7.2).
const shapeRelabel = "none"

// Shape is the create-time SHAPE of a container under this engine, derived
// purely from the Engine value (no exec): the engine family, the namespace
// mapping it emits, and the relabel marker. Two daemons that would create
// structurally DIFFERENT containers must never share a warm one.
func (e Engine) Shape() string {
	return fmt.Sprintf("engine=%s|userns=%s|relabel=%s",
		e.name(), strings.Join(e.UsernsArgs(), ","), shapeRelabel)
}

// name is the engine's normalized family name; a zero-valued Engine is the
// default engine, never "".
func (e Engine) name() string {
	n := strings.ToLower(strings.TrimSpace(e.Name))
	switch n {
	case enginePodman, engineDocker:
		return n
	}
	return DefaultEngineName()
}

// binary is the executable to run: the resolved path, else the engine name
// (resolved by the OS from PATH).
func (e Engine) binary() string {
	if strings.TrimSpace(e.Bin) != "" {
		return e.Bin
	}
	return e.name()
}

// isPodman reports which behaviour family applies.
func (e Engine) isPodman() bool { return e.name() == enginePodman }

// CreateBaseArgs is the engine-independent prefix of every create: detached,
// named, labelled for ownership, and with a stable host alias.
//
// `--add-host host.docker.internal:host-gateway` is emitted for BOTH engines on
// every platform (ADR-0013 D2.4): a host-resident plane advertises
// host.docker.internal (lifecycle.go planePublicURL) so a worker's plane MCP
// channel can dial it. Podman also sets host.containers.internal itself; we
// emit this spelling so the plane URL string stays platform-independent, and
// host-gateway is each engine's own "the host, whatever its address is".
func (e Engine) CreateBaseArgs(name, workflowLabel, instance string) []string {
	return []string{"run", "-d", "--name", name,
		"--label", "orchicon.workflow=" + workflowLabel,
		"--label", "orchicon.instance=" + instance,
		"--add-host", "host.docker.internal:host-gateway",
	}
}

// UsernsArgs is the namespace mapping that puts the container's uid where the
// host uid actually is.
//
// Podman is rootless by default, so `--user <hostuid>:<hostgid>` alone resolves
// into the SUBUID range: the container's uid is not the operator's, and every
// bind-mounted project directory would be written as an unmapped uid. keep-id
// maps the container uid back onto the invoking (host) uid, which is what the
// runtime has always promised — the container writes project files AS THE
// OPERATOR, never as root. `--user` is preserved alongside it (AC 6).
//
// Docker has no equivalent: `--user` is already the host uid there.
func (e Engine) UsernsArgs() []string {
	if !e.isPodman() {
		return nil
	}
	return []string{"--userns=keep-id"}
}

// PublishArgs publishes each container port on the HOST LOOPBACK with an
// engine-assigned host port ("-p 127.0.0.1::<containerPort>").
//
// Podman only: a rootless podman container's IP lives in a per-user network
// namespace and is NOT host-routable, so the plane cannot dial the container IP
// the way it does under Docker (ADR-0013 D2.1). The published loopback port is
// the address instead. Docker keeps today's behaviour exactly — no publish, and
// the plane dials the bridge IP — because the grandfathered containerized-plane
// case depends on it.
//
// The container ports this publishes are FIXED constants (defaultServePort,
// sandboxPlanePort), which is what makes publishing at create time possible at
// all.
func (e Engine) PublishArgs(ports ...int) []string {
	if !e.isPodman() {
		return nil
	}
	var out []string
	for _, p := range ports {
		if p <= 0 {
			continue
		}
		out = append(out, "-p", fmt.Sprintf("127.0.0.1::%d", p))
	}
	return out
}

// HostAddrArgs is the argv that reports where a container port is reachable
// FROM THE HOST: podman's published-port mapping, or docker's bridge IP.
func (e Engine) HostAddrArgs(name string, cport int) []string {
	if e.isPodman() {
		return []string{"port", name, fmt.Sprintf("%d/tcp", cport)}
	}
	return []string{"inspect", "--format", containerIPFormat, name}
}

// containerIPFormat is docker's bridge-IP format. Podman answers the same
// query but the IP is inside the Podman machine VM on macOS/Windows and
// unroutable, which is why it is only ever used on the docker path.
const containerIPFormat = "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"

// HostEndpoint resolves the host-reachable host:port for a container port, and
// the host port to advertise.
//
// Podman: parse `podman port <name> <cport>/tcp`, which prints the
// engine-assigned host port bound to loopback. Docker: the container IP, and
// the container port itself (the serve binds inside the container and the
// plane dials the bridge).
//
// run is the engine invoker (d.engine in production), kept as a parameter so
// the resolution is unit-testable without an engine binary.
func (e Engine) HostEndpoint(name string, cport int, run func(args ...string) (string, error)) (string, int, error) {
	if run == nil {
		return "", 0, errors.New("engine: no invoker supplied for host address resolution")
	}
	out, err := run(e.HostAddrArgs(name, cport)...)
	if err != nil {
		return "", 0, fmt.Errorf("%s: resolve host address for %s port %d/tcp: %s: %w",
			e.name(), name, cport, strings.TrimSpace(out), err)
	}
	if !e.isPodman() {
		ip := strings.TrimSpace(out)
		if ip == "" || ip == "<no value>" {
			return "", 0, fmt.Errorf("%s: container %s reports no IP address (is it running?)", e.name(), name)
		}
		return ip, cport, nil
	}
	return parsePublishedAddr(out, cport)
}

// parsePublishedAddr picks the loopback address out of `podman port` output.
// The output is one mapping per line ("127.0.0.1:32987"); publishing with
// `-p 127.0.0.1::<port>` normally yields exactly that, but the parser prefers a
// loopback host and treats a wildcard binding as loopback rather than failing.
func parsePublishedAddr(out string, cport int) (string, int, error) {
	var fbHost string
	var fbPort int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		host, portStr, err := net.SplitHostPort(line)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 {
			continue
		}
		host = strings.Trim(host, "[]")
		switch host {
		case "", "0.0.0.0", "::", "::1":
			host = "127.0.0.1"
		}
		if strings.HasPrefix(host, "127.") {
			return host, port, nil
		}
		if fbPort == 0 {
			fbHost, fbPort = host, port
		}
	}
	if fbPort != 0 {
		return fbHost, fbPort, nil
	}
	return "", 0, fmt.Errorf("no host port published for container port %d/tcp (engine said %q)",
		cport, strings.TrimSpace(out))
}

// ImageLabelFormat is the inspect template that reads one image label.
//
// THIS DIFFERS BY ENGINE: podman exposes labels at the top level (`.Labels`),
// docker nests them under the image config (`.Config.Labels`). The runtime-base
// gate is fail-closed, so using docker's spelling on podman does not error — it
// silently reports "no label" and rejects EVERY custom image built on the base.
func (e Engine) ImageLabelFormat(label string) string {
	if e.isPodman() {
		return `{{index .Labels "` + label + `"}}`
	}
	return `{{index .Config.Labels "` + label + `"}}`
}

// NameInUseTokens are the substrings that mean "the container name is still
// settling" — remove-and-retry, do not fail the create.
//
// The wordings differ by engine and the predicate must cover each engine's
// real text, or a retry that used to work silently starts failing the create:
//   - docker: "Conflict. The container name ... is already in use",
//     "removal of container ... is already in progress".
//   - podman: `Error: creating container storage: the container name "x" is
//     already in use by y` and, right after `rm -f`, "container is in use".
func (e Engine) NameInUseTokens() []string {
	if e.isPodman() {
		return []string{"already in use", "is in use", "in progress"}
	}
	return []string{"Conflict", "in progress", "is already"}
}

// EngineNameForError is the engine's name for operator-facing error strings
// (e.g. "<engine> build: ..."), never the zero value.
func (e Engine) EngineNameForError() string { return e.name() }

// enginePlatform is the GOOS the engine runs on; a hook so the same code paths
// can name the platform in diagnostics.
func enginePlatform() string { return runtime.GOOS }

// nameInUse reports whether an engine's failed create failed for the RECOVERABLE
// reason — the name it wants is still settling in the engine's storage. The
// comparison is case-insensitive because the engines capitalise differently
// (docker's "Conflict.", podman's lowercase "error: ...").
func nameInUse(out string, tokens []string) bool {
	lower := strings.ToLower(out)
	for _, t := range tokens {
		if t == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// IsAbsentOutput reports whether an engine command's output means "that image
// is not here". This wording is one of the few places the engines differ in a
// way the image path cares about:
//
//   - docker rmi: "Error: No such image: foo:latest"
//   - podman rmi: "Error: foo:latest: image not known"
//   - inspect of a missing ref: the same two families of wording
//
// Matching is case-insensitive and deliberately narrow: it exists ONLY to turn
// a failed removal into the idempotent "already gone" result (DELETE
// /v1/images on an image that is already absent), never to accept any other
// failure. Anything unrecognised stays a real error, so a genuine rmi failure
// cannot be swallowed.
func (e Engine) IsAbsentOutput(out string) bool {
	l := strings.ToLower(out)
	for _, s := range []string{"no such image", "image not known", "no such object"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// planEngineNameForError is the engine family name for an operator-facing
// message (lower case: "podman" / "docker").
//
// Resolution cannot fail into the WRONG family here: ResolveEngine reports the
// family it resolved even when the binary is missing, so a host without the
// engine on PATH still names the engine it was configured for.
func planEngineNameForError() string {
	eng, _ := ResolveEngineFromEnv()
	return eng.name()
}

// containerResidencyNeedsOverride reports whether a CONTAINER-RESIDENT plane
// under this engine can derive its own address at all, and therefore whether an
// explicit ORCHICON_PLANE_PUBLIC_URL is REQUIRED.
//
// Docker writes "<ip> <hostname>" into the container's /etc/hosts and its
// 172.17.0.1 gateway exists, so planePublicURL's (lifecycle.go) historical
// gateway fallback stays a correct guard. Podman — rootless on Linux
// (slirp4netns/pasta) and always across the podman-machine VM boundary on
// macOS/Windows — writes no such entry and has no 172.17.0.1 gateway: there is
// NO derivable address. Emitting the Docker gateway there would hand every
// worker in the run a URL that can only time out, far from the cause, so
// planePublicURL returns "" and planeURLForRun fails LOUDLY naming
// ORCHICON_PLANE_PUBLIC_URL.
func containerResidencyNeedsOverride() bool {
	eng, _ := ResolveEngineFromEnv()
	return eng.isPodman()
}
