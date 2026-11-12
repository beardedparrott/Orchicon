package main

// Host-side port selection and plane residency for `orchicon install`.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// residencyForInstall resolves the plane residency for this install.
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
type installPort struct {
	// Env is the variable that pins the port and skips the prompt.
	Env string
	// Label is the name shown in the prompt.
	Label string
	// Defaults is the per-instance default, keyed by instance name.
	Defaults map[string]int
	// ContainerPort is the FIXED in-container port this host port maps to.
	ContainerPort int
	// ContainerBind / HostBind: what the instance CONTAINER publishes in each residency.
	ContainerBind portBinding
	HostBind      portBinding
	// HostPlaneBinds marks the port the HOST PLANE listens on in host residency (the control port).
	HostPlaneBinds bool
	// Prompt marks the ports an operator is asked about.
	Prompt bool
}

// instanceOrder is the canonical set of instances this table defines, in the order they are
// documented and reported.
var instanceOrder = []string{"dev", "prod", "test"}

// portDefaults builds one port's per-instance defaults.
func portDefaults(dev, prod, test int) map[string]int {
	return map[string]int{"dev": dev, "prod": prod, "test": test}
}

// installerPorts returns every port an instance can use, in prompt order.
func installerPorts() []installPort {
	return []installPort{
		{Env: "ORCHICON_CONTROL_PORT", Label: "Control plane (web UI + API)", Defaults: portDefaults(8080, 8091, 8092),
			ContainerPort: 8080, ContainerBind: bindAny, HostBind: bindNone, HostPlaneBinds: true, Prompt: true},
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
		// Tempo's OTLP receivers are OFFSET from the collector's own (the two shared one network
		// namespace), so they are separate rows with their own defaults.
		{Env: "ORCHICON_TEMPO_OTLP_GRPC_PORT", Label: "Tempo OTLP gRPC", Defaults: portDefaults(14317, 14318, 14319),
			ContainerPort: 14317, ContainerBind: bindNone, HostBind: bindNone},
		{Env: "ORCHICON_TEMPO_OTLP_HTTP_PORT", Label: "Tempo OTLP HTTP", Defaults: portDefaults(14318, 14319, 14320),
			ContainerPort: 14318, ContainerBind: bindNone, HostBind: bindNone},
		// The collector's health extension has no container-side publish, but a host
		// install needs a per-instance offset or two instances collide on 13133.
		{Env: "ORCHICON_OTEL_HEALTH_PORT", Label: "OTel collector health", Defaults: portDefaults(13133, 13134, 13135),
			ContainerPort: 13133, ContainerBind: bindNone, HostBind: bindNone},
		{Env: "ORCHICON_TEMPO_PORT", Label: "Tempo (traces)", Defaults: portDefaults(3200, 3201, 3202),
			ContainerPort: 3200, ContainerBind: bindNone, HostBind: bindLoopback},
		{Env: "ORCHICON_LOKI_PORT", Label: "Loki (logs)", Defaults: portDefaults(3100, 3101, 3102),
			ContainerPort: 3100, ContainerBind: bindNone, HostBind: bindLoopback},
		{Env: "ORCHICON_VM_PORT", Label: "VictoriaMetrics (metrics)", Defaults: portDefaults(8428, 8429, 8430),
			ContainerPort: 8428, ContainerBind: bindNone, HostBind: bindLoopback},
	}
}

// defaultFor returns the port this instance uses, and whether the table defines one.
func (p installPort) defaultFor(instance string) (int, bool) {
	v, ok := p.Defaults[instance]
	return v, ok
}

// validateInstance reports whether the table defines defaults for every port on this instance.
func validateInstance(instance string) error {
	for _, p := range installerPorts() {
		if _, ok := p.Defaults[instance]; !ok {
			return fmt.Errorf("instance %q has no port defaults (known: %s) — pin every port explicitly with ORCHICON_*_PORT, or add the instance to the table",
				instance, strings.Join(instanceOrder, ", "))
		}
	}
	return nil
}

// boundIn reports whether this port is exposed at all in a residency.
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
func portFree(port int) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// parseInstancePorts reads `docker inspect` port bindings and maps them back to the
// ORCHICON_<X>_PORT keys.
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

// runningInstancePorts reads the host ports an ALREADY-RUNNING instance of ours publishes.
func runningInstancePorts(name string, specs []installPort) (map[string]int, bool) {
	out, err := engineCommand("inspect",
		"--format", "{{json .NetworkSettings.Ports}}", name).Output()
	if err != nil {
		return nil, false
	}
	return parseInstancePorts(out, specs)
}

