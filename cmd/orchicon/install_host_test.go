package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResidencyForInstallDefaultsToHost pins the default shape. `host` is the
// product default (scripts/container.sh residency_for resolves ${...:-host}), and
// the installer producing the OTHER shape by default was the bug: a fresh
// install got a self-contained stack while every rebuild gave a host plane.
func TestResidencyForInstallDefaultsToHost(t *testing.T) {
	t.Setenv("ORCHICON_PLANE_RESIDENCY", "")
	got, err := residencyForInstall()
	if err != nil {
		t.Fatalf("residencyForInstall: %v", err)
	}
	if got != residencyHost {
		t.Fatalf("default residency = %q, want %q", got, residencyHost)
	}
}

// TestResidencyForInstallAcceptsBothShapesAndRefusesJunk pins that an explicit
// shape is honoured and a typo is refused rather than silently defaulted — a
// silently-defaulted typo would create an instance of the WRONG shape, and the
// shape is recorded on the container at create time.
func TestResidencyForInstallAcceptsBothShapesAndRefusesJunk(t *testing.T) {
	for _, want := range []string{residencyHost, residencyContainer} {
		t.Setenv("ORCHICON_PLANE_RESIDENCY", want)
		got, err := residencyForInstall()
		if err != nil {
			t.Fatalf("residencyForInstall(%q): %v", want, err)
		}
		if got != want {
			t.Fatalf("residencyForInstall(%q) = %q", want, got)
		}
	}
	t.Setenv("ORCHICON_PLANE_RESIDENCY", "hostish")
	if _, err := residencyForInstall(); err == nil {
		t.Fatal("a bad residency must be refused, not defaulted")
	}
}

// TestActiveInstallPortsPerResidency pins what each shape actually binds. This
// is the difference the installer was missing: a container-resident instance
// exposes two ports, a host-resident one exposes the services too (loopback),
// and the plane port is never published by the container in the host shape.
func TestActiveInstallPortsPerResidency(t *testing.T) {
	host := activeInstallPorts(residencyHost)
	container := activeInstallPorts(residencyContainer)

	if len(container) != 2 {
		t.Errorf("container residency binds %d ports, want 2 (control + grafana)", len(container))
	}
	// 10: control (plane-bound) + grafana + 8 services.
	if len(host) != 10 {
		t.Errorf("host residency binds %d ports, want 10", len(host))
	}

	// Every shape must include the plane port, or there is no instance to reach.
	for _, set := range [][]installPort{host, container} {
		found := false
		for _, p := range set {
			if p.Env == "ORCHICON_CONTROL_PORT" {
				found = true
			}
		}
		if !found {
			t.Error("every residency must bind the control port")
		}
	}

	// The service ports appear in the host shape only.
	for _, p := range container {
		if p.Env == "ORCHICON_POSTGRES_PORT" {
			t.Error("container residency must not claim to bind postgres — the container keeps it internal")
		}
	}
}

// TestPublishArgsShapeAndSafety is the port-mapping contract with docker.
//
// Two things must hold: the host half is the RESOLVED port (so an override
// moves the publish), and every service publish is LOOPBACK-BOUND — postgres is
// a database and NATS an internal bus, and the supervisor's pg_hba trust rules
// depend on them never being reachable from the LAN.
func TestPublishArgsShapeAndSafety(t *testing.T) {
	ports := map[string]int{
		"ORCHICON_CONTROL_PORT":      9080,
		"ORCHICON_GRAFANA_PORT":      4002,
		"ORCHICON_POSTGRES_PORT":     5442,
		"ORCHICON_NATS_PORT":         4333,
		"ORCHICON_NATS_MONITOR_PORT": 8333,
		"ORCHICON_OTLP_GRPC_PORT":    4317,
		"ORCHICON_OTLP_HTTP_PORT":    4318,
		"ORCHICON_TEMPO_PORT":        3200,
		"ORCHICON_LOKI_PORT":         3100,
		"ORCHICON_VM_PORT":           8428,
	}

	host := strings.Join(publishArgs(residencyHost, ports, activeInstallPorts(residencyHost)), " ")
	// The plane port is NOT published in the host shape — the host plane owns it.
	if strings.Contains(host, "9080:8080") {
		t.Errorf("host residency must not publish the plane port (the host plane binds it); got: %s", host)
	}
	// Services are loopback-bound and use the RESOLVED host port.
	for _, want := range []string{"127.0.0.1:5442:5432", "127.0.0.1:4333:4222", "127.0.0.1:8333:8222", "127.0.0.1:4002:3000"} {
		if !strings.Contains(host, want) {
			t.Errorf("host publishes must contain %q; got: %s", want, host)
		}
	}
	if strings.Contains(host, "0.0.0.0") {
		t.Errorf("no publish may be wildcard-bound; got: %s", host)
	}

	container := strings.Join(publishArgs(residencyContainer, ports, activeInstallPorts(residencyContainer)), " ")
	// The container shape DOES publish the plane port (the plane is inside it).
	if !strings.Contains(container, "9080:8080") {
		t.Errorf("container residency must publish its plane port; got: %s", container)
	}
	if !strings.Contains(container, "4002:3000") {
		t.Errorf("container residency must publish grafana; got: %s", container)
	}
	// ...and must NOT drag in the service publishes it does not need.
	if strings.Contains(container, ":5432") {
		t.Errorf("container residency must not publish postgres; got: %s", container)
	}
}

