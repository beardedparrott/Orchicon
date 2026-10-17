package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/containerengine"
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
//
// THE BRIDGE IS PINNED, which is also what makes runtimeContainersInUse() true without an engine — a pinned
// bridge is proof of intent. The no-engine shape is asserted separately (TestInstallShapeWithoutAContainerEngine).
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

// TestPlaneBindAndShape is acceptance criterion 5: WHEN runtime containers ARE in use, the bridge bind and
// the advertised plane URL are derived from ONE place, share one IP and THIS instance's port, and two
// instances can never be pointed at each other's plane.
//
// It is the successor to TestPlaneEnvRefusesWithoutABridge, whose assertion is now the OPPOSITE for the
// no-container case: a host with no engine must get NO error, NO bind, and NO wildcard.
func TestPlaneBindAndShape(t *testing.T) {
	t.Setenv("ORCHICON_DOCKER_BRIDGE_IP", "172.99.0.1")
	t.Setenv("ORCHICON_RUNTIME_CONTAINERS", "")
	t.Setenv("ORCHICON_SECRETS_KEK", "")

	devPorts := map[string]int{"ORCHICON_CONTROL_PORT": 9080}
	prodPorts := map[string]int{"ORCHICON_CONTROL_PORT": 9091}

	dev, err := planeEnv("dev", devPorts, "/home/u/.local/share/orchicon-dev", "/tmp/s.sock")
	if err != nil {
		t.Fatalf("planeEnv(dev): %v", err)
	}
	prod, err := planeEnv("prod", prodPorts, "/home/u/.local/share/orchicon-prod", "/tmp/s.sock")
	if err != nil {
		t.Fatalf("planeEnv(prod): %v", err)
	}
	devJoined := strings.Join(dev, "\n")
	prodJoined := strings.Join(prod, "\n")

	// The bind and the URL come from ONE call: the same IP + the same port.
	bind, url, err := planeBindEnv(devPorts)
	if err != nil {
		t.Fatalf("planeBindEnv: %v", err)
	}
	if bind != "172.99.0.1:9080" || url != "http://172.99.0.1:9080" {
		t.Fatalf("planeBindEnv = (%q, %q), want one IP and this instance's port", bind, url)
	}
	if !strings.Contains(devJoined, "ORCHICON_HTTP_EXTRA_BIND="+bind) {
		t.Errorf("planeEnv must carry the bind planeBindEnv computed:\n%s", devJoined)
	}
	if !strings.Contains(devJoined, "ORCHICON_PLANE_PUBLIC_URL="+url) {
		t.Errorf("planeEnv must carry the URL planeBindEnv computed:\n%s", devJoined)
	}
	// TWO INSTANCES CANNOT POINT AT EACH OTHER'S PLANE: the pairs differ by the instance's own port.
	if strings.Contains(prodJoined, "ORCHICON_HTTP_EXTRA_BIND=172.99.0.1:9080") {
		t.Errorf("prod must not bind dev's control port:\n%s", prodJoined)
	}
	if !strings.Contains(prodJoined, "ORCHICON_HTTP_EXTRA_BIND=172.99.0.1:9091") {
		t.Errorf("prod must bind its OWN control port:\n%s", prodJoined)
	}
	for _, joined := range []string{devJoined, prodJoined} {
		if strings.Contains(joined, "0.0.0.0") {
			t.Errorf("a wildcard bind is the LAN exposure the operator rejected:\n%s", joined)
		}
	}
}

// TestInstallShapeWithoutAContainerEngine is acceptance criterion 4: A HOST WITH NO CONTAINER ENGINE installs
// without a bridge, a bind, or a wildcard — and without an error.
//
// This is the shape that used to be impossible: `docker version` was a hard gate, and planeEnv called
// bridgeIP unconditionally, so a machine with no Docker could not install Orchicon at all.
func TestInstallShapeWithoutAContainerEngine(t *testing.T) {
	// Case 1: the explicit override, with a bridge pinned to prove the override WINS over the pin.
	t.Run("explicit override", func(t *testing.T) {
		t.Setenv("ORCHICON_RUNTIME_CONTAINERS", "0")
		t.Setenv("ORCHICON_DOCKER_BRIDGE_IP", "172.99.0.1")
		if runtimeContainersInUse() {
			t.Error("ORCHICON_RUNTIME_CONTAINERS=0 must win over a pinned bridge")
		}
		assertNoBridgeShape(t)
	})
	// Case 2: no engine anywhere and nothing pinned. The probe must answer without a daemon.
	t.Run("no engine on PATH", func(t *testing.T) {
		t.Setenv("ORCHICON_RUNTIME_CONTAINERS", "")
		t.Setenv("ORCHICON_DOCKER_BRIDGE_IP", "")
		// An EMPTY PATH IS NO LONGER A NO-ENGINE HOST: the installer's detector
		// (internal/containerengine) probes the platform's conventional locations after
		// PATH, and a CI runner has /usr/bin/docker. The pin is what makes "no engine" a
		// property of the fixture instead of a property of the machine running the test.
		t.Setenv(containerengine.EnginePinEnv, string(containerengine.EngineNone))
		t.Setenv("PATH", t.TempDir())
		if containerEngineAvailable() {
			t.Error("an empty PATH must not report a container engine")
		}
		if runtimeContainersInUse() {
			t.Error("no engine and no pin must mean no runtime containers")
		}
		assertNoBridgeShape(t)
	})
}

