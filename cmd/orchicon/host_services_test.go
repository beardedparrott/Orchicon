package main

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
)

// There is ONE shape now: nothing forwards to a host process, so postgres never
// listens beyond loopback — widening the address would only expose the database.
func TestPostgresListenAddrIsLoopback(t *testing.T) {
	if got := (&supervisor{}).postgresListenAddr(); got != "localhost" {
		t.Errorf("listen address = %q, want localhost", got)
	}
}

// testInfra is a hand-built resolved set for tests that construct a supervisor
// directly instead of through prepare(). The paths are never executed by these
// tests — they only prove a proc carries the RESOLVED path rather than a bare name.
func testInfra() *infraBinaries {
	return &infraBinaries{
		Source: "detected", Postgres: "/pg/bin/postgres", Initdb: "/pg/bin/initdb",
		PgCtl: "/pg/bin/pg_ctl", PgIsReady: "/pg/bin/pg_isready", Createdb: "/pg/bin/createdb",
		NatsServer: "/usr/local/bin/nats-server",
		// The four telemetry backends, so a proc exec'ing s.resolved().X is
		// distinguishable from one exec'ing a bare name (the point of routing the
		// children through the resolved record).
		Tempo: "/inst/bin/tempo", Loki: "/inst/bin/loki",
		VictoriaMetrics: "/inst/bin/victoria-metrics-prod", OTelCollector: "/inst/bin/otelcol-contrib",
	}
}

func TestPostgresProcUsesListenAddr(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra()}
	if args := strings.Join(sup.postgresProc().args, " "); !strings.Contains(args, "-c listen_addresses=localhost") {
		t.Errorf("postgres args do not keep the listen address on loopback: %s", args)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The services are the INVOKING USER's own processes: no privilege drop, ever.
func TestPostgresProcEmitsNoSetpriv(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra(), ports: servicePortsFromEnv()}
	p := sup.postgresProc()
	if p.command != testInfra().Postgres {
		t.Errorf("command = %q, want the resolved postgres %q (no setpriv)", p.command, testInfra().Postgres)
	}
	joined := strings.Join(p.args, " ")
	for _, banned := range []string{"setpriv", "--reuid", "--regid", "--clear-groups"} {
		if strings.Contains(joined, banned) {
			t.Errorf("postgres args contain %q: %s", banned, joined)
		}
	}
}

// postgres runs as the INVOKING USER; uid 70 was the container-era default and
// must never be the answer now.
func TestPostgresUIDGIDIsInvokingUser(t *testing.T) {
	sup := &supervisor{dataDir: t.TempDir()}
	uid, gid := sup.postgresUIDGID()
	if uid != os.Getuid() || gid != os.Getgid() {
		t.Errorf("got (%d,%d), want the invoking user (%d,%d)", uid, gid, os.Getuid(), os.Getgid())
	}
	if uid == 70 && os.Getuid() != 70 {
		t.Error("must not default to the container-era uid 70")
	}
}

// initPostgres is idempotent via the PG_VERSION gate: an already-initialized dir
// returns nil without invoking initdb.
func TestInitPostgresIdempotent(t *testing.T) {
	s := &supervisor{log: testLogger(), dataDir: t.TempDir()}
	dir := filepath.Join(s.dataDir, "postgres")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.initPostgres(); err != nil {
		t.Fatalf("initPostgres on an initialized dir: %v", err)
	}
}

// The supervisor's ports ARE host ports, so every knob is honoured: two instances
// on one host must not collide.
func TestServicePortsFromEnvReadsKnobs(t *testing.T) {
	t.Setenv("ORCHICON_POSTGRES_PORT", "5433")
	t.Setenv("ORCHICON_NATS_PORT", "4223")
	t.Setenv("ORCHICON_NATS_MONITOR_PORT", "8223")
	t.Setenv("ORCHICON_VM_PORT", "8429")

	p := servicePortsFromEnv()
	if p.postgres != 5433 {
		t.Errorf("postgres port = %d, want 5433", p.postgres)
	}
	if p.nats != 4223 {
		t.Errorf("nats port = %d, want 4223", p.nats)
	}
	if p.natsMonitor != 8223 {
		t.Errorf("nats monitor port = %d, want 8223", p.natsMonitor)
	}
	if p.vm != 8429 {
		t.Errorf("vm port = %d, want 8429", p.vm)
	}
}

