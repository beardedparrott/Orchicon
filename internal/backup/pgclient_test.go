package backup

// pgclient_test.go — backup reaches Postgres with a LOCAL client, on every install.
//
// The retired single-container shape used to route the tools through `docker exec`.
// The plane and its Postgres are host processes now, so these tests pin the one
// remaining resolution order (the instance's own resolved record, then PATH) and the
// argv that reaches the tool — "it resolved" is not the same claim as "the flags are
// the same ones the local path used".

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const devDSN = "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"

// A backup of a local install runs the LOCAL tool, with every flag, and the dump
// lands in the file. This is the shape every install has now.
func TestCreateUsesALocalTool(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	installFakeTool(t, pgToolDump, fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" > %q
echo "-- fake local dump"
`, argvFile))
	t.Setenv("ORCHICON_DATA_DIR", "")

	info, err := Create(context.Background(), devDSN, dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	got := strings.TrimSpace(string(argv))
	want := "-d " + devDSN + " --clean --if-exists --no-owner --no-acl"
	if got != want {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
	if fi, err := os.Stat(info.Path); err != nil || fi.Size() == 0 {
		t.Fatalf("the dump did not land: %v", err)
	}
}

// Restore pipes the dump into psql's stdin, which is how a restore works without a
// temp step. If that ever stopped being stdin, this fails instead of production.
func TestRestorePipesTheDumpIntoTheLocalTool(t *testing.T) {
	dir := t.TempDir()
	stdinFile := filepath.Join(dir, "stdin")
	argvFile := filepath.Join(dir, "argv")
	installFakeTool(t, pgToolRestore, fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" > %q
cat > %q
`, argvFile, stdinFile))
	t.Setenv("ORCHICON_DATA_DIR", "")

	dumpPath := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(dumpPath, []byte("SELECT 1;\n"), 0o600); err != nil {
		t.Fatalf("seed dump: %v", err)
	}
	if err := Restore(context.Background(), devDSN, dumpPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("psql never received the dump on stdin: %v", err)
	}
	if string(got) != "SELECT 1;\n" {
		t.Fatalf("stdin = %q, want the dump piped through", string(got))
	}
	argv, _ := os.ReadFile(argvFile)
	want := "-d " + devDSN + " -v ON_ERROR_STOP=1 --single-transaction"
	if got := strings.TrimSpace(string(argv)); got != want {
		t.Fatalf("argv = %q\nwant %q", got, want)
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

// ORCHICON_DATA_DIR names the instance's own state dir, and cmd/orchicon/binaries.go
// records the resolved PostgreSQL distribution there. On a host whose Postgres came
// from the DOWNLOAD path that distribution is at <DataDir>/bin/postgres/bin — on NO
// PATH at all — so without this the local client is unfindable on the very install
// the resolver provisioned. This is that case, pinned.
func TestLocalClientPrefersTheResolvedRecord(t *testing.T) {
	dataDir := t.TempDir()
	toolsDir := filepath.Join(dataDir, "bin", "postgres", "bin")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved := filepath.Join(toolsDir, "pg_dump")
	if err := os.WriteFile(resolved, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A record shaped exactly like the resolver writes it (JSON field names).
	rec := fmt.Sprintf(`{"source":"downloaded","pg_dump":%q,"psql":%q}`, resolved, filepath.Join(toolsDir, "psql"))
	if err := os.WriteFile(filepath.Join(dataDir, "infra-binaries.json"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCHICON_DATA_DIR", dataDir)
	// NO pg_dump ON PATH, so only the record can resolve it.
	t.Setenv("PATH", t.TempDir())

	got, err := resolvePGClient(devDSN).execPath(pgToolDump)
	if err != nil {
		t.Fatalf("the resolved record was not consulted: %v", err)
	}
	if got != resolved {
		t.Fatalf("execPath = %q, want the resolved absolute path %q", got, resolved)
	}
}

// A MISSING or malformed record must degrade to the PATH lookup and its actionable
// message — a broken record can never be what makes backup impossible.
func TestBrokenRecordDegradesToPathAndStillExplains(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "infra-binaries.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCHICON_DATA_DIR", dataDir)
	t.Setenv("PATH", t.TempDir())

	_, err := resolvePGClient(devDSN).execPath(pgToolDump)
	if err == nil {
		t.Fatal("with no record and no PATH there is no client, and that must be an error")
	}
	// The error must offer a next step, and it must NOT send the operator looking for
	// a container: the container shape is gone and naming one would be a dead end.
	for _, want := range []string{"postgresql-client", "ORCHICON_DATA_DIR", "infra-binaries.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must name %q, got: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "container") {
		t.Fatalf("the error must not point at the retired container client, got: %v", err)
	}
}

// The DSN reaches the tool UNCHANGED: there is no container whose own view of
// Postgres would require rewriting the host's published port.
func TestDSNIsPassedThroughUnchanged(t *testing.T) {
	dsn := "postgres://orchicon:orchicon@db.internal:5433/orchicon?sslmode=disable"
	if got := resolvePGClient(dsn).dsn; got != dsn {
		t.Fatalf("dsn = %q, want it passed through unchanged", got)
	}
}
