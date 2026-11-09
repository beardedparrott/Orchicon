package main

// Host-resident plane support for `orchicon install`.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ensureHostDataDir prepares the HOST data dir before the plane starts, PRESERVING THE KEK.
func ensureHostDataDir(instance, hostDataDir string, out io.Writer) error {
	kekPath := filepath.Join(hostDataDir, "secrets", "kek")
	if _, err := os.Stat(kekPath); err == nil {
		fmt.Fprintf(out, "host data dir already carries a KEK (%s) — leaving it untouched\n", hostDataDir)
		return nil
	}
	// AN OPERATOR-MANAGED KEK IS KEY MATERIAL, and it never lands in the file checked above: ResolveKEK
	// returns the ORCHICON_SECRETS_KEK override verbatim.
	if strings.TrimSpace(os.Getenv("ORCHICON_SECRETS_KEK")) != "" {
		fmt.Fprintf(out, "the KEK for %s is supplied by ORCHICON_SECRETS_KEK — no secrets/kek file is minted or expected\n", hostDataDir)
		return nil
	}
	if err := os.MkdirAll(filepath.Join(hostDataDir, "secrets"), 0o700); err != nil {
		return fmt.Errorf("create the host secrets dir %s: %w", filepath.Join(hostDataDir, "secrets"), err)
	}
	// Instance state with NO KEK: refuse. This is the orphan guard, and it is the reason this function is
	// allowed to be an error rather than a log line.
	if leftovers := hostDataDirInstanceState(hostDataDir); len(leftovers) > 0 {
		return fmt.Errorf("the host data dir %s already holds instance state (%s) but no KEK at %s —\n"+
			"  switching the data dir without the KEK ORPHANS EVERY TENANT SECRET, so this install will not mint a new one over it.\n"+
			"  Restore %s from a backup, or move %s aside and install into a fresh dir",
			hostDataDir, strings.Join(leftovers, ", "), kekPath, kekPath, hostDataDir)
	}
	fmt.Fprintf(out, "no KEK yet in %s — one is minted on first boot\n", hostDataDir)
	return nil
}

// They are records an install lays down, not state an instance produces, and resolvedPortsFile says
// as much: "a state dir containing ONLY this file is not an instance".
func hostDataDirInstanceState(hostDataDir string) []string {
	var found []string
	for _, name := range []string{"blobs", "serve", "postgres", "config"} {
		if _, err := os.Stat(filepath.Join(hostDataDir, name)); err == nil {
			found = append(found, name)
		}
	}
	return found
}

// waitServicesReady waits for an instance's services and REPORTS whether they came up.
func waitServicesReady(instance string, ports map[string]int, out io.Writer) bool {
	return waitServicesReadyFor(instance, ports, 60, time.Second, out)
}

// waitServicesReadyFor is waitServicesReady with the retry count and interval made explicit, so a
// test can assert the announce-then-warn posture without sitting through a real minute.
func waitServicesReadyFor(instance string, ports map[string]int, tries int, sleep time.Duration, out io.Writer) bool {
	// NO HOST ENDPOINT TO WAIT ON: see servicesReady for why this carrier cannot be probed from here,
	// and why that is NOT the same as "not ready".
	if !servicesProbeableFromHost(ports) {
		fmt.Fprintf(out, "%s services run inside the instance container and are published on no host port — "+
			"their readiness is the plane's own health\n", instance)
		return true
	}
	// Announced before the wait: this can sit here for a minute, and a silent minute
	// reads as a hang.
	fmt.Fprintf(out, "waiting for %s services (postgres 127.0.0.1:%d, nats monitor 127.0.0.1:%d) …\n",
		instance, ports["ORCHICON_POSTGRES_PORT"], ports["ORCHICON_NATS_MONITOR_PORT"])
	for i := 0; i < tries; i++ {
		if servicesReady(instance, ports) {
			fmt.Fprintf(out, "%s services ready (postgres 127.0.0.1:%d, nats monitor 127.0.0.1:%d)\n",
				instance, ports["ORCHICON_POSTGRES_PORT"], ports["ORCHICON_NATS_MONITOR_PORT"])
			return true
		}
		// A DEAD SUPERVISOR CANNOT BECOME READY, SO STOP WAITING FOR IT.
		if hostServicesRanAndDied(instance) {
			fmt.Fprintf(out, "%s services supervisor exited during startup — it cannot become ready.\n", instance)
			return false
		}
		time.Sleep(sleep)
	}
	fmt.Fprintf(out, "warning: %s services were not confirmed ready within %s — continuing\n",
		instance, time.Duration(tries)*sleep)
	return false
}