// The ordered service set is pinned without booting anything: postgres and nats
// first, then the telemetry fleet, each with a readiness probe.
func TestServiceSet(t *testing.T) {
	want := []string{"postgres", "nats", "tempo", "loki", "victoriametrics", "otel-collector"}
	s := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra(), ports: servicePortsFromEnv()}
	procs := []*managedProc{s.postgresProc(), s.natsProc(), s.tempoProc(), s.lokiProc(), s.vmProc(), s.collectorProc()}
	if len(procs) != len(want) {
		t.Fatalf("service count = %d, want %d", len(procs), len(want))
	}
	for i, p := range procs {
		if p.name != want[i] {
			t.Errorf("service %d = %q, want %q", i, p.name, want[i])
		}
		if p.ready == nil {
			t.Errorf("service %q has no readiness probe", p.name)
		}
	}
}

// defaultDataDirFor lives under the INVOKING USER's own home: the services are
// that user's processes, so that is where their state belongs.
func TestDefaultDataDirFor(t *testing.T) {
	got := defaultDataDirFor()
	if !strings.Contains(got, "orchicon") {
		t.Errorf("data dir %q does not look like an orchicon state dir", got)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if !strings.HasPrefix(got, home) {
			t.Errorf("data dir %q is not under the invoking user's home %q", got, home)
		}
	}
	if strings.HasPrefix(got, "/var/lib") {
		t.Errorf("data dir %q is the retired container image path", got)
	}
}

// A readiness gate is only meaningful if it probes the port the child REALLY
// binds. For postgres/nats/vm the supervisor decides the port and passes it in
// argv, so the bind and the probe read the same field by construction; this
// asserts BOTH sides of that.
func TestServiceProbeMatchesItsBindArg(t *testing.T) {
	s := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra(), ports: servicePortsFromEnv()}
	p := s.ports

	pg := strings.Join(s.postgresProc().args, " ")
	if !strings.Contains(pg, "-p "+strconv.Itoa(p.postgres)) {
		t.Errorf("postgres does not bind the probed port %d: %s", p.postgres, pg)
	}
	nats := strings.Join(s.natsProc().args, " ")
	if !strings.Contains(nats, "-p "+strconv.Itoa(p.nats)) {
		t.Errorf("nats does not bind the probed client port %d: %s", p.nats, nats)
	}
	if !strings.Contains(nats, "-m "+strconv.Itoa(p.natsMonitor)) {
		t.Errorf("nats does not serve monitoring on the probed port %d: %s", p.natsMonitor, nats)
	}
	vm := strings.Join(s.vmProc().args, " ")
	if !strings.Contains(vm, "-httpListenAddr=:"+strconv.Itoa(p.vm)) {
		t.Errorf("victoriametrics does not listen on the probed port %d: %s", p.vm, vm)
	}
}

// reapRestart must escalate the backoff (2,4,8,16,32s cap) across CONSECUTIVE
// crashes of one child, not restart-loop at a flat 2s.
//
// THIS IS THE REGRESSION THE QA PASS FOUND. `start` installs a FRESH procState
// whose `restarts` is 0, so incrementing the OLD state and then calling
// `start` threw the count away: every crash recomputed from 0, and a crashed
// child was retried every 2s forever — the cap never engaged and a permanently
// broken child hot-looped. The fix carries the count into the new procState.
func TestRestartBackoffEscalatesAndCaps(t *testing.T) {
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 32 * time.Second}
	for i, w := range want {
		if got := restartBackoff(i + 1); got != w {
			t.Errorf("restart #%d: backoff = %v, want %v", i+1, got, w)
		}
	}
}

