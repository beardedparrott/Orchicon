package backup

// pgclient_test.go — backup reaches Postgres in EVERY residency.
//
// The operator's report was a HOST-residency install:
//
//	Error: [internal] backup: pg_dump: exec: "pg_dump": executable file not found in $PATH
//
// The host has docker and no postgresql-client; Postgres is inside the instance's
// services container behind a loopback publish (dev 5432, prod 5433). These tests
// pin the resolution rules AND the argv that reaches the tool, because "it
// resolved" is not the same claim as "the flags are the same ones the local path
// used".

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProber stands in for the host probes, so none of this needs docker.
type fakeProber struct {
	dockerOnPath bool
	running      map[string]bool
	portOut      map[string]string
	portErr      error
}

func (f fakeProber) lookPath(name string) (string, error) {
	if name == "docker" && f.dockerOnPath {
		return "/usr/bin/docker", nil
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
}

func (f fakeProber) output(name string, args ...string) (string, error) {
	if name != "docker" || len(args) == 0 {
		return "", fmt.Errorf("unexpected probe: %s %v", name, args)
	}
	switch args[0] {
	case "inspect":
		container := args[len(args)-1]
		return fmt.Sprintf("%t", f.running[container]), nil
	case "port":
		if f.portErr != nil {
			return "", f.portErr
		}
		return f.portOut[args[1]], nil
	}
	return "", fmt.Errorf("unexpected probe: %s %v", name, args)
}

// useProber installs a fake prober for one test and restores the real one.
func useProber(t *testing.T, f fakeProber) {
	t.Helper()
	old := probe
	probe = f
	t.Cleanup(func() { probe = old })
}

// hostResidencyProber is the operator's real shape: docker present, no
// postgresql-client on the host, the instance's container up and publishing.
func hostResidencyProber() fakeProber {
	return fakeProber{
		dockerOnPath: true,
		running:      map[string]bool{"orchicon-cnt-dev": true, "orchicon-cnt-prod": true},
		portOut:      map[string]string{"orchicon-cnt-dev": "127.0.0.1:5432", "orchicon-cnt-prod": "127.0.0.1:5433"},
	}
}

const (
	devDSN  = "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"
	prodDSN = "postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable"
)

// THE BUG: a host-residency plane, no env var, no local client — the instance's
// container must be discovered and the DSN translated to that container's own
// view (prod publishes 5433; Postgres inside listens on 5432).
func TestResolveDiscoversTheInstanceContainer(t *testing.T) {
	useProber(t, hostResidencyProber())
	t.Setenv(pgContainerEnv, "")
	t.Setenv("ORCHICON_INSTANCE", "prod")

	client, err := resolvePGClient(prodDSN)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if client.container != "orchicon-cnt-prod" {
		t.Fatalf("container = %q, want orchicon-cnt-prod", client.container)
	}
	if client.local() {
		t.Fatal("the instance container must be used, not a local (absent) client")
	}
	// The published port cannot be used INSIDE the container: nothing listens on
	// 5433 there, so an unrewritten DSN would fail with a connection error.
	if !strings.Contains(client.dsn, "@localhost:5432/") {
		t.Fatalf("the container's DSN port was not rewritten: %q", client.dsn)
	}
	// …and nothing else about the DSN may change: same credentials, database and
	// options, so the dump is of the same database over the same settings.
	for _, want := range []string{"orchicon:orchicon@", "/orchicon?", "sslmode=disable"} {
		if !strings.Contains(client.dsn, want) {
			t.Fatalf("rewritten DSN dropped %q: %q", want, client.dsn)
		}
	}
}

// An explicitly named container WINS, and needs no probing at all — that is the
// launcher's and the installer's way of being unambiguous.
func TestNamedContainerWinsWithoutProbing(t *testing.T) {
	// A prober that fails every probe: if resolution probed, it would fall through.
	useProber(t, fakeProber{})
	t.Setenv(pgContainerEnv, "orchicon-cnt-prod")

	client, err := resolvePGClient(prodDSN)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if client.container != "orchicon-cnt-prod" || !strings.Contains(client.dsn, ":5432/") {
		t.Fatalf("named container not honoured: %+v", client)
	}
}

// A NON-loopback DSN is a server this host reaches directly (a remote or
// separately-managed Postgres). Exec'ing into a local container would be wrong,
// so the local client is used.
func TestRemoteDSNStaysLocal(t *testing.T) {
	useProber(t, hostResidencyProber())
	t.Setenv(pgContainerEnv, "")
	dsn := "postgres://orchicon:orchicon@db.internal:5432/orchicon?sslmode=disable"
	client, err := resolvePGClient(dsn)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !client.local() {
		t.Fatalf("a remote DSN must not be routed through a local container: %+v", client)
	}
	if client.dsn != dsn {
		t.Fatalf("the local DSN must be passed through unchanged, got %q", client.dsn)
	}
}

// A CONTAINER-RESIDENT plane has no docker CLI (the image does not ship one) and
// does have pg_dump/psql. Discovery must not invent a container there.
func TestNoDockerMeansLocalClient(t *testing.T) {
	useProber(t, fakeProber{dockerOnPath: false})
	t.Setenv(pgContainerEnv, "")

	client, err := resolvePGClient(devDSN)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !client.local() {
		t.Fatalf("without docker the client must be local: %+v", client)
	}
}

// The port mapping is the GUARD against dumping the wrong database: if the
// container does not publish OUR port, it is not the database this DSN names.
func TestPortMismatchFallsBackToLocal(t *testing.T) {
	useProber(t, hostResidencyProber())
	t.Setenv(pgContainerEnv, "")
	t.Setenv("ORCHICON_INSTANCE", "dev") // orchicon-cnt-dev publishes 5432…
	// …but this DSN names 5433, which the dev container does NOT publish.
	client, err := resolvePGClient(prodDSN)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !client.local() {
		t.Fatalf("a container publishing a different port must not be used: %+v", client)
	}
}

// A stopped container cannot be exec'd into.
func TestStoppedContainerFallsBackToLocal(t *testing.T) {
	p := hostResidencyProber()
	p.running["orchicon-cnt-dev"] = false
	useProber(t, p)
	t.Setenv(pgContainerEnv, "")
	t.Setenv("ORCHICON_INSTANCE", "dev")

	client, _ := resolvePGClient(devDSN)
	if !client.local() {
		t.Fatalf("a stopped container must not be used: %+v", client)
	}
}

// docker port prints a v6 wildcard as ":::5433"; the host port is still the last
// colon field, and that is the one compared.
func TestIPv6WildcardPortMappingIsParsed(t *testing.T) {
	p := hostResidencyProber()
	p.portOut["orchicon-cnt-prod"] = ":::5433"
	useProber(t, p)
	t.Setenv(pgContainerEnv, "")
	t.Setenv("ORCHICON_INSTANCE", "prod")

	client, err := resolvePGClient(prodDSN)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if client.container != "orchicon-cnt-prod" {
		t.Fatalf("the :::5433 mapping was not recognised: %+v", client)
	}
}

// NO CLIENT AND NO CONTAINER must produce guidance, not the bare exec error the
// operator had to decode.
func TestNoClientNoContainerIsActionable(t *testing.T) {
	useProber(t, fakeProber{dockerOnPath: true})
	t.Setenv(pgContainerEnv, "")
	t.Setenv("ORCHICON_INSTANCE", "dev")
	// A DSN that cannot be a local services container (non-loopback) and no local
	// pg_dump on this PATH.
	t.Setenv("PATH", t.TempDir())

	client, err := resolvePGClient("postgres://orchicon:orchicon@db.internal:5432/orchicon?sslmode=disable")
	if err != nil {
		t.Fatalf("resolve must not fail here; the TOOL resolution reports it: %v", err)
	}
	_, err = client.command(context.Background(), pgToolDump, "-d", client.dsn)
	if err == nil {
		t.Fatal("a missing tool must be an error")
	}
	if !strings.Contains(err.Error(), pgContainerEnv) {
		t.Fatalf("the error must name %s so the operator has a next step, got: %v", pgContainerEnv, err)
	}
}

// A non-URL DSN cannot be translated for a container, and silently sending the
// host's published port inside the container would fail obscurely — so it is
// refused with what to do instead.
func TestNonURLDSNWithContainerIsRefused(t *testing.T) {
	useProber(t, fakeProber{})
	t.Setenv(pgContainerEnv, "orchicon-cnt-dev")

	_, err := resolvePGClient("host=localhost port=5432 dbname=orchicon")
	if err == nil {
		t.Fatal("a keyword/value DSN cannot be rewritten for a container and must be refused")
	}
	if !strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("the refusal must say what DSN form to use, got: %v", err)
	}
}

