package scheduler

// DB-backed tests for the de-outboxed per-token execution.text path. Skipped
// without ORCHICON_TEST_DSN (the disposable sandbox plane), like the other
// DB-backed scheduler tests.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestOnText' -v

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// failingPublisher simulates a NATS outage at commit time: every direct publish
// errors, exactly as it would with the broker down.
type failingPublisher struct {
	attempts int
}

func (p *failingPublisher) Publish(_ context.Context, _, _ string, _ []byte) error {
	p.attempts++
	return errors.New("nats: no servers available (simulated outage)")
}

// newOnTextEnv seeds a project + running task + execution in tnt_dev (the
// tenant OnText hardcodes) and returns the reconciler and execution id.
func newOnTextEnv(t *testing.T) (*TaskReconciler, *db.Pool, string) {
	t.Helper()
	pool := approvalTestPool(t)
	ctx := context.Background()

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	slug := "ontext-test-" + strings.ToLower(db.NewID())
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "OnText", Slug: slug,
		Status: domain.ProjectActive, Goals: []byte("[]"), ProjectDir: "/tmp/orchicon/" + slug,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	task, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "OnText streaming", Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		TaskID: task.ID, WorkerID: "w_se_senior_software_engineer", WorkerVersion: 1,
		Status: domain.ExecutionRunning, HealthState: "healthy",
	})
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
		if err != nil {
			return
		}
		_, _ = ttx.Tx.Exec(ctx, `DELETE FROM execution_session_parts WHERE execution_id = $1`, exec.ID)
		_, _ = ttx.Tx.Exec(ctx, `DELETE FROM outbox WHERE aggregate_id = $1`, exec.ID)
		_, _ = ttx.Tx.Exec(ctx, `DELETE FROM worker_executions WHERE id = $1`, exec.ID)
		_, _ = ttx.Tx.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, task.ID)
		_, _ = ttx.Tx.Exec(ctx, `DELETE FROM projects WHERE id = $1`, proj.ID)
		_ = ttx.Commit(ctx)
	})

	r := NewTaskReconciler(pool, discardLogger(), nil)
	return r, pool, exec.ID
}

func countTextOutboxRows(t *testing.T, pool *db.Pool, execID string) int64 {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var n int64
	if err := ttx.Tx.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE event_type = 'execution.text' AND aggregate_id = $1`, execID).Scan(&n); err != nil {
		t.Fatalf("count execution.text outbox rows: %v", err)
	}
	return n
}

// TestOnTextDoesNotEnqueueOutbox: per-token execution.text events are no
// longer written to the outbox (the direct publish is the delivery path), and
// that holds even when the direct publish fails — the dropped "NATS was down"
// guarantee is covered by the durable transcript, verified below.
func TestOnTextDoesNotEnqueueOutbox(t *testing.T) {
	r, pool, execID := newOnTextEnv(t)
	ctx := context.Background()

	full := "the model streams this reply token by token"
	chunks := []string{"the model ", "streams this ", "reply token ", "by token"}
	pub := &failingPublisher{}
	r.SetEventPublisher(pub)
	for _, c := range chunks {
		r.OnText(ctx, execID, c)
	}

	if pub.attempts != len(chunks) {
		t.Fatalf("expected %d direct publish attempts despite the outage, got %d", len(chunks), pub.attempts)
	}
	if n := countTextOutboxRows(t, pool, execID); n != 0 {
		t.Fatalf("per-token execution.text must write ZERO outbox rows, found %d", n)
	}

	// The dropped guarantee: with NATS down at commit time the per-chunk event
	// is gone, but the completed text part is durably in execution_session_parts
	// — the GUI's reconnect refetch path (GetExecutionSession ->
	// db.ListExecutionSessionParts) — so the full text is recoverable.
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := db.AppendExecutionSessionParts(ctx, ttx.Tx, approvalTestTenant, []db.SessionPart{{
		ExecutionID: execID, TenantID: approvalTestTenant, Seq: 1,
		Kind:    db.SessionPartText,
		Payload: db.MarshalPartPayload(map[string]any{"part": map[string]any{"type": "text", "text": full}}),
	}}); err != nil {
		t.Fatalf("append session part: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit session part: %v", err)
	}

	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx2.Rollback(ctx)
	parts, err := db.ListExecutionSessionParts(ctx, ttx2.Tx, approvalTestTenant, execID, 0, 0)
	if err != nil {
		t.Fatalf("list session parts: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected exactly the completed text part in the transcript, got %d parts", len(parts))
	}
	if !strings.Contains(string(parts[0].Payload), full) {
		t.Fatalf("durable transcript did not recover the full text %q; got %s", full, parts[0].Payload)
	}
}

// TestOnToolCallStillEnqueuesOutbox pins the evidence-based decision to KEEP
// execution.tool_call outboxed: it is low-frequency (not per-token) and its
// direct publish is the same code path, so the outbox row is the durable
// delivery path for it. Same for the low-frequency state events.
func TestOnToolCallStillEnqueuesOutbox(t *testing.T) {
	r, pool, execID := newOnTextEnv(t)
	ctx := context.Background()
	r.SetEventPublisher(&failingPublisher{})

	r.OnToolCall(ctx, execID, "read", []byte(`{"path":"a.go"}`), []byte("content"))

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var n int64
	if err := ttx.Tx.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE event_type = 'execution.tool_call' AND aggregate_id = $1`, execID).Scan(&n); err != nil {
		t.Fatalf("count tool_call outbox rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("execution.tool_call must stay outboxed (1 row per call), got %d", n)
	}
}
