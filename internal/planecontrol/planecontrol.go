// Package planecontrol is THE shared local plane-control mechanism: status, start, stop and restart for an
// instance's host-resident plane.
//
// # THE DECISION: LOCAL PROCESS INVOCATION ONLY. THERE IS NO LIFECYCLE RPC AND THIS PACKAGE ADDS NONE.
//
// The question every lifecycle surface depends on is "does a control go through the plane's Connect API, or
// does each client invoke the binary locally?" The answer is the local one, and the reason is that the API
// path existed for exactly one consumer: the web app. A browser cannot spawn a process, so a Stop button in
// the browser had to reach the plane over the API — and that RPC had to reply BEFORE killing the server
// answering it, which is the awkward part. The web app is OUT OF SCOPE for lifecycle control. What remains
// runs on the same host as the plane and can simply invoke the mechanism:
//
//   - the desktop app — a Go process (Wails) that can spawn another;
//   - the TUI (`orch`) — a separate binary that does not depend on the plane serving it;
//   - the CLI — the operator/admin path;
//   - `scripts/orchicon.sh` / `scripts/container.sh`, which delegate to the same `orchicon plane` verb.
//
// So the shared layer is a Go package, not an RPC. `internal/api/api.go`, `proto/` and the generated clients
// are untouched by this package, and internal/api's surface test asserts the API did not grow.
//
// A future surface that genuinely needs REMOTE control is a separate decision with its own brief; it must
// not creep in here.
//
// # WHAT THIS PACKAGE MUST NOT BREAK
//
//   - THE FAIL-SAFE: a plane that cannot be POSITIVELY identified is LEFT ALONE. A restart we cannot justify
//     is a blip on a working instance (see the `default:` arm of Start).
//   - GRACEFUL, BOUNDED STOP: `serve --stop` is re-execed (it sends SIGTERM, waits the supervisor's shutdown
//     window, escalates, and exits non-zero on a survivor); then this layer polls the PID file a bounded
//     number of times and REPORTS failure rather than hanging when the socket is not released.
//   - PER-INSTANCE ISOLATION: every path derives from HostDataDir(instance), so two instances can never be
//     controlled as one. ORCHICON_SERVE_STATE_DIR is only ever set as a CHILD env built from that dir.
//   - PORTS ARE READ BACK, NEVER RE-RESOLVED: re-resolving prompts, and a lifecycle command must never ask.
//
// IT IMPORTS THE STANDARD LIBRARY AND NOTHING ELSE. That is what lets the TUI (`cmd/orch`) and a Wails app
// depend on it without pulling in the plane's HTTP server, the api package, the generated clients or the db
// layer — an imports test in this package asserts it.
package planecontrol

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DriftState says whether the plane now serving matches the binary this build is.
type DriftState int

const (
	// Unknown: the serving process could not be identified. Treat as "leave it alone".
	Unknown DriftState = iota
	// Current: the plane is running the binary in hand.
	Current
	// Drifted: the plane is running a superseded binary and should be restarted.
	Drifted
)

func (d DriftState) String() string {
	switch d {
	case Current:
		return "binary current"
	case Drifted:
		return "binary superseded"
	default:
		return "identity unknown"
	}
}

// Bounded, so a slow shutdown is REPORTED rather than waited on forever.
const (
	startHealthTimeout = 60 * time.Second
	startHealthPoll    = time.Second
	stopPolls          = 20
	stopPollInterval   = 250 * time.Millisecond
	healthProbeTimeout = 2 * time.Second
)

// runtimeSocketDirEnv is the directory holding an instance's workflow-runtime socket, matching the launcher.
const runtimeSocketDirEnv = "ORCHICON_RUNTIME_SOCKET_DIR"

// THE SEAMS. Production code never assigns these; tests do (and restore them) to make planeUnknown,
// stop-timeout and two-instance cases assertable in milliseconds instead of by racing real processes.
var (
	executable   = os.Executable // this binary: the thing we re-exec and compare the plane against
	driftProbe   = Drift         // (string, io.Writer) DriftState
	planeAlive   = ServingPlaneAlive
	healthyProbe = Healthy
	runCommand   = runSelf
	binaryOf     = procExe
	sleep        = time.Sleep
	now          = time.Now
)

