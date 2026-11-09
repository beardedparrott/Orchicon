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

// Engine is the container engine the runtime daemon drives: which CLI family it is, and the
// resolved binary to execute.
type Engine struct {
	// Name is the engine's CLI family: "podman" or "docker".
	Name string
	// Bin is the resolved binary (an absolute path from exec.LookPath when the engine was resolved by
	// ResolveEngine; "" means "not resolved".
	Bin string
}

// Engine names.
const (
	enginePodman = "podman"
	engineDocker = "docker"
)

// EnvEngine is the environment variable that selects the engine, so an operator
// (or a docker-only host) can override the default without a rebuild.
const EnvEngine = "ORCHICON_RUNTIME_ENGINE"

// EnvEngineAlias is the engine variable the rest of the project already honours:
// scripts/lib/engine.sh, scripts/runtime.sh (image build/pull) and the Makefile all read
// ORCHICON_ENGINE.
const EnvEngineAlias = "ORCHICON_ENGINE"

// DefaultEngineName returns the engine the daemon uses when nothing overrides it.
func DefaultEngineName() string { return enginePodman }

// EngineMissingError is a container engine that is not on PATH.
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

// ResolveEngine resolves the configured engine to an executable.
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
		// PATHEXT's own default order, so a `podman.exe` (or the rarer `.cmd`, which an engine never is)
		// resolves the way the shell would.
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

// ResolveEngineFromEnv resolves the engine from the environment: EnvEngine first, then the
// project-wide EnvEngineAlias, else DefaultEngineName().
func ResolveEngineFromEnv() (Engine, error) {
	requested := strings.TrimSpace(os.Getenv(EnvEngine))
	if requested == "" {
		requested = strings.TrimSpace(os.Getenv(EnvEngineAlias))
	}
	return ResolveEngine(requested)
}

// MissingError is the actionable error for THIS engine being unavailable, mirroring what
// ResolveEngine returns.
func (e Engine) MissingError() error {
	name := e.name()
	var cause error
	if e.Bin != "" {
		cause = fmt.Errorf("%s: %w", e.Bin, exec.ErrNotFound)
	}
	return &EngineMissingError{Name: name, Requested: e.Name, Err: cause}
}

// VersionArgs is the argv that reports the engine's CLIENT version, which both engines answer with
// the same template (`--format {{.Client.Version}}`).
func (e Engine) VersionArgs() []string {
	return []string{"version", "--format", "{{.Client.Version}}"}
}

// shapeRelabel is the create-time SELinux relabel marker this build emits for its bind mounts.
const shapeRelabel = "none"

// Shape is the create-time SHAPE of a container under this engine, derived purely from the Engine
// value (no exec): the engine family, the namespace mapping it emits, and the relabel marker.
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

// CreateBaseArgs is the engine-independent prefix of every create: detached, named, labelled for
// ownership, and with a stable host alias.
func (e Engine) CreateBaseArgs(name, workflowLabel, instance string) []string {
	return []string{"run", "-d", "--name", name,
		"--label", "orchicon.workflow=" + workflowLabel,
		"--label", "orchicon.instance=" + instance,
		"--add-host", "host.docker.internal:host-gateway",
	}
}

// UsernsArgs is the namespace mapping that puts the container's uid where the host uid actually is.
func (e Engine) UsernsArgs() []string {
	if !e.isPodman() {
		return nil
	}
	return []string{"--userns=keep-id"}
}

// PublishArgs publishes each container port on the HOST LOOPBACK with an engine-assigned host port
// ("-p 127.0.0.1::<containerPort>").
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

// containerIPFormat is docker's bridge-IP format.
const containerIPFormat = "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"

// HostEndpoint resolves the host-reachable host:port for a container port, and the host port to
// advertise.
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

// nameInUse reports whether an engine's failed create failed for the RECOVERABLE reason — the name
// it wants is still settling in the engine's storage.
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

// This wording is one of the few places the engines differ in a way the image path cares about: -
// docker rmi: "Error: No such image: foo:latest" - podman rmi: "Error: foo:latest.
func (e Engine) IsAbsentOutput(out string) bool {
	l := strings.ToLower(out)
	for _, s := range []string{"no such image", "image not known", "no such object"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// planEngineNameForError is the engine family name for an operator-facing message (lower case:
// "podman" / "docker").
func planEngineNameForError() string {
	eng, _ := ResolveEngineFromEnv()
	return eng.name()
}

// containerResidencyNeedsOverride reports whether a CONTAINER-RESIDENT plane under this engine can
// derive its own address at all.
func containerResidencyNeedsOverride() bool {
	eng, _ := ResolveEngineFromEnv()
	return eng.isPodman()
}
