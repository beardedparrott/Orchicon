package main

// Host-resident plane support for `orchicon install`.
//
// WHY THIS IS IN GO AND NOT A CALL TO scripts/container.sh. The launcher owns
// the host-residency implementation in bash — plane_env, bridge_bind_env,
// plane_start, migrate_host_data_dir — but container.sh is NOT IN THE RELEASE
// ARCHIVE (install.sh extracts only `orchicon` and `orch`, and the Makefile
// builds only ./cmd/orchicon and ./cmd/orch); the binary EMBEDS what it needs
// instead. So a curl-installed host has the one-command installer and no shell
// library behind it, and the only way `orchicon install` can produce the same
// shape as the launcher is to implement it here.
//
// The semantics are mirrored deliberately, not approximated: the same variable
// names, the same per-instance ports, the same "loopback + docker bridge" bind
// pair, and the same refusal to guess a bridge address. Anything that disagrees
// with container.sh would show up as an instance whose shape depends on which
// launcher created it.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// residencyRecordFile is the per-instance record of the plane's SHAPE, written next to the other install
// records in the instance state dir (ports.json, infra-binaries.json).
//
// RESIDENCY STOPPED BEING SOMETHING AN ENGINE IS ASKED ABOUT. It used to come from `docker inspect` on the
// instance container's create-time environment (ORCHICON_CONTAINER_SERVICES_ONLY=1); with the container gone
// there is nothing to inspect, and on a machine with no engine a `docker` invocation in an install path is
// exactly the thing that fails silently. The installer therefore RECORDS the shape it created, and reads it
// back. It is per-instance by construction (it lives in that instance's state dir), which is what keeps two
// instances from disagreeing about each other's shape.
const residencyRecordFile = "residency"

// residencyRecordPath is <hostDataDir>/residency — the instance's own state dir, never a shared location.
func residencyRecordPath(instance string) (string, error) {
	dir, err := hostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, residencyRecordFile), nil
}

// recordedResidency reports the shape this install created for the instance.
//
// THE BOOL IS THE WHOLE POINT, and it is the direct successor of containerResidency's contract: ok=false
// means the shape was NOT readable — absent, empty, or holding something that is not `host`/`container` — and
// every caller must treat that as "leave it alone", NEVER as a mismatch. Recreating a live instance because
// a shape could not be read is worse than leaving a wrong-shaped one for the operator to see.
//
// NO ERROR IS SURFACED, deliberately. An unreadable record cannot be turned into an error by a caller,
// which is what makes the fail-safe mechanical rather than a discipline each call site has to remember.
func recordedResidency(instance string) (string, bool) {
	path, err := residencyRecordPath(instance)
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	switch v := strings.TrimSpace(string(b)); v {
	case residencyHost, residencyContainer:
		return v, true
	default:
		return "", false
	}
}

// recordResidency writes the shape down. Best-effort like writeResolvedPorts: the install has already
// succeeded by the time this runs, and losing the record costs the operator a re-resolved shape, not a
// broken instance.
func recordResidency(instance, residency string) {
	path, err := residencyRecordPath(instance)
	if err != nil {
		fmt.Printf("warning: could not locate the instance state dir to record the residency: %v\n", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Printf("warning: could not record the residency shape: %v\n", err)
		return
	}
	if err := os.WriteFile(path, []byte(residency+"\n"), 0o644); err != nil {
		fmt.Printf("warning: could not record the residency shape: %v\n", err)
	}
}

// runtimeContainersEnv is the explicit override for whether runtime containers are in use. Unset means
// "decide from the host" (see runtimeContainersInUse), which is what a plain install needs.
const runtimeContainersEnv = "ORCHICON_RUNTIME_CONTAINERS"

// containerEngineAvailable reports whether a container engine is INSTALLED, as a PATH probe.
//
// IT IS A LookPath AND NOT AN ENGINE CALL, on purpose: `docker version` needs a running daemon and can
// fail for a dozen reasons that have nothing to do with whether the binary exists, and a machine with no
// Docker must be able to answer "no" without an error. The daemon check happens later, in install.go, and
// ONLY when this returns true — so a host with no engine never runs a doomed `docker version`.
func containerEngineAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// runtimeContainersInUse reports whether this install should assume runtime containers. Order:
//
//  1. ORCHICON_RUNTIME_CONTAINERS — an explicit answer (`0|off|no|false|none` = no, anything else = yes).
//  2. ORCHICON_DOCKER_BRIDGE_IP non-empty — a PINNED bridge is proof of intent: an operator who named the
//     bridge they want their containers to use is running containers.
//  3. otherwise, the engine probe.
//
// The middle rung is what keeps a pinned-bridge host working with no daemon reachable, and it is why the
// per-instance bind/URL test stays meaningful when the engine is absent.
func runtimeContainersInUse() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(runtimeContainersEnv))) {
	case "0", "off", "no", "false", "none":
		return false
	case "":
		// fall through to the next rung
	default:
		return true
	}
	if strings.TrimSpace(os.Getenv("ORCHICON_DOCKER_BRIDGE_IP")) != "" {
		return true
	}
	return containerEngineAvailable()
}