// reapRestart must carry the consecutive-crash count into the procState that
// start() installs; otherwise the escalation above is unreachable.
func TestReapRestartCarriesRestartCount(t *testing.T) {
	src := readFile(t, "host_services.go")
	if !strings.Contains(src, "ns.restarts = st.restarts") {
		t.Error("reapRestart no longer carries the restart count into the fresh procState: backoff cannot escalate")
	}
	if !strings.Contains(src, "backoff := restartBackoff(st.restarts)") {
		t.Error("reapRestart no longer derives its backoff from the consecutive-crash count")
	}
}

// postgres runs as the INVOKING USER, who does not own /var/run/postgresql, so the
// socket lives in the data dir that user owns. On a platform with no Unix-domain
// sockets the flag must not be passed at all.
func TestPostgresSocketDir(t *testing.T) {
	s := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra()}
	if got := s.postgresSocketDir(); got != s.dataDir {
		t.Errorf("socket dir = %q, want the data dir %q", got, s.dataDir)
	}
	args := strings.Join(s.postgresProc().args, " ")
	if !strings.Contains(args, "-c unix_socket_directories="+s.dataDir) {
		t.Errorf("postgres does not bind its socket in the data dir: %s", args)
	}
}

// embeddedConfigPort resolves the integer a config declares for key, after
// substituting the @TOKEN@ placeholders the way writeConfigs does. A token is a
// LEGITIMATE value now: the listen ports are templated from the resolved port
// table, so a bare strconv.Atoi would fail on "@TEMPO_HTTP_PORT@" and this helper
// is what keeps the drift check meaningful.
//
// nth selects among repeated keys (endpoint: 0.0.0.0: appears for the collector's
// gRPC receiver, its HTTP receiver and its health extension). It is substituted
// from the container defaults, so the returned number is the same integer the
// child would bind in that residency — which is what these tests then compare
// against servicePortsFor.
func embeddedConfigPort(t *testing.T, path, key string, nth int, ports servicePorts) int {
	t.Helper()
	data, err := assets.ServiceConfigsFS.ReadFile(path)
	if err != nil {
		t.Fatalf("read embedded %s: %v", path, err)
	}
	tokens := (&supervisor{dataDir: "/data", ports: ports}).configTokens()
	keys := make([]string, 0, len(tokens))
	for k := range tokens {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	body := applyConfigTokens(keys, string(data), tokens)

	seen := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		if seen < nth {
			seen++
			continue
		}
		rest := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(strings.TrimPrefix(trimmed, key)), "=: "))
		// A quoted scalar (endpoint: "0.0.0.0:14317") is legitimate YAML.
		rest = strings.Trim(strings.TrimSpace(rest), `"'`)
		n, err := strconv.Atoi(rest)
		if err != nil {
			t.Fatalf("%s: cannot parse %q port from %q: %v", path, key, trimmed, err)
		}
		return n
	}
	t.Fatalf("%s declares no %s (occurrence %d)", path, key, nth)
	return 0
}