// runSelf re-execs THIS binary with args and env — the mechanism by which `serve --detach` and `serve --stop`
// are performed. Re-exec is what makes a curl-installed host sufficient: the installed `orchicon` is the one
// binary needed, with no sibling script or build.
func runSelf(args []string, env []string, out io.Writer) error {
	self, err := executable()
	if err != nil {
		return fmt.Errorf("locate this binary to run `%s`: %w", strings.Join(args, " "), err)
	}
	cmd := exec.Command(self, args...)
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// Options carries everything one lifecycle call needs. Zero values take this instance's recorded defaults,
// so every surface can pass Defaults(instance, out) and hand nothing else in.
type Options struct {
	// Instance is the instance name. "" → "dev".
	Instance string
	// Out is where progress is printed. nil → io.Discard.
	Out io.Writer
	// HostDataDir is the instance's data dir. "" → HostDataDirFor(Instance).
	HostDataDir string
	// Ports is the instance's RESOLVED port set. nil → ReadPorts(Instance), never a re-resolve.
	Ports map[string]int
	// SocketDir holds the runtime socket. "" → ORCHICON_RUNTIME_SOCKET_DIR, else <tmp>/orchicon-runtime.
	SocketDir string
}

// Defaults is the Options every caller wants: one instance, one output.
func Defaults(instance string, out io.Writer) Options {
	return Options{Instance: instance, Out: out}
}

// normalized fills in the zero values. It does NOT read the ports (see resolve) so Status can tolerate an
// instance with no port record.
func (o Options) normalized() Options {
	if o.Instance == "" {
		o.Instance = "dev"
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.HostDataDir == "" {
		o.HostDataDir = HostDataDirFor(o.Instance)
	}
	if o.SocketDir == "" {
		o.SocketDir = strings.TrimSpace(os.Getenv(runtimeSocketDirEnv))
		if o.SocketDir == "" {
			o.SocketDir = filepath.Join(os.TempDir(), "orchicon-runtime")
		}
	}
	return o
}

// resolve normalized plus the ports, for the verbs that cannot act without them.
func (o Options) resolve() (Options, error) {
	o = o.normalized()
	if o.Ports == nil {
		ports, err := ReadPorts(o.Instance)
		if err != nil {
			return o, err
		}
		o.Ports = ports
	}
	return o, nil
}

// runtimeSocket is the socket path handed to the plane, matching the launcher's naming.
func (o Options) runtimeSocket() string { return filepath.Join(o.SocketDir, "runtime.sock") }

// Status is what "is it up?" must answer: the INSTANCE and the BINARY serving, not merely that something is
// listening on a port.
type Status struct {
	Instance    string
	Residency   string // recorded shape ("host"/"container"); "" when no record
	DataDir     string
	HealthURL   string
	Binary      string // /proc/<pid>/exe as resolved; "" when unidentifiable
	PID         int    // 0 when no live PID file
	ControlPort int
	Alive       bool
	Healthy     bool
	Drift       DriftState
}

// String renders a Status as one line, always naming the instance and the data dir it was read from.
func (s Status) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "instance %s", s.Instance)
	if s.Residency != "" {
		fmt.Fprintf(&b, " (%s-resident)", s.Residency)
	}
	if s.PID > 0 {
		fmt.Fprintf(&b, ": plane pid %d", s.PID)
		if s.Binary != "" {
			fmt.Fprintf(&b, " running %s", s.Binary)
		} else {
			b.WriteString(" (serving binary unidentifiable)")
		}
		fmt.Fprintf(&b, " — %s", s.Drift)
	} else {
		b.WriteString(": no plane PID file (nothing running, or no PID recorded)")
	}
	if s.HealthURL != "" {
		if s.Healthy {
			fmt.Fprintf(&b, ", healthy at %s", s.HealthURL)
		} else {
			fmt.Fprintf(&b, ", NOT answering %s", s.HealthURL)
		}
	}
	fmt.Fprintf(&b, " — data dir %s", s.DataDir)
	return b.String()
}

