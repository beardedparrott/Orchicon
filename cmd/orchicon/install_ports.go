package main

// Host-side port selection and plane residency for `orchicon install`.
//
// THE PROBLEM THIS SOLVES. Every port the installer published was a literal
// chosen by instance name: dev got 8080/3002, prod got 8091/3003, and anything
// else fell through to dev's numbers. On a host that already runs something on
// 8080 — another product, an earlier Orchicon, a dev server, a local Postgres —
// the install died with a raw docker bind error, and an unrecognised instance
// name was GUARANTEED to collide, because the fall-through reused dev's ports.
//
// The operator now sees each port with its default in parentheses and takes it
// with an empty line, so the common case is still "press enter", and every port
// can be pinned non-interactively with ORCHICON_<X>_PORT.
//
// WHERE THE PROMPT READS — NOT STDIN, AND THAT IS LOAD-BEARING.
// scripts/install.sh is normally run as `curl -fsSL …/install | bash`, so
// BASH'S STDIN IS THE SCRIPT ITSELF and bash hands that same descriptor to
// `"$bin" install` (install.sh's one-command setup line has no redirect). A
// read from stdin therefore sees EOF or, worse, EATS THE REST OF THE SCRIPT.
// The prompt reads the CONTROLLING TERMINAL (/dev/tty) instead — the terminal
// the operator is actually looking at, whether the installer arrived by curl or
// was run directly — and takes the defaults, without blocking, when there is no
// terminal at all (CI, a container build, cron, nohup).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Plane residency: WHERE the control plane process runs. `host` is the
// product's default (scripts/container.sh residency_for resolves ${...:-host}):
// the container runs the SERVICES only and the plane runs on the host as its
// own `orchicon serve` process. `container` is the self-contained shape, with
// the whole stack — plane included — inside the instance container.
const (
	residencyHost      = "host"
	residencyContainer = "container"
)

// residencyForInstall resolves the plane residency for this install.
//
// IT MIRRORS container.sh's residency_for, including the default, because the
// two launchers must produce the SAME shape for one instance: the residency is
// recorded on the container at CREATE time (the ORCHICON_CONTAINER_SERVICES_ONLY
// flag) and cannot be changed on a running container, so a disagreement here is
// a disagreement about what the instance IS.
func residencyForInstall() (string, error) {
	v := strings.TrimSpace(os.Getenv("ORCHICON_PLANE_RESIDENCY"))
	if v == "" {
		return residencyHost, nil
	}
	if v != residencyHost && v != residencyContainer {
		return "", fmt.Errorf("ORCHICON_PLANE_RESIDENCY must be %q or %q (got %q)", residencyHost, residencyContainer, v)
	}
	return v, nil
}

// hostDataDir is the per-instance host state directory — where the secrets KEK,
// the blob store, the ask history and the detached plane's PID file live.
//
// IT IS DERIVED FROM THE INSTANCE, exactly as container.sh's instance_info does,
// and ORCHICON_DATA_DIR is deliberately NOT read here. That is not an oversight:
// container.sh's plane_env SETS ORCHICON_DATA_DIR for the host plane, so an
// operator-exported value is overwritten there — and the KEK lives at
// <DataDir>/secrets/kek. Two launchers disagreeing about the data dir is the one
// disagreement that silently orphans every tenant secret, so this follows the
// launcher rather than inventing a knob.
func hostDataDir(instance string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for the instance data dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "orchicon-"+instance), nil
}

// portBinding says how a port is exposed to the host in a given residency.
type portBinding string

const (
	// bindNone: not exposed by the instance CONTAINER in this shape.
	bindNone portBinding = "none"
	// bindLoopback: published on 127.0.0.1 only.
	bindLoopback portBinding = "loopback"
	// bindAny: published on every interface.
	bindAny portBinding = "any"
)