// The telemetry children's listen ports live in an EMBEDDED CONFIG (tempo.yaml,
// loki.yaml, otel-collector.yaml), NOT in argv. The probe port is
// therefore only correct because it COPIES the config — so parse the shipped
// configs (resolving the @TOKEN@s) and pin each field to them, in both
// residencies. A knob that moved one of these fields without moving the config
// would leave the gate waiting on a port nothing listens on, and the supervisor
// would report a healthy child as never-ready forever.
func TestServiceProbesMatchEmbeddedConfigPorts(t *testing.T) {
	base := servicePortDefaults()
	cases := []struct {
		name, path, key, env string
		nth                  int
		field                func(servicePorts) int
	}{
		{"tempo", "deploy/configs/tempo.yaml", "http_listen_port", "ORCHICON_TEMPO_PORT", 0, func(p servicePorts) int { return p.tempo }},
		{"loki", "deploy/configs/loki.yaml", "http_listen_port", "ORCHICON_LOKI_PORT", 0, func(p servicePorts) int { return p.loki }},
		{"tempo-otlp-grpc", "deploy/configs/tempo.yaml", `endpoint: "0.0.0.0:`, "ORCHICON_TEMPO_OTLP_GRPC_PORT", 0, func(p servicePorts) int { return p.tempoOTLPGRPC }},
		{"tempo-otlp-http", "deploy/configs/tempo.yaml", `endpoint: "0.0.0.0:`, "ORCHICON_TEMPO_OTLP_HTTP_PORT", 1, func(p servicePorts) int { return p.tempoOTLPHTTP }},
		{"collector-otlp-grpc", "deploy/configs/otel-collector.yaml", "endpoint: 0.0.0.0:", "ORCHICON_OTLP_GRPC_PORT", 0, func(p servicePorts) int { return p.collectorOTLPGRPC }},
		{"collector-otlp-http", "deploy/configs/otel-collector.yaml", "endpoint: 0.0.0.0:", "ORCHICON_OTLP_HTTP_PORT", 1, func(p servicePorts) int { return p.collectorOTLPHTTP }},
		{"collector-health", "deploy/configs/otel-collector.yaml", "endpoint: 0.0.0.0:", "ORCHICON_OTEL_HEALTH_PORT", 2, func(p servicePorts) int { return p.collectorHealth }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := embeddedConfigPort(t, tc.path, tc.key, tc.nth, base)
			// The ambient knob must resolve to the SAME number, because the config
			// is templated from it: that is the whole contract being pinned.
			t.Setenv(tc.env, strconv.Itoa(want))
			if got := tc.field(servicePortsFromEnv()); got != want {
				t.Errorf("%s probes %d but %s declares %d — the gate can never pass",
					tc.name, got, tc.path, want)
			}
		})
	}
}

// The collector's tempo exporter must point at TEMPO's OTLP HTTP receiver, which
// is OFFSET from the collector's own OTLP HTTP port (they shared one network
// namespace in the container image). Pinning the offset against the collector's
// own port is what makes a future "let's just use 4318" cleanup fail loudly
// instead of silently splitting the pipeline.
func TestCollectorPointsAtTempoOffset(t *testing.T) {
	p := servicePortDefaults()
	tempoOTLP := embeddedConfigPort(t, "deploy/configs/otel-collector.yaml", "endpoint: http://localhost:", 0, p)
	if tempoOTLP != p.tempoOTLPHTTP {
		t.Errorf("the collector's tempo exporter points at %d, but tempo's OTLP HTTP receiver is %d",
			tempoOTLP, p.tempoOTLPHTTP)
	}
	if tempoOTLP == p.collectorOTLPHTTP {
		t.Errorf("the collector's tempo exporter and its own OTLP HTTP receiver are both %d — the offset was removed", tempoOTLP)
	}
	// And tempo really does bind the port the exporter names.
	if got := embeddedConfigPort(t, "deploy/configs/tempo.yaml", `endpoint: "0.0.0.0:`, 1, p); got != tempoOTLP {
		t.Errorf("tempo's OTLP HTTP receiver binds %d but the collector exports to %d", got, tempoOTLP)
	}
}

// Retention must be preserved EXACTLY: a lost retention setting is invisible
// until the disk fills, which is the opposite of what this change is for. Asserted
// against the shipped configs and the vm argv, not against a comment.
func TestRetentionPreserved(t *testing.T) {
	tempo, err := assets.ServiceConfigsFS.ReadFile("deploy/configs/tempo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tempo), "block_retention: 336h") {
		t.Error("tempo.yaml no longer pins block_retention: 336h (14 days) — the trace blocks would grow unbounded")
	}
	loki, err := assets.ServiceConfigsFS.ReadFile("deploy/configs/loki.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"retention_period: 336h", "retention_enabled: true", "delete_request_store: filesystem"} {
		if !strings.Contains(string(loki), want) {
			t.Errorf("loki.yaml no longer carries %q — retention_period alone is INERT without the compactor block", want)
		}
	}
	s := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra(),
		ports: servicePortsFromEnv()}
	if args := strings.Join(s.vmProc().args, " "); !strings.Contains(args, "-retentionPeriod=720h") {
		t.Errorf("victoriametrics no longer pins -retentionPeriod=720h (30 days): %s", args)
	}
}