// StatusOf reports the instance's plane as this instance sees it. It never fails for want of a port record or
// a PID file: those are states ("not installed", "not running"), not errors.
func StatusOf(opts Options) (Status, error) {
	o := opts.normalized()
	st := Status{Instance: o.Instance, DataDir: o.HostDataDir}
	if r, ok := RecordedResidency(o.Instance); ok {
		st.Residency = r
	}
	ports := o.Ports
	if ports == nil {
		if p, err := ReadPorts(o.Instance); err == nil {
			ports = p
		}
	}
	if ports != nil {
		st.ControlPort = ports["ORCHICON_CONTROL_PORT"]
		if st.ControlPort > 0 {
			st.HealthURL = fmt.Sprintf("http://localhost:%d/healthz", st.ControlPort)
		}
	}
	// THE BINARY, NOT JUST THE PORT: a PID file plus /proc/<pid>/exe is the only pair that says WHICH process
	// is serving. A version endpoint would answer with whatever the OLD process believes about itself.
	if pid, ok := ServingPlanePID(o.Instance); ok {
		st.PID = pid
		st.Alive = planeAlive(o.Instance)
		if exe, err := binaryOf(pid); err == nil {
			st.Binary = exe
		}
	}
	st.Drift = driftProbe(o.Instance, o.Out)
	if st.HealthURL != "" {
		st.Healthy = healthyProbe(st.HealthURL, healthProbeTimeout)
	}
	return st, nil
}

// Start launches the HOST plane for an instance, detached, and waits for its /healthz. Idempotent: an
// already-healthy plane running the installed binary is left alone.
//
// THE BINARY IS THIS ONE, which is what makes the release archive sufficient: the installed `orchicon`
// re-execs itself as `serve --detach`, so no sibling build or script is required.
//
// ORCHICON_CONTAINER_MODE is REMOVED from the inherited environment: a stray export in a shell profile must
// never flip the HOST plane into container semantics — it would then rewrite custom provider URLs that are
// already correct on the host.
func Start(opts Options) error {
	o, err := opts.resolve()
	if err != nil {
		return err
	}
	healthURL := fmt.Sprintf("http://localhost:%d/healthz", o.Ports["ORCHICON_CONTROL_PORT"])

	// A PLANE THAT ANSWERS IS NOT NECESSARILY THE PLANE WE JUST INSTALLED.
	//
	// This used to be `if Healthy(...) { "already serving"; return nil }`, which asks whether SOMETHING is
	// listening — not whether the listening process is the binary in hand. An install replaces that binary, so
	// the old process keeps serving revised code from a replaced (unlinked) executable, and the installer
	// reports success over an instance that was never upgraded. Reported: "said it was complete but no auth
	// works and the previous dev container has been up for two hours".
	//
	// So the SERVING PROCESS is asked what it is running: the instance's own PID file names it, and
	// /proc/<pid>/exe answers with the path — or with a `(deleted)` suffix once that path has been replaced
	// underneath it, which is precisely the state an install creates.
	if healthyProbe(healthURL, healthProbeTimeout) {
		switch driftProbe(o.Instance, o.Out) {
		case Current:
			fmt.Fprintf(o.Out, "host plane already serving %s (running the installed binary)\n", healthURL)
			return nil
		case Drifted:
			// AN INSTALL IS AN UPGRADE, so replacing the running plane is what the operator asked for by
			// running it. Stopping it here lets the start below bring up the NEW binary.
			fmt.Fprintf(o.Out, "host plane at %s is running a superseded binary — restarting it on the installed one\n", healthURL)
			if err := Stop(o); err != nil {
				return err
			}
		default:
			// The serving process cannot be identified (no PID file, unreadable /proc — another OS, a
			// container-resident plane). Leave it ALONE: a restart we cannot justify is a blip on a working
			// instance, and the health check a few lines below still reports whether it is serving.
			fmt.Fprintf(o.Out, "host plane already serving %s (could not identify the process; leaving it)\n", healthURL)
			return nil
		}
	}
	// A host plane its own workers cannot reach is worse than one that refuses to start: every plane-channel
	// MCP call in every dispatched run would time out. There is no loopback-only fallback, so an unresolvable
	// bridge is fatal here (PlaneEnv is what raises it).
	env, err := PlaneEnv(o.Instance, o.Ports, o.HostDataDir, o.runtimeSocket())
	if err != nil {
		return fmt.Errorf("refusing to start the host plane: %w", err)
	}
	cmdEnv := append(EnvWithout(os.Environ(), "ORCHICON_CONTAINER_MODE"), env...)
	if err := runCommand([]string{"serve", "--detach"}, cmdEnv, o.Out); err != nil {
		return fmt.Errorf("start the host plane for %s: %w", o.Instance, err)
	}
	deadline := now().Add(startHealthTimeout)
	for now().Before(deadline) {
		if healthyProbe(healthURL, healthProbeTimeout) {
			fmt.Fprintf(o.Out, "host plane ready: %s\n", healthURL)
			return nil
		}
		sleep(startHealthPoll)
	}
	return fmt.Errorf("the host plane did not answer %s within 60s — see %s/logs/orchicon.log",
		healthURL, filepath.Join(o.HostDataDir, "serve"))
}

