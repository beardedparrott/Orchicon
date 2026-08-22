package db_test

import (
	"context"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// TestListExecutionSessionPartsTail verifies the recovery-tail read-back:
// the LAST `limit` parts are returned in DESC (tail-first) order, the
// limit is clamped, and a missing execution returns an empty slice.
// Guarded by ORCHICON_TEST_DSN like the other DB-backed tests.
func TestListExecutionSessionPartsTail(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed session parts tail test")
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

	const tenant = "tnt_dev"
	execID := "exec-tail-" + db.NewID()

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`INSERT INTO worker_executions (id, tenant_id, project_id, task_id, worker_id, worker_version, status, health_state)
		 VALUES ($1, $2, 'proj', 'task', 'worker', 1, 'running', 'healthy')`,
		execID, tenant); err != nil {
		t.Fatalf("insert execution row: %v", err)
	}
	var parts []db.SessionPart
	for i := 0; i < 10; i++ {
		parts = append(parts, db.SessionPart{
			ExecutionID: execID,
			Seq:         int64(i + 1),
			Kind:        db.SessionPartText,
			Payload:     []byte(`{"part":{"text":"m"}}`),
		})
	}
	if err := db.AppendExecutionSessionParts(ctx, ttx.Tx, tenant, parts); err != nil {
		t.Fatalf("append session parts: %v", err)
	}

	t.Cleanup(func() {
		ct := context.Background()
		dttx, err := pool.BeginTenantTx(ct, tenant)
		if err == nil {
			_, _ = dttx.Exec(ct, `DELETE FROM execution_session_parts WHERE execution_id = $1`, execID)
			_, _ = dttx.Exec(ct, `DELETE FROM worker_executions WHERE id = $1`, execID)
			_ = dttx.Commit(ct)
		}
	})

	// Tail of 3 → the LAST three parts in DESC order.
	tail, err := db.ListExecutionSessionPartsTail(ctx, ttx.Tx, tenant, execID, 3)
	if err != nil {
		t.Fatalf("list tail: %v", err)
	}
	if len(tail) != 3 {
		t.Fatalf("tail length = %d, want 3", len(tail))
	}
	wantSeqs := []int64{10, 9, 8}
	for i, p := range tail {
		if p.Seq != wantSeqs[i] {
			t.Errorf("tail[%d].Seq = %d, want %d", i, p.Seq, wantSeqs[i])
		}
	}

	// Missing execution → empty slice, no error.
	empty, err := db.ListExecutionSessionPartsTail(ctx, ttx.Tx, tenant, "no-such-exec-"+db.NewID(), 5)
	if err != nil {
		t.Fatalf("list tail missing execution: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("missing execution tail = %d parts, want 0", len(empty))
	}

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestLatestToolUseParts verifies the todos read path: only kind='tool_use'
// parts are returned, in DESC-by-seq order, with the limit clamped, and a
// missing execution returns an empty slice. Guarded by ORCHICON_TEST_DSN.
func TestLatestToolUseParts(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed latest tool-use parts test")
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

	const tenant = "tnt_dev"
	execID := "exec-todos-" + db.NewID()

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`INSERT INTO worker_executions (id, tenant_id, project_id, task_id, worker_id, worker_version, status, health_state)
		 VALUES ($1, $2, 'proj', 'task', 'worker', 1, 'running', 'healthy')`,
		execID, tenant); err != nil {
		t.Fatalf("insert execution row: %v", err)
	}
	// Interleave tool_use and text parts so the kind filter is exercised.
	// Tool_use lands on EVEN seqs (2,4,6,8,10); text on odd seqs (1,3,5,7,9).
	var parts []db.SessionPart
	for i := 0; i < 10; i++ {
		kind := db.SessionPartText
		if i%2 == 1 {
			kind = db.SessionPartToolUse
		}
		parts = append(parts, db.SessionPart{
			ExecutionID: execID,
			Seq:         int64(i + 1),
			Kind:        kind,
			Payload:     []byte(`{"part":{"tool":"todowrite","state":{"input":{"todos":[]}}}}`),
		})
	}
	if err := db.AppendExecutionSessionParts(ctx, ttx.Tx, tenant, parts); err != nil {
		t.Fatalf("append session parts: %v", err)
	}

	t.Cleanup(func() {
		ct := context.Background()
		dttx, err := pool.BeginTenantTx(ct, tenant)
		if err == nil {
			_, _ = dttx.Exec(ct, `DELETE FROM execution_session_parts WHERE execution_id = $1`, execID)
			_, _ = dttx.Exec(ct, `DELETE FROM worker_executions WHERE id = $1`, execID)
			_ = dttx.Commit(ct)
		}
	})

	// Tool_use parts are at seqs 10,8,6,4,2 → DESC returns 10,8,6,4,2.
	got, err := db.LatestToolUseParts(ctx, ttx.Tx, tenant, execID, 1000)
	if err != nil {
		t.Fatalf("list latest tool use parts: %v", err)
	}
	wantSeqs := []int64{10, 8, 6, 4, 2}
	if len(got) != len(wantSeqs) {
		t.Fatalf("got %d tool-use parts, want %d", len(got), len(wantSeqs))
	}
	for i, p := range got {
		if p.Seq != wantSeqs[i] {
			t.Errorf("part[%d].Seq = %d, want %d", i, p.Seq, wantSeqs[i])
		}
		if p.Kind != db.SessionPartToolUse {
			t.Errorf("part[%d].Kind = %q, want %q", i, p.Kind, db.SessionPartToolUse)
		}
	}

	// Limit clamping: limit 2 → the two most recent tool_use parts.
	limited, err := db.LatestToolUseParts(ctx, ttx.Tx, tenant, execID, 2)
	if err != nil {
		t.Fatalf("list limited tool use parts: %v", err)
	}
	if len(limited) != 2 || limited[0].Seq != 10 || limited[1].Seq != 8 {
		t.Errorf("limited tool-use parts seqs = %v, want [10 8]", seqsOf(limited))
	}

	// Missing execution → empty slice, no error.
	empty, err := db.LatestToolUseParts(ctx, ttx.Tx, tenant, "no-such-exec-"+db.NewID(), 5)
	if err != nil {
		t.Fatalf("list tool use parts missing execution: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("missing execution tool-use parts = %d, want 0", len(empty))
	}

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func seqsOf(parts []db.SessionPart) []int64 {
	out := make([]int64, len(parts))
	for i, p := range parts {
		out[i] = p.Seq
	}
	return out
}