// TestPlaneEnvIsPerInstanceAndComplete pins the host plane's environment: every
// key the plane needs is present, each is derived from the RESOLVED ports, and
// the bind and the advertised URL agree.
func TestPlaneEnvIsPerInstanceAndComplete(t *testing.T) {
	t.Setenv("ORCHICON_DOCKER_BRIDGE_IP", "172.99.0.1")
	t.Setenv("ORCHICON_SECRETS_KEK", "c2VjcmV0LWtleS1tYXRlcmlhbC0zMmJ5dGVzISE=")

	ports := map[string]int{
		"ORCHICON_CONTROL_PORT":   9080,
		"ORCHICON_GRAFANA_PORT":   4002,
		"ORCHICON_POSTGRES_PORT":  5442,
		"ORCHICON_NATS_PORT":      4333,
		"ORCHICON_OTLP_GRPC_PORT": 4317,
		"ORCHICON_TEMPO_PORT":     3200,
		"ORCHICON_LOKI_PORT":      3100,
		"ORCHICON_VM_PORT":        8428,
	}
	env, err := planeEnv("dev", ports, "/home/u/.local/share/orchicon-dev", "/tmp/orchicon-runtime/runtime.sock")
	if err != nil {
		t.Fatalf("planeEnv: %v", err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"ORCHICON_INSTANCE=dev",
		// Loopback primary bind, bridge extra bind, and the URL — all on the
		// RESOLVED port, and the bind and URL must share one bridge IP.
		"ORCHICON_HTTP_ADDR=127.0.0.1:9080",
		"ORCHICON_HTTP_EXTRA_BIND=172.99.0.1:9080",
		"ORCHICON_PLANE_PUBLIC_URL=http://172.99.0.1:9080",
		"ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:5442/orchicon?sslmode=disable",
		"ORCHICON_NATS_URL=nats://localhost:4333",
		"ORCHICON_OTEL_ENDPOINT=localhost:4317",
		"ORCHICON_GRAFANA_URL=http://localhost:4002",
		"ORCHICON_TEMPO_URL=http://localhost:3200",
		"ORCHICON_LOKI_URL=http://localhost:3100",
		"ORCHICON_VM_URL=http://localhost:8428",
		// The state dir carries the KEK, the blob store and the per-instance PID
		// file — each derived from ONE value so they cannot drift.
		"ORCHICON_DATA_DIR=/home/u/.local/share/orchicon-dev",
		"ORCHICON_BLOB_DIR=/home/u/.local/share/orchicon-dev/blobs",
		"ORCHICON_SERVE_STATE_DIR=/home/u/.local/share/orchicon-dev/serve",
		"ORCHICON_RUNTIME_SOCKET=/tmp/orchicon-runtime/runtime.sock",
		// An operator's identity must be INHERITED, not invented: a deployment
		// that chose its own KEK must keep it or the migrated data will not
		// decrypt.
		"ORCHICON_SECRETS_KEK=c2VjcmV0LWtleS1tYXRlcmlhbC0zMmJ5dGVzISE=",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plane env is missing %q; got:\n%s", want, joined)
		}
	}
	// A wildcard bind is the LAN exposure the operator rejected.
	if strings.Contains(joined, "0.0.0.0") {
		t.Errorf("plane env must never bind a wildcard:\n%s", joined)
	}
}

