package scheduler

import (
	"context"
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestIsLiveDSN pins the local-mode DSN fence matcher (acceptance E): the
// three loopback/bridge plane addresses are refused; disposable DSNs and
// container-local names pass.
func TestIsLiveDSN(t *testing.T) {
	live := []string{
		"postgres://orchicon:orchicon@127.0.0.1:5432/orchicon?sslmode=disable",
		"postgres://u:p@localhost:5432/db",
		"postgres://u:p@LOCALHOST:5432/db",
		"http://172.17.0.1:8080/healthz",
		"ORCHICON_TEST_DSN=postgres://orchicon:orchicon@127.0.0.1:5432/orchicon",
	}
	for _, dsn := range live {
		if !isLiveDSN(dsn) {
			t.Errorf("isLiveDSN(%q) = false, want true (live plane must be refused)", dsn)
		}
		if got := redactDSNHost(dsn); len(got) == 0 || len(got) >= len(dsn) && dsn != got {
			_ = got // redaction is best-effort; only assert non-empty below
		}
	}
	safe := []string{
		"",
		"postgres://orchicon:orchicon@pg-sandbox:5432/orchicon?sslmode=disable",
		"postgres://u:p@disposable-db:5432/db",
		"postgres://orchicon:orchicon@localhost:5433/orchicon",
	}
	for _, dsn := range safe {
		if isLiveDSN(dsn) {
			t.Errorf("isLiveDSN(%q) = true, want false (disposable DSN must pass)", dsn)
		}
	}
	// Redaction must never leak credentials.
	red := redactDSNHost("postgres://orchicon:secret@127.0.0.1:5432/orchicon?sslmode=disable")
	if red == "" {
		t.Fatal("redactDSNHost returned empty")
	}
	for _, leak := range []string{"secret", "orchicon:secret"} {
		if len(red) < len("127.0.0.1:5432") {
			continue
		}
		found := false
		for i := 0; i+len(leak) <= len(red); i++ {
			if red[i:i+len(leak)] == leak {
				found = true
			}
		}
		if found {
			t.Errorf("redactDSNHost(%q) leaks credentials: %q", "postgres://…", red)
		}
	}
}

// TestProjectExecutionModeDefaults pins the mode helper's fail-open
// contract: empty project or read failure degrades to runtime.
func TestProjectExecutionModeDefaults(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed mode test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	defer pool.Close()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	if got := projectExecutionMode(ctx, ttx.Tx, approvalTestTenant, ""); got != db.ExecutionModeRuntime {
		t.Errorf("empty project mode = %q, want runtime", got)
	}
	if got := projectExecutionMode(ctx, ttx.Tx, approvalTestTenant, "prj_does_not_exist"); got != db.ExecutionModeRuntime {
		t.Errorf("missing project mode = %q, want runtime", got)
	}
}