// runningHostInstancePorts is rung 0 for the HOST carrier — the no-engine counterpart of
// runningInstancePorts.
func runningHostInstancePorts(instance string, specs []installPort, out io.Writer) (map[string]int, bool) {
	if !servingPlaneAlive(instance) && !hostServicesAlive(instance) {
		return nil, false
	}
	live, err := readResolvedPorts(instance)
	if err != nil || len(live) == 0 {
		return nil, false
	}
	filled := map[string]int{}
	for _, p := range specs {
		v, ok := live[p.Env]
		if !ok || v <= 0 {
			// Not in the record for this shape: resolve it the way the plane was started. The
			// instance is validated by the caller, so a default exists.
			d, ok := p.defaultFor(instance)
			if !ok {
				return nil, false
			}
			v = d
		}
		filled[p.Env] = v
	}
	fmt.Fprintf(out, "instance %q is already running — keeping its ports\n", instance)
	for _, p := range specs {
		fmt.Fprintf(out, "  %s: %d\n", p.Label, filled[p.Env])
	}
	return filled, true
}

// promptTerminal opens the CONTROLLING TERMINAL for the port prompt — never stdin (see the file
// comment).
func promptTerminal() (*os.File, bool) {
	// A TERMINAL WE CAN OPEN IS NOT THE SAME AS A HUMAN WATCHING US.
	//
	// This used to open /dev/tty unconditionally, which is the controlling terminal of the PROCESS — and for
	// an install started by anything that is not the operator (a build, a script, CI, a backgrounded
	// `make instance-rebuild`) that is the operator's OWN terminal, sitting there running the `orch` TUI.
	// The install then wrote its port banners into that screen and blocked on a read nobody would answer:
	//
	//	the words were spaced out and it pushed the UI up          (operator, second occurrence)
	//
	// Two failures in one: the TUI was corrupted, and the install hung forever — which also means the
	// command that started it never returned.
	//
	// SO INTERACTIVITY IS DECIDED BY OUR OWN STDOUT. If the stream this process writes is not a terminal,
	// nothing is watching it, so there is nobody to ask: take the record/defaults and say which ports were
	// used (planInstallPorts already prints them when the reader is nil). A wrapper that knows a human IS
	// present can say so with ORCHICON_INSTALL_INTERACTIVE=1.
	if !installIsInteractive() {
		return nil, false
	}
	if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		return f, true
	}
	// No /dev/tty (Windows, or a host whose controlling terminal was detached).
	if isCharDevice(os.Stdin) {
		return os.Stdin, false
	}
	// WINDOWS, AFTER `irm … | iex`: stdin IS A PIPE (it is the downloaded script), while the console
	// the operator is looking at exists and is reachable through CONIN$.
	if in, err := openConsoleInput(); err == nil {
		return in, true
	}
	return nil, false
}

// installIsInteractive reports whether this install may ask questions at all.
func installIsInteractive() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ORCHICON_INSTALL_INTERACTIVE"))) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return isCharDevice(os.Stdout)
}

// isCharDevice reports whether f is a terminal-like character device.
func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// askInstallPort reads one port, returning def when the line is empty — which is what makes "hit
// enter to accept defaults" true.
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

// planInstallPorts is the whole port decision, with every host interaction injected so it is
// deterministic under test.
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
		// A pin is the operator's decision and is never reassigned — but silently carrying a busy port
		// through to a docker bind failure would hide the cause.
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

// nextFreePortFor walks upward from `from` for a port that is free AND not already claimed by
// another port in this same run.
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

