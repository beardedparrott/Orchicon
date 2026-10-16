package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
)

// Services-only mode is the OPT-IN half of host residency: the launcher sets
// ORCHICON_CONTAINER_SERVICES_ONLY=1 for an instance whose plane runs on the
// host. Every value other than "1" must leave today's behaviour untouched.
func TestServicesOnlyFromEnv(t *testing.T) {
	cases := map[string]bool{
		"1":    true,
		"0":    false,
		"":     false,
		"true": false,
		"yes":  false,
		"host": false,
		" 1":   false,
	}
	for val, want := range cases {
		t.Run("value="+val, func(t *testing.T) {
			t.Setenv(servicesOnlyEnv, val)
			if got := servicesOnlyFromEnv(); got != want {
				t.Errorf("%s=%q: got %v, want %v", servicesOnlyEnv, val, got, want)
			}
		})
	}
	t.Run("unset", func(t *testing.T) {
		t.Setenv(servicesOnlyEnv, "sentinel")
		if err := os.Unsetenv(servicesOnlyEnv); err != nil {
			t.Fatal(err)
		}
		if servicesOnlyFromEnv() {
			t.Errorf("unset %s must not enable services-only mode", servicesOnlyEnv)
		}
	})
}

func TestPostgresListenAddr(t *testing.T) {
	if got := (&supervisor{servicesOnly: true}).postgresListenAddr(); got != "0.0.0.0" {
		t.Errorf("services-only: got %q, want 0.0.0.0 (a published port forwards to the container IP, not loopback)", got)
	}
	if got := (&supervisor{}).postgresListenAddr(); got != "localhost" {
		t.Errorf("default: got %q, want localhost (unchanged behaviour)", got)
	}
	// Host residency stays on loopback EVEN WITH servicesOnly set: nothing
	// forwards to a host process, so widening the address would only expose the
	// database to the LAN.
	if got := (&supervisor{residency: residencyHost, servicesOnly: true}).postgresListenAddr(); got != "localhost" {
		t.Errorf("host residency widened the listen address: got %q, want localhost", got)
	}
	if got := (&supervisor{residency: residencyHost}).postgresListenAddr(); got != "localhost" {
		t.Errorf("host residency: got %q, want localhost", got)
	}
}

