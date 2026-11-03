package db_test

// workflow_run_error_test.go — the run's FAILURE round-trips through every read path.
//
// WHY THIS EXISTS AS A DB TEST. Adding a column to workflow_runs means editing SIXTEEN select lists and every
// scan that consumes them, and Go cannot check SQL: a list with one more column than its destinations compiles
// perfectly and fails at runtime with "number of field descriptions must equal number of destinations".
//
// That is exactly what happened while building this — CreateWorkflowRun went to 22 columns and 20 destinations,
// every structural check I ran in the editor passed, and the Database caught it. This test is the assertion
// that makes the next one fail in CI instead of on an operator's first run.

import (
	"context"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"

	"github.com/oklog/ulid/v2"
)

func TestWorkflowRunErrorRoundTrips(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed workflow run error test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	// A tenant per run, so concurrent test processes never contend on the row.
	tenantID := "tnt_err_" + ulid.Make().String()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = ttx.Rollback(ctx) }()
	if _, err := ttx.Tx.Exec(ctx,
		`INSERT INTO tenants (id, name, slug, created_at, updated_at) VALUES ($1,$1,$1,now(),now())`, tenantID); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit tenant: %v", err)
	}

	runID := ulid.Make().String()
	const (
		msg  = "This repository has no valid HEAD to branch from.\n\nDetail: fatal: invalid reference: HEAD"
		kind = "worktree_no_base_ref"
	)

	// CREATE: a fresh run carries NO error (the columns default to ''), and its scan must be consistent.
	ttx, err = pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	created, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: runID, TenantID: tenantID, WorkflowID: "wf-1", WorkflowVersion: 1,
		ProjectID: "proj-1", Status: "pending", RunContext: []byte("{}"),
	})
	if err != nil {
		ttx.Rollback(ctx)
		t.Fatalf("create run (a column/scan mismatch fails HERE): %v", err)
	}
	if created.ErrorMessage != "" || created.ErrorKind != "" {
		t.Errorf("a new run carried an error: %q / %q", created.ErrorMessage, created.ErrorKind)
	}

	// UPDATE: record the failure the way the worktree reconciler does.
	fields := db.UpdateWorkflowRunFields{
		Status:       strPtrDB("failed"),
		ErrorMessage: strPtrDB(msg),
		ErrorKind:    strPtrDB(kind),
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, created.Version, fields); err != nil {
		ttx.Rollback(ctx)
		t.Fatalf("update run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// GET: the reason is readable as a COLUMN, not buried in run_context.
	ttx, err = pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = ttx.Rollback(ctx) }()
	got, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.ErrorMessage != msg {
		t.Errorf("error_message = %q, want the recorded message", got.ErrorMessage)
	}
	if got.ErrorKind != kind {
		t.Errorf("error_kind = %q, want %q", got.ErrorKind, kind)
	}
	// The multi-line shape must survive verbatim: the GUI renders it with the blank line intact.
	if got.ErrorMessage != msg {
		t.Errorf("the message was altered in the round trip: %q", got.ErrorMessage)
	}
}

func strPtrDB(s string) *string { return &s }