// installPort is one host-side port an instance uses.
//
// THE DEFAULTS ARE NOT ARBITRARY. "dev" and "prod" are two instances on ONE
// host (scripts/container.sh instance_info), so their ports must be disjoint or
// one instance's plane answers for the other's. These literals are the same
// table container.sh uses, and TestInstallPortDefaultsMatchContainerScript
// parses that file and fails if the two ever drift — the Go and bash halves
// cannot share a constant, so the agreement is asserted instead.
type installPort struct {
	// Env is the variable that pins the port and skips the prompt.
	Env string
	// Label is the name shown in the prompt.
	Label string
	// Defaults is the per-instance default, keyed by instance name.
	//
	// A MAP RATHER THAN Dev/Prod FIELDS, and that is a correctness fix as much as
	// a convenience: the old shape made anything that was not literally "prod"
	// fall back to dev's column, so a THIRD instance silently collided with dev on
	// every port — the collision this table exists to prevent. With a map, an
	// instance that is not listed is an ERROR (defaultFor reports ok=false) rather
	// than a silent clash.
	Defaults map[string]int
	// ContainerPort is the FIXED in-container port this host port maps to. Only
	// the host half varies, so this is what identifies the port in a running
	// container's published bindings.
	ContainerPort int
	// ContainerBind / HostBind: what the instance CONTAINER publishes in each
	// residency. They differ because the shapes expose different things — a
	// container-resident plane needs its own port published, while a
	// host-resident one takes the plane port for itself and publishes the
	// services the host plane reaches over loopback instead.
	ContainerBind portBinding
	HostBind      portBinding
	// HostPlaneBinds marks the port the HOST PLANE listens on in host residency
	// (the control port). The container does NOT publish it in that shape —
	// docker reports it as exposed-but-null — so unlike every other port it
	// cannot be read back from the container's bindings, and the plane's own
	// invocation must be the source of truth for it.
	HostPlaneBinds bool
	// Prompt marks the ports an operator is asked about. All bound ports are
	// still probed and pinnable; this only decides which are offered
	// interactively, because the telemetry-internal ports are derived plumbing
	// rather than decisions. Add the field to widen the prompt.
	Prompt bool
}

// instanceOrder is the canonical set of instances this table defines, in the
// order they are documented and reported. It must match the columns
// scripts/container.sh's instance_info defines, because the two launchers have
// to agree on which instances exist — TestInstallPortDefaultsMatchContainerScript
// asserts that.
var instanceOrder = []string{"dev", "prod", "test"}

// portDefaults builds one port's per-instance defaults. A helper so every row of
// the table has the same columns by construction, rather than by remembering to
// add a third number each time.
func portDefaults(dev, prod, test int) map[string]int {
	return map[string]int{"dev": dev, "prod": prod, "test": test}
}

