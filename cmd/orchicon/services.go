package main

// services.go — `orchicon services up|down|status`: run Postgres and NATS on the
// HOST for an instance, with NO container engine involved.
//
// WHY THIS COMMAND EXISTS. `make run` is `go run ./cmd/orchicon` — it starts the
// plane with no backends at all. Until now the backends came from the instance
// container's loopback publishes (dev 5432/4222, prod 5433/4223), and that was
// true for every DB-backed path: `make migrate`, `make rls-check`, the
// ORCHICON_TEST_DSN tests, the E2E developer session. Take the container out of
// the inner loop and all of them break at once, none of them loudly.
//
// So this is the developer's own copy of what the container was providing:
//
//	orchicon services up      start postgres + nats for the instance (detached)
//	orchicon services down    stop them, and VERIFY they are gone
//	orchicon services status  what is running, and the DSN the plane should use
//
// WHAT IT REUSES, DELIBERATELY. The supervisor machinery in container.go —
// dependency-ordered start, readiness gates, restart-with-backoff, group
// shutdown — through the `hostMode` branch. There is no second supervisor: the
// container path and the host path differ in WHERE the binaries live and WHICH
// user runs them, and that is all `hostMode` changes.
//
// WHAT IT IS NOT. It is not the container path with a flag flipped: no
// `setpriv`, no uid 70, no /var/lib/orchicon, no docker-bridge pg_hba trust
// rules. Those exist for the container's multi-uid layout and its published
// port, and each would be actively wrong on a developer's machine.
//
// TELEMETRY IS OFF. Only postgres + nats start. A dev inner loop needs the
// database and the bus; bringing up a 384 MiB observability stack would make
// `make run` slow and would widen the port set the per-instance table has to
// keep disjoint for no benefit.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/beardedparrott/orchicon/internal/version"
)

// servicesDetachedEnv marks the re-executed child that actually supervises the
// services. It is set by the parent only, exactly as serveEnvDetached is for
// `serve --detach`: one binary, one code path, and the "am I the supervisor or
// the caller" decision made from the environment rather than from a second
// hidden subcommand.
const servicesDetachedEnv = "ORCHICON_SERVICES_DETACHED"

// servicesReadyTimeout bounds how long `services up` waits for the detached
// supervisor to report both services up. It is generous because the FIRST run
// on a machine with no Postgres downloads and unpacks ~35 MiB before initdb can
// even run; every later run is a few hundred milliseconds.
const servicesReadyTimeout = 10 * time.Minute

// runServices is the `orchicon services` entrypoint. The verb defaults to
// `status`, so a bare `orchicon services` reports rather than starts — a command
// with no argument must never be the one that starts a database.
func runServices(args []string) int {
	// The detached supervisor re-executes this binary with the marker set and
	// the same argv, so it must be checked BEFORE any verb dispatch.
	if os.Getenv(servicesDetachedEnv) == "1" {
		return runServicesSupervisor(servicesInstanceArg(args))
	}

	verb := "status"
	instance := ""
	switch len(args) {
	case 0:
	case 1:
		if isServicesVerb(args[0]) {
			verb = args[0]
		} else {
			instance = args[0]
		}
	default:
		verb, instance = args[0], args[1]
	}
	if !isServicesVerb(verb) {
		fmt.Fprintf(os.Stderr, "unknown services verb: %s\n", verb)
		fmt.Fprint(os.Stderr, servicesUsage())
		return 2
	}
	if instance == "" {
		instance = env("ORCHICON_INSTANCE", "dev")
	}

	switch verb {
	case "up":
		return servicesUp(instance)
	case "down":
		return servicesDown(instance)
	default:
		return servicesStatus(instance)
	}
}

func isServicesVerb(v string) bool {
	return v == "up" || v == "down" || v == "status"
}

func servicesUsage() string {
	bin := filepath.Base(os.Args[0])
	return fmt.Sprintf(`Usage: %s services <up|down|status> [instance]

  up      start this instance's Postgres + NATS on the host (idempotent: an
          already-serving instance is reported, not restarted)
  down    stop them and verify the ports are actually free
  status  report what is running and the DSN the plane should use

instance defaults to $ORCHICON_INSTANCE (dev). Ports come from the ONE
per-instance table (cmd/orchicon/install_ports.go), so dev and prod never
collide. ORCHICON_PG_BINDIR points at an existing PostgreSQL if you have one.
`, bin)
}

