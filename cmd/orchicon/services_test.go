package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// services_test.go — the HOST services path (`orchicon services up|down|status`).
//
// The rules pinned here are the ones a reviewer cannot see from the happy path:
//
//   - HOST POSTGRES MUST NEVER GO THROUGH setpriv OR uid 70. Those exist for the
//     container's multi-uid layout; on a developer's machine neither is possible
//     (no root) and a command line that carried them would fail at exec time with
//     a permission error naming setpriv rather than Postgres.
//   - THE PORT IS THE INSTANCE'S, not a literal, and the listen address is
//     LOOPBACK ONLY. A host postgres widened to 0.0.0.0 with trust auth is a
//     LAN-reachable database, which is exactly what the container path's
//     gateway-scoped rule exists to avoid.
//   - THE PID/LOG FILES LIVE UNDER THE INSTANCE'S DATA DIR, so dev and prod
//     cannot stomp each other's state.
//   - IDEMPOTENCE IS DECIDED BY A PORT PROBE, before anything is started.

func TestServicePortsComeFromTheOneTable(t *testing.T) {
	// The dev/prod/test columns are the same table container.sh carries and
	// TestInstallPortDefaultsMatchContainerScript compares the two against.
	cases := map[string][3]int{
		"dev":  {5432, 4222, 8222},
		"prod": {5433, 4223, 8223},
		"test": {5434, 4224, 8224},
	}
	for instance, want := range cases {
		pg, nats, mon, err := servicePorts(instance)
		if err != nil {
			t.Fatalf("servicePorts(%q): %v", instance, err)
		}
		if pg != want[0] || nats != want[1] || mon != want[2] {
			t.Errorf("%s: got pg=%d nats=%d mon=%d, want %d/%d/%d",
				instance, pg, nats, mon, want[0], want[1], want[2])
		}
	}
}

// An override must move the port, because that is the ONLY way to run beside
// something else that already holds 5432 — and the DSN has to follow it, or the
// plane and the services would address different ports.
func TestServicePortsHonourTheOverride(t *testing.T) {
	t.Setenv("ORCHICON_POSTGRES_PORT", "7654")
	pg, _, _, err := servicePorts("dev")
	if err != nil {
		t.Fatalf("servicePorts: %v", err)
	}
	if pg != 7654 {
		t.Fatalf("pg = %d, want the pinned 7654", pg)
	}
	if got := serviceDSN(pg); !strings.Contains(got, ":7654/") {
		t.Errorf("the DSN must follow the resolved port; got %s", got)
	}
}

// An instance the table does not define is a REFUSAL, not a fallback to dev's
// column: a silent fallback is how a third instance collides with dev on every
// port (the reason installerPorts uses a map rather than Dev/Prod fields).
func TestServicePortsRefuseAnUnknownInstance(t *testing.T) {
	if _, _, _, err := servicePorts("staging"); err == nil {
		t.Fatal("an unknown instance must be refused, not given another instance's ports")
	}
}

// TEST-DSN SHAPE. `make migrate` and `orchicon services status` must name the
// SAME database, and the shape is what the Makefile's DB_URL resolves to.
func TestServiceDSNShape(t *testing.T) {
	want := "postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable"
	if got := serviceDSN(5433); got != want {
		t.Fatalf("serviceDSN(5433) = %q, want %q", got, want)
	}
}

