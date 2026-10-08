package db

// CountActiveExecutionsForWorker is the per-worker twin of
// CountActiveExecutionsForAdapter (adapter.go:227-233): this test proves
// the exact regression that comment records, now per worker — a stale
// terminal row (succeeded/failed/terminated/failed_to_start/unhealthy)
// must not count toward a worker's concurrency budget. DB-backed;
// skipped without ORCHICON_TEST_DSN.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestCountActiveExecutionsForWorker -v

import (
	"context"
	"os"
	"strings"
	"testing"
)

func countExecTestPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed worker execution count test")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCountActiveExecutionsForWorkerExcludesTerminalRows(t *testing.T) {
	pool := countExecTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_dev"

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	suffix := strings.ToLower(NewID())[:12]
	worker, err := CreateWorker(ctx, ttx.Tx, WorkerRow{
		ID: "w-cnt-" + suffix, TenantID: tenant,
		Name: "count-test-" + suffix[:8], Slug: "w-cnt-" + suffix,
		Status: "published",
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		cttx, cerr := pool.BeginTenantTx(cctx, tenant)
		if cerr != nil {
			return
		}
		defer cttx.Rollback(cctx)
		_, _ = cttx.Exec(cctx, `DELETE FROM worker_executions WHERE tenant_id = $1 AND worker_id = $2`, tenant, worker.ID)
		_ = DeleteWorker(cctx, cttx.Tx, tenant, worker.ID)
		_ = cttx.Commit(cctx)
	})

	proj, err := CreateProject(ctx, ttx.Tx, ProjectRow{
		ID: NewID(), TenantID: tenant, Name: "count-test-proj",
		Slug: "count-test-" + suffix, Status: "active",
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	task, err := CreateWorkItem(ctx, ttx.Tx, WorkItemRow{
		ID: NewID(), TenantID: tenant, ProjectID: proj.ID,
		Kind: "task", Title: "count-test-task", Status: "running",
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}

	mkExec := func(status string) ExecutionRow {
		e, err := CreateExecution(ctx, ttx.Tx, ExecutionRow{
			ID: NewID(), TenantID: tenant, ProjectID: proj.ID, TaskID: task.ID,
			WorkerID: worker.ID, WorkerVersion: 1, Status: status,
		})
		if err != nil {
			t.Fatalf("create execution (status=%s): %v", status, err)
		}
		return e
	}

	// One active (dispatching) + one stale terminal row of each kind the
	// adapter-count comment names.
	mkExec("dispatching")
	for _, terminal := range []string{"succeeded", "failed", "terminated", "failed_to_start", "unhealthy"} {
		mkExec(terminal)
	}

	count, err := CountActiveExecutionsForWorker(ctx, ttx.Tx, tenant, worker.ID)
	if err != nil {
		t.Fatalf("count active executions for worker: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1 (terminal rows must not count)", count)
	}
}