// servicesInstanceArg pulls the instance out of the child's argv. The child is
// re-executed with the parent's arguments verbatim, so this is the same
// resolution the parent did — no second argv shape to keep in sync.
func servicesInstanceArg(args []string) string {
	for _, a := range args {
		if a == "up" || a == "down" || a == "status" {
			continue
		}
		if a != "" {
			return a
		}
	}
	return env("ORCHICON_INSTANCE", "dev")
}

// servicePorts resolves the three ports the host services bind for an instance.
//
// IT DOES NOT PROBE, AND THAT IS THE POINT. The installer's resolver
// (resolveInstallPorts) walks UPWARD from a busy default, which is right when
// choosing a port for a new container and catastrophically wrong here: the ports
// of our OWN running services are busy by definition, so a probe would move each
// instance to the next free port on every invocation and `make migrate` would
// address a different database than `make run` started. Pinned and per-instance
// values only; a busy port is reported by the idempotence guard instead.
func servicePorts(instance string) (pg, nats, natsMon int, err error) {
	if err := validateInstance(instance); err != nil {
		return 0, 0, 0, err
	}
	pick := func(envName string) (int, error) {
		for _, p := range installerPorts() {
			if p.Env != envName {
				continue
			}
			if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
				n, convErr := strconv.Atoi(v)
				if convErr != nil || n < 1 || n > 65535 {
					return 0, fmt.Errorf("%s=%q is not a port (1..65535)", envName, v)
				}
				return n, nil
			}
			n, ok := p.defaultFor(instance)
			if !ok {
				return 0, fmt.Errorf("instance %q has no default for %s", instance, envName)
			}
			return n, nil
		}
		return 0, fmt.Errorf("no port table entry for %s", envName)
	}
	if pg, err = pick("ORCHICON_POSTGRES_PORT"); err != nil {
		return 0, 0, 0, err
	}
	if nats, err = pick("ORCHICON_NATS_PORT"); err != nil {
		return 0, 0, 0, err
	}
	if natsMon, err = pick("ORCHICON_NATS_MONITOR_PORT"); err != nil {
		return 0, 0, 0, err
	}
	return pg, nats, natsMon, nil
}

// servicesDataDir is the instance's host state dir — the SAME one
// `orchicon install` and scripts/container.sh use (hostDataDir), because two
// launchers disagreeing about this directory is the one disagreement that
// silently orphans state.
func servicesDataDir(instance string) string { return hostDataDirFor(instance) }

// servicesPIDFile / servicesLogFile locate the detached supervisor's state.
func servicesPIDFile(instance string) string {
	return filepath.Join(servicesDataDir(instance), "services", "services.pid")
}
func servicesLogFile(instance string) string {
	return filepath.Join(servicesDataDir(instance), "services", "services.log")
}

// serviceDSN is the connection string the plane (and `make migrate`) should use
// for this instance's host services. It is the same shape the Makefile's DB_URL
// carries, derived from the SAME resolved port, so the two cannot drift.
func serviceDSN(pgPort int) string {
	return fmt.Sprintf("postgres://orchicon:orchicon@localhost:%d/orchicon?sslmode=disable", pgPort)
}

// newHostSupervisor builds a host-mode supervisor for an instance.
func newHostSupervisor(instance string, log *slog.Logger) (*supervisor, error) {
	pg, nats, natsMon, err := servicePorts(instance)
	if err != nil {
		return nil, err
	}
	sup := &supervisor{
		log:          log,
		dataDir:      servicesDataDir(instance),
		hostMode:     true,
		pgPort:       pg,
		natsPort:     nats,
		natsMonPort:  natsMon,
		children:     make(map[string]*procState),
		hostUID:      0, // host mode runs as the current user; setpriv is never used
		servicesOnly: false,
	}
	return sup, nil
}