// HOST POSTGRES ARGS: the instance's port, loopback only, NO setpriv, NO uid 70,
// and the binaries resolved from the ACQUIRED bindir rather than from PATH (a
// developer with no Postgres installed has nothing on PATH to find).
func TestHostPostgresProcArgs(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir(), hostMode: true, pgPort: 5433, binDir: "/opt/pg/bin"}
	p := sup.postgresProc()

	if p.command != filepath.Join("/opt/pg/bin", "postgres") {
		t.Errorf("command = %q, want the acquired bindir's postgres", p.command)
	}
	args := strings.Join(p.args, " ")
	// The uid dance is checked on the ARGUMENTS THEMSELVES, not as a substring of
	// the joined line: `-D <dataDir>` legitimately contains whatever the temp dir
	// is named, and a bare "70" match would fire on a path.
	if p.args[0] == "setpriv" {
		t.Errorf("host postgres must never go through setpriv: %s", args)
	}
	for _, a := range p.args {
		if strings.HasPrefix(a, "--reuid") || strings.HasPrefix(a, "--regid") || a == "--clear-groups" {
			t.Errorf("host postgres must never carry the container's privilege flags: %q in %s", a, args)
		}
	}
	if !strings.Contains(args, "-p 5433") {
		t.Errorf("host postgres must bind the INSTANCE's port, not a literal: %s", args)
	}
	if !strings.Contains(args, "-c listen_addresses=127.0.0.1") {
		t.Errorf("host postgres must listen on LOOPBACK ONLY (trust auth + 0.0.0.0 is a LAN-reachable database): %s", args)
	}
	// The data dir is the instance's own, not /var/lib/orchicon.
	if !strings.Contains(args, "-D "+filepath.Join(sup.dataDir, "postgres")) {
		t.Errorf("host postgres must use the instance's data dir: %s", args)
	}
	if !p.restart {
		t.Error("postgres must be restarted on crash, as in the container path")
	}
}

// HOST POSTGRES MUST BE GIVEN A WRITABLE unix_socket_directories, explicitly.
//
// This is a regression pin, not a style preference: the compiled-in default on
// Debian/Ubuntu (and the upstream theseus-rs build) is /var/run/postgresql, which
// a non-root developer cannot write, so postgres dies at startup with
// `could not create lock file "/var/run/postgresql/.s.PGSQL.<port>.lock":
// Permission denied` and nothing else — measured on this host. The container path
// never hits it (it runs as uid 70 inside an image where that dir is that uid's);
// the host path MUST name its own. Without this the whole feature is dead on the
// default developer host while every unit test still passes.
func TestHostPostgresProcUsesAWritableSocketDir(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir(), hostMode: true, pgPort: 5433, binDir: "/opt/pg/bin"}
	p := sup.postgresProc()
	args := strings.Join(p.args, " ")

	want := "-c unix_socket_directories=" + sup.pgSocketDir()
	if !strings.Contains(args, want) {
		t.Fatalf("host postgres must set an explicit writable socket dir (%q); a non-root user cannot "+
			"write the compiled-in /var/run/postgresql default and postgres dies at startup. args: %s", want, args)
	}
	// The socket dir must live UNDER the instance's data dir — not /var/run, not a
	// shared /tmp — so two instances' sockets are as disjoint as their ports.
	if !strings.HasPrefix(sup.pgSocketDir(), sup.dataDir+string(os.PathSeparator)) {
		t.Errorf("the socket dir %q must live under the instance data dir %q", sup.pgSocketDir(), sup.dataDir)
	}
	if strings.Contains(sup.pgSocketDir(), "/var/run") {
		t.Errorf("the socket dir must not be /var/run/postgresql (unwritable for a non-root user): %q", sup.pgSocketDir())
	}
}