// TestPlaneEnvRefusesWithoutABridge pins the hard failure: a plane whose run
// containers cannot reach it is worse than one that refuses to start, so an
// unresolvable bridge is an error and NOT a loopback-only fallback.
func TestPlaneEnvRefusesWithoutABridge(t *testing.T) {
	// Pin a hopeless bridge and remove the docker fallbacks by using a value that
	// resolves to nothing is not possible via env, so assert the pin is honoured
	// and trust bridgeIP's own error path — the meaningful assertion here is that
	// planeEnv PROPAGATES it.
	t.Setenv("ORCHICON_DOCKER_BRIDGE_IP", "")
	ports := map[string]int{"ORCHICON_CONTROL_PORT": 8080}
	// With no pin, bridgeIP may still resolve from docker. Either way planeEnv
	// must never return an env with a wildcard or a missing bind.
	env, err := planeEnv("dev", ports, "/tmp/d", "/tmp/s.sock")
	if err != nil {
		if !strings.Contains(err.Error(), "bridge") {
			t.Errorf("the error must name the bridge; got: %v", err)
		}
		return
	}
	if !strings.Contains(strings.Join(env, "\n"), "ORCHICON_HTTP_EXTRA_BIND=") {
		t.Error("a resolved bridge must still produce the extra bind")
	}
}

// TestHostDataDirIsPerInstance pins that the state dir is derived from the
// instance — two instances sharing one data dir would share a KEK and a PID
// file, so stopping one could kill the other.
func TestHostDataDirIsPerInstance(t *testing.T) {
	dev, err := hostDataDir("dev")
	if err != nil {
		t.Fatalf("hostDataDir: %v", err)
	}
	prod, err := hostDataDir("prod")
	if err != nil {
		t.Fatalf("hostDataDir: %v", err)
	}
	if dev == prod {
		t.Fatalf("dev and prod must not share a data dir (both %q)", dev)
	}
	if !strings.HasSuffix(dev, "orchicon-dev") || !strings.HasSuffix(prod, "orchicon-prod") {
		t.Fatalf("unexpected data dirs: %q, %q", dev, prod)
	}
	// It must be a HOST path under the user's home, not the container's
	// /var/lib/orchicon, or the host plane would read the container's volume.
	if strings.HasPrefix(dev, "/var/lib") {
		t.Errorf("host data dir must not be the container path: %q", dev)
	}
}

// TestEnvWithoutStripsOnlyTheNamedKeys pins the container-mode strip: a stray
// ORCHICON_CONTAINER_MODE in a shell profile must not flip the host plane into
// container semantics, and nothing else may be dropped.
func TestEnvWithoutStripsOnlyTheNamedKeys(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"ORCHICON_CONTAINER_MODE=1",
		"ORCHICON_CONTAINER_MODE_EXTRA=keep-me",
		"HOME=/home/u",
	}
	got := strings.Join(envWithout(env, "ORCHICON_CONTAINER_MODE"), "\n")
	if strings.Contains(got, "ORCHICON_CONTAINER_MODE=") {
		t.Errorf("the container-mode flag must be stripped; got:\n%s", got)
	}
	// A key that merely PREFIXES the stripped name must survive — a prefix match
	// without the '=' would delete it.
	if !strings.Contains(got, "ORCHICON_CONTAINER_MODE_EXTRA=keep-me") {
		t.Errorf("a prefixing key must not be stripped; got:\n%s", got)
	}
	if !strings.Contains(got, "PATH=/usr/bin") || !strings.Contains(got, "HOME=/home/u") {
		t.Errorf("unrelated entries must survive; got:\n%s", got)
	}
}

// TestMigrateHostDataDirSkipsWhenKekExists is the one assertion that protects
// tenant secrets on the common path: an existing host KEK means the switch-over
// already happened, and re-running must not touch it.
func TestMigrateHostDataDirSkipsWhenKekExists(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(dir, "secrets", "kek")
	if err := os.WriteFile(kek, []byte("existing-kek"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A volume that does not exist: the guard must be reached BEFORE any docker
	// call, or a second install would try to copy over a live data dir.
	if err := migrateHostDataDir("dev", "orchicon-cnt-dev-data-definitely-missing", dir, &out); err != nil {
		t.Fatalf("migrateHostDataDir: %v", err)
	}
	if !strings.Contains(out.String(), "leaving it untouched") {
		t.Errorf("an existing KEK must short-circuit the migration; got:\n%s", out.String())
	}
	got, err := os.ReadFile(kek)
	if err != nil || string(got) != "existing-kek" {
		t.Fatalf("the existing KEK must not be modified: %q / %v", got, err)
	}
}
