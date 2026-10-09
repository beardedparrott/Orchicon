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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// containerResidency reports the plane residency a container was CREATED for,
// read from its own environment.
//
// The services-only flag is CREATE-time state — `docker start` cannot rewrite it
// — so it is the authoritative record of an instance's shape: the supervisor
// reads it to decide whether to boot the plane inside the container or leave it
// to the host. A successful inspect with no flag means container residency,
// which is what an instance created before the host-residency migration is.
//
// The bool reports whether the shape was readable at all: the caller must NOT
// treat an unreadable shape as a mismatch, because recreating a live instance on
// a transient inspect failure is worse than leaving a wrong-shaped one visible.
func containerResidency(name string) (string, bool) {
	out, err := exec.Command("docker", "inspect",
		"--format", "{{range .Config.Env}}{{println .}}{{end}}", name).Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "ORCHICON_CONTAINER_SERVICES_ONLY=1" {
			return residencyHost, true
		}
	}
	return residencyContainer, true
}

// bridgeIP resolves the docker bridge address — the address a runtime container
// on the default bridge uses to reach the HOST.
//
// A HOST-RESIDENT plane must bind it, and a published host port cannot be used
// in its place: docker's hairpin NAT drops gateway→published-port traffic from
// bridge containers, which is exactly the plane-channel MCP timeout this bind
// removes.
//
// Resolution order mirrors container.sh: ORCHICON_DOCKER_BRIDGE_IP (tests and
// hand-pinning) → docker's own IPAM config → the docker0 interface address.
//
// An unresolvable bridge is a HARD error. There is deliberately no 0.0.0.0
// fallback — that is the LAN exposure the operator rejected — and no guessed
// address, because a wrong bind surfaces much later as "workers cannot reach the
// plane" rather than as a failure here.
func bridgeIP() (string, error) {
	if v := strings.TrimSpace(os.Getenv("ORCHICON_DOCKER_BRIDGE_IP")); v != "" {
		return v, nil
	}
	if out, err := exec.Command("docker", "network", "inspect", "bridge",
		"--format", "{{(index .IPAM.Config 0).Gateway}}").Output(); err == nil {
		ip := strings.TrimSpace(string(out))
		// The Go template prints "<no value>" for a nil gateway.
		if ip != "" && ip != "<no value>" && ip != "null" {
			return ip, nil
		}
	}
	// docker0 fallback through net.Interfaces rather than shelling out to `ip`:
	// no external binary, and it works wherever the interface is readable.
	if iface, err := net.InterfaceByName("docker0"); err == nil {
		if addrs, err := iface.Addrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
					return ipn.IP.String(), nil
				}
			}
		}
	}
	return "", fmt.Errorf("cannot resolve the docker bridge address (docker network inspect bridge / docker0) — " +
		"pin it with ORCHICON_DOCKER_BRIDGE_IP=<bridge gateway ip>; refusing to guess or bind 0.0.0.0")
}

// healthArgs returns the container healthcheck overrides for a residency.
//
// THE IMAGE'S HEALTHCHECK PROBES THE PLANE, and in host residency the plane is
// NOT in the container — nothing listens on :8080 there, so a perfectly healthy
// services-only container reports UNHEALTHY FOREVER. That is not hypothetical:
// it was observed on a real instance this installer created (FailingStreak 92,
// every probe exit 1) before the override was carried over.
//
// scripts/container.sh documents exactly this and applies the same override
// (its instance-scoped HEALTH_ARGS); the installer must agree with it, or an
// installed host-resident instance looks broken while it is actually working —
// which is worse than broken, because it teaches the operator to ignore the
// container's health.
//
// Container residency needs NO override: the plane runs inside the container, so
// the image's own :8080 probe is correct and is left to the image.
func healthArgs(residency string) []string {
	if residency != residencyHost {
		return nil
	}
	return []string{
		"--health-cmd", "pg_isready -h localhost -p 5432 -U orchicon && curl -fs http://localhost:8222/healthz",
		"--health-interval", "10s", "--health-timeout", "5s",
		"--health-start-period", "30s", "--health-retries", "20",
	}
}

