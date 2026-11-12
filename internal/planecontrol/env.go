package planecontrol

// env.go — the HOST plane's environment and the health probe: the two things a lifecycle call needs
// and that must never be computed twice.

import (
	"context"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/containerengine"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// runtimeContainersEnv is the explicit override for whether runtime containers are in use. Unset means
// "decide from the host" (see runtimeContainersInUse), which is what a plain install needs.
const runtimeContainersEnv = "ORCHICON_RUNTIME_CONTAINERS"

// The daemon check happens later, in the installer, and ONLY when this returns true — so a host
// with no engine never runs a doomed `version`.
func ContainerEngineAvailable() bool {
	e, err := containerengine.Detect(containerengine.DefaultProbe())
	return err == nil && e.Name != containerengine.EngineNone
}

// engineBinary is the engine binary to invoke for the few calls this package makes itself (the
// bridge lookup).
func engineBinary() string {
	if e, err := containerengine.Detect(containerengine.DefaultProbe()); err == nil && e.Bin != "" {
		return e.Bin
	}
	return "docker"
}

// RuntimeContainersInUse reports whether this install should assume runtime containers.
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
	// THE LAST RUNG IS THE PROBLEM, AND IT IS WHY A LOCAL INSTALL DEMANDED A DOCKER BRIDGE. An engine
	// that CANNOT RUN A CONTAINER IS NOT AN ENGINE IN USE.
	return engineDaemonAnswers()
}

// PlaneBindEnv is the ONE place the bridge bind and the advertised plane URL are computed.
func PlaneBindEnv(ports map[string]int) (bind, url string, err error) {
	if !RuntimeContainersInUse() {
		return "", "", nil
	}
	control := ports["ORCHICON_CONTROL_PORT"]
	// AN UNRESOLVABLE BRIDGE IS NO LONGER FATAL, AND MUST NOT BE.
	ip, iperr := BridgeIP()
	if iperr != nil {
		return "", fmt.Sprintf("http://host.docker.internal:%d", control), nil
	}
	return fmt.Sprintf("%s:%d", ip, control), fmt.Sprintf("http://%s:%d", ip, control), nil
}

// The bind and the advertised URL are computed from the SAME bridge IP and the SAME port in ONE
// place (PlaneBindEnv).
func PlaneEnv(instance string, ports map[string]int, hostDataDir, runtimeSocket string) ([]string, error) {
	_, url, err := PlaneBindEnv(ports)
	if err != nil {
		return nil, err
	}
	control := ports["ORCHICON_CONTROL_PORT"]
	env := []string{
		"ORCHICON_INSTANCE=" + instance,
		// THE PRIMARY BIND IS A WILDCARD, AND THAT IS THE POINT.
		fmt.Sprintf("ORCHICON_HTTP_ADDR=:%d", control),
		fmt.Sprintf("ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:%d/orchicon?sslmode=disable", ports["ORCHICON_POSTGRES_PORT"]),
		fmt.Sprintf("ORCHICON_NATS_URL=nats://localhost:%d", ports["ORCHICON_NATS_PORT"]),
		fmt.Sprintf("ORCHICON_OTEL_ENDPOINT=localhost:%d", ports["ORCHICON_OTLP_GRPC_PORT"]),
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
	// THE ADVERTISED URL ONLY — ORCHICON_HTTP_EXTRA_BIND IS GONE.
	if url != "" {
		env = append(env, "ORCHICON_PLANE_PUBLIC_URL="+url)
	}
	// Identity/keys the operator already set are INHERITED rather than invented — a deployment that
	// picked its own tenant or signing key must keep them.
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

// Health is the probe's FULL answer: did anything answer, and with what status.
type Health struct {
	// Reachable is true when a TCP connection was made and an HTTP response came back.
	Reachable bool
	// StatusCode is the HTTP status, 0 when nothing answered.
	StatusCode int
	// Err is the transport error, nil when a response came back.
	Err error
}

// Healthy reports whether the plane is reachable AND answers 200.
func (h Health) Healthy() bool { return h.Reachable && h.StatusCode == 200 }

// Probe is the ONE health probe.
func Probe(url string, timeout time.Duration) Health {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return Health{Reachable: false, Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return Health{Reachable: true, StatusCode: resp.StatusCode}
}

// Healthy reports whether a URL answers 200 within the timeout.
func Healthy(url string, timeout time.Duration) bool {
	return Probe(url, timeout).Healthy()
}

// BridgeIP resolves the docker bridge address — the address a runtime container on the default
// bridge uses to reach the HOST.
func BridgeIP() (string, error) {
	if v := strings.TrimSpace(os.Getenv("ORCHICON_DOCKER_BRIDGE_IP")); v != "" {
		return v, nil
	}
	if out, err := procwin.Command(engineBinary(), "network", "inspect", "bridge",
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

// engineDaemonAnswers reports whether the container engine's DAEMON is reachable — which is a
// different question from whether its binary exists, and is the one that matters here.
func engineDaemonAnswers() bool {
	engineProbeOnce.Do(func() {
		probe := containerengine.DefaultProbe()
		e, err := containerengine.Detect(probe)
		if err != nil || e.Name == containerengine.EngineNone || e.Bin == "" {
			engineProbeOK = false
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), engineProbeTimeout)
		defer cancel()
		_, err = probe.Run(ctx, e.Bin, "info")
		engineProbeOK = err == nil
	})
	return engineProbeOK
}

// engineProbeTimeout bounds the daemon check.
const engineProbeTimeout = 20 * time.Second

var (
	engineProbeOnce sync.Once
	engineProbeOK   bool
)