// installerPorts returns every port an instance can use, in prompt order.
//
// The `test` column exists so a throwaway instance can run beside dev and prod
// without colliding with either — it is the third instance `container.sh` now
// knows, and its ports mirror that table's.
func installerPorts() []installPort {
	return []installPort{
		{Env: "ORCHICON_CONTROL_PORT", Label: "Control plane (web UI + API)", Defaults: portDefaults(8080, 8091, 8092),
			ContainerPort: 8080, ContainerBind: bindAny, HostBind: bindNone, HostPlaneBinds: true, Prompt: true},
		{Env: "ORCHICON_GRAFANA_PORT", Label: "Grafana (dashboards)", Defaults: portDefaults(3002, 3003, 3004),
			ContainerPort: 3000, ContainerBind: bindAny, HostBind: bindLoopback, Prompt: true},
		{Env: "ORCHICON_POSTGRES_PORT", Label: "PostgreSQL", Defaults: portDefaults(5432, 5433, 5434),
			ContainerPort: 5432, ContainerBind: bindNone, HostBind: bindLoopback, Prompt: true},
		{Env: "ORCHICON_NATS_PORT", Label: "NATS (event bus)", Defaults: portDefaults(4222, 4223, 4224),
			ContainerPort: 4222, ContainerBind: bindNone, HostBind: bindLoopback},
		{Env: "ORCHICON_NATS_MONITOR_PORT", Label: "NATS monitoring", Defaults: portDefaults(8222, 8223, 8224),
			ContainerPort: 8222, ContainerBind: bindNone, HostBind: bindLoopback},
		{Env: "ORCHICON_OTLP_GRPC_PORT", Label: "OTLP gRPC (telemetry)", Defaults: portDefaults(4317, 4319, 4321),
			ContainerPort: 4317, ContainerBind: bindNone, HostBind: bindLoopback},
		{Env: "ORCHICON_OTLP_HTTP_PORT", Label: "OTLP HTTP (telemetry)", Defaults: portDefaults(4318, 4320, 4322),
			ContainerPort: 4318, ContainerBind: bindNone, HostBind: bindLoopback},
		{Env: "ORCHICON_TEMPO_PORT", Label: "Tempo (traces)", Defaults: portDefaults(3200, 3201, 3202),
			ContainerPort: 3200, ContainerBind: bindNone, HostBind: bindLoopback},
		{Env: "ORCHICON_LOKI_PORT", Label: "Loki (logs)", Defaults: portDefaults(3100, 3101, 3102),
			ContainerPort: 3100, ContainerBind: bindNone, HostBind: bindLoopback},
		{Env: "ORCHICON_VM_PORT", Label: "VictoriaMetrics (metrics)", Defaults: portDefaults(8428, 8429, 8430),
			ContainerPort: 8428, ContainerBind: bindNone, HostBind: bindLoopback},
	}
}

// defaultFor returns the port this instance uses, and whether the table defines
// one. ok=false is a REFUSAL, not a fallback: see the Defaults field for why an
// unknown instance must not inherit another's port.
func (p installPort) defaultFor(instance string) (int, bool) {
	v, ok := p.Defaults[instance]
	return v, ok
}

// validateInstance reports whether the table defines defaults for every port on
// this instance. Called once up front so the rest of the resolution can assume a
// known instance.
func validateInstance(instance string) error {
	for _, p := range installerPorts() {
		if _, ok := p.Defaults[instance]; !ok {
			return fmt.Errorf("instance %q has no port defaults (known: %s) — pin every port explicitly with ORCHICON_*_PORT, or add the instance to the table",
				instance, strings.Join(instanceOrder, ", "))
		}
	}
	return nil
}

// boundIn reports whether this port is exposed at all in a residency. A port
// that is not bound in a shape cannot collide there, so it is neither probed nor
// prompted for that shape — which is why a container-resident install asks about
// two ports and a host-resident one about the services too.
func (p installPort) boundIn(residency string) bool {
	if residency == residencyHost {
		return p.HostBind != bindNone || p.HostPlaneBinds
	}
	return p.ContainerBind != bindNone
}

// activeInstallPorts filters the table to the ports a residency actually binds.
func activeInstallPorts(residency string) []installPort {
	var out []installPort
	for _, p := range installerPorts() {
		if p.boundIn(residency) {
			out = append(out, p)
		}
	}
	return out
}

// publishArgs builds the instance container's -p arguments for a residency.
//
// EVERY service publish is LOOPBACK-BOUND, and that is the security boundary:
// postgres is a database and NATS is an internal event bus, and neither may be
// reachable from the LAN. The supervisor's pg_hba trust rules depend on it.
func publishArgs(residency string, ports map[string]int, specs []installPort) []string {
	var args []string
	for _, p := range specs {
		bind := p.ContainerBind
		if residency == residencyHost {
			bind = p.HostBind
		}
		if bind == bindNone {
			continue
		}
		host := ""
		if bind == bindLoopback {
			host = "127.0.0.1:"
		}
		args = append(args, "-p", fmt.Sprintf("%s%d:%d", host, ports[p.Env], p.ContainerPort))
	}
	return args
}

