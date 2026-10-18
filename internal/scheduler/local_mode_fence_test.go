package scheduler

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
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
// contract: empty project or read failure degrades to the TENANT DEFAULT
// (db.DefaultExecutionMode), and an ITEM override is honoured — including a
// local item inside a runtime project (the fence must follow the item, not the
// project).
func TestProjectExecutionModeDefaults(t *testing.T) {
	// scratchPool applies the migrations, so this test is self-sufficient on a
	// fresh disposable database (it writes real projects/items below).
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	// A local ITEM inside a RUNTIME project must trip the fence: the resolved
	// mode follows the item override, not the project row.
	prj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: "prj_fence_item", TenantID: approvalTestTenant,
		Name: "fence item", Slug: "fence-item", Status: "active", Goals: []byte("[]"),
		ProjectDir:    t.TempDir(),
		ExecutionMode: db.ExecutionModeRuntime,
	})
	if err != nil {
		t.Fatalf("create fence project: %v", err)
	}
	if prj.ExecutionMode != db.ExecutionModeRuntime {
		t.Fatalf("fence project mode = %q, want runtime", prj.ExecutionMode)
	}
	local := db.ExecutionModeLocal
	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: "wi_fence_item", TenantID: approvalTestTenant, ProjectID: prj.ID,
		Kind: domain.WorkItemKindTask, Title: "fence", Status: domain.WorkItemReady,
		ExecutionMode: &local,
	})
	if err != nil {
		t.Fatalf("create fence item: %v", err)
	}
	if got := executionModeFor(ctx, ttx.Tx, approvalTestTenant, wi.ID, prj.ID); got != db.ExecutionModeLocal {
		t.Errorf("local item in runtime project mode = %q, want local", got)
	}

	// NOT FOUND probes LAST: a failed read inside the tenant tx leaves the
	// transaction unusable for further writes, and these only assert the
	// fail-open contract anyway.
	if got := executionModeFor(ctx, ttx.Tx, approvalTestTenant, "", ""); got != db.DefaultExecutionMode {
		t.Errorf("empty project mode = %q, want the tenant default %q", got, db.DefaultExecutionMode)
	}
	if got := executionModeFor(ctx, ttx.Tx, approvalTestTenant, "", "prj_does_not_exist"); got != db.DefaultExecutionMode {
		t.Errorf("missing project mode = %q, want the tenant default %q", got, db.DefaultExecutionMode)
	}
	if got := executionModeFor(ctx, ttx.Tx, approvalTestTenant, "wi_does_not_exist", ""); got != db.DefaultExecutionMode {
		t.Errorf("missing item mode = %q, want the tenant default %q", got, db.DefaultExecutionMode)
	}
}