// Stop stops the instance's detached host plane, so the caller can start it on the installed binary.
//
// WHY IT RE-EXECS RATHER THAN SIGNALLING: `serve --stop` is the ONE implementation of a graceful stop — it
// sends SIGTERM, waits the supervisor's shutdown window, escalates to SIGKILL, and exits non-zero if a process
// survives. It re-execs THIS binary with the instance's state dir, because `serve --stop` resolves its PID
// file from ORCHICON_SERVE_STATE_DIR and a bare call would look in the wrong place.
func Stop(opts Options) error {
	o := opts.normalized()
	cmdEnv := append(EnvWithout(os.Environ(), "ORCHICON_CONTAINER_MODE"),
		ServeStateDirEnv+"="+filepath.Join(o.HostDataDir, "serve"))
	if err := runCommand([]string{"serve", "--stop"}, cmdEnv, o.Out); err != nil {
		return fmt.Errorf("stop the existing host plane for %s: %w", o.Instance, err)
	}
	// The socket has to be free before the new plane binds it. Bounded, and a failure here is reported rather
	// than slept through: an address still in use is the one reason a start would then fail.
	for i := 0; i < stopPolls; i++ {
		if !planeAlive(o.Instance) {
			return nil
		}
		sleep(stopPollInterval)
	}
	return fmt.Errorf("the host plane for %s did not stop — it may still be shutting down", o.Instance)
}

// Restart stops and starts the instance's HOST plane, so it re-reads its environment (a newly installed
// adapter CLI on PATH, for instance) without re-resolving anything or asking a question.
//
// A CONTAINER-RESIDENT instance is refused rather than silently ignored: its plane is inside a container this
// local mechanism does not own, and `orchicon restart` is the verb that handles that shape. Guessing here is
// exactly the "control a plane you cannot identify" failure the fail-safe exists to prevent.
func Restart(opts Options) error {
	o, err := opts.resolve()
	if err != nil {
		return err
	}
	residency, ok := RecordedResidency(o.Instance)
	if !ok {
		return fmt.Errorf("no install record for instance %q — nothing is installed for it.\n"+
			"  Install it first:  orchicon install\n"+
			"  Another instance:  ORCHICON_INSTANCE=<name> orchicon plane restart", o.Instance)
	}
	if residency != ResidencyHost {
		return fmt.Errorf("instance %s is container-resident (%s): its plane runs inside the instance container, "+
			"which this local mechanism does not own — use `orchicon restart`", o.Instance, residency)
	}
	if err := Stop(o); err != nil {
		return err
	}
	return Start(o)
}