// servicesUp starts the instance's host services, starting a detached
// supervisor, and waits until both report ready.
//
// IDEMPOTENCE COMES FIRST, before anything is started or downloaded: if
// something already answers on the instance's postgres port, `up` reports the
// state and exits 0. That is what lets `run: services` be a Makefile
// prerequisite — a prerequisite that is not idempotent turns every second
// `make run` into a failure.
func servicesUp(instance string) int {
	ctx := context.Background()
	pg, nats, natsMon, err := servicePorts(instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		return 2
	}

	if tcpReady(ctx, "127.0.0.1:"+strconv.Itoa(pg)) {
		pid, _ := procRunning(servicesPIDFile(instance))
		extra := ""
		if pid != "" {
			extra = fmt.Sprintf(" (supervisor PID %s)", pid)
		} else {
			extra = " (not started by `orchicon services` — the instance container, or an external Postgres)"
		}
		fmt.Printf("✓ services for %s are already running: 127.0.0.1:%d answers%s\n", instance, pg, extra)
		fmt.Printf("  DSN: %s\n", serviceDSN(pg))
		return 0
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	sup, err := newHostSupervisor(instance, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		return 2
	}

	// Acquire in the FOREGROUND, so a first-run download is visible instead of
	// appearing to hang behind a detached child.
	fmt.Printf("services %s: binding postgres:%d nats:%d nats-mon:%d\n", instance, pg, nats, natsMon)
	bins, err := ensureServiceBinaries(sup.dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		return 1
	}
	fmt.Printf("  postgres: %s (%s)\n", filepath.Join(bins.PG, "postgres"), bins.Source)
	fmt.Printf("  nats:     %s\n", bins.NATS)

	if err := os.MkdirAll(filepath.Dir(servicesPIDFile(instance)), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "✗ create services dir: %v\n", err)
		return 1
	}
	logFile, err := os.OpenFile(servicesLogFile(instance), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ open services log: %v\n", err)
		return 1
	}
	defer logFile.Close()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	child := exec.Command(self, "services", "up", instance)
	child.Env = append(os.Environ(), servicesDetachedEnv+"=1")
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	// Its own process group: `services down` signals the GROUP, so postgres and
	// nats — children of the supervisor — are reached even if the supervisor is
	// wedged.
	setProcAttrBackground(child)
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ start services supervisor: %v\n", err)
		return 1
	}
	pid := child.Process.Pid
	if err := os.WriteFile(servicesPIDFile(instance), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  ! failed to write PID file: %v\n", err)
	}
	_ = child.Process.Release()

	if !waitForServicePorts(pid, pg, natsMon, servicesReadyTimeout) {
		// Distinguish "the supervisor died" from "it is still trying" — the two
		// need different operator actions, and a single waitForServicePorts bool
		// cannot tell them apart.
		if !pidAlive(pid) {
			fmt.Fprintf(os.Stderr, "✗ the services supervisor (PID %d) exited before the services were ready\n", pid)
		} else {
			fmt.Fprintf(os.Stderr, "✗ services did not become ready within %s (supervisor PID %d still running)\n",
				servicesReadyTimeout, pid)
		}
		// A dead supervisor's PID file is stale state that would make a later
		// `down` address a PID that no longer exists; clear it. A LIVE supervisor
		// keeps its file so `down` can still reach it.
		if !pidAlive(pid) {
			_ = os.Remove(servicesPIDFile(instance))
		}
		fmt.Fprintf(os.Stderr, "  log: %s\n", servicesLogFile(instance))
		fmt.Fprint(os.Stderr, logTail(servicesLogFile(instance), 20))
		return 1
	}

	fmt.Printf("✓ services started for %s (supervisor PID %d)\n", instance, pid)
	fmt.Printf("  postgres: 127.0.0.1:%d\n", pg)
	fmt.Printf("  nats:     127.0.0.1:%d (monitor %d)\n", nats, natsMon)
	fmt.Printf("  DSN:      %s\n", serviceDSN(pg))
	fmt.Printf("  log:      %s\n", servicesLogFile(instance))
	fmt.Printf("  stop:     %s services down %s\n", filepath.Base(os.Args[0]), instance)
	return 0
}