// THE ARGV IS THE SAME IN BOTH RESIDENCIES. This is the half that would rot
// silently: the resolver could pick the right container and still drop --clean.
func TestCreateReachesTheContainerToolWithEveryFlag(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	installFakeTool(t, "docker", fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" > %q
echo "-- fake container dump"
`, argvFile))

	useProber(t, hostResidencyProber())
	t.Setenv(pgContainerEnv, "")
	t.Setenv("ORCHICON_INSTANCE", "prod")

	info, err := Create(context.Background(), prodDSN, dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body, err := os.ReadFile(info.Path)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	if !strings.Contains(string(body), "-- fake container dump") {
		t.Fatalf("the tool's stdout did not reach the backup file: %q", string(body))
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the container tool was never run: %v", err)
	}
	got := strings.TrimSpace(string(argv))
	want := "exec -i orchicon-cnt-prod pg_dump -d " + strings.Replace(prodDSN, ":5433", ":5432", 1) +
		" --clean --if-exists --no-owner --no-acl"
	if got != want {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
}

// RESTORE FEEDS STDIN, which only works because exec uses -i. A missing -i would
// fail here rather than in production.
func TestRestoreFeedsTheContainerToolOnStdin(t *testing.T) {
	dir := t.TempDir()
	stdinFile := filepath.Join(dir, "stdin")
	installFakeTool(t, "docker", fmt.Sprintf(`#!/bin/sh
cat > %q
`, stdinFile))

	useProber(t, hostResidencyProber())
	t.Setenv(pgContainerEnv, "")
	t.Setenv("ORCHICON_INSTANCE", "prod")

	dumpPath := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(dumpPath, []byte("SELECT 1;\n"), 0o600); err != nil {
		t.Fatalf("seed dump: %v", err)
	}
	if err := Restore(context.Background(), prodDSN, dumpPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("the container's psql never received the dump: %v", err)
	}
	if string(got) != "SELECT 1;\n" {
		t.Fatalf("stdin = %q, want the dump piped through", string(got))
	}
}

// The LOCAL path still works exactly as before — a container-resident plane or a
// host with its own postgresql-client is not regressed by this change.
func TestCreateStillUsesALocalTool(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	installFakeTool(t, "pg_dump", fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" > %q
echo "-- fake local dump"
`, argvFile))

	useProber(t, fakeProber{dockerOnPath: false})
	t.Setenv(pgContainerEnv, "")

	info, err := Create(context.Background(), devDSN, dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	got := strings.TrimSpace(string(argv))
	want := "-d " + devDSN + " --clean --if-exists --no-owner --no-acl"
	if got != want {
		t.Fatalf("local argv = %q\nwant %q", got, want)
	}
	if fi, err := os.Stat(info.Path); err != nil || fi.Size() == 0 {
		t.Fatalf("the local dump did not land: %v", err)
	}
}

// installFakeTool writes an executable stub named `name` into a temp dir and
// PREPENDS that dir to PATH, so exec.LookPath finds the stub and not the real
// binary.
func installFakeTool(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