// hostServicesRanAndDied reports that a supervisor was launched for this instance and is now gone.
func hostServicesRanAndDied(instance string) bool {
	pidFile, err := hostServicesPIDFile(instance)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	return !pidAlive(pid)
}

// IT MUST NOT REPORT "NOT READY" FOR A CARRIER IT HAS NO HOST ENDPOINT FOR, and that is a real bug
// this function used to have.
func servicesReady(instance string, ports map[string]int) bool {
	if !servicesProbeableFromHost(ports) {
		// No host-visible endpoint: this is not this function's question. The fail-safe direction is the same
		// one recordedResidency takes — never manufacture a negative you cannot observe (see above).
		return true
	}
	// THE WHOLE SET, NOT THE CRITICAL CHAIN. It asks for the REQUIRED components only.
	return requiredServicesReadyWithin(instance, ports, 500*time.Millisecond)
}

// servicesProbeableFromHost reports whether this instance has a host endpoint to ask at all, i.e.
// whether its Postgres and NATS are published on a resolved host port.
func servicesProbeableFromHost(ports map[string]int) bool {
	return ports["ORCHICON_POSTGRES_PORT"] > 0 && ports["ORCHICON_NATS_MONITOR_PORT"] > 0
}

// pgIsReadyBin resolves the `pg_isready` to probe with — the one belonging to the PostgreSQL the
// install actually resolved (recorded in the instance's infra-binaries.json), falling back to PATH.
func pgIsReadyBin(instance string) string {
	rec, err := loadInfraBinaries(hostDataDirFor(instance))
	if err == nil && rec.PgIsReady != "" {
		return rec.PgIsReady
	}
	return "pg_isready"
}

// hostServicesPIDFile is where the host-services supervisor's PID is recorded, under the instance's own
// serve dir so it is per-instance like every other piece of instance state.
func hostServicesPIDFile(instance string) (string, error) {
	dir, err := hostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "serve", "services.pid"), nil
}

// hostServicesAlive reports whether the host-services supervisor this installer started is still
// running, from the PID file startHostServices records.
func hostServicesAlive(instance string) bool {
	pidFile, err := hostServicesPIDFile(instance)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	return pidAlive(pid)
}

// hostServicesPortEnvs are the port knobs the host-services supervisor reads (residency.go's
// servicePortsFor), in the exact names install_ports.go and scripts/container.sh already use.
var hostServicesPortEnvs = []string{
	"ORCHICON_POSTGRES_PORT",
	"ORCHICON_NATS_PORT",
	"ORCHICON_NATS_MONITOR_PORT",
	"ORCHICON_OTLP_GRPC_PORT",
	"ORCHICON_OTLP_HTTP_PORT",
	"ORCHICON_TEMPO_PORT",
	"ORCHICON_LOKI_PORT",
	"ORCHICON_VM_PORT",
	"ORCHICON_TEMPO_OTLP_GRPC_PORT",
	"ORCHICON_TEMPO_OTLP_HTTP_PORT",
	"ORCHICON_OTEL_HEALTH_PORT",
	// EVERY KNOB servicePortsFromEnv HONOURS MUST BE FORWARDED, OR THE TWO HALVES OF ONE INSTANCE
	// DISAGREE — which is the class of bug this whole file's port handling exists to prevent.
	"ORCHICON_TEMPO_GRPC_PORT",
	"ORCHICON_OTEL_SELF_METRICS_PORT",
}

// startHostServices starts the instance's SERVICES as HOST PROCESSES, detached, and records the
// PID.
func startHostServices(instance string, ports map[string]int, hostDataDir string, out io.Writer) error {
	if servicesReady(instance, ports) {
		fmt.Fprintf(out, "host services for %s are already ready — leaving them running\n", instance)
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this binary to start the host services: %w", err)
	}
	// ORCHICON_CONTAINER_MODE is stripped for the same reason startHostPlane strips it: a stray export in a
	// shell profile must never flip a host process into container semantics.
	env := envWithout(os.Environ(), "ORCHICON_CONTAINER_MODE")
	env = append(env,
		"ORCHICON_INSTANCE="+instance,
		"ORCHICON_DATA_DIR="+hostDataDir,
	)
	for _, k := range hostServicesPortEnvs {
		if v, ok := ports[k]; ok {
			env = append(env, fmt.Sprintf("%s=%d", k, v))
		}
	}
	pidFile, err := hostServicesPIDFile(instance)
	if err != nil {
		return fmt.Errorf("resolve the host-services PID file for %s: %w", instance, err)
	}
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		return fmt.Errorf("create the host-services state dir %s: %w", filepath.Dir(pidFile), err)
	}
	fmt.Fprintf(out, "starting the host services for %s (postgres, nats, telemetry as processes of this user) …\n", instance)
	pid, err := startDetachedDaemonEnv(self, []string{"host-services"}, env, filepath.Join(hostDataDir, "services.log"))
	if err != nil {
		return fmt.Errorf("start the host services for %s: %w", instance, err)
	}
	// THE PID IS RECORDED because there is no pre-written PID file to read the way `serve --detach` has
	// one: stopHostServices needs to know which process it may signal.
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("record the host-services PID at %s: %w", pidFile, err)
	}
	return nil
}