// waitForServicePorts blocks until postgres answers AND the NATS monitor
// endpoint answers, the supervisor dies, or the timeout elapses.
//
// BOTH PORTS, not just postgres: the plane needs the bus as well, and reporting
// "services up" while NATS is still starting would hand a developer a plane that
// fails on its first publish. The supervisor's death is checked separately so a
// crashed child is reported at once instead of after the full timeout.
func waitForServicePorts(pid, pgPort, natsMonPort int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ctx := context.Background()
	for {
		if tcpReady(ctx, "127.0.0.1:"+strconv.Itoa(pgPort)) &&
			httpReady(fmt.Sprintf("http://127.0.0.1:%d/healthz", natsMonPort))(ctx) {
			return true
		}
		if !pidAlive(pid) {
			return false
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// logTail returns the last n lines of a file, for a failure report. A missing or
// unreadable log is not an error here — the caller is already reporting a
// failure and must not be derailed by a second one.
func logTail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var b strings.Builder
	b.WriteString("  --- last lines of the services log ---\n")
	for _, l := range lines {
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}

// servicesDown stops the instance's host services and VERIFIES the ports are
// free before it claims success.
//
// THE VERIFICATION IS THE WHOLE POINT, and it is the lesson `serveStop` already
// learned (cmd/orchicon/serve.go): a stop that signals and then reports success
// without checking leaves a postgres holding 5432 while every command says the
// instance is down. The next start then fails confusedly — a port bind error
// naming neither the old process nor this tool. So: signal the group, wait,
// escalate, and only remove the PID file once NOTHING answers on the ports.
func servicesDown(instance string) int {
	pg, nats, natsMon, err := servicePorts(instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		return 2
	}
	ctx := context.Background()
	pgAddr := "127.0.0.1:" + strconv.Itoa(pg)
	pidFile := servicesPIDFile(instance)

	pidStr, running := procRunning(pidFile)
	if !running {
		if _, statErr := os.Stat(pidFile); statErr == nil {
			// A PID file naming a dead process is stale state: clear it, but do
			// not report a stop we did not perform.
			_ = os.Remove(pidFile)
		}
		if tcpReady(ctx, pgAddr) {
			// SOMETHING IS SERVING AND IT IS NOT OURS. Killing whatever holds the
			// port would be killing a process this command did not start — very
			// possibly the instance CONTAINER's postgres. Refuse, and say how to
			// find out what it is.
			fmt.Fprintf(os.Stderr, "✗ %s answers but no `orchicon services` supervisor is recorded for %s\n",
				pgAddr, instance)
			fmt.Fprintf(os.Stderr, "  refusing to kill an unidentified process. See `%s services status %s`,\n",
				filepath.Base(os.Args[0]), instance)
			fmt.Fprintf(os.Stderr, "  or stop the instance container with `scripts/container.sh down %s`.\n", instance)
			return 1
		}
		fmt.Printf("services for %s are not running\n", instance)
		return 0
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ invalid PID file (%s): %v\n", pidFile, err)
		return 1
	}

	// THE GROUP, not the leader: postgres and nats are in the supervisor's
	// process group, and a wedged supervisor must not be able to keep them alive.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		if killErr := syscall.Kill(pid, syscall.SIGTERM); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			fmt.Fprintf(os.Stderr, "✗ failed to signal services group %d: %v\n", pid, killErr)
			return 1
		}
	}
	gone := waitForPortsFree(pg, nats, natsMon, serveStopGrace)
	if !gone {
		fmt.Fprintf(os.Stderr, "… services did not exit within %s — escalating to SIGKILL\n", serveStopGrace)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		gone = waitForPortsFree(pg, nats, natsMon, serveStopGrace)
	}
	if !gone {
		// KEEP the PID file: a state file that names the supervisor is the only
		// handle left on it, and reporting success here is exactly how a lingering
		// postgres becomes invisible.
		fmt.Fprintf(os.Stderr, "✗ services for %s are STILL SERVING after SIGKILL.\n", instance)
		fmt.Fprintf(os.Stderr, "  A port is still bound (postgres:%d nats:%d mon:%d). The PID file is KEPT at %s;\n",
			pg, nats, natsMon, pidFile)
		fmt.Fprintf(os.Stderr, "  a lingering postgres on %d is what makes the NEXT start fail confusingly.\n", pg)
		return 1
	}
	_ = os.Remove(pidFile)
	fmt.Printf("✓ services stopped for %s (supervisor PID %d, group signalled)\n", instance, pid)
	return 0
}

// waitForPortsFree polls until none of the three ports answers, or the grace
// expires. Every one of them is checked: postgres is the port that causes the
// confusing next-start failure, but a NATS server still holding 4222 breaks the
// plane just as thoroughly.
func waitForPortsFree(pg, nats, natsMon int, grace time.Duration) bool {
	ctx := context.Background()
	deadline := time.Now().Add(grace)
	for {
		if !tcpReady(ctx, "127.0.0.1:"+strconv.Itoa(pg)) &&
			!tcpReady(ctx, "127.0.0.1:"+strconv.Itoa(nats)) &&
			!tcpReady(ctx, "127.0.0.1:"+strconv.Itoa(natsMon)) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// servicesStatus reports what is running for an instance, and the DSN the plane
// should use. It exits 0 when the services are up, 1 when they are not, so a
// script can gate on it.
func servicesStatus(instance string) int {
	pg, nats, natsMon, err := servicePorts(instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		return 2
	}
	ctx := context.Background()

	pid, running := procRunning(servicesPIDFile(instance))
	pgUp := tcpReady(ctx, "127.0.0.1:"+strconv.Itoa(pg))
	natsUp := tcpReady(ctx, "127.0.0.1:"+strconv.Itoa(nats))

	fmt.Printf("services: %s\n", instance)
	fmt.Printf("  supervisor:        %s\n", orNot(running, "running (PID "+pid+")", "not running"))
	fmt.Printf("  postgres:          %s\n", orNot(pgUp, fmt.Sprintf("up on 127.0.0.1:%d", pg), fmt.Sprintf("down (127.0.0.1:%d)", pg)))
	fmt.Printf("  nats:              %s\n", orNot(natsUp, fmt.Sprintf("up on 127.0.0.1:%d", nats), fmt.Sprintf("down (127.0.0.1:%d)", nats)))
	fmt.Printf("  nats monitor:      %s\n", orNot(tcpReady(ctx, "127.0.0.1:"+strconv.Itoa(natsMon)),
		fmt.Sprintf("up on 127.0.0.1:%d", natsMon), fmt.Sprintf("down (127.0.0.1:%d)", natsMon)))
	fmt.Printf("  data dir:          %s\n", servicesDataDir(instance))
	fmt.Printf("  DSN:               %s\n", serviceDSN(pg))
	if running || pgUp {
		fmt.Printf("  log:               %s\n", servicesLogFile(instance))
	}
	if !pgUp {
		fmt.Printf("\nStart them with: %s services up %s\n", filepath.Base(os.Args[0]), instance)
		return 1
	}
	return 0
}

// orNot renders a two-way state with the "yes" form first.
func orNot(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

// runServicesSupervisor is the detached child: it initializes the cluster (if
// needed), starts postgres then nats, and monitors them until SIGTERM/SIGINT.
//
// It is a separate entry path rather than a goroutine of the parent because the
// parent must RETURN — `make run` cannot block on the services any more than
// `serve --detach` can block on the plane — while the supervisor must outlive it.
func runServicesSupervisor(instance string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("orchicon services supervisor starting", "instance", instance, "version", versionString())

	sup, err := newHostSupervisor(instance, log)
	if err != nil {
		log.Error("services configuration failed", "error", err)
		return 1
	}
	bins, err := ensureServiceBinaries(sup.dataDir)
	if err != nil {
		log.Error("service binary acquisition failed", "error", err)
		return 1
	}
	sup.binDir, sup.natsBin = bins.PG, bins.NATS

	if err := sup.prepare(ctx); err != nil {
		log.Error("services prepare failed", "error", err)
		return 1
	}

	// NO ORPHANS ON AN EARLY RETURN. Once a child is started, every failure path
	// below must reap what it started: this process is the one `services down`
	// identifies through the PID file, and if it exits leaving a postgres behind,
	// the port stays bound (see servicesDown's own comment — that is the exact
	// failure mode it exists to prevent). `cleanup` is armed only once a child
	// could exist, so the pre-start failure paths do not log a spurious shutdown.
	cleanup := false
	defer func() {
		if cleanup {
			sup.shutdown(ctx)
		}
	}()

	// A FAILED SERVICE MUST KILL THE SUPERVISOR, not be logged and stepped over.
	// The parent's readiness wait (waitForServicePorts) watches for exactly this: it
	// treats the supervisor's death as "give up now" and the timeout as "give up in ten
	// minutes". Logging the failure and continuing to `monitor` leaves the supervisor
	// alive with a dead postgres, so the parent waits the FULL timeout and then reports
	// a timeout — hiding the actual error (e.g. postgres's lock-file message) behind a
	// generic one. Returning here surfaces the real cause in the log within seconds.
	cleanup = true
	if err := sup.startAndWait(ctx, sup.postgresProc(), 120*time.Second); err != nil {
		log.Error("postgres did not become ready", "error", err)
		return 1
	}
	if err := sup.startAndWait(ctx, sup.natsProc(), 60*time.Second); err != nil {
		log.Error("nats did not become ready", "error", err)
		return 1
	}
	// Past this point `monitor` owns shutdown; the deferred reap must not fire on
	// the normal exit (shutdown has already run by then).
	cleanup = false
	log.Info("services ready", "instance", instance,
		"postgres", fmt.Sprintf("127.0.0.1:%d", sup.pgPort),
		"nats", fmt.Sprintf("127.0.0.1:%d", sup.natsPort),
		"dsn", serviceDSN(sup.pgPort))

	if err := sup.monitor(ctx); err != nil {
		log.Error("services monitor failed", "error", err)
		return 1
	}
	log.Info("orchicon services supervisor stopped", "instance", instance)
	return 0
}

// versionString is the binary's version tag, used in the supervisor's own log so
// a stale supervisor is identifiable from its log alone.
func versionString() string { return version.Current().String() }