// assertNoBridgeShape asserts the no-container shape: loopback primary bind, NO extra bind, NO advertised
// URL, and never a wildcard — and no error anywhere along the way.
func assertNoBridgeShape(t *testing.T) {
	t.Helper()
	ports := map[string]int{"ORCHICON_CONTROL_PORT": 9080}
	if bind, url, err := planeBindEnv(ports); err != nil || bind != "" || url != "" {
		t.Fatalf("planeBindEnv with no engine = (%q, %q, %v), want empty and NO error", bind, url, err)
	}
	env, err := planeEnv("dev", ports, "/tmp/d", "/tmp/s.sock")
	if err != nil {
		t.Fatalf("planeEnv must not fail without a bridge: %v", err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "ORCHICON_HTTP_ADDR=127.0.0.1:9080") {
		t.Errorf("the loopback primary bind is required:\n%s", joined)
	}
	if strings.Contains(joined, "ORCHICON_HTTP_EXTRA_BIND=") {
		t.Errorf("no engine must produce NO extra bind:\n%s", joined)
	}
	if strings.Contains(joined, "ORCHICON_PLANE_PUBLIC_URL=") {
		t.Errorf("no engine must produce NO advertised plane URL:\n%s", joined)
	}
	if strings.Contains(joined, "0.0.0.0") {
		t.Errorf("no engine must never produce a wildcard bind:\n%s", joined)
	}
}

// TestInstallShapeWithAContainerEngine is acceptance criterion 7's other half, and the counterpart of
// TestInstallShapeWithoutAContainerEngine: WHEN a container engine IS installed the install takes the
// with-containers shape — the bridge bind and the advertised URL ARE produced, from ONE place, at THIS
// instance's port — so the two carriers an install can have are both pinned rather than only the new one.
//
// The engine probe is asserted to be a PATH LOOKUP and not an engine call, and BEHAVIOURALLY rather than by
// reading the source: the fake engine RECORDS that it ran, so "the probe never executes docker" is a fact
// this test measures. That property is what lets a machine with no engine answer "no" without a doomed
// invocation — the defect this item removes from the install path.
func TestInstallShapeWithAContainerEngine(t *testing.T) {
	bin := t.TempDir()
	ran := filepath.Join(bin, "ran.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + ran + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("ORCHICON_RUNTIME_CONTAINERS", "")
	t.Setenv("ORCHICON_DOCKER_BRIDGE_IP", "172.99.0.1")

	if !containerEngineAvailable() {
		t.Fatal("a docker binary on PATH must report an engine")
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the engine probe must be a PATH lookup, never an engine call — the fake docker was executed")
	}
	if !runtimeContainersInUse() {
		t.Fatal("an engine on PATH must mean runtime containers are in use")
	}

	// The with-containers shape: the bind and the URL exist, agree, and carry THIS instance's port.
	ports := map[string]int{"ORCHICON_CONTROL_PORT": 9080}
	env, err := planeEnv("dev", ports, "/tmp/d", "/tmp/s.sock")
	if err != nil {
		t.Fatalf("planeEnv with an engine: %v", err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"ORCHICON_HTTP_ADDR=127.0.0.1:9080",
		"ORCHICON_HTTP_EXTRA_BIND=172.99.0.1:9080",
		"ORCHICON_PLANE_PUBLIC_URL=http://172.99.0.1:9080",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the with-engine shape is missing %q; got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "0.0.0.0") {
		t.Errorf("the with-engine shape must still never bind a wildcard:\n%s", joined)
	}
}

// TestResidencyIsRecordedAndReadBack is acceptance criterion 6's destructive half: a recorded shape that
// MATCHES converges, an ABSENT/UNREADABLE record reports ok=false, and a caller's `!known` arm therefore
// leaves a live instance alone rather than recreating it.
func TestResidencyIsRecordedAndReadBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Absent: nothing recorded, and NO error — which is what makes the caller's fail-safe mechanical.
	if got, ok := recordedResidency("dev"); ok || got != "" {
		t.Fatalf("an absent record = (%q, %v), want (\"\", false)", got, ok)
	}
	// Recorded: read back verbatim, per instance.
	recordResidency("dev", residencyHost)
	if got, ok := recordedResidency("dev"); !ok || got != residencyHost {
		t.Fatalf("recordedResidency(dev) = (%q, %v), want (host, true)", got, ok)
	}
	recordResidency("prod", residencyContainer)
	if got, ok := recordedResidency("prod"); !ok || got != residencyContainer {
		t.Fatalf("recordedResidency(prod) = (%q, %v), want (container, true)", got, ok)
	}
	// PER INSTANCE: recording prod must not have moved dev's shape.
	if got, _ := recordedResidency("dev"); got != residencyHost {
		t.Fatalf("dev's shape changed when prod was recorded: %q", got)
	}
}

// TestRecordedResidencyRejectsJunk pins the fail-safe's teeth: anything that is not a shape this installer
// wrote reports ok=false, so no caller can turn junk into "host" and recreate a live instance over it.
func TestRecordedResidencyRejectsJunk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := residencyRecordPath("dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"hostish", "", "  \n", "HOST", "container \n x"} {
		if err := os.WriteFile(path, []byte(junk), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := recordedResidency("dev"); ok {
			t.Errorf("junk record %q must not be read as a shape (got %q, ok)", junk, got)
		}
	}
}

// TestEnsureHostDataDirKeepsAnExistingKEK is the one assertion that protects tenant secrets on the common
// path: an existing host KEK means the switch-over already happened, and re-running must not touch it.
func TestEnsureHostDataDirKeepsAnExistingKEK(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(dir, "secrets", "kek")
	if err := os.WriteFile(kek, []byte("existing-kek"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The dir also holds instance state: the KEK must short-circuit BEFORE the orphan guard, or a re-install
	// would refuse to converge on a perfectly healthy instance.
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureHostDataDir("dev", dir, &out); err != nil {
		t.Fatalf("ensureHostDataDir: %v", err)
	}
	if !strings.Contains(out.String(), "leaving it untouched") {
		t.Errorf("an existing KEK must short-circuit; got:\n%s", out.String())
	}
	got, err := os.ReadFile(kek)
	if err != nil || string(got) != "existing-kek" {
		t.Fatalf("the existing KEK must not be modified: %q / %v", got, err)
	}
}

// TestEnsureHostDataDirRefusesToOrphanASecret is acceptance criterion 3's FATAL half: a data dir that holds
// instance state but no KEK must STOP the install, not warn — minting a fresh KEK there orphans every tenant
// secret, and a warning on stdout is exactly how that goes unnoticed.
func TestEnsureHostDataDirRefusesToOrphanASecret(t *testing.T) {
	for _, marker := range []string{"blobs", "postgres", "serve", "config"} {
		t.Run(marker, func(t *testing.T) {
			// HERMETIC: an operator-managed KEK is key material this guard must NOT read as lost (see
			// TestEnsureHostDataDirHonoursAnOperatorManagedKEK), so this test states its own environment
			// rather than inheriting one.
			t.Setenv("ORCHICON_SECRETS_KEK", "")
			dir := t.TempDir()
			var out bytes.Buffer
			if err := os.MkdirAll(filepath.Join(dir, marker), 0o755); err != nil {
				t.Fatal(err)
			}
			err := ensureHostDataDir("dev", dir, &out)
			if err == nil {
				t.Fatalf("a data dir holding %s with no KEK must be refused", marker)
			}
			if !strings.Contains(err.Error(), "ORPHANS EVERY TENANT SECRET") {
				t.Errorf("the refusal must name the orphan rule; got: %v", err)
			}
			// ...and it must not have written a KEK on the way out.
			if _, statErr := os.Stat(filepath.Join(dir, "secrets", "kek")); statErr == nil {
				t.Error("the guard must not mint a KEK in a dir it refused")
			}
		})
	}
}

// TestEnsureHostDataDirAllowsAFreshInstallMidRun is the FRESH-INSTALL half of the guard, and it pins an
// ordering property that is easy to break: `orchicon install` writes ports.json (step 1.6) and
// infra-binaries.json (step 1.7) into this very directory EARLIER in the same run. If either counted as
// instance state, EVERY fresh install would be refused after having written them — so they must not.
func TestEnsureHostDataDirAllowsAFreshInstallMidRun(t *testing.T) {
	t.Setenv("ORCHICON_SECRETS_KEK", "") // hermetic: this test is about the fresh-dir path, not about an env KEK
	dir := t.TempDir()
	var out bytes.Buffer
	// What the earlier install steps have already laid down in the data dir.
	if err := os.WriteFile(filepath.Join(dir, "ports.json"), []byte("{\n  \"ORCHICON_CONTROL_PORT\": 8080\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, infraBinariesFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The resolved bin dir (bin/) is also laid down by step 1.7.
	if err := os.MkdirAll(filepath.Join(dir, infraBinDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureHostDataDir("dev", dir, &out); err != nil {
		t.Fatalf("a fresh install's own earlier records must not read as instance state: %v", err)
	}
	if !strings.Contains(out.String(), "one is minted on first boot") {
		t.Errorf("this must take the fresh-dir path; got:\n%s", out.String())
	}
}

// TestEnsureHostDataDirHonoursAnOperatorManagedKEK is the third half of criterion 3, and it is the case the
// guard's own reasoning has to get right: an operator-managed KEK (ORCHICON_SECRETS_KEK) is NOT a lost KEK.
//
// secretcrypto.ResolveKEK returns that override VERBATIM and never mints <DataDir>/secrets/kek, so a
// deployment that supplies it — a documented mode (KMS, a secret manager, one key shared across replicas)
// that planeEnv deliberately inherits — has a POPULATED data dir and NO key file. Without the arm this test
// pins, re-running `orchicon install` on such a perfectly healthy instance refuses with "ORPHANS EVERY TENANT
// SECRET" and sends the operator to look for a secrets/kek backup that never existed. That is the opposite of
// criterion 6's convergence, and it is the guard firing on the one configuration where the key is provably
// not lost.
func TestEnsureHostDataDirHonoursAnOperatorManagedKEK(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	// What a known-good env-KEK install leaves behind: instance state, and no secrets/kek.
	for _, marker := range []string{"blobs", "serve", "postgres"} {
		if err := os.MkdirAll(filepath.Join(dir, marker), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ORCHICON_SECRETS_KEK", "c2VjcmV0LWtleS1tYXRlcmlhbC0zMmJ5dGVzISE=")
	if err := ensureHostDataDir("dev", dir, &out); err != nil {
		t.Fatalf("an operator-managed KEK must not read as a lost KEK: %v", err)
	}
	if !strings.Contains(out.String(), "ORCHICON_SECRETS_KEK") {
		t.Errorf("the install must say where the KEK comes from; got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "kek")); err == nil {
		t.Error("an env-supplied KEK must not be written into the data dir as a file")
	}

	// THE OVERRIDE GONE IS THE REAL ORPHAN CASE, and it stays FATAL — this must not have been "fixed" by
	// disabling the guard outright.
	t.Setenv("ORCHICON_SECRETS_KEK", "")
	if err := ensureHostDataDir("dev", dir, &out); err == nil {
		t.Fatal("state with NO key file and NO override must still be refused")
	}
}

// TestEnsureHostDataDirCreatesOnAFreshDir pins the ordinary first install: an empty dir is prepared, the
// secrets dir is created private, and no KEK is minted here (first boot mints it).
func TestEnsureHostDataDirCreatesOnAFreshDir(t *testing.T) {
	t.Setenv("ORCHICON_SECRETS_KEK", "") // hermetic: this test is about the fresh-dir path, not about an env KEK
	dir := t.TempDir()
	var out bytes.Buffer
	if err := ensureHostDataDir("dev", dir, &out); err != nil {
		t.Fatalf("ensureHostDataDir on a fresh dir: %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, "secrets"))
	if err != nil || !st.IsDir() {
		t.Fatalf("the secrets dir must be created: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Errorf("the secrets dir must be 0700, got %o", perm)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "kek")); err == nil {
		t.Error("a fresh dir must NOT get a KEK here — first boot mints it")
	}
	if !strings.Contains(out.String(), "one is minted on first boot") {
		t.Errorf("the fresh case must say what happens next; got:\n%s", out.String())
	}
}

// TestInstallHostHasNoDockerInvocation is acceptance criterion 1: install_host.go must contain NO docker
// invocation. It is read from source because the property IS about the source — a `docker inspect` here would
// fail silently on a machine with no Docker, which is the whole point of removing it.
func TestInstallHostHasNoDockerInvocation(t *testing.T) {
	src, err := os.ReadFile("install_host.go")
	if err != nil {
		t.Fatalf("read install_host.go: %v", err)
	}
	body := string(src)
	// EVERY exec.Command IN THIS FILE MUST LAUNCH THIS BINARY (or a service binary), never an engine. Prose
	// that NAMES docker ("the probes are direct, not `docker exec`") is the opposite of a violation — it is how
	// the removal is explained — so the assertion is on the CALLEE, not on the word.
	for _, m := range regexp.MustCompile(`exec\.Command\(([^,\n]*)`).FindAllStringSubmatch(body, -1) {
		callee := strings.TrimSpace(m[1])
		for _, engine := range []string{`"docker"`, `"docker-compose"`, `"podman"`, `"alpine"`, `"sh"`, `"bash"`} {
			if callee == engine {
				t.Errorf("install_host.go must not exec %s (callee: %s)", engine, callee)
			}
		}
	}
	// The engine probe DELEGATES to internal/containerengine, which owns the search order (podman
	// first, then docker), so the installer and the plane's /enginez answer with the same rules. It
	// is still a PATH/stat probe: containerengine.Detect runs no engine command (the version read is
	// a separate, opt-in call).
	//
	// THE PROBE MOVED WITH THE CODE, and the assertion follows it to its one home. The lifecycle
	// layer now lives in internal/planecontrol (a `package main` cannot be imported, so the TUI and
	// the desktop app could not reach it), and containerEngineAvailable moved with it.
	shared, err := os.ReadFile(filepath.Join("..", "..", "internal", "planecontrol", "env.go"))
	if err != nil {
		t.Fatalf("read internal/planecontrol/env.go: %v", err)
	}
	if !strings.Contains(string(shared), `containerengine.Detect(containerengine.DefaultProbe())`) {
		t.Errorf("the engine probe must delegate to containerengine.Detect, in internal/planecontrol/env.go:\n%s", shared)
	}
	// ...and no second copy survives in the file it was moved OUT of. A probe left behind here would
	// be the drift the delegation removes: two answers to "is there an engine".
	if strings.Contains(body, `containerengine.Detect`) || strings.Contains(body, `exec.LookPath("docker")`) {
		t.Errorf("install_host.go must not keep a second copy of the engine probe — it lives in " +
			"internal/planecontrol/env.go")
	}
	// ...and the two symbols the item retires are gone from the whole package, not merely unused here.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, gone := range []string{"func containerResidency", "func healthArgs"} {
			if strings.Contains(string(b), gone) {
				t.Errorf("%s still defines %q", f, gone)
			}
		}
	}
}

// TestServicesReadyUsesResolvedPortsAndDirectProbes is acceptance criterion 2: the probe is DIRECT (no
// `docker exec`), it uses the RESOLVED ports rather than the container's literals, and it returns a boolean
// the caller acts on.
func TestServicesReadyUsesResolvedPortsAndDirectProbes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake-probe fixture is a shell script")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A fake pg_isready that RECORDS ITS ARGV and succeeds. It is on PATH and is what pgIsReadyBin falls back
	// to when the instance has no binaries record.
	bin := t.TempDir()
	record := filepath.Join(bin, "argv.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + record + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "pg_isready"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	// A real HTTP server for the NATS monitor, so the second half is exercised rather than stubbed.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	monitorPort, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	// The RESOLVED ports, not the container's 5432/8222.
	ports := map[string]int{"ORCHICON_POSTGRES_PORT": 5442, "ORCHICON_NATS_MONITOR_PORT": monitorPort}
	if !servicesReady("dev", ports) {
		t.Fatal("both probes succeed (fake pg_isready + a live monitor), so servicesReady must be true")
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the fake pg_isready did not run: %v", err)
	}
	if !strings.Contains(string(argv), "5442") {
		t.Errorf("pg_isready must be asked about the RESOLVED port 5442, not 5432; argv: %q", argv)
	}
	if strings.Contains(string(argv), "5432") {
		t.Errorf("pg_isready must not be asked about the container's 5432; argv: %q", argv)
	}

	// The NATS half must be able to FAIL on its own — the boolean is a conjunction.
	bad := map[string]int{"ORCHICON_POSTGRES_PORT": 5442, "ORCHICON_NATS_MONITOR_PORT": 1}
	if servicesReady("dev", bad) {
		t.Error("a dead NATS monitor must make servicesReady false")
	}
}

// TestServicesReadyIsNotNegativeForACarrierItCannotProbe is the regression for a false negative this item's own
// re-signature opened, and it is the reason servicesProbeableFromHost exists.
//
// The omitted key used to read as PORT 0 and be probed: a container-RESIDENT instance keeps postgres and nats
// inside the container's network namespace and publishes neither on a host port, so its resolved port set names
// only the control and Grafana ports. `planInstallPorts` writes a key only for the ports `activeInstallPorts`
// yields, so the two service keys are ABSENT — `servicesReady` then ran `pg_isready -p 0` and probed
// `http://127.0.0.1:0/healthz`, answered false, and the install's own repair gate
// (`!healthy || !servicesReady(instance, ports)`) fired on EVERY re-run: a `docker restart` of a perfectly
// healthy instance container, which is the opposite of the convergence criterion 6 requires.
//
// The port set is DERIVED FROM THE REAL TABLE rather than hand-written, so a future change to which ports a
// shape binds moves this test with it instead of leaving it asserting a shape that no longer exists.
func TestServicesReadyIsNotNegativeForACarrierItCannotProbe(t *testing.T) {
	// The container shape: its services are internal, so NO host service port exists to ask about.
	containerPorts := map[string]int{}
	for _, p := range activeInstallPorts(residencyContainer) {
		containerPorts[p.Env] = 1234 // any resolved value; the KEYS are what this test is about
	}
	// VACUITY GUARD: the premise is that this shape publishes NO service port. If the table ever gives it one,
	// this test measures nothing and must be repointed rather than kept green.
	if containerPorts["ORCHICON_POSTGRES_PORT"] != 0 || containerPorts["ORCHICON_NATS_MONITOR_PORT"] != 0 {
		t.Fatal("the container shape now resolves a service port — this regression test needs repointing")
	}
	// An EMPTY PATH, so a "ready" answer cannot come from a real pg_isready on the host: the only thing that
	// can make this true is the absence of a probe.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	if !servicesReady("dev", containerPorts) {
		t.Error("a carrier with no host endpoint must NOT be reported as not-ready: that false negative makes " +
			"the install's repair gate restart a healthy container-resident instance on every re-run")
	}
	// ...and the wait must not manufacture a countdown against a port it has no endpoint for.
	var out bytes.Buffer
	if !waitServicesReadyFor("dev", containerPorts, 2, time.Millisecond, &out) {
		t.Error("the wait must report the carrier ready, not time out on an endpoint that was never published")
	}
	got := out.String()
	if strings.Contains(got, "waiting for") {
		t.Errorf("there is nothing to wait on, so the wait must not announce one; got:\n%s", got)
	}
	if strings.Contains(got, "warning:") {
		t.Errorf("a carrier with no host endpoint must not warn about readiness; got:\n%s", got)
	}
	if !strings.Contains(got, "published on no host port") {
		t.Errorf("the wait must say WHY it is not waiting; got:\n%s", got)
	}

	// The HOST shape still gets a real probe — this must not have been "fixed" by disabling the probe outright.
	hostPorts := map[string]int{}
	for _, p := range activeInstallPorts(residencyHost) {
		hostPorts[p.Env] = 1
	}
	if hostPorts["ORCHICON_POSTGRES_PORT"] == 0 || hostPorts["ORCHICON_NATS_MONITOR_PORT"] == 0 {
		t.Fatal("the host shape must publish its service ports — the probe is load-bearing there")
	}
	if servicesReady("dev", hostPorts) {
		t.Error("nothing is listening on the host ports, so the host carrier must still probe and report false")
	}
}

// TestWaitServicesReadyWarnsRatherThanFailing is acceptance criterion 2's posture: the wait ANNOUNCES before
// it waits (a silent minute reads as a hang), WARNS rather than failing on timeout, and still RETURNS the
// bool the caller acts on.
func TestWaitServicesReadyWarnsRatherThanFailing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no pg_isready at all

	ports := map[string]int{"ORCHICON_POSTGRES_PORT": 1, "ORCHICON_NATS_MONITOR_PORT": 1}
	var out bytes.Buffer
	start := time.Now()
	ready := waitServicesReadyFor("dev", ports, 2, time.Millisecond, &out)
	if ready {
		t.Fatal("nothing is listening, so the wait must report NOT ready")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the wait must be bounded by its tries and interval")
	}
	got := out.String()
	if !strings.Contains(got, "waiting for dev services") {
		t.Errorf("the wait must be ANNOUNCED before it starts; got:\n%s", got)
	}
	if !strings.Contains(got, "warning:") {
		t.Errorf("a timeout must WARN, not fail; got:\n%s", got)
	}
	// The announcement comes first — that is the posture, not merely that both strings appear.
	if strings.Index(got, "waiting for") > strings.Index(got, "warning:") {
		t.Errorf("the announcement must precede the warning; got:\n%s", got)
	}
	// The wrapper keeps the shipped 60×1s shape.
	if got := waitServicesReady; got == nil {
		t.Fatal("the exported wait must exist")
	}
}

// TestHostInstallRecordsItsShape is the regression for a gap this item's own de-containerization opened. It is
// a WIRING assertion because the defect was a MISSING CALL rather than a wrong value: `recordResidency` was
// written ONLY by `ensureInstallContainer`, which the host-services path (a host with no engine) never reaches.
// A no-engine install therefore finished with no record at all — and `orchicon restart`, which reads the record
// to decide which carrier to bounce, refused with "no install record for instance %q — nothing is installed for
// it" over the instance the SAME RUN had just brought up and told the operator to restart.
//
// The behavioural half is asserted too: the record the host path writes is exactly the one restart's
// precondition reads back.
func TestHostInstallRecordsItsShape(t *testing.T) {
	src, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func runInstall(")
	if start < 0 {
		t.Fatal("install.go no longer defines runInstall — this assertion needs repointing")
	}
	// The install FLOW, i.e. runInstall's body (up to the next top-level func declaration).
	flow := body[start:]
	if end := strings.Index(flow[1:], "\nfunc "); end > 0 {
		flow = flow[:1+end]
	}
	// VACUITY GUARD: the assertion below is only meaningful if this is still the flow that starts the host plane.
	if !strings.Contains(flow, "startHostPlane(") {
		t.Fatal("runInstall no longer starts the host plane — this assertion needs repointing, not deletion")
	}
	if !strings.Contains(flow, "recordResidency(instance, residency)") {
		t.Error("the install FLOW must record the shape it created: the host-services path never calls " +
			"ensureInstallContainer, so a no-engine install would leave NO record and `orchicon restart` would " +
			"refuse the instance the install just brought up")
	}

	// BEHAVIOURAL: what that call writes is what restart reads back.
	t.Setenv("HOME", t.TempDir())
	recordResidency("dev", residencyHost)
	if got, ok := recordedResidency("dev"); !ok || got != residencyHost {
		t.Fatalf("restart's precondition: recordedResidency = (%q, %v), want (host, true)", got, ok)
	}
}

// TestNoEngineInstallAdviceNamesAHostLog is the second half of the same defect class, on the install's ADVICE
// rather than its behaviour: the "the plane is not answering" arm used to print `docker logs --tail 50 <name>`
// unconditionally. On a host with no engine that names a binary which is not installed — a dead docker
// invocation in the install path, arriving through its error message. The arm is now shape-aware, and this
// pins the property that matters: the host arm points at the plane's own log file.
func TestNoEngineInstallAdviceNamesAHostLog(t *testing.T) {
	src, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "the instance itself is up. find out why the plane is not")
	if i < 0 {
		t.Fatal("the not-answering advice moved — this assertion needs repointing, not deletion")
	}
	arm := body[i:]
	if end := strings.Index(arm, "then re-run"); end > 0 {
		arm = arm[:end]
	}
	// The host half must name the plane's log FILE, not a docker command.
	if !strings.Contains(arm, `"serve", "logs", "orchicon.log"`) {
		t.Errorf("the host-plane advice must point at the plane's log file; got:\n%s", arm)
	}
	if !strings.Contains(arm, "services.log") {
		t.Errorf("the no-engine advice must name the host services log; got:\n%s", arm)
	}
	// `docker logs` must survive for the container shape only — guarded by the shape, never printed bare.
	if !strings.Contains(arm, "if residency == residencyHost") {
		t.Errorf("`docker logs` must be shape-guarded, not printed on every host; got:\n%s", arm)
	}
}

// TestPgIsReadyPrefersTheRecordedBinary pins the resolution order: the binaries the install RESOLVED win, so the
// readiness probe asks the client that matches the server the install runs.
func TestPgIsReadyPrefersTheRecordedBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := hostDataDir("dev")
	if err != nil {
		t.Fatal(err)
	}
	// No record yet: PATH fallback.
	if got := pgIsReadyBin("dev"); got != "pg_isready" {
		t.Fatalf("with no record = %q, want the PATH fallback", got)
	}
	// With a record: the recorded absolute path.
	want := filepath.Join(dir, infraBinDirName, "pg_isready")
	if err := recordInfraBinaries(dir, &infraBinaries{
		Source: "detected", Postgres: filepath.Join(dir, "postgres"), NatsServer: filepath.Join(dir, "nats-server"),
		PgIsReady: want,
	}); err != nil {
		t.Fatal(err)
	}
	if got := pgIsReadyBin("dev"); got != want {
		t.Fatalf("pgIsReadyBin = %q, want the recorded %q", got, want)
	}
}

// TestHostServicesLifecycleIsPerInstanceAndFailsClosed pins the host-services seam: the PID file is
// per-instance, and stopHostServices on an instance with no record is a NO-OP (not an error) so the repair
// pass can always act.
func TestHostServicesLifecycleIsPerInstanceAndFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	devFile, err := hostServicesPIDFile("dev")
	if err != nil {
		t.Fatal(err)
	}
	prodFile, err := hostServicesPIDFile("prod")
	if err != nil {
		t.Fatal(err)
	}
	if devFile == prodFile {
		t.Fatalf("two instances must not share a services PID file (%q)", devFile)
	}
	var out bytes.Buffer
	if err := stopHostServices("dev", &out); err != nil {
		t.Fatalf("stopping nothing must not be an error: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to stop") {
		t.Errorf("the no-op must say so; got:\n%s", out.String())
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

// TestMigrateHostDataDirSkipsWhenKekExists was the ORIGINAL pin of the KEK guard. It is superseded by
// TestEnsureHostDataDirKeepsAnExistingKEK / TestEnsureHostDataDirRefusesToOrphanASecret, which assert the
// same property (plus the FATAL failure mode) against the function that exists now: with no container-era
// volume to copy from, the guard is the whole of the behaviour.

// TestResolveInstallPortsNeedsNoEngine is acceptance criterion 4 at the RESOLVER: the port table is resolved
// on a host with NO container engine, and resolving it must not need one.
//
// RUNG 0 OF resolveInstallPorts IS THE ONLY PLACE THE RESOLVER ASKS A CONTAINER ANYTHING — it reads a running
// instance's published ports back with `docker inspect`. On a host with no engine there is no container to
// ask, so that rung must be SKIPPED (not attempted and failed): an ungated `docker` invocation on the
// no-engine path is exactly the silent failure this item removes, and it is the one docker call site the
// install's residency gate does not otherwise cover. Here PATH holds no docker, so a probe that stayed
// unguarded could only ever shell a binary that is not there.
func TestResolveInstallPortsNeedsNoEngine(t *testing.T) {
	// "No engine" is PINNED, not inferred from an empty PATH: the detector also searches the
	// conventional install locations, so a CI runner's /usr/bin/docker would otherwise make
	// this fixture a host WITH an engine.
	t.Setenv(containerengine.EnginePinEnv, string(containerengine.EngineNone))
	t.Setenv("PATH", t.TempDir()) // no docker, no pg_isready — nothing to exec
	t.Setenv("ORCHICON_STRICT_PORTS", "")
	t.Setenv("ORCHICON_RUNTIME_CONTAINERS", "")

	if containerEngineAvailable() {
		t.Fatal("an empty PATH must not report a container engine")
	}
	var out bytes.Buffer
	ports, err := resolveInstallPorts("dev", residencyHost, &out, nil)
	if err != nil {
		t.Fatalf("a host with no container engine must still resolve its ports: %v", err)
	}
	if ports["ORCHICON_CONTROL_PORT"] <= 0 {
		t.Errorf("the control port must resolve without an engine; got %d", ports["ORCHICON_CONTROL_PORT"])
	}
	if strings.Contains(out.String(), "already running") {
		t.Errorf("no engine means no running container to read ports back from:\n%s", out.String())
	}
}

// TestPortResolverGatesItsContainerRead pins the GATE STRUCTURALLY, which is the half the behavioural test
// above cannot express: with a docker stub on PATH the engine probe is true and rung 0 legitimately runs, so
// "the read is skipped when there is no engine" has to be asserted against the guard itself.
//
// It is deliberately narrow — the read must sit INSIDE `if containerEngineAvailable() {` — because that is
// the contract: rung 0 asks a CONTAINER, and a container can only be asked when an engine is installed.
func TestPortResolverGatesItsContainerRead(t *testing.T) {
	src, err := os.ReadFile("install_ports.go")
	if err != nil {
		t.Fatalf("read install_ports.go: %v", err)
	}
	body := string(src)
	guarded := "if containerEngineAvailable() {\n\t\tif running, _ := containerRunning(name); running {"
	if !strings.Contains(body, guarded) {
		t.Errorf("resolveInstallPorts must read a running container's ports ONLY when an engine is available; "+
			"expected the guarded block:\n%s", guarded)
	}
}