// resolveInstallPorts decides the host-side ports this install uses: the running-instance rung,
// then the environment pins, then planInstallPorts.
func resolveInstallPorts(instance, residency string, out io.Writer, tty *os.File) (map[string]int, error) {
	specs := activeInstallPorts(residency)
	name := "orchicon-cnt-" + instance

	// Refuse an instance the table does not define, BEFORE anything is resolved or asked.
	if err := validateInstance(instance); err != nil {
		return nil, err
	}

	// Rung 0, and it must come FIRST: a running instance of ours owns its ports.
	if containerEngineAvailable() {
		if running, _ := containerRunning(name); running {
			if live, ok := runningInstancePorts(name, specs); ok {
				fmt.Fprintf(out, "instance %q is already running — keeping its ports\n", name)
				for _, p := range specs {
					if _, ok := live[p.Env]; !ok {
						// Not published by the container in this shape (the plane port in host residency): resolve it the
						// way the plane was started.
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
	}
	// The host carrier's rung 0.
	if live, ok := runningHostInstancePorts(instance, specs, out); ok {
		return live, nil
	}

	// THE RECORD COMES FIRST, AND IT IS NOT CONDITIONAL ON THE INSTANCE BEING UP.
	//
	// A rebuild stops the instance and then installs it again, so by the time ports are resolved NOTHING
	// of this instance is running — which is exactly when the record matters most and exactly when it was
	// ignored. Without this, the install re-derived the table, probed, and reported its own instance's
	// ports as conflicts, offering "the next free" one:
	//
	//	PostgreSQL: 5432 in use — use 5433?   (this instance is the thing that was using 5432)
	//
	// An operator who accepts that has MOVED their instance's database port on a rebuild, and `rebuild-dev`
	// stops being repeatable. A recorded port is this instance's own until something else proves otherwise.
	//
	// SO THE RULE IS BY OWNERSHIP, NOT BY AVAILABILITY:
	//   free                      -> keep the recorded port
	//   held by THIS instance     -> keep it (rebuild/upgrade in place, silently)
	//   held by ANOTHER instance   -> genuine collision -> resolve a new one, naming the owner
	//   held by anything else      -> genuine collision -> resolve a new one, naming the pid
	recorded, _ := readResolvedPorts(instance)
	pinned := map[string]int{}
	// AN EXISTING INSTANCE'S PORT IS PINNED, NOT SUGGESTED. This is the difference between a reinstall
	// that converges and one that silently moves the operator's database.
	//
	// MEASURED on a real Windows machine: a leftover process held 5432, this function did what it is
	// designed to do — walk to the next free port — and because the MSI runs `--non-interactive` it
	// ACCEPTED ITS OWN SUGGESTION and persisted it:
	//
	//	PostgreSQL: port 5432 is in use — suggesting 5433
	//	{"ORCHICON_POSTGRES_PORT": 5433, ...}    <-- ports.json
	//
	// Every later start then waited on 5433, where postgres would never listen, and reported "the
	// services did not come up". The database port of an INSTALLED instance is not a suggestion: the
	// instance's cluster, its pg_hba, its recorded state and its operator's notes all say 5433 now — and
	// nothing in the product will ever move it back. Renumbering an instance is a decision, so it is
	// left to an explicit ORCHICON_*_PORT (handled below, which outranks this record) and never
	// performed implicitly by a re-run.
	//
	// A GENUINE conflict therefore FAILS, naming the holder, which is actionable; silently relocating is
	// not. This is the same posture ORCHICON_STRICT_PORTS already takes for a fresh install.
	for _, p := range specs {
		if v, ok := recorded[p.Env]; ok && v > 0 {
			if free := portFree(v); free || portHeldByThisInstance(v, instance) {
				pinned[p.Env] = v
				continue
			}
			if owner := portOwnerLabel(v, instance); owner != "" {
				fmt.Fprintf(out, "%s: %d is held by %s\n", p.Label, v, owner)
			} else {
				// THE HOLDER COULD NOT BE IDENTIFIED, which is the NORMAL answer on a platform with no /proc (see
				// reclaim_proc_windows.go).
				fmt.Fprintf(out, "%s: %d is held by a process this tool cannot identify — find it with %s\n",
					p.Label, v, portHolderHint(v))
			}
		}
	}
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

	// A RECORDED PORT THAT IS STILL BUSY IS A HARD FAILURE, UNLESS THE OPERATOR PINS A DIFFERENT ONE.
	for _, p := range specs {
		v, ok := recorded[p.Env]
		if !ok || v <= 0 {
			continue
		}
		if _, overridden := pinned[p.Env]; overridden && os.Getenv(p.Env) != "" {
			continue // an explicit pin is the operator's decision, including moving it
		}
		if portFree(v) || portHeldByThisInstance(v, instance) {
			continue
		}
		holder := portOwnerLabel(v, instance)
		if holder == "" {
			holder = "a process this tool cannot identify — find it with " + portHolderHint(v)
		}
		return nil, fmt.Errorf("%s: this instance is already installed on port %d, which is held by %s\n"+
			"  An existing instance's port is not renumbered by a re-run: its database, its recorded\n"+
			"  state and your own notes all refer to %d. Free the port, or move it deliberately with\n"+
			"    %s=%d %s install --instance %s",
			p.Label, v, holder, v, p.Env, v, filepath.Base(os.Args[0]), instance)
	}

	var r *bufio.Reader
	if tty != nil {
		r = bufio.NewReader(tty)
	}
	return planInstallPorts(instance, specs, pinned, portFree, r, out)
}

// askInstance asks which instance to set up, offering the already-decided value as the default.
func askInstance(r io.Reader, current string) (string, bool) {
	// UNBUFFERED ON A REAL TERMINAL, DELIBERATELY.
	readLine := func() (string, error) {
		if f, ok := r.(*os.File); ok {
			var b []byte
			one := make([]byte, 1)
			for {
				if _, err := f.Read(one); err != nil {
					return string(b), err
				}
				if one[0] == '\n' {
					return string(b), nil
				}
				if one[0] != '\r' {
					b = append(b, one[0])
				}
			}
		}
		line, err := bufio.NewReader(r).ReadString('\n')
		return line, err
	}
	if current == "" {
		current = "dev"
	}
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(os.Stderr, "\n  Which instance? dev, test or prod (%s): ", current)
		line, err := readLine()
		if err != nil && line == "" {
			return current, false // nothing on the reader: keep the decision, do not block
		}
		name := strings.ToLower(strings.TrimSpace(line))
		if name == "" {
			return current, true // ENTER takes the default
		}
		if err := validateInstance(name); err != nil {
			fmt.Fprintf(os.Stderr, "  %q is not an instance (dev, test or prod) — try again.\n", name)
			continue
		}
		return name, true
	}
	fmt.Fprintf(os.Stderr, "  too many unusable entries — using %q.\n", current)
	return current, true
}
