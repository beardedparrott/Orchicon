package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, rule := range pgHBATrustRules {
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
func TestServicesOnlyGateSkipsPlaneChildAndLogsIt(t *testing.T) {
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
}
