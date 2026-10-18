package db_test

// DB-backed proof of the execution_mode default at the DATA-ACCESS layer and at
// the COLUMN (AC1's db entry point + AC2). Skipped unless ORCHICON_TEST_DSN
// points at a disposable database:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestProjectExecutionMode -v

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

func createProjectWithMode(t *testing.T, pool *db.Pool, tenantID, mode string) db.ProjectRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID:            db.NewID(),
		TenantID:      tenantID,
		Name:          "mode " + db.NewID(),
		Slug:          "mode-" + db.NewID(),
		Status:        "active",
		Goals:         []byte("[]"),
		ExecutionMode: mode,
	})
	if err != nil {
		t.Fatalf("CreateProject(mode=%q): %v", mode, err)
	}
	return row
}

// TestProjectExecutionModeCreateDefault proves an UNSET mode lands in local.
func TestProjectExecutionModeCreateDefault(t *testing.T) {
	pool := openTestPool(t)
	const tenantID = "tnt_dev"

	if row := createProjectWithMode(t, pool, tenantID, ""); row.ExecutionMode != db.ExecutionModeLocal {
		t.Fatalf("unset mode created %q, want %q", row.ExecutionMode, db.ExecutionModeLocal)
	}
	if row := createProjectWithMode(t, pool, tenantID, db.ExecutionModeRuntime); row.ExecutionMode != db.ExecutionModeRuntime {
		t.Fatalf("explicit runtime created %q, want runtime", row.ExecutionMode)
	}
	if row := createProjectWithMode(t, pool, tenantID, db.ExecutionModeLocal); row.ExecutionMode != db.ExecutionModeLocal {
		t.Fatalf("explicit local created %q, want local", row.ExecutionMode)
	}
}

// TestProjectExecutionModeInvalidRejected proves a bogus mode never reaches the
// CHECK constraint: it is rejected at the data-access boundary (AC3).
func TestProjectExecutionModeInvalidRejected(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	_, err = db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: "tnt_dev", Name: "bad mode " + db.NewID(),
		Slug: "bad-" + db.NewID(), Status: "active", Goals: []byte("[]"),
		ExecutionMode: "banana",
	})
	if err == nil {
		t.Fatal("CreateProject with execution_mode=banana succeeded, want rejection")
	}
}

// TestProjectExecutionModeColumnDefault proves a RAW insert that omits the column
// lands in local — the migration's DEFAULT, independent of the service layer (AC2).
func TestProjectExecutionModeColumnDefault(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	id := db.NewID()
	if _, err := ttx.Tx.Exec(ctx, `INSERT INTO projects
		(id, tenant_id, name, slug, status, goals, project_dir, max_concurrent_runs, git_strategy)
		VALUES ($1, $2, $3, $4, 'active', '[]'::jsonb, '', 0, 'local')`,
		id, "tnt_dev", "raw "+id, "raw-"+id); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	var mode string
	if err := ttx.Tx.QueryRow(ctx, `SELECT execution_mode FROM projects WHERE id = $1`, id).Scan(&mode); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if mode != db.ExecutionModeLocal {
		t.Fatalf("column default = %q, want %q", mode, db.ExecutionModeLocal)
	}
}