func TestPostgresProcUsesListenAddr(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir(), servicesOnly: true}
	if args := strings.Join(sup.postgresProc().args, " "); !strings.Contains(args, "-c listen_addresses=0.0.0.0") {
		t.Errorf("services-only postgres args do not widen the listen address: %s", args)
	}
	sup.servicesOnly = false
	if args := strings.Join(sup.postgresProc().args, " "); !strings.Contains(args, "-c listen_addresses=localhost") {
		t.Errorf("default postgres args changed: %s", args)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEnsurePostgresHBATrust(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir()}
	dir := filepath.Join(sup.dataDir, "postgres")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pg_hba.conf")
	// initdb --auth=trust's real body: loopback host rules only, which is
	// exactly why a connection from the published port (bridge-gateway peer)
	// is refused.
	seed := "# PostgreSQL Client Authentication Configuration File\n" +
		"local   all             all                                     trust\n" +
		"host    all             all             127.0.0.1/32            trust\n" +
		"host    all             all             ::1/128                 trust\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sup.ensurePostgresHBATrust(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := readFile(t, path)
	for _, rule := range pgHBATrustRules() {
		if n := strings.Count(first, rule); n != 1 {
			t.Errorf("want exactly one %q, got %d in:\n%s", rule, n, first)
		}
	}
	for _, kept := range []string{"local   all", "127.0.0.1/32            trust", "::1/128                 trust"} {
		if !strings.Contains(first, kept) {
			t.Errorf("existing pg_hba content was rewritten (missing %q):\n%s", kept, first)
		}
	}

	// Every boot runs this: the second run must be a byte-for-byte no-op.
	if err := sup.ensurePostgresHBATrust(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second := readFile(t, path); second != first {
		t.Errorf("second run changed pg_hba.conf:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}

	// A data dir without a pg_hba yet is not an error — initPostgres owns it.
	fresh := &supervisor{log: testLogger(), dataDir: t.TempDir()}
	if err := fresh.ensurePostgresHBATrust(); err != nil {
		t.Errorf("missing pg_hba.conf must be a no-op, got %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The plane child must sit in the ELSE branch of the services-only decision,
// and that decision must be logged: a container silently running without a
// plane looks like a failed boot. Static (the alternative is booting real
// children in a unit test), and it fails loudly if the gate is refactored away.
func TestPlaneChildGateSkipsForBothServicesOnlyAndHostResidency(t *testing.T) {
	src := readFile(t, "container.go")
	if !strings.Contains(src, "if servicesOnly {") {
		t.Error("container.go no longer branches on servicesOnly")
	}
	if !strings.Contains(src, "} else if err := sup.startAndWait(ctx, sup.planeProc(), 120*time.Second); err != nil {") {
		t.Error("the plane child is no longer gated on servicesOnly (it must only start when residency is container)")
	}
	if !strings.Contains(src, "services-only mode: postgres, nats and telemetry run in this container") {
		t.Error("services-only mode is not logged plainly")
	}
	if !strings.Contains(src, "servicesOnly: servicesOnly") {
		t.Error("the parsed services-only setting is not carried into the supervisor")
	}
	// Host residency has no container to hold a plane child, so it must reach
	// the SAME skipped branch — widened at its definition, not at the gate.
	if !strings.Contains(src, "servicesOnly := servicesOnlyFromEnv() || residency == residencyHost") {
		t.Error("host residency no longer skips the plane child through the services-only branch")
	}
	if !strings.Contains(src, "log.Info(\"service residency resolved\"") {
		t.Error("the resolved service residency is not logged")
	}
	if !strings.Contains(src, "serviceResidencyFromEnv()") {
		t.Error("container.go no longer reads the service residency")
	}
}

// The trust rule must name the bridge gateway and NOTHING ELSE when the
// gateway is detectable: every other container on the host shares that bridge,
// and a wildcard rule would hand it password-less superuser access to this
// instance's database. The loopback publish is the security boundary.
func TestPostgresHBATrustRulesAreGatewayScoped(t *testing.T) {
	rules := pgHBATrustRules()
	gw := bridgeGatewayIP()
	if gw == "" {
		// No detectable gateway: the wide fallback is deliberate (the host
		// plane must still reach postgres), so assert it exists and is
		// auditable rather than silently empty.
		if len(rules) == 0 {
			t.Fatal("no detectable gateway and no fallback rule: the host plane could never connect")
		}
		return
	}
	want := "host all all " + gw + "/32 trust"
	if len(rules) != 1 || rules[0] != want {
		t.Fatalf("gateway %s detected: rules = %v, want exactly [%s] (a wildcard rule trusts every container on the host)", gw, rules, want)
	}
}

func TestDefaultRouteGatewayParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "route")
	body := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"lo\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultRouteGateway(path); got != "172.17.0.1" {
		t.Errorf("got %q, want 172.17.0.1 (gateway 010011AC is little-endian)", got)
	}

	// A routing table with no default route (and a missing file) must not
	// invent a gateway: the caller falls back, it never guesses a peer.
	noDefault := filepath.Join(dir, "no-default")
	if err := os.WriteFile(noDefault, []byte("Iface\tDestination\tGateway\neth0\t000011AC\t00000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultRouteGateway(noDefault); got != "" {
		t.Errorf("no default route: got %q, want empty", got)
	}
	if got := defaultRouteGateway(filepath.Join(dir, "absent")); got != "" {
		t.Errorf("missing file: got %q, want empty", got)
	}
}

// ----- service residency (ORCHICON_SERVICE_RESIDENCY) -----

// Anything other than "host" is container residency: the supervisor runs the
// product's services and must never refuse to boot over a knob.
func TestServiceResidencyFromEnv(t *testing.T) {
	cases := map[string]string{
		"host":      residencyHost,
		"container": residencyContainer,
		"":          residencyContainer,
		" true":     residencyContainer,
		"hostish":   residencyContainer,
		"HOST":      residencyContainer,
	}
	for val, want := range cases {
		t.Run("value="+val, func(t *testing.T) {
			t.Setenv(serviceResidencyEnv, val)
			if got := serviceResidencyFromEnv(); got != want {
				t.Errorf("%s=%q: got %q, want %q", serviceResidencyEnv, val, got, want)
			}
		})
	}
	t.Run("unset", func(t *testing.T) {
		t.Setenv(serviceResidencyEnv, "sentinel")
		if err := os.Unsetenv(serviceResidencyEnv); err != nil {
			t.Fatal(err)
		}
		if got := serviceResidencyFromEnv(); got != residencyContainer {
			t.Errorf("unset %s: got %q, want %q", serviceResidencyEnv, got, residencyContainer)
		}
	})
}

// The zero value of supervisor.residency IS container residency: that is what
// keeps every pre-existing supervisor literal (and therefore every pre-existing
// test) on today's behaviour.
func TestIsHostZeroValueIsContainerResidency(t *testing.T) {
	if (&supervisor{}).isHost() {
		t.Error("the zero-value supervisor must be container residency")
	}
	if (&supervisor{residency: residencyContainer}).isHost() {
		t.Error("explicit container residency reported as host")
	}
	if !(&supervisor{residency: residencyHost}).isHost() {
		t.Error("host residency not reported as host")
	}
}

// The host path must require NO setpriv (acceptance criterion 6): a host-
// resident service is a direct process of the invoking user. Container
// residency with a non-root uid must still produce setpriv, unchanged.
func TestPostgresProcRequiresNoSetprivInHostResidency(t *testing.T) {
	host := &supervisor{log: testLogger(), dataDir: t.TempDir(),
		residency: residencyHost, ports: servicePortsFor(residencyHost)}
	p := host.postgresProc()
	if p.command != "postgres" {
		t.Errorf("host residency: command = %q, want postgres (no setpriv)", p.command)
	}
	joined := strings.Join(p.args, " ")
	for _, banned := range []string{"setpriv", "--reuid", "--regid", "--clear-groups"} {
		if strings.Contains(joined, banned) {
			t.Errorf("host residency postgres args contain %q: %s", banned, joined)
		}
	}

	container := &supervisor{log: testLogger(), dataDir: t.TempDir(),
		residency: residencyContainer, hostUID: 70, hostGID: 70,
		ports: servicePortsFor(residencyContainer)}
	cp := container.postgresProc()
	if cp.command != "setpriv" {
		t.Errorf("container residency lost the privilege drop: command = %q, want setpriv", cp.command)
	}
	cargs := strings.Join(cp.args, " ")
	for _, want := range []string{"setpriv", "--reuid=", "--regid=", "--clear-groups"} {
		if want == "setpriv" {
			continue // it is the command, not an arg
		}
		if !strings.Contains(cargs, want) {
			t.Errorf("container residency args missing %q: %s", want, cargs)
		}
	}
}

// asUser is the single launch seam: setpriv ONLY for container residency with a
// non-root uid. Host residency never produces it.
func TestAsUser(t *testing.T) {
	cases := []struct {
		name      string
		residency string
		uid, gid  int
		wantCmd   string
		wantArgs  bool // contains --reuid
	}{
		{name: "host-uid0", residency: residencyHost, uid: 0, gid: 0, wantCmd: "postgres", wantArgs: false},
		{name: "host-uid1000", residency: residencyHost, uid: 1000, gid: 1000, wantCmd: "postgres", wantArgs: false},
		{name: "container-uid0", residency: residencyContainer, uid: 0, gid: 0, wantCmd: "postgres", wantArgs: false},
		{name: "container-uid70", residency: residencyContainer, uid: 70, gid: 70, wantCmd: "setpriv", wantArgs: true},
		{name: "zero-value-uid70", uid: 70, gid: 70, wantCmd: "setpriv", wantArgs: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &supervisor{residency: tc.residency}
			cmd, args := s.asUser(tc.uid, tc.gid, "postgres", []string{"-D", "/data"})
			if cmd != tc.wantCmd {
				t.Errorf("command = %q, want %q", cmd, tc.wantCmd)
			}
			joined := strings.Join(args, " ")
			if got := strings.Contains(joined, "--reuid="); got != tc.wantArgs {
				t.Errorf("args %q: contains --reuid = %v, want %v", joined, got, tc.wantArgs)
			}
			// The payload must survive verbatim: for setpriv it follows the
			// privilege flags, and for the identity path it IS the argv.
			want := "-D /data"
			if tc.wantArgs {
				want = "postgres -D /data"
			}
			if !strings.HasSuffix(joined, want) {
				t.Errorf("args lost the payload (want suffix %q): %q", want, joined)
			}
		})
	}
}

// Host residency runs as the INVOKING USER; uid 70 is a container-era default
// that must never be the host answer. Container residency keeps uid 70 (and the
// preserved-owner path) unchanged.
func TestPostgresUIDGIDHostResidency(t *testing.T) {
	host := &supervisor{residency: residencyHost}
	uid, gid := host.postgresUIDGID()
	if uid != os.Getuid() || gid != os.Getgid() {
		t.Errorf("host residency: got (%d,%d), want the invoking user (%d,%d)", uid, gid, os.Getuid(), os.Getgid())
	}
	if uid == 70 && os.Getuid() != 70 {
		t.Error("host residency must not default to the container-era uid 70")
	}

	container := &supervisor{residency: residencyContainer, dataDir: t.TempDir()}
	uid, gid = container.postgresUIDGID()
	if uid != 70 || gid != 70 {
		t.Errorf("container residency: got (%d,%d), want (70,70) (unchanged)", uid, gid)
	}

	// A preserved NON-ROOT-owned postgres dir still wins in container
	// residency — the compose-era volume path must remain reachable. The dir is
	// chowned to the invoking user (which needs no privilege because we already
	// own it), so the assertion holds whether or not the test runs as root.
	t.Run("container-preserved-owner", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("running as root: a fresh dir would already be root-owned")
		}
		preserved := &supervisor{residency: residencyContainer, dataDir: t.TempDir()}
		dir := filepath.Join(preserved.dataDir, "postgres")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		wantUID, wantGID := os.Getuid(), os.Getgid()
		if err := os.Chown(dir, wantUID, wantGID); err != nil {
			t.Fatalf("chown %s: %v", dir, err)
		}
		if uid, gid := preserved.postgresUIDGID(); uid != wantUID || gid != wantGID {
			t.Errorf("preserved volume owner lost: got (%d,%d), want (%d,%d)", uid, gid, wantUID, wantGID)
		}
	})
}

// Host residency creates the data dirs and writes configs, but performs NO
// chown and populates NO container home subdirs — running as the invoking user
// is the point.
func TestPrepareHostResidencyDoesNotChown(t *testing.T) {
	home := t.TempDir()
	sup := &supervisor{
		log:       testLogger(),
		dataDir:   t.TempDir(),
		residency: residencyHost,
		// Deliberately non-zero: the container path WOULD touch the home.
		hostUID:  1000,
		hostGID:  1000,
		hostHome: home,
	}
	// Seed PG_VERSION so prepare's initPostgres is a hermetic early return.
	pgDir := filepath.Join(sup.dataDir, "postgres")
	if err := os.MkdirAll(pgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pgDir, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sup.prepare(context.Background()); err != nil {
		t.Fatalf("host prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sup.dataDir, configDirName)); err != nil {
		t.Errorf("prepare did not create the config dir: %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("host residency populated the container home: %v", names)
	}
}

// initPostgres is idempotent via the PG_VERSION gate in BOTH residencies
// (acceptance criterion 5): an already-initialized dir returns nil without
// invoking initdb.
func TestInitPostgresIdempotentBothResidencies(t *testing.T) {
	for _, residency := range []string{residencyHost, residencyContainer} {
		t.Run(residency, func(t *testing.T) {
			s := &supervisor{log: testLogger(), dataDir: t.TempDir(), residency: residency}
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
		})
	}
}

// Host residency honours the operator's port knobs; container residency
// ignores them (its ports are the container's own namespace).
func TestServicePortsForHostReadsEnv(t *testing.T) {
	t.Setenv("ORCHICON_POSTGRES_PORT", "5433")
	t.Setenv("ORCHICON_NATS_PORT", "4223")
	t.Setenv("ORCHICON_NATS_MONITOR_PORT", "8223")
	t.Setenv("ORCHICON_VM_PORT", "8429")

	host := servicePortsFor(residencyHost)
	if host.postgres != 5433 {
		t.Errorf("host postgres port = %d, want 5433", host.postgres)
	}
	if host.nats != 4223 {
		t.Errorf("host nats port = %d, want 4223", host.nats)
	}
	if host.natsMonitor != 8223 {
		t.Errorf("host nats monitor port = %d, want 8223", host.natsMonitor)
	}
	if host.vm != 8429 {
		t.Errorf("host vm port = %d, want 8429", host.vm)
	}

	container := servicePortsFor(residencyContainer)
	if container.postgres != 5432 || container.nats != 4222 || container.vm != 8428 {
		t.Errorf("container residency must ignore the host port knobs: %+v", container)
	}
}

// The SAME ordered service set is built in both residencies (acceptance
// criterion 1, "same service set"), pinned without booting anything.
func TestServiceSetParityBothResidencies(t *testing.T) {
	want := []string{"postgres", "nats", "tempo", "loki", "victoriametrics", "otel-collector", "grafana"}
	for _, residency := range []string{residencyHost, residencyContainer} {
		t.Run(residency, func(t *testing.T) {
			s := &supervisor{log: testLogger(), dataDir: t.TempDir(),
				residency: residency, ports: servicePortsFor(residency)}
			procs := []*managedProc{s.postgresProc(), s.natsProc(), s.tempoProc(), s.lokiProc(), s.vmProc(), s.collectorProc(), s.grafanaProc()}
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
		})
	}
}

// defaultDataDirFor: container residency keeps the image path; host residency
// lives under the invoking user's own home.
func TestDefaultDataDirFor(t *testing.T) {
	if got := defaultDataDirFor(residencyContainer); got != "/var/lib/orchicon" {
		t.Errorf("container: got %q, want /var/lib/orchicon", got)
	}
	if got := defaultDataDirFor(""); got != "/var/lib/orchicon" {
		t.Errorf("zero value: got %q, want /var/lib/orchicon", got)
	}
	host := defaultDataDirFor(residencyHost)
	if !strings.Contains(host, "orchicon") {
		t.Errorf("host data dir %q does not look like an orchicon state dir", host)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if !strings.HasPrefix(host, home) {
			t.Errorf("host data dir %q is not under the invoking user's home %q", host, home)
		}
	}
}

// A readiness gate is only meaningful if it probes the port the child REALLY
// binds. For postgres/nats/vm the supervisor decides the port and passes it in
// argv, so the bind and the probe read the same field by construction; this
// asserts BOTH sides of that in BOTH residencies.
func TestServiceProbeMatchesItsBindArg(t *testing.T) {
	for _, residency := range []string{residencyContainer, residencyHost} {
		t.Run(residency, func(t *testing.T) {
			s := &supervisor{log: testLogger(), dataDir: t.TempDir(),
				residency: residency, ports: servicePortsFor(residency)}
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
		})
	}
}

// Criterion 4: container residency is UNCHANGED. The pre-change argv and probe
// ports are pinned here as literals, so any drift in the container path — not
// just in the host path — fails loudly. (The residency refactor routes these
// through s.ports / asUser; for container residency those must resolve to
// exactly these values.)
func TestContainerResidencyArgvUnchanged(t *testing.T) {
	s := &supervisor{log: testLogger(), dataDir: t.TempDir(), hostUID: 70, hostGID: 70,
		residency: residencyContainer, ports: servicePortsFor(residencyContainer)}

	pg := s.postgresProc()
	if pg.command != "setpriv" {
		t.Fatalf("container postgres command = %q, want setpriv", pg.command)
	}
	wantPG := "--reuid=70 --regid=70 --clear-groups postgres -D " + filepath.Join(s.dataDir, "postgres") + " -p 5432 -c listen_addresses=localhost"
	if got := strings.Join(pg.args, " "); got != wantPG {
		t.Errorf("container postgres argv drifted:\n got %s\nwant %s", got, wantPG)
	}

	wantNats := "-js -sd " + filepath.Join(s.dataDir, "nats") + " -p 4222 -m 8222"
	if got := strings.Join(s.natsProc().args, " "); got != wantNats {
		t.Errorf("container nats argv drifted:\n got %s\nwant %s", got, wantNats)
	}

	wantVM := "-storageDataPath=" + filepath.Join(s.dataDir, "victoriametrics") + " -retentionPeriod=720h -httpListenAddr=:8428"
	if got := strings.Join(s.vmProc().args, " "); got != wantVM {
		t.Errorf("container victoriametrics argv drifted:\n got %s\nwant %s", got, wantVM)
	}

	for _, p := range []*managedProc{s.tempoProc(), s.lokiProc(), s.collectorProc(), s.grafanaProc()} {
		switch p.name {
		case "tempo":
			if got := strings.Join(p.args, " "); got != "-config.file="+filepath.Join(s.configDir, "tempo.yaml") {
				t.Errorf("container tempo argv drifted: %s", got)
			}
		case "loki":
			if got := strings.Join(p.args, " "); got != "-config.file="+filepath.Join(s.configDir, "loki.yaml") {
				t.Errorf("container loki argv drifted: %s", got)
			}
		case "otel-collector":
			if got := strings.Join(p.args, " "); got != "--config="+filepath.Join(s.configDir, "otel-collector.yaml") {
				t.Errorf("container otel argv drifted: %s", got)
			}
		case "grafana":
			if got := strings.Join(p.args, " "); got != "server --homepath=/usr/share/grafana --config="+filepath.Join(s.configDir, "grafana.ini") {
				t.Errorf("container grafana argv drifted: %s", got)
			}
		}
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
	src := readFile(t, "container.go")
	if !strings.Contains(src, "ns.restarts = st.restarts") {
		t.Error("reapRestart no longer carries the restart count into the fresh procState: backoff cannot escalate")
	}
	if !strings.Contains(src, "backoff := restartBackoff(st.restarts)") {
		t.Error("reapRestart no longer derives its backoff from the consecutive-crash count")
	}
}

// Host residency runs postgres as the INVOKING USER, who does NOT own
// /var/run/postgresql (the postgres package owns it as postgres:postgres), so
// postgres refuses to start with "could not create lock file
// .../.s.PGSQL.<port>.lock: Permission denied" and the readiness gate can never
// pass. The socket must move to the data dir, which the invoking user owns —
// the same fix the container sandbox plane makes (internal/runtime/agent.go).
//
// Container residency must keep postgres on its own default: there the child
// runs as uid 70, which owns /var/run/postgresql (this is criterion 4).
func TestPostgresSocketDirIsResidencyScoped(t *testing.T) {
	if got := (&supervisor{}).postgresSocketDir(); got != "" {
		t.Errorf("container residency must leave postgres on its default socket dir: got %q", got)
	}
	if got := (&supervisor{residency: residencyContainer}).postgresSocketDir(); got != "" {
		t.Errorf("explicit container residency moved the socket dir: got %q", got)
	}

	host := &supervisor{log: testLogger(), dataDir: t.TempDir(), residency: residencyHost}
	if got := host.postgresSocketDir(); got != host.dataDir {
		t.Errorf("host residency socket dir = %q, want the data dir %q", got, host.dataDir)
	}
	args := strings.Join(host.postgresProc().args, " ")
	if !strings.Contains(args, "-c unix_socket_directories="+host.dataDir) {
		t.Errorf("host residency postgres does not bind its socket in the data dir: %s", args)
	}

	// And the container child must NOT carry the flag, so today's argv is
	// byte-for-byte what it was.
	c := &supervisor{log: testLogger(), dataDir: t.TempDir(), residency: residencyContainer}
	if cargs := strings.Join(c.postgresProc().args, " "); strings.Contains(cargs, "unix_socket_directories") {
		t.Errorf("container residency argv changed: %s", cargs)
	}
}

// tempo/loki/grafana do NOT take their listen port in argv: it lives in an
// EMBEDDED CONFIG (tempo.yaml, loki.yaml, grafana.ini) that writeConfigs ships
// verbatim. The probe port is therefore only correct because it COPIES the
// config — so parse the shipped configs and pin the field to them, in both
// residencies. A knob that moved one of these fields without moving the config
// would leave the gate waiting on a port nothing listens on, and the supervisor
// would report a healthy child as never-ready forever.
func TestServiceProbesMatchEmbeddedConfigPorts(t *testing.T) {
	configPort := func(path, key string) int {
		t.Helper()
		data, err := assets.ContainerFS.ReadFile(path)
		if err != nil {
			t.Fatalf("read embedded %s: %v", path, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, key) {
				continue
			}
			rest := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(strings.TrimPrefix(trimmed, key)), "=: "))
			n, err := strconv.Atoi(rest)
			if err != nil {
				t.Fatalf("%s: cannot parse %q port from %q: %v", path, key, trimmed, err)
			}
			return n
		}
		t.Fatalf("%s declares no %s", path, key)
		return 0
	}

	cases := []struct {
		name, path, key, env string
		field                func(servicePorts) int
	}{
		{"tempo", "deploy/container/configs/tempo.yaml", "http_listen_port", "ORCHICON_TEMPO_PORT", func(p servicePorts) int { return p.tempo }},
		{"loki", "deploy/container/configs/loki.yaml", "http_listen_port", "ORCHICON_LOKI_PORT", func(p servicePorts) int { return p.loki }},
		{"grafana", "deploy/container/configs/grafana.ini", "http_port", "ORCHICON_GRAFANA_PORT", func(p servicePorts) int { return p.grafana }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := configPort(tc.path, tc.key)
			// The ambient knob must NOT move the probe: the child still binds
			// the config's port, so probing the knob's port would never pass.
			t.Setenv(tc.env, strconv.Itoa(want+1))
			for _, residency := range []string{residencyContainer, residencyHost} {
				if got := tc.field(servicePortsFor(residency)); got != want {
					t.Errorf("%s residency: %s probes %d but %s binds %d — the gate can never pass",
						residency, tc.name, got, tc.path, want)
				}
			}
		})
	}
}
