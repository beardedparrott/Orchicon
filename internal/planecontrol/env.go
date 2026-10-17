package planecontrol

// env.go — the HOST plane's environment and the health probe: the two things a lifecycle call needs and that
// must never be computed twice. MOVED from cmd/orchicon/install_host.go and install_bridge.go, comments
// intact, because a second copy of "which port does this instance's plane bind" is how one instance's workers
// end up pointed at another's plane.

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

// runtimeContainersEnv is the explicit override for whether runtime containers are in use. Unset means
// "decide from the host" (see runtimeContainersInUse), which is what a plain install needs.
const runtimeContainersEnv = "ORCHICON_RUNTIME_CONTAINERS"

// ContainerEngineAvailable reports whether a container engine is INSTALLED, as a PATH probe.
//
// IT IS A LookPath AND NOT AN ENGINE CALL, on purpose: `docker version` needs a running daemon and can
// fail for a dozen reasons that have nothing to do with whether the binary exists, and a machine with no
// Docker must be able to answer "no" without an error. The daemon check happens later, in install.go, and
// ONLY when this returns true — so a host with no engine never runs a doomed `docker version`.
func ContainerEngineAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// RuntimeContainersInUse reports whether this install should assume runtime containers. Order:
//
//  1. ORCHICON_RUNTIME_CONTAINERS — an explicit answer (`0|off|no|false|none` = no, anything else = yes).
//  2. ORCHICON_DOCKER_BRIDGE_IP non-empty — a PINNED bridge is proof of intent: an operator who named the
//     bridge they want their containers to use is running containers.
//  3. otherwise, the engine probe.
//
// The middle rung is what keeps a pinned-bridge host working with no daemon reachable, and it is why the
// per-instance bind/URL test stays meaningful when the engine is absent.
func RuntimeContainersInUse() bool {
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
	return ContainerEngineAvailable()
}

// PlaneBindEnv is the ONE place the bridge bind and the advertised plane URL are computed.
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
func PlaneBindEnv(ports map[string]int) (bind, url string, err error) {
	if !RuntimeContainersInUse() {
		return "", "", nil
	}
	ip, err := BridgeIP()
	if err != nil {
		return "", "", err
	}
	control := ports["ORCHICON_CONTROL_PORT"]
	return fmt.Sprintf("%s:%d", ip, control), fmt.Sprintf("http://%s:%d", ip, control), nil
}

// PlaneEnv builds the HOST plane's environment for an instance.
//
// EVERY value is derived from the RESOLVED ports, so no value can be global: a
// shared HTTP port or plane URL would silently point one instance's workers at
// the other's plane. The bind and the advertised URL are computed from the SAME
// bridge IP and the SAME port in ONE place (PlaneBindEnv), so what the plane
// listens on and what it hands its run containers can never disagree — and the
// pair is present ONLY when runtime containers are in use, because a host with
// no container engine has no bridge to bind and needs none.
func PlaneEnv(instance string, ports map[string]int, hostDataDir, runtimeSocket string) ([]string, error) {
	bind, url, err := PlaneBindEnv(ports)
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
		ServeStateDirEnv + "=" + filepath.Join(hostDataDir, "serve"),
	}
	// ONLY WHEN RUNTIME CONTAINERS ARE IN USE. Absent is the correct state for a host with no container
	// engine: there is no bridge to bind, no run container to reach the plane, and inventing a wildcard
	// bind here is the LAN exposure the operator rejected. See PlaneBindEnv — both entries come from that
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

// EnvWithout returns env with every entry for the named keys removed.
func EnvWithout(env []string, keys ...string) []string {
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

// Healthy reports whether a URL answers 200 within the timeout.
func Healthy(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == 200
}

// BridgeIP resolves the docker bridge address — the address a runtime container
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
// An unresolvable bridge is a HARD error, and it is only ever reached when
// runtime containers ARE in use (PlaneBindEnv checks first). There is
// deliberately no 0.0.0.0 fallback — that is the LAN exposure the operator
// rejected — and no guessed address, because a wrong bind surfaces much later as
// "workers cannot reach the plane" rather than as a failure here.
func BridgeIP() (string, error) {
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