// planeBindEnv is the ONE place the bridge bind and the advertised plane URL are computed.
//
// IT RETURNS ("", "", nil) WHEN RUNTIME CONTAINERS ARE NOT IN USE. That is the whole de-containerization
// change: a local-mode user has no bridge, needs no bind, and must not have an install fail over it. It is
// an ERROR only in the case where containers ARE in use — because there the bind is load-bearing: a plane
// its own workers cannot reach is worse than one that refuses to start, and there is deliberately no
// 0.0.0.0 fallback (that is the LAN exposure the operator rejected).
//
// BOTH VALUES COME FROM THIS ONE CALL — the same IP and the SAME instance's control port — because a bind
// computed at one call site and a URL at another is how the two drift apart and how one instance's workers
// end up pointed at another's plane. The port is read from the phase-0-resolved set, never re-derived.
func planeBindEnv(ports map[string]int) (bind, url string, err error) {
	if !runtimeContainersInUse() {
		return "", "", nil
	}
	ip, err := bridgeIP()
	if err != nil {
		return "", "", err
	}
	control := ports["ORCHICON_CONTROL_PORT"]
	return fmt.Sprintf("%s:%d", ip, control), fmt.Sprintf("http://%s:%d", ip, control), nil
}

// planeEnv builds the HOST plane's environment for an instance.
//
// EVERY value is derived from the RESOLVED ports, so no value can be global: a
// shared HTTP port or plane URL would silently point one instance's workers at
// the other's plane. The bind and the advertised URL are computed from the SAME
// bridge IP and the SAME port in ONE place (planeBindEnv), so what the plane
// listens on and what it hands its run containers can never disagree — and the
// pair is present ONLY when runtime containers are in use, because a host with
// no container engine has no bridge to bind and needs none.
func planeEnv(instance string, ports map[string]int, hostDataDir, runtimeSocket string) ([]string, error) {
	bind, url, err := planeBindEnv(ports)
	if err != nil {
		return nil, err
	}
	control := ports["ORCHICON_CONTROL_PORT"]
	env := []string{
		"ORCHICON_INSTANCE=" + instance,
		// THE PRIMARY BIND IS LOOPBACK, NOT A WILDCARD — and that is load-bearing,
		// not cosmetic: `:<port>` already owns <bridge ip>:<port> on EVERY
		// interface, so the extra bind below could never bind ("address already in
		// use") and the plane retried a doomed listener on every boot. Loopback
		// keeps host clients (orch, the GUI) working and leaves the bridge address
		// free for the listener that exists to serve runtime containers.
		fmt.Sprintf("ORCHICON_HTTP_ADDR=127.0.0.1:%d", control),
		fmt.Sprintf("ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:%d/orchicon?sslmode=disable", ports["ORCHICON_POSTGRES_PORT"]),
		fmt.Sprintf("ORCHICON_NATS_URL=nats://localhost:%d", ports["ORCHICON_NATS_PORT"]),
		fmt.Sprintf("ORCHICON_OTEL_ENDPOINT=localhost:%d", ports["ORCHICON_OTLP_GRPC_PORT"]),
		// Grafana is proxied same-origin under /grafana, so the plane reaches it
		// on the Grafana publish's host port.
		fmt.Sprintf("ORCHICON_GRAFANA_URL=http://localhost:%d", ports["ORCHICON_GRAFANA_PORT"]),
		fmt.Sprintf("ORCHICON_TEMPO_URL=http://localhost:%d", ports["ORCHICON_TEMPO_PORT"]),
		fmt.Sprintf("ORCHICON_LOKI_URL=http://localhost:%d", ports["ORCHICON_LOKI_PORT"]),
		fmt.Sprintf("ORCHICON_VM_URL=http://localhost:%d", ports["ORCHICON_VM_PORT"]),
		// The KEK and ask-history live here; the VALUE moves between shapes, the
		// expectation (<DataDir>/secrets/kek) does not — see ensureHostDataDir.
		"ORCHICON_DATA_DIR=" + hostDataDir,
		"ORCHICON_BLOB_DIR=" + filepath.Join(hostDataDir, "blobs"),
		"ORCHICON_RUNTIME_SOCKET=" + runtimeSocket,
		// Per-instance PID/log file for serve --detach/--stop: two host planes
		// must never share one, or stopping dev would kill prod.
		"ORCHICON_SERVE_STATE_DIR=" + filepath.Join(hostDataDir, "serve"),
	}
	// ONLY WHEN RUNTIME CONTAINERS ARE IN USE. Absent is the correct state for a host with no container
	// engine: there is no bridge to bind, no run container to reach the plane, and inventing a wildcard
	// bind here is the LAN exposure the operator rejected. See planeBindEnv — both entries come from that
	// ONE call, so they cannot disagree, and they carry THIS instance's port so two instances cannot be
	// pointed at each other's plane.
	if bind != "" {
		env = append(env,
			"ORCHICON_HTTP_EXTRA_BIND="+bind,
			"ORCHICON_PLANE_PUBLIC_URL="+url,
		)
	}
	// Identity/keys the operator already set are INHERITED rather than invented —
	// a deployment that picked its own tenant or signing key must keep them, or
	// the migrated data would be read under the wrong identity.
	for _, k := range []string{"ORCHICON_DEPLOYMENT_TENANT_ID", "ORCHICON_AUTH_SIGNING_KEY", "ORCHICON_SECRETS_KEK"} {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env, nil
}

// envWithout returns env with every entry for the named keys removed.
func envWithout(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		skip := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}

// httpHealthy reports whether a URL answers 200 within the timeout.
func httpHealthy(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == 200
}

// ensureHostDataDir prepares the HOST data dir before the plane starts, PRESERVING THE KEK.
//
// THE KEK LIVES AT <DataDir>/secrets/kek, so pointing an instance at a data dir without carrying it over
// ORPHANS EVERY TENANT SECRET — the data is still there and simply stops decrypting. Two rules follow, and
// both are kept here even though the docker-volume COPY this function used to perform is gone:
//
//   - An EXISTING <DataDir>/secrets/kek is never overwritten and never re-minted. It is the operator's key
//     material and the difference between a working instance and a decryptable-looking one.
//   - A data dir that ALREADY HOLDS INSTANCE STATE (blobs/, serve/, ports.json, a postgres cluster) with NO
//     KEK is a FATAL error, not a warning: minting a fresh KEK there orphans every tenant secret, and a
//     warning on stdout is exactly how that goes unnoticed.
//
// WHY THE VOLUME COPY IS GONE: the container shape that wrote a docker volume is being retired, so there is
// no container-era volume to migrate FROM on a fresh install. What must survive is the FAIL-SAFE, and it is
// strictly stronger now — the guard runs on every install, not just the first switch-over.
func ensureHostDataDir(instance, hostDataDir string, out io.Writer) error {
	kekPath := filepath.Join(hostDataDir, "secrets", "kek")
	if _, err := os.Stat(kekPath); err == nil {
		fmt.Fprintf(out, "host data dir already carries a KEK (%s) — leaving it untouched\n", hostDataDir)
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

// hostDataDirInstanceState names the entries that prove a data dir belongs to a LIVE instance rather than
// being a directory an install is still filling. It is deliberately a small, explicit list: these are the
// artifacts the plane and its services create, and finding any of them without a KEK means the KEK was LOST
// rather than never created.
//
// ports.json AND infra-binaries.json ARE NOT IN IT, and that is a correctness requirement rather than a
// judgement call: `orchicon install` writes BOTH of them into this very directory EARLIER in the same run
// (step 1.6 records the resolved ports, step 1.7 resolves the infra binaries) — before this guard is reached.
// Counting them as instance state would refuse EVERY fresh install. They are records an install lays down,
// not state an instance produces, and resolvedPortsFile says as much: "a state dir containing ONLY this file
// is not an instance".
func hostDataDirInstanceState(hostDataDir string) []string {
	var found []string
	for _, name := range []string{"blobs", "serve", "postgres", "config"} {
		if _, err := os.Stat(filepath.Join(hostDataDir, name)); err == nil {
			found = append(found, name)
		}
	}
	return found
}

// waitServicesReady waits for an instance's services and REPORTS whether they came up. It used to return
// nothing, so a caller could not act on "the stack is not ready" — which is one of the states the install's
// repair pass exists to fix.
//
// A timeout WARNS rather than failing: a slow first boot should not abort an install that is otherwise
// complete, and the plane's own retries cover a backend that comes up late. The WAIT is announced first,
// because a silent minute reads as a hang.
func waitServicesReady(instance string, ports map[string]int, out io.Writer) bool {
	return waitServicesReadyFor(instance, ports, 60, time.Second, out)
}

// waitServicesReadyFor is waitServicesReady with the retry count and interval made explicit, so a test can
// assert the announce-then-warn posture without sitting through a real minute. The exported behaviour is
// exactly the wrapper above: announce → poll → warn → return whether it came up.
func waitServicesReadyFor(instance string, ports map[string]int, tries int, sleep time.Duration, out io.Writer) bool {
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
		time.Sleep(sleep)
	}
	fmt.Fprintf(out, "warning: %s services were not confirmed ready within %s — continuing\n",
		instance, time.Duration(tries)*sleep)
	return false
}

// servicesReady asks the instance's services the same two questions the waiting loop does, as a boolean: is
// Postgres accepting connections, and is the NATS monitor healthy.
//
// THE PROBES ARE DIRECT, NOT `docker exec`. Asking the container was how the installer learned about the
// services, and it encoded WHERE they live into the question — so a host with no container could not ask it
// at all. These two probes work for BOTH carriers (a 127.0.0.1 loopback publish from the instance container,
// or a host process from host service residency), because they reach the RESOLVED host ports either way.
func servicesReady(instance string, ports map[string]int) bool {
	pg := exec.Command(pgIsReadyBin(instance),
		"-h", "127.0.0.1", "-p", strconv.Itoa(ports["ORCHICON_POSTGRES_PORT"]), "-U", "orchicon")
	nats := fmt.Sprintf("http://127.0.0.1:%d/healthz", ports["ORCHICON_NATS_MONITOR_PORT"])
	return pg.Run() == nil && httpHealthy(nats, 3*time.Second)
}

// pgIsReadyBin resolves the `pg_isready` to probe with — the one belonging to the PostgreSQL the install
// actually resolved (recorded in the instance's infra-binaries.json), falling back to PATH.
//
// THE RECORDED BINARY FIRST, because the point of the record is that both server and client tools come from
// THE SAME distribution: probing PATH on a host whose PATH holds an older postgres would ask a client that
// does not match the server, and "is postgres ready" would answer for the wrong build. The PATH fallback
// covers a hand-run instance that never went through the resolver — the same shape loadInfraBinaries already
// accommodates.
func pgIsReadyBin(instance string) string {
	rec, err := loadInfraBinaries(hostDataDirFor(instance))
	if err == nil && rec.PgIsReady != "" {
		return rec.PgIsReady
	}
	return "pg_isready"
}

// planeDrift says whether the host plane now serving matches the binary this install put in place.
type planeDriftState int

const (
	// planeUnknown: the serving process could not be identified. Treat as "leave it alone".
	planeUnknown planeDriftState = iota
	// planeCurrent: the plane is running the installed binary.
	planeCurrent
	// planeDrifted: the plane is running a superseded binary and should be restarted.
	planeDrifted
)

// stopHostPlane stops the instance's detached host plane, so the caller can start it on the installed
// binary.
//
// THE INSTALLER HAD NO WAY TO DO THIS AT ALL — `serve --stop` appears nowhere in it — which is why a stale
// plane could never be corrected. It re-execs THIS binary (the newly installed one) with the instance's
// state dir, exactly as the shell launcher does, because `serve --stop` resolves its PID file from
// ORCHICON_SERVE_STATE_DIR and a bare call would look in the wrong place.
func stopHostPlane(instance string, out io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this binary to stop the host plane: %w", err)
	}
	dataDir, derr := hostDataDir(instance)
	if derr != nil {
		return fmt.Errorf("resolve the host plane state dir for %s: %w", instance, derr)
	}
	cmd := exec.Command(self, "serve", "--stop")
	cmd.Env = append(envWithout(os.Environ(), "ORCHICON_CONTAINER_MODE"),
		serveStateDirEnv+"="+filepath.Join(dataDir, "serve"))
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("stop the existing host plane for %s: %w", instance, err)
	}
	// The socket has to be free before the new plane binds it. Bounded, and a failure here is reported
	// rather than slept through: an address still in use is the one reason the start below would fail.
	for i := 0; i < 20; i++ {
		if !servingPlaneAlive(instance) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the host plane for %s did not stop — it may still be shutting down", instance)
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

// hostServicesPortEnvs are the port knobs the host-services supervisor reads (residency.go's
// servicePortsFor), in the exact names install_ports.go and scripts/container.sh already use — so a
// host-resident install needs no second vocabulary.
var hostServicesPortEnvs = []string{
	"ORCHICON_POSTGRES_PORT",
	"ORCHICON_NATS_PORT",
	"ORCHICON_NATS_MONITOR_PORT",
	"ORCHICON_OTLP_GRPC_PORT",
	"ORCHICON_OTLP_HTTP_PORT",
	"ORCHICON_TEMPO_PORT",
	"ORCHICON_LOKI_PORT",
	"ORCHICON_VM_PORT",
	"ORCHICON_GRAFANA_PORT",
	"ORCHICON_TEMPO_OTLP_GRPC_PORT",
	"ORCHICON_TEMPO_OTLP_HTTP_PORT",
	"ORCHICON_OTEL_HEALTH_PORT",
}

// startHostServices starts the instance's SERVICES as HOST PROCESSES, detached, and records the PID.
//
// WHY A RE-EXEC AS `container`: container.go's supervisor ALREADY implements exactly this — runContainer
// resolves the service residency, and with ORCHICON_SERVICE_RESIDENCY=host it runs postgres, nats and the
// telemetry children as processes of the invoking user with NO control-plane child and no container engine
// (container.go: `servicesOnly := servicesOnlyFromEnv() || residency == residencyHost`). Writing a second
// supervisor here would be a second implementation of the same thing, so this re-execs this binary.
//
// IT IS IDEMPOTENT: already-ready services are left alone, which is what keeps a re-run a convergence rather
// than a second stack racing the first for the same ports.
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
	env := envWithout(os.Environ(), "ORCHICON_CONTAINER_MODE", servicesOnlyEnv)
	env = append(env,
		"ORCHICON_INSTANCE="+instance,
		// THE ONE KNOB THAT DECIDES THE CARRIER: host services run as this user's processes.
		"ORCHICON_SERVICE_RESIDENCY="+residencyHost,
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
	pid, err := startDetachedDaemonEnv(self, []string{"container"}, env, filepath.Join(hostDataDir, "services.log"))
	if err != nil {
		return fmt.Errorf("start the host services for %s: %w", instance, err)
	}
	// THE PID IS RECORDED because there is no pre-written PID file to read the way `serve --detach` has one:
	// stopHostServices needs to know which process it may signal, and guessing by port owner would signal
	// something this installer did not start.
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("record the host-services PID at %s: %w", pidFile, err)
	}
	return nil
}

// stopHostServices stops the host-services supervisor this installer started, so the caller can start it
// again on the installed binaries.
//
// A MISSING PID FILE IS NOT AN ERROR: an instance whose services were never started as host processes (or
// were already stopped) has nothing to stop, and failing here would make the repair pass refuse to act.
// A process that will not die IS an error — the port is still held and a start would fail.
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
	if err := terminateProcess(pid); err != nil {
		return fmt.Errorf("stop the host services for %s (pid %d): %w", instance, pid, err)
	}
	// BOUNDED, so a supervisor that ignores SIGTERM is REPORTED rather than slept through — the same posture
	// stopHostPlane takes, and for the same reason: the ports are still held until it exits, so the start
	// that follows would fail with a bind error that names nothing.
	for i := 0; i < 40; i++ {
		if !pidAlive(pid) {
			_ = os.Remove(pidFile)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the host services for %s (pid %d) did not stop — they may still be shutting down", instance, pid)
}

// startHostPlane launches the HOST plane for an instance, detached, and waits
// for its /healthz. Idempotent: an already-healthy plane is left alone.
//
// THE BINARY IS THIS ONE (os.Executable), which is what makes the release
// archive sufficient: the installed `orchicon` re-execs itself as
// `serve --detach`, so no sibling build or script is required.
//
// ORCHICON_CONTAINER_MODE is REMOVED from the inherited environment: a stray
// export in a shell profile must never flip the HOST plane into container
// semantics — it would then rewrite custom provider URLs that are already
// correct on the host.
func startHostPlane(instance string, ports map[string]int, hostDataDir, runtimeSocket string, out io.Writer) error {
	healthURL := fmt.Sprintf("http://localhost:%d/healthz", ports["ORCHICON_CONTROL_PORT"])

	// A PLANE THAT ANSWERS IS NOT NECESSARILY THE PLANE WE JUST INSTALLED.
	//
	// This used to be `if httpHealthy(...) { "already serving"; return nil }`, which asks whether SOMETHING
	// is listening — not whether the listening process is the binary in hand. An install replaces that
	// binary, so the old process keeps serving revised code from a replaced (unlinked) executable, and the
	// installer reports success over an instance that was never upgraded. Reported: "said it was complete
	// but no auth works and the previous dev container has been up for two hours".
	//
	// So the SERVING PROCESS is asked what it is running: the instance's own PID file names it, and
	// /proc/<pid>/exe answers with the path — or with a `(deleted)` suffix once that path has been replaced
	// underneath it, which is precisely the state an install creates.
	if httpHealthy(healthURL, 2*time.Second) {
		switch planeDrift(instance, out) {
		case planeCurrent:
			fmt.Fprintf(out, "host plane already serving %s (running the installed binary)\n", healthURL)
			return nil
		case planeDrifted:
			// AN INSTALL IS AN UPGRADE, so replacing the running plane is what the operator asked for by
			// running it. Stopping it here lets the start below bring up the NEW binary.
			fmt.Fprintf(out, "host plane at %s is running a superseded binary — restarting it on the installed one\n", healthURL)
			if err := stopHostPlane(instance, out); err != nil {
				return err
			}
		default:
			// The serving process cannot be identified (no PID file, unreadable /proc — another OS, a
			// container-resident plane). Leave it ALONE: a restart we cannot justify is a blip on a working
			// instance, and the health check a few lines below still reports whether it is serving.
			fmt.Fprintf(out, "host plane already serving %s (could not identify the process; leaving it)\n", healthURL)
			return nil
		}
	}
	// A host plane its own workers cannot reach is worse than one that refuses to
	// start: every plane-channel MCP call in every dispatched run would time out.
	// There is no loopback-only fallback, so an unresolvable bridge is fatal here
	// (planeEnv is what raises it).
	env, err := planeEnv(instance, ports, hostDataDir, runtimeSocket)
	if err != nil {
		return fmt.Errorf("refusing to start the host plane: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this binary to start the host plane: %w", err)
	}
	cmd := exec.Command(self, "serve", "--detach")
	cmd.Env = append(envWithout(os.Environ(), "ORCHICON_CONTAINER_MODE"), env...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start the host plane for %s: %w", instance, err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if httpHealthy(healthURL, 2*time.Second) {
			fmt.Fprintf(out, "host plane ready: %s\n", healthURL)
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("the host plane did not answer %s within 60s — see %s/logs/orchicon.log",
		healthURL, filepath.Join(hostDataDir, "serve"))
}
