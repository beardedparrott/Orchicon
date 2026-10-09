package main

// Host-side port selection for `orchicon install`.
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
	"strconv"
	"strings"
)

// installPort is one host-side port the installer publishes for an instance.
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
	// Dev and Prod are the per-instance defaults (the historical literals).
	Dev, Prod int
	// ContainerPort is the FIXED in-container port this host port maps to: the
	// supervisor listens on :8080 and Grafana on :3000. Only the host half
	// varies, so this is what identifies the port in a running container's
	// published bindings.
	ContainerPort int
}

// installerPorts returns the ports the installer publishes, in prompt order.
func installerPorts() []installPort {
	return []installPort{
		{Env: "ORCHICON_CONTROL_PORT", Label: "Control plane (web UI + API)", Dev: 8080, Prod: 8091, ContainerPort: 8080},
		{Env: "ORCHICON_GRAFANA_PORT", Label: "Grafana (dashboards)", Dev: 3002, Prod: 3003, ContainerPort: 3000},
	}
}

// defaultFor returns the port this instance has always used.
func (p installPort) defaultFor(instance string) int {
	if instance == "prod" {
		return p.Prod
	}
	return p.Dev
}

// portFree reports whether the loopback address at port can be bound.
//
// LOOPBACK IS THE CORRECT SCOPE: every published port is bound to 127.0.0.1
// (that loopback publish is the security boundary the supervisor's pg_hba trust
// rules depend on). A bind attempt also catches a holder on 0.0.0.0:port,
// because a wildcard listener overlaps the loopback address rather than sitting
// beside it — so the overlap shows up here as a failed bind, which is exactly
// the collision the caller needs to know about.
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
		def := p.defaultFor(instance)
		if claimed[def] || !probe(def) {
			next, ok := nextFreePortFor(def+1, claimed, probe)
			if !ok {
				return nil, fmt.Errorf("no free port for %s (%s) at or above %d — pin it explicitly", p.Label, p.Env, def+1)
			}
			if strict {
				return nil, fmt.Errorf("%s default port %d is in use — set %s (ORCHICON_STRICT_PORTS=1)", p.Label, def, p.Env)
			}
			fmt.Fprintf(out, "%s: port %d is in use — suggesting %d\n", p.Label, def, next)
			if r != nil {
				def = next
			} else {
				// No terminal to confirm with: take the suggestion, and say so
				// below, because the install's shape now depends on host state.
				ports[p.Env] = next
				claimed[next] = true
				continue
			}
		}
		if r == nil {
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

	// Without a terminal nothing echoed the choices, so state them once: an
	// operator should never have to infer which ports a headless install bound.
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

// resolveInstallPorts decides the host-side ports this install publishes: the
// running-instance rung, then the environment pins, then planInstallPorts.
//
// tty may be nil, meaning no terminal: the defaults are taken without blocking.
func resolveInstallPorts(instance string, out io.Writer, tty *os.File) (map[string]int, error) {
	specs := installerPorts()
	name := "orchicon-cnt-" + instance

	// Rung 0, and it must come FIRST: a running instance of ours owns its ports.
	// Probing them would report our own listener as a conflict; see
	// runningInstancePorts for why that would split the instance.
	if running, _ := containerRunning(name); running {
		if live, ok := runningInstancePorts(name, specs); ok {
			fmt.Fprintf(out, "instance %q is already running — keeping its ports\n", name)
			for _, p := range specs {
				if _, ok := live[p.Env]; !ok {
					live[p.Env] = p.defaultFor(instance)
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