// portFree reports whether the loopback address at port can be bound.
//
// LOOPBACK IS THE CORRECT SCOPE: every service publish is bound to 127.0.0.1,
// and the plane binds loopback plus the docker bridge. A bind attempt also
// catches a holder on 0.0.0.0:port, because a wildcard listener overlaps the
// loopback address rather than sitting beside it — so the overlap shows up here
// as a failed bind, which is exactly the collision the caller needs to know
// about.
//
// The probe is inherently a moment in time: it closes the listener and docker
// binds later. That race is accepted deliberately — the alternative is no
// pre-flight at all — which is why a docker bind failure must still be reported
// with the port knob named rather than as a bare docker error.
func portFree(port int) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// parseInstancePorts reads `docker inspect` port bindings and maps them back to
// the ORCHICON_<X>_PORT keys. Split from runningInstancePorts so the parsing is
// testable without a docker daemon.
//
// A port the container EXPOSES but does not PUBLISH parses as docker's `null`
// binding and is deliberately skipped: in host residency the control port looks
// exactly like that (the host plane owns it), and reporting it as a read-back
// value would invent a port nothing is listening on.
func parseInstancePorts(raw []byte, specs []installPort) (map[string]int, bool) {
	var bindings map[string][]struct {
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal(raw, &bindings); err != nil {
		return nil, false
	}
	// The container-side port is the numeric half of the key ("8080/tcp").
	hostByContainer := map[int]int{}
	for key, bs := range bindings {
		cp, err := strconv.Atoi(strings.SplitN(key, "/", 2)[0])
		if err != nil || len(bs) == 0 {
			continue
		}
		hp, err := strconv.Atoi(bs[0].HostPort)
		if err != nil {
			continue
		}
		hostByContainer[cp] = hp
	}
	ports := map[string]int{}
	for _, p := range specs {
		if hp, ok := hostByContainer[p.ContainerPort]; ok {
			ports[p.Env] = hp
		}
	}
	return ports, len(ports) > 0
}

// runningInstancePorts reads the host ports an ALREADY-RUNNING instance of ours
// publishes.
//
// IT EXISTS TO STOP THE INSTALL SPLITTING AN INSTANCE. `orchicon install` is
// idempotent by design — ensureInstallContainer reports the existing instance
// rather than making a second one — and a bare port probe would break exactly
// that: the ports of a RUNNING instance are, by definition, taken BY US.
// Treating our own listener as a conflict and "resolving" it upward would leave
// the running instance untouched and put a second one beside it. Reading back
// the real bindings is what keeps a re-run a no-op — and it is why this rung
// sits ABOVE the prompt rather than inside it.
//
// THE PLANE PORT IS NOT RECOVERABLE THIS WAY in host residency, and that is a
// documented limit rather than a bug: the container does not publish it, so its
// value comes from resolving the override again. An override must therefore be
// STABLE across invocations (a shell profile or a unit file) — the same
// constraint container.sh's plane_status already has, since it too probes the
// port it just resolved.
func runningInstancePorts(name string, specs []installPort) (map[string]int, bool) {
	out, err := exec.Command("docker", "inspect",
		"--format", "{{json .NetworkSettings.Ports}}", name).Output()
	if err != nil {
		return nil, false
	}
	return parseInstancePorts(out, specs)
}

// promptTerminal opens the CONTROLLING TERMINAL for the port prompt — never
// stdin (see the file comment). The bool reports whether the caller owns the
// file and must close it: os.Stdin is borrowed, /dev/tty is ours.
func promptTerminal() (*os.File, bool) {
	if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		return f, true
	}
	// No /dev/tty (Windows, or a host whose controlling terminal was detached).
	// Fall back to stdin only when it really is a terminal — otherwise a prompt
	// on a pipe would consume whatever is being piped.
	if isCharDevice(os.Stdin) {
		return os.Stdin, false
	}
	return nil, false
}

