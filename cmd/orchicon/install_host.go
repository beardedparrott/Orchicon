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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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
//   - AN OPERATOR-MANAGED KEK IS NOT A MISSING KEK. secretcrypto.ResolveKEK returns an ORCHICON_SECRETS_KEK
//     override VERBATIM and never writes <DataDir>/secrets/kek, so a deployment that supplies one — a
//     documented mode that planeEnv deliberately inherits — reaches the guard below with a populated data dir
//     and no key FILE. The key is not lost, it is supplied from outside, and the guard must not fire.
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
	// AN OPERATOR-MANAGED KEK IS KEY MATERIAL, and it never lands in the file checked above: ResolveKEK
	// returns the ORCHICON_SECRETS_KEK override verbatim. So a deployment that supplies it (KMS, a secret
	// manager, one key shared across replicas — a mode planeEnv deliberately carries through) arrives here
	// with blobs/, serve/ and a postgres cluster on disk and NO secrets/kek.
	//
	// THAT IS NOT A LOST KEK, so the guard below must not fire. Refusing here would break convergence for
	// exactly the deployment the guard protects: a re-run of a healthy env-KEK install would stop with
	// "ORPHANS EVERY TENANT SECRET" and tell the operator to restore a file that was never part of it. The
	// rule is unchanged for the case that matters — state with NEITHER a key file NOR an override is still
	// FATAL, because there a fresh KEK really would orphan stored secrets.
	//
	// The override is deliberately NOT validated here: a malformed one is the plane's warning to give
	// (server.go logs "secrets KEK unavailable" and disables the store), and an install must not be refused
	// over a setting.
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
	// NO HOST ENDPOINT TO WAIT ON: see servicesReady for why this carrier cannot be probed from here, and why
	// that is NOT the same as "not ready". There is deliberately no wait at all in that case — announcing one
	// would print a countdown against port 0, i.e. a wait on nothing dressed as a wait.
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
// at all. These two probes work for BOTH host-reachable carriers (a 127.0.0.1 loopback publish from the
// instance container in HOST residency, or a host process from host service residency), because they reach
// the RESOLVED host ports either way.
//
// IT MUST NOT REPORT "NOT READY" FOR A CARRIER IT HAS NO HOST ENDPOINT FOR, and that is a real bug this
// function used to have. A container-RESIDENT plane keeps postgres and nats in the container's OWN network
// namespace and publishes NEITHER on a host port (install_ports.go: their ContainerBind is bindNone), so the
// resolved port set for that shape names only the control port — the service keys are
// ABSENT, read as port 0, and the probe then asked a nonexistent endpoint and answered false. Consequence:
// the install's own repair gate (`!healthy || !servicesReady(instance, ports)`) fired on EVERY
// container-resident install, `docker restart`ing the instance container — and the plane inside it — on every
// re-run over a perfectly healthy instance. That is the exact opposite of the convergence criterion 6 asks
// for, and it is why the missing key answers TRUE below rather than false.
//
// TRUE IS ALSO TRUE IN SUBSTANCE THERE: container.go blocks the plane's boot on postgres → nats ("Block on
// each so the plane never boots without its backends"), so a container-resident plane that answers /healthz
// has its backends up, and the plane's health is the gate that actually applies to that carrier.
func servicesReady(instance string, ports map[string]int) bool {
	if !servicesProbeableFromHost(ports) {
		// No host-visible endpoint: this is not this function's question. The fail-safe direction is the same
		// one recordedResidency takes — never manufacture a negative you cannot observe (see above).
		return true
	}
	pg := exec.Command(pgIsReadyBin(instance),
		"-h", "127.0.0.1", "-p", strconv.Itoa(ports["ORCHICON_POSTGRES_PORT"]), "-U", "orchicon")
	nats := fmt.Sprintf("http://127.0.0.1:%d/healthz", ports["ORCHICON_NATS_MONITOR_PORT"])
	return pg.Run() == nil && httpHealthy(nats, 3*time.Second)
}

// servicesProbeableFromHost reports whether this instance has a host endpoint to ask at all, i.e. whether its
// Postgres and NATS are published on a resolved host port.
//
// THE RESOLVED PORT SET IS THE RIGHT SIGNAL because it IS the carrier: activeInstallPorts gives the host
// shape the service ports (they are published on 127.0.0.1 so a host plane can reach them) and gives the
// container-resident shape only the control port (its services are internal). Keying on the
// ports rather than on a residency string keeps the one place that decides which ports exist
// (install_ports.go's table) authoritative.
func servicesProbeableFromHost(ports map[string]int) bool {
	return ports["ORCHICON_POSTGRES_PORT"] > 0 && ports["ORCHICON_NATS_MONITOR_PORT"] > 0
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

// hostServicesPIDFile is where the host-services supervisor's PID is recorded, under the instance's own
// serve dir so it is per-instance like every other piece of instance state.
func hostServicesPIDFile(instance string) (string, error) {
	dir, err := hostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "serve", "services.pid"), nil
}

// hostServicesAlive reports whether the host-services supervisor this installer started is still running, from
// the PID file startHostServices records. A missing/unreadable/stale PID file is false, never an error: it is
// the same "is anything of ours up?" question servingPlaneAlive answers for the plane, and the two together
// are what let a re-run tell "this instance is running" from "this instance's records merely exist".
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
	"ORCHICON_TEMPO_OTLP_GRPC_PORT",
	"ORCHICON_TEMPO_OTLP_HTTP_PORT",
	"ORCHICON_OTEL_HEALTH_PORT",
}

// startHostServices starts the instance's SERVICES as HOST PROCESSES, detached, and records the PID.
//
// WHY A RE-EXEC AS `host-services`: host_services.go's supervisor ALREADY implements exactly this —
// runHostServices runs postgres, nats and the telemetry children as processes of the invoking user, with NO
// control-plane child and no container engine. Writing a second supervisor here would be a second
// implementation of the same thing, so this re-execs this binary.
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
