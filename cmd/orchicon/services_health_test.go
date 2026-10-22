package main

// services_health_test.go — the readiness answer must name EVERY component, and must keep the
// required/optional distinction the install now depends on.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// serveOK starts a test server answering 200 on every path and returns its port.
func serveOK(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return portOf(t, srv.URL)
}

// serveStatus answers a fixed status on every path — the honest way to model Loki's /ready, which
// answers 503 for as long as it is replaying its WAL.
func serveHTTPStatus(t *testing.T, code int) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return portOf(t, srv.URL)
}

func portOf(t *testing.T, url string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A DEAD TELEMETRY CHILD MUST NOT BE REPORTED AS "SERVICES UP".
//
// This is the operator's Windows failure in one test. All four telemetry children had crash-looped
// on a config they could not parse, and the install — which asked only about postgres and nats —
// reported "services ready" and went on to start the plane. The Logs and Traces views then reported
// their backends unreachable on an instance that had just been declared healthy.
//
// THE ASSERTION IS COMPONENT-NAMED, because "not ready" is what the old code would already have said
// if anything had asked. What has to be true now is that the missing component is NAMED.
func TestServicesHealthNamesEveryDeadComponent(t *testing.T) {
	// pg_isready is a stub that always succeeds; nats, tempo and victoriametrics answer 200.
	// LOKI and the COLLECTOR are pointed at closed ports, so exactly those two are down.
	t.Setenv("PATH", writeFakePgIsReady(t, 0))

	ports := map[string]int{
		"ORCHICON_POSTGRES_PORT":     5432,
		"ORCHICON_NATS_MONITOR_PORT": serveOK(t),
		"ORCHICON_TEMPO_PORT":        serveOK(t),
		"ORCHICON_LOKI_PORT":         closedPort(t),
		"ORCHICON_VM_PORT":           serveOK(t),
		"ORCHICON_OTEL_HEALTH_PORT":  closedPort(t),
	}
	t.Setenv(telemetryEnv, "embedded")

	health := servicesHealth("dev", ports)
	if len(health) != 6 {
		t.Fatalf("expected all six components to be asked, got %d: %+v", len(health), health)
	}
	down := map[string]bool{}
	requiredDown := map[string]bool{}
	for _, c := range health {
		if !c.up {
			down[c.name] = true
			if c.required {
				requiredDown[c.name] = true
			}
		}
	}
	if !down["loki"] || !down["otel-collector"] {
		t.Errorf("loki and otel-collector are unreachable, so both must be reported down; got %v", down)
	}
	if down["tempo"] || down["victoriametrics"] || down["nats"] {
		t.Errorf("a component answering 200 must not be reported down; got %v", down)
	}
	// THE SEVERITY SPLIT IS THE POINT OF THE REPORT, so it is asserted rather than assumed: two
	// optional components down makes the instance DEGRADED, not broken.
	if len(requiredDown) != 0 {
		t.Errorf("only optional components are down, so none may be REQUIRED-down; got %v", requiredDown)
	}
	if !requiredServicesReady("dev", ports) {
		t.Error("requiredServicesReady must be true when only optional components are down — a plane " +
			"with no logs is degraded, not broken")
	}
	if got := degradedServices("dev", ports); len(got) != 2 {
		t.Errorf("degradedServices must name both dead optional components, got %v", got)
	}
}

// A DEAD REQUIRED COMPONENT IS NOT A DEGRADATION — the instance is broken and the install must not
// proceed. This pins the other side of the severity split, so the split cannot be collapsed.
func TestServicesHealthRequiredDownFailsTheGate(t *testing.T) {
	ports := map[string]int{
		"ORCHICON_POSTGRES_PORT":     5432, // the fake pg_isready below will FAIL
		"ORCHICON_NATS_MONITOR_PORT": closedPort(t),
		"ORCHICON_TEMPO_PORT":        serveOK(t),
		"ORCHICON_LOKI_PORT":         serveOK(t),
		"ORCHICON_VM_PORT":           serveOK(t),
		"ORCHICON_OTEL_HEALTH_PORT":  serveOK(t),
	}
	t.Setenv(telemetryEnv, "embedded") // irrelevant: required wins
	t.Setenv("PATH", writeFakePgIsReady(t, 1))

	if requiredServicesReady("dev", ports) {
		t.Fatal("a dead postgres AND a dead nats monitor must fail requiredServicesReady")
	}
	// And the required failures are NOT reported as "degradations": they are faults.
	for _, name := range degradedServices("dev", ports) {
		if name == "postgres" || name == "nats" {
			t.Errorf("%s is REQUIRED and must never be listed as a degradation", name)
		}
	}
}

// LOKI'S /ready IS 503 WHILE IT REPLAYS ITS WAL, and that must read as NOT READY.
//
// Measured on the affected host: loki took 15.3s to answer 200 from a cold start, while its port was
// bound and its process was alive the whole time. A probe that accepted a bound socket would tell
// the install the log backend was ready and then hand the operator a Logs view that fails.
func TestLokiReplayingIsNotReady(t *testing.T) {
	t.Setenv(telemetryEnv, "embedded")
	t.Setenv("PATH", writeFakePgIsReady(t, 0))
	ports := map[string]int{
		"ORCHICON_POSTGRES_PORT":     5432,
		"ORCHICON_NATS_MONITOR_PORT": serveOK(t),
		"ORCHICON_TEMPO_PORT":        serveOK(t),
		"ORCHICON_LOKI_PORT":         serveHTTPStatus(t, http.StatusServiceUnavailable),
		"ORCHICON_VM_PORT":           serveOK(t),
		"ORCHICON_OTEL_HEALTH_PORT":  serveOK(t),
	}
	for _, c := range servicesHealth("dev", ports) {
		if c.name == "loki" && c.up {
			t.Error("loki answering 503 on /ready is still replaying its WAL and must read as down")
		}
	}
}

// TELEMETRY=none MUST NOT REPORT FOUR PERMANENTLY-MISSING COMPONENTS.
//
// A `none` boot is a supported shape (the plane degrades by design), so the optional set is empty
// and the instance is neither degraded nor faulted. Without this, `status` and the install's own
// degradation note would cry wolf on every headless instance.
func TestTelemetryNoneHasNoOptionalComponents(t *testing.T) {
	t.Setenv(telemetryEnv, "none")
	t.Setenv("PATH", writeFakePgIsReady(t, 0))
	ports := map[string]int{
		"ORCHICON_POSTGRES_PORT":     5432,
		"ORCHICON_NATS_MONITOR_PORT": serveOK(t),
		// Deliberately dead: a `none` boot must not ask about them at all.
		"ORCHICON_TEMPO_PORT":       closedPort(t),
		"ORCHICON_LOKI_PORT":        closedPort(t),
		"ORCHICON_VM_PORT":          closedPort(t),
		"ORCHICON_OTEL_HEALTH_PORT": closedPort(t),
	}
	health := servicesHealth("dev", ports)
	if len(health) != 2 {
		t.Fatalf("a telemetry=none boot asks about postgres and nats only, got %d: %+v", len(health), health)
	}
	if down := degradedServices("dev", ports); len(down) != 0 {
		t.Errorf("telemetry=none has no optional components to degrade, got %v", down)
	}
	if !requiredServicesReady("dev", ports) {
		t.Error("telemetry=none with postgres and nats up must read as ready")
	}
}

// THE FORWARDED-KNOB LIST MUST COVER EVERY KNOB THE SUPERVISOR READS.
//
// hostServicesPortEnvs is what startHostServices exports into the `host-services` child.
// servicePortsFromEnv is what that child reads. A knob in the second list and not the first is a
// silent disagreement between the process that PROBES a port and the process that BINDS it — the
// bug the whole port table exists to prevent, arriving through a list that looks maintained.
//
// This is asserted rather than trusted because it HAS drifted: ORCHICON_TEMPO_GRPC_PORT and
// ORCHICON_OTEL_SELF_METRICS_PORT were both honoured by the child and forwarded by nobody, which
// only stayed harmless because each child's own default happened to equal the table's value.
func TestHostServicesPortEnvsCoversEveryKnob(t *testing.T) {
	forwarded := map[string]bool{}
	for _, k := range hostServicesPortEnvs {
		forwarded[k] = true
	}
	// The knobs the supervisor honours, taken from the single place that decides: a value set to a
	// sentinel must show up as that value in the resolved table.
	for _, knob := range []string{
		"ORCHICON_POSTGRES_PORT", "ORCHICON_NATS_PORT", "ORCHICON_NATS_MONITOR_PORT",
		"ORCHICON_VM_PORT", "ORCHICON_TEMPO_PORT", "ORCHICON_TEMPO_GRPC_PORT",
		"ORCHICON_LOKI_PORT", "ORCHICON_OTLP_GRPC_PORT", "ORCHICON_OTLP_HTTP_PORT",
		"ORCHICON_TEMPO_OTLP_GRPC_PORT", "ORCHICON_TEMPO_OTLP_HTTP_PORT",
		"ORCHICON_OTEL_HEALTH_PORT", "ORCHICON_OTEL_SELF_METRICS_PORT",
	} {
		if !forwarded[knob] {
			t.Errorf("%s is honoured by servicePortsFromEnv but NOT forwarded by hostServicesPortEnvs "+
				"— the process that probes the port and the process that binds it can then disagree", knob)
		}
	}
}

// writeFakePgIsReady puts a fake `pg_isready` on PATH that exits with the given code, and returns
// the directory. pgIsReadyBin falls back to PATH when the instance has no binaries record, which is
// the case in a test with no install.
func writeFakePgIsReady(t *testing.T, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake-probe fixture is a shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pg_isready"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// closedPort returns a port with nothing listening on it: the honest way to model a child that is
// dead, rather than a port that merely refuses on some platforms and hangs on others.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// THE PORTS RECORD MUST BE COMPLETE, AND "COMPLETE" MEANS EVERY PORT THE INSTANCE BINDS.
//
// portsRecordIsComplete asked activeInstallPorts, which filters to the ports a residency EXPOSES on a
// host interface — the right question for "what may collide" and the wrong one for "is this record
// coherent". Five ports are bound but not published (Tempo's gRPC listener and its two OTLP
// receivers, the collector's health extension and its self-metrics exporter), so no REAL ports.json
// ever named them, the completeness test answered false for every correct install, and the record
// was therefore never trusted by anything that read it. The only caller fell back to the defaults —
// identical, so nothing broke — but a readiness probe asking the record for
// ORCHICON_OTEL_HEALTH_PORT got 0 and reported a healthy collector as "not answering".
func TestPortsRecordIsCompleteCoversUnpublishedPorts(t *testing.T) {
	// A record containing only the PUBLISHED ports — i.e. what every install wrote before this fix.
	publishedOnly := map[string]int{}
	for _, p := range activeInstallPorts(residencyHost) {
		publishedOnly[p.Env] = 1234
	}
	if portsRecordIsComplete(publishedOnly) {
		t.Error("a record naming only the PUBLISHED ports must not count as complete: the instance " +
			"also binds Tempo's gRPC/OTLP listeners and the collector's health + self-metrics ports")
	}

	// The full table must be accepted.
	full := map[string]int{}
	for _, key := range everyServicePortEnv() {
		full[key] = 1234
	}
	if !portsRecordIsComplete(full) {
		t.Error("a record naming every port in the table must count as complete")
	}

	// And the record the install now writes really does satisfy it: withUnpublishedPorts is what
	// closes the gap, so it is exercised rather than assumed.
	written := withUnpublishedPorts("dev", publishedOnly)
	for _, key := range everyServicePortEnv() {
		if written[key] <= 0 {
			t.Errorf("withUnpublishedPorts left %s unset — the written record would still be incomplete", key)
		}
	}
	if written["ORCHICON_OTEL_HEALTH_PORT"] <= 0 {
		t.Error("ORCHICON_OTEL_HEALTH_PORT is the port the collector's readiness is probed on and must " +
			"be recorded")
	}
	// The operator's answers win over defaults for the ports that were already there.
	for env, want := range publishedOnly {
		if got := written[env]; got != want {
			t.Errorf("%s = %d, want the resolved value %d — an explicit answer must not be overwritten "+
				"by a default", env, got, want)
		}
	}
}

// A MISSING PORT IN THE RECORD MUST NOT MANUFACTURE A NEGATIVE.
//
// This is the trap the fix above leaves behind if the probe is careless: a 0 port is the ABSENCE of
// an address, not a component listening on 0. Probing it can only ever fail, so a healthy collector
// would be reported down and a correct instance would look degraded for ever.
func TestServicesHealthSkipsComponentsWithNoRecordedPort(t *testing.T) {
	t.Setenv(telemetryEnv, "embedded")
	t.Setenv("PATH", writeFakePgIsReady(t, 0))
	ports := map[string]int{
		"ORCHICON_POSTGRES_PORT":     5432,
		"ORCHICON_NATS_MONITOR_PORT": serveOK(t),
		"ORCHICON_TEMPO_PORT":        serveOK(t),
		"ORCHICON_LOKI_PORT":         serveOK(t),
		"ORCHICON_VM_PORT":           serveOK(t),
		// ORCHICON_OTEL_HEALTH_PORT deliberately ABSENT — an older/incomplete record.
	}
	for _, c := range servicesHealth("dev", ports) {
		if c.name == "otel-collector" {
			t.Fatal("a component with no recorded port must not be asked at all: probing port 0 can " +
				"only fail, which reports a healthy collector as down")
		}
	}
	// And it must not read as a degradation either.
	for _, name := range degradedServices("dev", ports) {
		if name == "otel-collector" {
			t.Error("a component with no recorded port must not be listed as degraded")
		}
	}
}