// isCharDevice reports whether f is a terminal-like character device. Uses the
// standard library rather than golang.org/x/term: x/term is only a TRANSITIVE
// dependency here (reached through bubbletea), so importing it would promote it
// to a direct one and rewrite go.mod for a single mode test.
func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// askInstallPort reads one port, returning def when the line is empty — which is
// what makes "hit enter to accept defaults" true.
//
// Invalid input is re-asked, but only a bounded number of times: a degenerate
// terminal that never delivers a newline (or delivers EOF forever) must fall
// back to the default rather than spin. An EOF with no content takes the
// default immediately, which is what makes a closed tty harmless.
func askInstallPort(r *bufio.Reader, out io.Writer, label string, def int) int {
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(out, "  %s (%d): ", label, def)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(out)
			return def
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		n, convErr := strconv.Atoi(line)
		if convErr == nil && n >= 1 && n <= 65535 {
			return n
		}
		fmt.Fprintf(out, "  %q is not a port number (1..65535) — try again.\n", line)
	}
	fmt.Fprintf(out, "  too many invalid entries — using the default %d.\n", def)
	return def
}

// planInstallPorts is the whole port decision, with every host interaction
// injected so it is deterministic under test: `probe` decides conflicts and `r`
// is the prompt reader (nil = no terminal, take the defaults).
//
// PRECEDENCE, and the reason for each rung:
//
//  1. an explicit pin (pinned) always wins and is never prompted for, so
//     automation stays non-interactive.
//  2. the per-instance default — unchanged behaviour for anyone whose host is
//     not already using the port.
//  3. on a conflict, the next free port is OFFERED as the new default.
//
// A port taken by SOMETHING ELSE is never silently reassigned. The conflict is
// reported and the next free port becomes the default, so the operator still
// gets one-keystroke enumeration but can SEE that it happened — silent shifting
// makes the install's shape depend on host state, which is not reproducible and
// cannot be reviewed. ORCHICON_STRICT_PORTS=1 makes the conflict fatal instead,
// for automated installs where a moved port would be worse than a failed one.
//
// EVERY bound port is probed, prompted or not: the telemetry-internal ports are
// not worth a question, but they must not collide unhandled either.
func planInstallPorts(instance string, specs []installPort, pinned map[string]int, probe func(int) bool, r *bufio.Reader, out io.Writer) (map[string]int, error) {
	ports := map[string]int{}
	claimed := map[int]bool{}
	strict := os.Getenv("ORCHICON_STRICT_PORTS") == "1"
	var pending []installPort

	// Rung 1: explicit pins win.
	for _, p := range specs {
		n, ok := pinned[p.Env]
		if !ok {
			pending = append(pending, p)
			continue
		}
		if claimed[n] {
			return nil, fmt.Errorf("%s=%d collides with another port pinned for this install", p.Env, n)
		}
		ports[p.Env] = n
		claimed[n] = true
		fmt.Fprintf(out, "%s pinned to %d (%s)\n", p.Label, n, p.Env)
		// A pin is the operator's decision and is never reassigned — but silently
		// carrying a busy port through to a docker bind failure would hide the
		// cause. Warn, then comply.
		if !probe(n) {
			fmt.Fprintf(out, "warning: %s is pinned to %d, which is already in use — the instance will not start until it frees up\n", p.Label, n)
		}
	}

	// The note, printed only when something will actually ask.
	if r != nil && len(pending) > 0 {
		fmt.Fprintln(out, `Ports — hit "enter" to accept defaults.`)
	}

	for _, p := range pending {
		def, ok := p.defaultFor(instance)
		if !ok {
			return nil, fmt.Errorf("no default port for %s on instance %q (known: %s) — pin it with %s",
				p.Label, instance, strings.Join(instanceOrder, ", "), p.Env)
		}
		if claimed[def] || !probe(def) {
			next, ok := nextFreePortFor(def+1, claimed, probe)
			if !ok {
				return nil, fmt.Errorf("no free port for %s (%s) at or above %d — pin it explicitly", p.Label, p.Env, def+1)
			}
			if strict {
				return nil, fmt.Errorf("%s default port %d is in use — set %s (ORCHICON_STRICT_PORTS=1)", p.Label, def, p.Env)
			}
			fmt.Fprintf(out, "%s: port %d is in use — suggesting %d\n", p.Label, def, next)
			def = next
		}
		// Only the operator-facing ports are asked about; the rest resolve to
		// their default here, having still been probed and conflict-resolved.
		if r == nil || !p.Prompt {
			ports[p.Env] = def
			claimed[def] = true
			continue
		}
		chosen := askInstallPort(r, out, p.Label, def)
		if claimed[chosen] {
			fmt.Fprintf(out, "  %d is already assigned to another Orchicon port in this install — using %d\n", chosen, def)
			chosen = def
		}
		ports[p.Env] = chosen
		claimed[chosen] = true
	}

	// Without an interactive echo the choices would otherwise be invisible, and
	// an operator should never have to infer which ports an install bound.
	if r == nil {
		for _, p := range specs {
			fmt.Fprintf(out, "  %s: %d\n", p.Label, ports[p.Env])
		}
	}
	return ports, nil
}