// planeEnv builds the HOST plane's environment for an instance.
//
// EVERY value is derived from the RESOLVED ports, so no value can be global: a
// shared HTTP port or plane URL would silently point one instance's workers at
// the other's plane. The bind and the advertised URL are computed from the SAME
// bridge IP and the SAME port in ONE place, so what the plane listens on and
// what it hands its run containers can never disagree.
func planeEnv(instance string, ports map[string]int, hostDataDir, runtimeSocket string) ([]string, error) {
	ip, err := bridgeIP()
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
		// HOST-RESIDENT LISTENERS: the plane answers on loopback AND on the bridge
		// at THIS instance's port, and hands its run containers THAT address.
		fmt.Sprintf("ORCHICON_HTTP_EXTRA_BIND=%s:%d", ip, control),
		fmt.Sprintf("ORCHICON_PLANE_PUBLIC_URL=http://%s:%d", ip, control),
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
		// expectation (<DataDir>/secrets/kek) does not — see migrateHostDataDir.
		"ORCHICON_DATA_DIR=" + hostDataDir,
		"ORCHICON_BLOB_DIR=" + filepath.Join(hostDataDir, "blobs"),
		"ORCHICON_RUNTIME_SOCKET=" + runtimeSocket,
		// Per-instance PID/log file for serve --detach/--stop: two host planes
		// must never share one, or stopping dev would kill prod.
		"ORCHICON_SERVE_STATE_DIR=" + filepath.Join(hostDataDir, "serve"),
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

// migrateHostDataDir performs the ONE-TIME switch-over from a containerized
// plane's data volume to the host path.
//
// THE KEK LIVES AT <DataDir>/secrets/kek, so switching ORCHICON_DATA_DIR without
// carrying it over ORPHANS EVERY TENANT SECRET — the data is still there and
// simply stops decrypting. An existing host KEK is therefore never overwritten,
// and a failure here is fatal rather than a warning.
//
// The copy is ONE docker invocation (busybox `cp -a`) rather than the launcher's
// `docker run … tar cf - | tar xf -` pipeline: same result, no dependency on a
// host `tar`, and no pipe to lose a partial copy through.
func migrateHostDataDir(instance, volume, hostDataDir string, out io.Writer) error {
	kekPath := filepath.Join(hostDataDir, "secrets", "kek")
	if _, err := os.Stat(kekPath); err == nil {
		fmt.Fprintf(out, "host data dir already carries a KEK (%s) — leaving it untouched\n", hostDataDir)
		return nil
	}
	if err := exec.Command("docker", "volume", "inspect", volume).Run(); err != nil {
		fmt.Fprintf(out, "no existing data volume (%s) — starting with a fresh host data dir\n", volume)
		return nil
	}
	listing, _ := exec.Command("docker", "run", "--rm", "-v", volume+":/from", "alpine", "ls", "-A", "/from").Output()
	if strings.TrimSpace(string(listing)) == "" {
		fmt.Fprintf(out, "data volume %s is empty — nothing to migrate\n", volume)
		return nil
	}
	fmt.Fprintf(out, "first host switch-over for %s: copying volume %s to %s\n", instance, volume, hostDataDir)
	if err := os.MkdirAll(hostDataDir, 0o755); err != nil {
		return fmt.Errorf("create host data dir %s: %w", hostDataDir, err)
	}
	copyCmd := exec.Command("docker", "run", "--rm",
		"-v", volume+":/from", "-v", hostDataDir+":/to",
		"alpine", "cp", "-a", "/from/.", "/to/")
	if o, err := copyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy %s to %s: %v: %s", volume, hostDataDir, err, strings.TrimSpace(string(o)))
	}
	if _, err := os.Stat(kekPath); err == nil {
		fmt.Fprintln(out, "copied the instance data; KEK preserved, tenant secrets keep decrypting")
	} else {
		fmt.Fprintln(out, "warning: copied, but the volume has no secrets/kek — a NEW KEK will be created")
	}
	return nil
}

// waitServicesReady blocks until the container accepts postgres and nats
// connections, so the host plane never boots its migrations against a
// half-started backend.
//
// A timeout WARNS rather than failing, mirroring the launcher: a slow first boot
// should not abort an install that is otherwise complete, and the plane's own
// retries cover a backend that comes up late.
func waitServicesReady(name string, out io.Writer) {
	for i := 0; i < 60; i++ {
		pg := exec.Command("docker", "exec", name, "pg_isready", "-h", "localhost", "-p", "5432", "-U", "orchicon")
		nats := exec.Command("docker", "exec", name, "curl", "-fs", "http://localhost:8222/healthz")
		if pg.Run() == nil && nats.Run() == nil {
			fmt.Fprintf(out, "%s services ready (postgres 5432, nats monitor 8222)\n", name)
			return
		}
		time.Sleep(time.Second)
	}
	fmt.Fprintf(out, "warning: %s services were not confirmed ready within 60s — continuing\n", name)
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
	if httpHealthy(healthURL, 2*time.Second) {
		fmt.Fprintf(out, "host plane already serving %s\n", healthURL)
		return nil
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