// stopHostServices stops the host-services supervisor this installer started, so the caller can
// start it again on the installed binaries.
func stopHostServices(instance string, out io.Writer) error {
	pidFile, err := hostServicesPIDFile(instance)
	if err != nil {
		return fmt.Errorf("resolve the host-services PID file for %s: %w", instance, err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		fmt.Fprintf(out, "no host services recorded for %s — nothing to stop\n", instance)
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		fmt.Fprintf(out, "no host services recorded for %s — nothing to stop\n", instance)
		_ = os.Remove(pidFile)
		return nil
	}
	if !pidAlive(pid) {
		fmt.Fprintf(out, "the host services for %s (pid %d) are no longer running\n", instance, pid)
		_ = os.Remove(pidFile)
		return nil
	}
	// KILL THE TREE, NOT JUST THE SUPERVISOR.
	//
	// TerminateProcess terminates ONE process, and the supervisor's CHILDREN — postgres, nats,
	// the telemetry backends — are separate processes. So stopping the services left postgres
	// ORPHANED: still running, still holding the data directory's postmaster.pid lock and its
	// port. The next install's postgres then refused to start:
	//
	//	FATAL:  lock file "postmaster.pid" already exists
	//	HINT:  Is another postmaster (PID 26376) running in data directory "…/orchicon-dev/postgres"?
	//
	// which is a failure with no obvious cause and no way out but hand-editing the data dir.
	//
	// The tree kill is what makes the stop mean what it says. The lock sweep in
	// postgres_lock_windows.go is the SECOND half: a supervisor that was killed outright cannot
	// clean up after itself, so a start must tolerate the lock it left.
	if err := terminateProcessTree(pid); err != nil {
		return fmt.Errorf("stop the host services for %s (pid %d): %w", instance, pid, err)
	}
	// BOUNDED, so a supervisor that ignores SIGTERM is REPORTED rather than slept through — the same
	// posture stopHostPlane takes, and for the same reason: the ports are still held until it exits.
	for i := 0; i < 40; i++ {
		if !pidAlive(pid) {
			_ = os.Remove(pidFile)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the host services for %s (pid %d) did not stop — they may still be shutting down", instance, pid)
}

// servicesNotReadyError is the failure an install reports when the services it started never became
// reachable — and it carries the tail of the services log.
func servicesNotReadyError(instance, hostDataDir string) error {
	logPath := filepath.Join(hostDataDir, "services.log")

	var b strings.Builder
	fmt.Fprintf(&b, "the %s services did not come up, so the plane was not started.\n", instance)
	fmt.Fprintf(&b, "\n  what failed is recorded in:\n    %s\n", logPath)

	if tail := tailLines(logPath, 25); tail != "" {
		b.WriteString("\n  the last lines of that log:\n\n")
		for _, line := range strings.Split(tail, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			fmt.Fprintf(&b, "    %s\n", line)
		}
	} else {
		// NO LOG IS ITSELF A FINDING: it means the supervisor never started far enough to write
		// one, which is a different problem from "it started and the database failed".
		b.WriteString("\n  that log is empty or missing, which means the services process did not start\n" +
			"  far enough to report anything. Re-run `orchicon install` in a terminal and watch it.\n")
	}

	b.WriteString("\n  the binaries are installed and this is resumable — fix what the log shows, then:\n" +
		"    orchicon install\n")
	return errors.New(b.String())
}

// tailLines returns the last n lines of a file, or "" when it cannot be read.
func tailLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	const maxRead = 64 << 10 // a tail, not the whole file
	if st, err := f.Stat(); err == nil && st.Size() > maxRead {
		if _, err := f.Seek(-maxRead, io.SeekEnd); err != nil {
			return ""
		}
	}
	body, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