// nextFreePortFor walks upward from `from` for a port that is free AND not
// already claimed by another port in this same run, with the probe injected so
// the pure decision loop never reaches for a real socket.
//
// The walk is bounded: a fully occupied range must fail with a clear error
// rather than scanning 65k sockets one bind at a time.
func nextFreePortFor(from int, claimed map[int]bool, probe func(int) bool) (int, bool) {
	for p := from; p < from+1000 && p <= 65535; p++ {
		if claimed[p] {
			continue
		}
		if probe(p) {
			return p, true
		}
	}
	return 0, false
}

// resolveInstallPorts decides the host-side ports this install uses: the
// running-instance rung, then the environment pins, then planInstallPorts.
//
// tty may be nil, meaning no terminal: the defaults are taken without blocking.
func resolveInstallPorts(instance, residency string, out io.Writer, tty *os.File) (map[string]int, error) {
	specs := activeInstallPorts(residency)
	name := "orchicon-cnt-" + instance

	// Refuse an instance the table does not define, BEFORE anything is resolved or
	// asked. A silent fallback to dev's ports is what made adding an instance
	// collide with an existing one.
	if err := validateInstance(instance); err != nil {
		return nil, err
	}

	// Rung 0, and it must come FIRST: a running instance of ours owns its ports.
	// Probing them would report our own listener as a conflict; see
	// runningInstancePorts for why that would split the instance.
	if running, _ := containerRunning(name); running {
		if live, ok := runningInstancePorts(name, specs); ok {
			fmt.Fprintf(out, "instance %q is already running — keeping its ports\n", name)
			for _, p := range specs {
				if _, ok := live[p.Env]; !ok {
					// Not published by the container in this shape (the plane port in
					// host residency): resolve it the way the plane was started. The
					// instance is already validated above, so a default exists.
					if d, ok := p.defaultFor(instance); ok {
						live[p.Env] = d
					}
				}
				fmt.Fprintf(out, "  %s: %d\n", p.Label, live[p.Env])
			}
			return live, nil
		}
		fmt.Fprintf(out, "warning: %s is running but its published ports could not be read — resolving ports fresh\n", name)
	}

	pinned := map[string]int{}
	for _, p := range specs {
		v := strings.TrimSpace(os.Getenv(p.Env))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s=%q is not a port (1..65535)", p.Env, v)
		}
		pinned[p.Env] = n
	}

	var r *bufio.Reader
	if tty != nil {
		r = bufio.NewReader(tty)
	}
	return planInstallPorts(instance, specs, pinned, portFree, r, out)
}