// prepareHost must CREATE the socket dir up front: postgres is started by a later
// step and cannot create it itself (if it could, the explicit -c above would be
// unnecessary).
func TestPrepareHostCreatesTheSocketDir(t *testing.T) {
	dataDir := t.TempDir()
	pgData := filepath.Join(dataDir, "postgres")
	if err := os.MkdirAll(pgData, 0o700); err != nil {
		t.Fatal(err)
	}
	// A PG_VERSION marker makes initdb a no-op, so no real bindir is needed.
	if err := os.WriteFile(filepath.Join(pgData, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sup := &supervisor{log: testLogger(), dataDir: dataDir, hostMode: true, pgPort: 5432,
		binDir: filepath.Join(t.TempDir(), "does-not-exist")}
	if err := sup.prepareHost(t.Context()); err != nil {
		t.Fatalf("prepareHost: %v", err)
	}
	fi, err := os.Stat(sup.pgSocketDir())
	if err != nil {
		t.Fatalf("prepareHost must create the socket dir postgres needs: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("the socket dir must be a directory: %q", sup.pgSocketDir())
	}
}

// postgresListenAddr is the single decision point, so all three modes are pinned
// together: a change to one must not silently move another.
func TestPostgresListenAddrAllModes(t *testing.T) {
	if got := (&supervisor{hostMode: true, servicesOnly: true}).postgresListenAddr(); got != "127.0.0.1" {
		t.Errorf("host mode: got %q, want 127.0.0.1 (loopback only, and host mode WINS over services-only)", got)
	}
	if got := (&supervisor{servicesOnly: true}).postgresListenAddr(); got != "0.0.0.0" {
		t.Errorf("services-only: got %q, want 0.0.0.0 (unchanged)", got)
	}
	if got := (&supervisor{}).postgresListenAddr(); got != "localhost" {
		t.Errorf("default: got %q, want localhost (unchanged)", got)
	}
}

// HOST NATS: the client AND monitor ports are the instance's, and the readiness
// probe polls THIS instance's monitor port rather than the conventional 8222.
func TestHostNatsProcArgs(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir(), hostMode: true,
		natsPort: 4223, natsMonPort: 8223, natsBin: "/opt/nats/bin/nats-server"}
	p := sup.natsProc()

	if p.command != "/opt/nats/bin/nats-server" {
		t.Errorf("command = %q, want the acquired nats-server", p.command)
	}
	args := strings.Join(p.args, " ")
	if !strings.Contains(args, "-p 4223") {
		t.Errorf("host nats must bind the instance's client port: %s", args)
	}
	if !strings.Contains(args, "-m 8223") {
		t.Errorf("host nats must bind the instance's monitor port: %s", args)
	}
}

// The container branch must be untouched by the host branch: a host-mode default
// creeping into the container args would move the container's ports.
func TestNatsProcContainerBranchUnchanged(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir()}
	p := sup.natsProc()
	args := strings.Join(p.args, " ")
	if p.command != "nats-server" {
		t.Errorf("container nats command = %q, want nats-server (from PATH)", p.command)
	}
	if !strings.Contains(args, "-m 8222") || strings.Contains(args, "-p ") {
		t.Errorf("container nats args changed: %s", args)
	}
}

// PID/LOG FILES ARE PER INSTANCE. Two host planes (dev + prod) launched from one
// working directory must not share a PID file — `services down dev` killing
// prod's services is the failure this prevents.
func TestServiceStatePathsArePerInstance(t *testing.T) {
	devPid, prodPid := servicesPIDFile("dev"), servicesPIDFile("prod")
	if devPid == prodPid {
		t.Fatalf("dev and prod share a PID file: %s", devPid)
	}
	if !strings.Contains(devPid, "orchicon-dev") || !strings.Contains(prodPid, "orchicon-prod") {
		t.Errorf("PID files must live under the per-instance data dir: %s / %s", devPid, prodPid)
	}
	if !strings.HasSuffix(devPid, filepath.Join("services", "services.pid")) {
		t.Errorf("PID file location changed: %s", devPid)
	}
	if !strings.HasSuffix(servicesLogFile("dev"), filepath.Join("services", "services.log")) {
		t.Errorf("log file location changed: %s", servicesLogFile("dev"))
	}
	// And they must be under the SAME data dir `orchicon install` uses, or the
	// install's repair path and this command would disagree about the instance.
	if dir := filepath.Dir(filepath.Dir(devPid)); dir != servicesDataDir("dev") {
		t.Errorf("PID file dir %q is not the instance data dir %q", dir, servicesDataDir("dev"))
	}
}

// A host supervisor carries NO container identity: hostUID 0 (never setpriv) and
// servicesOnly false (the plane is not this process's concern at all).
func TestNewHostSupervisorIsHostShaped(t *testing.T) {
	sup, err := newHostSupervisor("dev", testLogger())
	if err != nil {
		t.Fatalf("newHostSupervisor: %v", err)
	}
	if !sup.hostMode {
		t.Error("hostMode must be set")
	}
	if sup.hostUID != 0 || sup.hostGID != 0 {
		t.Errorf("host mode must not run as a HOST uid (got uid=%d gid=%d) — that is setpriv's job in the container", sup.hostUID, sup.hostGID)
	}
	if sup.servicesOnly {
		t.Error("host mode is not services-only-in-a-container: the flag would widen postgres to 0.0.0.0")
	}
	if sup.pgPort != 5432 || sup.natsPort != 4222 || sup.natsMonPort != 8222 {
		t.Errorf("dev ports = %d/%d/%d, want 5432/4222/8222", sup.pgPort, sup.natsPort, sup.natsMonPort)
	}
	if sup.dataDir != servicesDataDir("dev") {
		t.Errorf("dataDir = %q, want %q", sup.dataDir, servicesDataDir("dev"))
	}
	// binDir is populated by the CALLER (after acquisition), deliberately: the
	// constructor must not download anything.
	if sup.binDir != "" {
		t.Errorf("newHostSupervisor must not acquire binaries; got binDir=%q", sup.binDir)
	}
}

// prepareHost must refuse a populated-bindir-less supervisor with a message that
// names the missing step, rather than failing later at exec.
func TestPrepareHostRefusesAnEmptyBindir(t *testing.T) {
	sup := &supervisor{log: testLogger(), dataDir: t.TempDir(), hostMode: true, pgPort: 5432}
	err := sup.prepareHost(t.Context())
	if err == nil {
		t.Fatal("prepareHost must refuse an empty bindir")
	}
	if !strings.Contains(err.Error(), "bindir") {
		t.Errorf("the refusal must name the bindir; got %v", err)
	}
	// It must still have created the data dirs before refusing, because those are
	// what the NEXT run (after acquisition) needs.
	for _, sub := range []string{"postgres", "nats"} {
		if _, statErr := os.Stat(filepath.Join(sup.dataDir, sub)); statErr != nil {
			t.Errorf("prepareHost did not create %s: %v", sub, statErr)
		}
	}
}

// prepareHost is IDEMPOTENT on an already-initialized cluster: a PG_VERSION
// marker means initdb is skipped, so a second `services up` is a no-op. Verified
// with a bindir that cannot run — proof initdb was never invoked.
func TestPrepareHostSkipsInitdbWhenTheClusterExists(t *testing.T) {
	dataDir := t.TempDir()
	pgData := filepath.Join(dataDir, "postgres")
	if err := os.MkdirAll(pgData, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pgData, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sup := &supervisor{log: testLogger(), dataDir: dataDir, hostMode: true, pgPort: 5432,
		binDir: filepath.Join(t.TempDir(), "does-not-exist")}
	if err := sup.prepareHost(t.Context()); err != nil {
		t.Fatalf("an initialized cluster must need no initdb: %v", err)
	}
}

// The verbs are a closed set, and the DEFAULT is `status`: a bare
// `orchicon services` must never be the invocation that starts a database.
func TestServicesVerbSetAndDefault(t *testing.T) {
	for _, v := range []string{"up", "down", "status"} {
		if !isServicesVerb(v) {
			t.Errorf("%q must be a recognised verb", v)
		}
	}
	for _, v := range []string{"", "start", "stop", "UP", "--up"} {
		if isServicesVerb(v) {
			t.Errorf("%q must NOT be a recognised verb", v)
		}
	}
}

// The detached child is told which instance to run by argv, and the marker is the
// ONLY difference from the parent's invocation — so the child must resolve the
// same instance the parent did.
func TestServicesInstanceArg(t *testing.T) {
	if got := servicesInstanceArg([]string{"up", "prod"}); got != "prod" {
		t.Errorf("got %q, want prod", got)
	}
	if got := servicesInstanceArg([]string{"up"}); got != "dev" {
		t.Errorf("no instance: got %q, want the dev default", got)
	}
	t.Setenv("ORCHICON_INSTANCE", "test")
	if got := servicesInstanceArg([]string{"up"}); got != "test" {
		t.Errorf("ORCHICON_INSTANCE must be honoured; got %q", got)
	}
}

// THE IDEMPOTENCE GUARD. `services up` must check the instance's postgres port
// BEFORE it does anything expensive, which is what makes it safe as a Makefile
// prerequisite. Asserted structurally (a real listener is the integration test's
// job) because the alternative is booting a cluster in a unit test.
func TestServicesUpProbesBeforeStarting(t *testing.T) {
	src, err := os.ReadFile("services.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	probe := strings.Index(body, "func servicesUp(instance string) int {")
	if probe < 0 {
		t.Fatal("servicesUp is gone — update this test's anchor")
	}
	rest := body[probe:]
	guard := strings.Index(rest, "tcpReady(ctx, \"127.0.0.1:\"+strconv.Itoa(pg))")
	acquire := strings.Index(rest, "ensureServiceBinaries(")
	if guard < 0 {
		t.Fatal("servicesUp no longer probes the postgres port — idempotence is gone")
	}
	if acquire < 0 {
		t.Fatal("servicesUp no longer acquires binaries — update this test's anchor")
	}
	if guard > acquire {
		t.Error("the idempotence guard must come BEFORE binary acquisition: an already-running " +
			"instance must not trigger a download (or restart) at all")
	}
	// ...and it must report rather than restart.
	if !strings.Contains(rest, "already running") {
		t.Error("the guard must SAY the services are already running, not exit silently")
	}
}

// THE STOP PATH MUST VERIFY. A stop that signals and reports success without
// checking is how a lingering postgres holding 5432 makes the next start fail
// confusingly — the lesson serveStop already learned. Structural, because the
// integration test (a real supervisor) covers the behaviour.
func TestServicesDownVerifiesThePortsAreFree(t *testing.T) {
	src, err := os.ReadFile("services.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "waitForPortsFree(") {
		t.Error("servicesDown must verify the ports are free, not merely signal")
	}
	// The PID file is removed ONLY on the verified-gone path: a survivor must keep
	// its PID file so `status` can still name it. The LAST removal is the one on the
	// verified path — the earlier one clears a PID file naming a DEAD process, which
	// is provably not a survivor.
	probe := strings.Index(body, "func servicesDown(instance string) int {")
	rest := body[probe:]
	verify := strings.Index(rest, "gone := waitForPortsFree(")
	remove := strings.LastIndex(rest, "_ = os.Remove(pidFile)")
	if verify < 0 || remove < 0 || remove < verify {
		t.Error("the PID file must be removed only AFTER the port verification confirms the services are gone")
	}
	// ...and the escalation must be there: a postgres that ignores SIGTERM is the
	// "it said stopped but 5432 is still held" case.
	if !strings.Contains(rest, "escalating to SIGKILL") {
		t.Error("servicesDown must escalate to SIGKILL when SIGTERM did not take")
	}
	// ...and it must not kill a process it did not start.
	if !strings.Contains(rest, "refusing to kill an unidentified process") {
		t.Error("servicesDown must refuse to kill a process with no recorded PID file (that is the " +
			"instance container's postgres) rather than guessing")
	}
}

// ensureServiceBinaries prefers a HOST install over downloading a second one.
// Driven by ORCHICON_PG_BINDIR so it needs no system Postgres.
func TestEnsureServiceBinariesPrefersAHostInstall(t *testing.T) {
	dir := t.TempDir()
	for _, b := range []string{"initdb", "postgres", "nats-server"} {
		writeExecutable(t, filepath.Join(dir, b))
	}
	t.Setenv("ORCHICON_PG_BINDIR", dir)
	t.Setenv("PATH", dir) // ...and a nats-server beside them

	got, err := ensureServiceBinaries(t.TempDir())
	if err != nil {
		t.Fatalf("ensureServiceBinaries: %v", err)
	}
	if got.PG != dir {
		t.Errorf("PG bindir = %q, want the host install %q (a second PostgreSQL must not be downloaded)", got.PG, dir)
	}
	if got.NATS != filepath.Join(dir, "nats-server") {
		t.Errorf("NATS = %q, want the one on PATH", got.NATS)
	}
	if got.Source != "host PATH" {
		t.Errorf("Source = %q, want \"host PATH\" so `up` can report where they came from", got.Source)
	}
}

// A bindir with initdb but NO postgres is a broken install: returning it would
// fail later at exec with a confusing error, so the probe must reject it.
func TestHostPostgresBinDirRequiresBothBinaries(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "initdb")) // postgres deliberately absent
	t.Setenv("ORCHICON_PG_BINDIR", dir)

	if got := hostPostgresBinDir(); got == dir {
		t.Error("a bindir without postgres must not be accepted: the failure would surface much later")
	}
}

// fetchVerified must REFUSE a payload whose digest does not match, and must not
// leave it in place. This is the rule `make tools` applies to buf/atlas, and it is
// what stops a truncated or substituted download becoming the developer's
// PostgreSQL. Driven against a local file:// server equivalent (an httptest
// server) so the test needs no network.
func TestFetchVerifiedRefusesABadDigest(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.tgz")
	if err := os.WriteFile(dest, []byte("not the pinned bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dest whose digest is wrong is treated as a stale partial download and
	// retried; here the retry hits an unreachable URL, so the call must FAIL
	// rather than accept the wrong bytes.
	err := fetchVerified("http://127.0.0.1:1/nothing", strings.Repeat("0", 64), dest)
	if err == nil {
		t.Fatal("fetchVerified accepted an archive it could not verify")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("an unverifiable artifact must not be left in place")
	}
}

// ...and a dest that ALREADY matches is a no-op: no download, which is what makes
// the Makefile prerequisite cheap on the second `make run`.
func TestFetchVerifiedSkipsAMatchingArtifact(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.tgz")
	if err := os.WriteFile(dest, []byte("pinned bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(dest)
	if err != nil {
		t.Fatal(err)
	}
	// An unreachable URL proves no network was touched.
	if err := fetchVerified("http://127.0.0.1:1/nothing", sum, dest); err != nil {
		t.Fatalf("a matching artifact must be a no-op, got %v", err)
	}
}

// The pinned URLs must be complete and per-arch, so a missing pin is a compile-
// time-visible failure rather than a silent fall-through to another arch's build.
func TestPinnedReleaseURLsAndDigests(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		u, ok := pgReleaseURL(arch)
		if !ok || !strings.Contains(u, postgresPinnedVersion) {
			t.Errorf("pgReleaseURL(%q) = %q, ok=%v", arch, u, ok)
		}
		if sha, ok := pgReleaseSHA256(arch); !ok || len(sha) != 64 {
			t.Errorf("pgReleaseSHA256(%q) = %q, ok=%v (want a 64-char hex digest)", arch, sha, ok)
		}
		u, ok = natsReleaseURL(arch)
		if !ok || !strings.Contains(u, natsPinnedVersion) {
			t.Errorf("natsReleaseURL(%q) = %q, ok=%v", arch, u, ok)
		}
		if sha, ok := natsReleaseSHA256(arch); !ok || len(sha) != 64 {
			t.Errorf("natsReleaseSHA256(%q) = %q, ok=%v", arch, sha, ok)
		}
	}
	// An unsupported arch must be reported, not silently served another's binary.
	if _, ok := pgReleaseURL("riscv64"); ok {
		t.Error("an unpinned arch must not resolve to a URL")
	}
	if _, ok := natsReleaseSHA256("riscv64"); ok {
		t.Error("an unpinned arch must not resolve to a digest")
	}
}

// stripComponents is the archive-path rule: a wrong strip writes the tree one
// level too deep (bin/postgresql-16.6.0-x86_64/bin/postgres), and the binaries
// would then not be found where the supervisor expects them.
func TestStripComponents(t *testing.T) {
	cases := map[string]string{
		"postgresql-16.6.0-x86_64-unknown-linux-gnu/bin/postgres": "bin/postgres",
		"nats-server-v2.11.6-linux-amd64/nats-server":             "nats-server",
		"./postgresql-16.6.0/bin/initdb":                          "bin/initdb",
		"postgresql-16.6.0/":                                      "",
	}
	for in, want := range cases {
		if got := stripComponents(in, 1); got != want {
			t.Errorf("stripComponents(%q, 1) = %q, want %q", in, got, want)
		}
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