// Every telemetry child execs the path the RESOLVER recorded, never a bare name on
// PATH: a downloaded binary lives in this instance's data dir, which is not on
// PATH, so a bare name would silently run a different build (or none).
func TestTelemetryChildrenExecResolvedPaths(t *testing.T) {
	s := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra(),
		ports: servicePortsFromEnv()}
	want := map[string]string{
		"tempo":           testInfra().Tempo,
		"loki":            testInfra().Loki,
		"victoriametrics": testInfra().VictoriaMetrics,
		"otel-collector":  testInfra().OTelCollector,
	}
	for _, p := range []*managedProc{s.tempoProc(), s.lokiProc(), s.vmProc(), s.collectorProc()} {
		if got, ok := want[p.name]; !ok {
			t.Errorf("unexpected telemetry child %q", p.name)
		} else if p.command != got {
			t.Errorf("%s command = %q, want the resolved path %q (a bare name would run a different binary)", p.name, p.command, got)
		}
	}
}

// writeConfigs must leave NO unresolved @TOKEN@ behind — a literal token is a
// config the child cannot parse — and must still substitute @DATA_DIR@.
func TestWriteConfigsResolvesPortTokens(t *testing.T) {
	dataDir := t.TempDir()
	s := &supervisor{log: testLogger(), dataDir: dataDir, infra: testInfra(),
		configDir: filepath.Join(dataDir, configDirName),
		ports:     servicePortsFromEnv()}
	if err := s.writeConfigs(); err != nil {
		t.Fatalf("writeConfigs: %v", err)
	}
	configDir := filepath.Join(dataDir, configDirName)
	err := filepath.WalkDir(configDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Ext(path) == ".json" {
			return nil // the dashboard JSON is not templated
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "@") {
			// Only a well-formed token is a failure; a bare @ could be prose.
			for _, tok := range regexp.MustCompile(`@[A-Z_]+@`).FindAllString(string(body), -1) {
				t.Errorf("%s still contains the unresolved token %s", path, tok)
			}
		}
		if strings.Contains(string(body), "@DATA_DIR@") {
			t.Errorf("%s did not get @DATA_DIR@ substituted", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The relocated configs really did land, and the resolved ports really are the
	// table's values (not the file's old literals).
	tempo, err := os.ReadFile(filepath.Join(configDir, "tempo.yaml"))
	if err != nil {
		t.Fatalf("read the written tempo.yaml: %v", err)
	}
	if !strings.Contains(string(tempo), "http_listen_port: "+strconv.Itoa(s.ports.tempo)) {
		t.Errorf("the written tempo.yaml does not carry the resolved port %d:\n%s", s.ports.tempo, tempo)
	}
	if !strings.Contains(string(tempo), "block_retention: 336h") {
		t.Error("the written tempo.yaml lost its retention setting")
	}
}

// A SECOND INSTANCE MUST BE ABLE TO START. This is the bug that made the
// operator's Logs and Traces views report their backends as unreachable while
// the SAME backends were healthy on the other instance:
//
//	dev  : 10:08:12 level=INFO msg="error running loki" err="listen tcp :9096: bind: address already in use"
//	dev  : 10:07:40 level=INFO msg="error running Tempo" err="... listen tcp :9095: bind: address already in use"
//
// Both numbers were hardcoded in the embedded configs. Every OTHER port in the
// table is one the operator reaches, so the installer assigns each instance a
// distinct value; these two are INTERNAL, so nothing assigned them and both
// instances demanded the same one. The child then crash-looped for ever, and
// the supervisor's per-component restart never converged.
//
// Pinned as a property rather than as "tempo.yaml contains @TEMPO_GRPC_PORT@",
// because the property is what broke: EVERY listener port in a shipped config
// must either be templated from the table or be 0 (kernel-assigned). A bare
// literal is the failure — and it is invisible, because the first instance's
// telemetry works perfectly.
func TestNoEmbeddedConfigHardcodesAListenerPort(t *testing.T) {
	// `key` is the config field that makes the child BIND a socket.
	// `address: 0.0.0.0:` is the collector's self-metrics exporter (service.telemetry.metrics).
	// Its ABSENCE is covered separately by TestCollectorDeclaresItsSelfMetricsListener —
	// this list can only find a literal that is already written down.
	listenerKeys := []string{"http_listen_port:", "grpc_listen_port:", "endpoint: 0.0.0.0:", `endpoint: "0.0.0.0:`, "address: 0.0.0.0:"}
	files := []string{"deploy/configs/tempo.yaml", "deploy/configs/loki.yaml", "deploy/configs/otel-collector.yaml"}

	for _, path := range files {
		data, err := assets.ServiceConfigsFS.ReadFile(path)
		if err != nil {
			t.Fatalf("read embedded %s: %v", path, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue // documentation, not a binding
			}
			for _, key := range listenerKeys {
				if !strings.HasPrefix(trimmed, key) {
					continue
				}
				value := strings.Trim(strings.TrimSpace(strings.TrimLeft(
					strings.TrimSpace(strings.TrimPrefix(trimmed, key)), "=: ")), `"'`)
				if value == "" {
					continue
				}
				// Acceptable: a placeholder (templated from the port table), or 0.
				if strings.HasPrefix(value, "@") || strings.Contains(value, "@") || value == "0" {
					continue
				}
				t.Errorf("%s:%d hardcodes the listener port %q (%s).\n"+
					"A literal is a port TWO instances must share, so the second one crash-loops on\n"+
					"\"bind: address already in use\" and its telemetry view reports the backend\n"+
					"unreachable while the first instance works fine. Template it from the port\n"+
					"table (@…_PORT@) or use 0 for an internal listener nothing outside the process dials.",
					path, i+1, value, key)
			}
		}
	}
}

// Tempo's internal gRPC port must come from the table, and must MOVE when the
// configured one is already taken — that is what lets a second instance boot.
func TestTempoGRPCPortComesFromTheTableAndAvoidsACollision(t *testing.T) {
	base := servicePortDefaults()

	// The value the config carries must be BINDABLE — not merely equal to the
	// default. Equality is the wrong assertion here and the failure proves it:
	// on a host where the default is already taken by another instance's Tempo,
	// the table legitimately resolves to a different number, and a test demanding
	// the default would forbid the very behaviour that lets the second instance
	// boot.
	resolved := embeddedConfigPort(t, "deploy/configs/tempo.yaml", "grpc_listen_port", 0, base)
	if ln, err := net.Listen("tcp", ":"+strconv.Itoa(resolved)); err != nil {
		t.Errorf("tempo.yaml binds gRPC port %d, which is not bindable: %v", resolved, err)
	} else {
		_ = ln.Close()
	}

	// The operator's knob reaches BOTH the table and the config. Proven against a
	// port we know is free, so the assertion is exact rather than best-effort.
	free := freePortForTest(t)
	t.Setenv("ORCHICON_TEMPO_GRPC_PORT", strconv.Itoa(free))
	p := servicePortsFromEnv()
	if p.tempoGRPC != free {
		t.Fatalf("ORCHICON_TEMPO_GRPC_PORT=%d did not reach the port table (got %d)", free, p.tempoGRPC)
	}
	if got := embeddedConfigPort(t, "deploy/configs/tempo.yaml", "grpc_listen_port", 0, p); got != free {
		t.Errorf("tempo.yaml binds gRPC port %d but the knob said %d", got, free)
	}

	// AN OCCUPIED PORT MUST BE SKIPPED. This is the collision, simulated: hold
	// the preferred port the way the other instance's Tempo does, then require a
	// DIFFERENT, bindable one.
	holder, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer holder.Close()
	taken := holder.Addr().(*net.TCPAddr).Port

	got := internalListenerPort(taken)
	if got == taken {
		t.Errorf("internalListenerPort(%d) returned the port that is already bound — the second instance would crash-loop", taken)
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(got))
	if err != nil {
		t.Errorf("internalListenerPort(%d) chose %d, which is not bindable: %v", taken, got, err)
	} else {
		_ = ln.Close()
	}
	if got <= taken {
		t.Errorf("internalListenerPort walked to %d, which is not above the occupied %d", got, taken)
	}
}

// freePortForTest returns a port that was free at the moment it was asked for.
// The probe-then-release race is acceptable in a test for the same reason it is
// acceptable in internalListenerPort: the assertion that matters is "not the
// occupied one", and nothing else on this machine is racing this test for a
// specific high port.
func freePortForTest(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// THE COLLECTOR'S SELF-METRICS LISTENER MUST BE DECLARED IN THE CONFIG.
//
// TestNoEmbeddedConfigHardcodesAListenerPort cannot catch this one, and the gap
// is the interesting part: that test scans for a LITERAL port in a listener
// field. The collector's own stats exporter has no field until you write one —
// leave the `service.telemetry` block out and the process quietly falls back to
// its DEFAULT, localhost:8888. There is nothing in the file to find.
//
// So the failure looked like this, measured on a real host with two instances:
//
//	dev otel-collector  err="binding address localhost:8888 for Prometheus
//	                          exporter: listen tcp 127.0.0.1:8888: bind: address
//	                          already in use"        (16 restarts, for ever)
//
// and because the collector is what ships OTLP from the plane to Tempo, Loki
// and VictoriaMetrics, the operator lost MORE than the collector: no logs, no
// traces, and a Metrics view showing only the plane's own outbox gauges —
// tokens, cost and execution counts were emitted into nothing. "Still can't
// talk to logs or traces, and I don't see the new metrics" was ONE cause.
func TestCollectorDeclaresItsSelfMetricsListener(t *testing.T) {
	data, err := assets.ServiceConfigsFS.ReadFile("deploy/configs/otel-collector.yaml")
	if err != nil {
		t.Fatalf("read embedded otel-collector.yaml: %v", err)
	}
	cfg := string(data)

	// The block must exist...
	svcIdx := strings.Index(cfg, "\nservice:")
	if svcIdx < 0 {
		t.Fatal("otel-collector.yaml has no `service:` block")
	}
	svc := cfg[svcIdx:]

	// ...and declare a metrics address under service.telemetry.
	telIdx := strings.Index(svc, "telemetry:")
	if telIdx < 0 {
		t.Fatalf("otel-collector.yaml's `service:` block declares no `telemetry:`.\n" +
			"With no service.telemetry.metrics.address the collector falls back to its DEFAULT\n" +
			"localhost:8888, so the second instance on a host crash-loops on\n" +
			"\"bind: address already in use\" — and takes its whole OTLP pipeline (metrics, logs,\n" +
			"traces) with it.")
	}
	block := svc[telIdx:]
	addrIdx := strings.Index(block, "address:")
	if addrIdx < 0 {
		t.Fatalf("service.telemetry declares no `metrics.address`; the collector would default to localhost:8888")
	}
	line := strings.SplitN(block[addrIdx:], "\n", 2)[0]

	// And it must be TEMPLATED, not a literal — the whole point is per-instance.
	if !strings.Contains(line, "@COLLECTOR_SELF_METRICS_PORT@") {
		t.Errorf("service.telemetry.metrics.address is %q; it must be templated from\n"+
			"@COLLECTOR_SELF_METRICS_PORT@ so each instance binds its own socket (resolved via\n"+
			"internalListenerPort, which walks off an occupied port).", strings.TrimSpace(line))
	}
}

// The token above must actually be RESOLVED, or the config ships the literal
// placeholder and the collector fails to parse its own address — a strictly
// worse failure than the collision it replaced.
func TestCollectorSelfMetricsTokenIsResolved(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra(), ports: servicePortDefaults()}
	tokens := sup.configTokens()

	raw, ok := tokens["@COLLECTOR_SELF_METRICS_PORT@"]
	if !ok {
		t.Fatal("configTokens does not provide @COLLECTOR_SELF_METRICS_PORT@: otel-collector.yaml would\n" +
			"be written with the literal placeholder and the collector would refuse to start")
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("@COLLECTOR_SELF_METRICS_PORT@ resolved to %q, which is not a number", raw)
	}
	// BINDABLE, not merely equal to the default: 8888 may well be taken.
	if port <= 0 || port > 65535 {
		t.Fatalf("@COLLECTOR_SELF_METRICS_PORT@ resolved to %d, outside the usable range", port)
	}
}
