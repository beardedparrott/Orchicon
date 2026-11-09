// Package planecontrol is THE shared local plane-control mechanism: status, start, stop and restart
// for an instance's host-resident plane.
package planecontrol

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
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

// instanceEnvKeys are the variables that decide WHICH INSTANCE a lifecycle command acts on, and
// they are STRIPPED from the ambient environment before this package sets its own.
var instanceEnvKeys = []string{
	"ORCHICON_CONTAINER_MODE",
	ServeStateDirEnv,
	"ORCHICON_INSTANCE",
	"ORCHICON_DATA_DIR",
	"ORCHICON_BLOB_DIR",
	"ORCHICON_POSTGRES_DSN",
	"ORCHICON_NATS_URL",
	// THE PLANE'S OWN BOOT MARKERS.
	"ORCHICON_SERVE_DETACHED",
	"ORCHICON_SERVE_LOG_FILE",
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
	// unrecordedPlane / terminateProcess / pidAlive belong to the ONE case where the PID file is not
	// the answer: a plane that outlived it (see hostproc_unix.go).
	unrecordedPlane  = unrecordedPlanePID
	terminateProcess = TerminateProcess
	pidAlive         = PidAlive
	runCommand   = runSelf
	binaryOf     = procExe
	sleep        = time.Sleep
	now          = time.Now
)

// planeBinaryName is the name of the binary that SERVES the plane (`serve --detach` / `serve --stop`).
const planeBinaryName = "orchicon"

// recordedControlPort is the control port this instance's record says it serves on, 0 when unknown.
func recordedControlPort(instance string) int {
	ports, err := ReadPorts(instance)
	if err != nil {
		return 0
	}
	return ports["ORCHICON_CONTROL_PORT"]
}

// planeBinary is the binary this package re-execs to run the plane (and the binary a live plane is compared
// against for drift). It is a seam so the resolution below is assertable without spawning processes.
var planeBinary = resolvePlaneBinary

// PlaneBinary is where the INSTALLED `orchicon` binary is, resolved for a thin client.
func PlaneBinary() (string, error) { return planeBinary() }

// resolvePlaneBinary answers "which binary runs the plane?" — which is NOT always "the binary that
// called this package".
func resolvePlaneBinary() (string, error) {
	self, err := executable()
	if err != nil {
		return "", fmt.Errorf("locate this binary to run the plane: %w", err)
	}
	if strings.TrimSuffix(filepath.Base(self), ".exe") == planeBinaryName {
		return self, nil // the CLI/installer IS the plane binary: no resolution, no surprise
	}
	if found, err := exec.LookPath(planeBinaryName); err == nil {
		return found, nil
	}
	if home, err := userHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", planeBinaryName)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot run the plane from %s: this is the %s client, which does not serve — "+
		"install `%s` (it lands in ~/.local/bin) and put it on PATH, then retry",
		self, filepath.Base(self), planeBinaryName)
}

// runSelf re-execs the PLANE binary (never merely "this" binary — see resolvePlaneBinary) with args
// and env — the mechanism by which `serve --detach` and `serve --stop` are performed.
func runSelf(args []string, env []string, out io.Writer) error {
	self, err := planeBinary()
	if err != nil {
		// NOT WRAPPED: the resolution error already says what is wrong AND what to do about it, and a
		// "run `serve --detach`: …" prefix in front of it only buries the remedy in the dock strip.
		return err
	}
	// quietCommand, because THIS PROCESS MAY HAVE NO CONSOLE.
	cmd := procwin.Command(self, args...)
	cmd.Env = env
	// THE PLANE'S OWN WORKING DIRECTORY, NOT THIS CALLER'S.
	cmd.Dir = filepath.Dir(self)
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
	// THE BINARY, NOT JUST THE PORT: a PID plus /proc/<pid>/exe is the only pair that says WHICH
	// process is serving.
	if pid, ok := ServingPlanePID(o.Instance); ok {
		st.PID = pid
		st.Alive = planeAlive(o.Instance)
		if exe, err := binaryOf(pid); err == nil {
			st.Binary = exe
		}
	} else if pid, ok := unrecordedPlane(o.Instance); ok {
		st.PID = pid
		st.Alive = pidAlive(pid)
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

// Start launches the HOST plane for an instance, detached, and waits for its /healthz.
func Start(opts Options) error {
	o, err := opts.resolve()
	if err != nil {
		return err
	}
	healthURL := fmt.Sprintf("http://localhost:%d/healthz", o.Ports["ORCHICON_CONTROL_PORT"])

	// A PLANE THAT ANSWERS IS NOT NECESSARILY THE PLANE WE JUST INSTALLED.
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
			// container-resident plane).
			fmt.Fprintf(o.Out, "host plane already serving %s (could not identify the process; leaving it)\n", healthURL)
			return nil
		}
	}
	// A host plane its own workers cannot reach is worse than one that refuses to start: every
	// plane-channel MCP call in every dispatched run would time out.
	env, err := PlaneEnv(o.Instance, o.Ports, o.HostDataDir, o.runtimeSocket())
	if err != nil {
		return fmt.Errorf("refusing to start the host plane: %w", err)
	}
	cmdEnv := append(EnvWithout(os.Environ(), instanceEnvKeys...), env...)
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

// Stop stops the instance's detached host plane, so the caller can start it on the installed
// binary.
func Stop(opts Options) error {
	o := opts.normalized()
	cmdEnv := append(EnvWithout(os.Environ(), instanceEnvKeys...),
		ServeStateDirEnv+"="+filepath.Join(o.HostDataDir, "serve"))
	if err := runCommand([]string{"serve", "--stop"}, cmdEnv, o.Out); err != nil {
		// A THIRD CASE, AND THE ONE THAT BLOCKED A REBUILD: the PID file is GONE but the plane is not.
		if pid, ok := unrecordedPlane(o.Instance); ok {
			fmt.Fprintf(o.Out, "host plane for %s has no PID file but is serving on %d (pid %d) — stopping it\n",
				o.Instance, recordedControlPort(o.Instance), pid)
			if terr := terminateProcess(pid); terr != nil {
				return fmt.Errorf("stop the host plane for %s (pid %d): %w", o.Instance, pid, terr)
			}
			for i := 0; i < stopPolls; i++ {
				if !pidAlive(pid) {
					return nil
				}
				sleep(stopPollInterval)
			}
			return fmt.Errorf("the host plane for %s (pid %d) did not stop — it may still be shutting down",
				o.Instance, pid)
		}
		// TELL "NOTHING TO STOP" APART FROM "IT WOULD NOT STOP".
		if !planeAlive(o.Instance) {
			return nil
		}
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

// Restart stops and starts the instance's HOST plane, so it re-reads its environment (a newly
// installed adapter CLI on PATH, for instance) without re-resolving anything or asking a question.
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
