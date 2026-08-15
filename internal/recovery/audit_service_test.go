package recovery

// Service-level audit tests for the recovery RPCs whose mutations commit
// inside the recovery engine's own transaction. The four engine-owned
// RPCs (TriggerRecovery / ApproveContinuationPlan /
// RejectContinuationPlan / MarkTaskSucceeded) record their audit row in
// the engine's tx so it commits atomically with the mutation (AC1
// transactional outbox); CancelRecovery / DeleteRecovery record in the
// handler's own tx. Read-only RPCs (Get/List) write zero. Skipped unless
// ORCHICON_TEST_DSN points at a disposable database (repo convention):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/recovery/ -run TestAuditService -v

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func auditEnv(t *testing.T, tenantID string) (*db.Pool, *Service, context.Context, string, string) {
	t.Helper()
	pool := auditPool(t)
	ident := seedAuditIdentity(t, pool, tenantID, "audit-rec-"+strings.ToLower(db.NewID()))
	ctx := tenant.WithID(context.Background(), tenantID)
	ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
		IdentityID: ident.ID,
		TenantID:   tenantID,
		Subject:    ident.Subject,
		AuthMethod: "oidc",
		IsAdmin:    true,
	})
	engine := New(pool, slog.New(slog.DiscardHandler))
	s := NewService(pool, slog.New(slog.DiscardHandler), engine, nil)
	return pool, s, ctx, tenantID, ident.ID
}

func auditPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed recovery audit tests")
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
	return pool
}

// seedAuditIdentity creates (or reuses) an identity row the audit FK can
// point at, returning its id.
func seedAuditIdentity(t *testing.T, pool *db.Pool, tenantID, subject string) db.IdentityRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, subject, "Audit Actor", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit identity: %v", err)
	}
	return ident
}

// seedAuditProject creates an active project row in the given tenant.
func seedAuditProject(t *testing.T, pool *db.Pool, tenantID string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenantID,
		Name: "Audit Test", Slug: "audit-test-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit project: %v", err)
	}
	return proj.ID
}

// seedAuditTask creates a task work item in the given project.
func seedAuditTask(t *testing.T, pool *db.Pool, tenantID, projectID string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: domain.WorkItemKindTask, Title: "Audit task " + strings.ToLower(db.NewID()),
		Status: domain.WorkItemFailed,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit task: %v", err)
	}
	return wi
}

// seedAuditExecution creates a failed execution for the task.
func seedAuditExecution(t *testing.T, pool *db.Pool, tenantID, taskID, projectID string) db.ExecutionRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	started := time.Now().UTC().Add(-time.Hour)
	ended := time.Now().UTC()
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID, TaskID: taskID,
		WorkerID: "wkr_audit_test", WorkerVersion: 1,
		Status: domain.ExecutionFailed, HealthState: "failed",
		StartedAt: &started, EndedAt: &ended,
		TokenUsage: 100, CostUSD: 0.01,
	})
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit execution: %v", err)
	}
	return exec
}

// seedAuditBlockedRecovery creates a blocked recovery linked to a pending
// continuation plan (the L3 state Approve/RejectContinuationPlan act on).
func seedAuditBlockedRecovery(t *testing.T, pool *db.Pool, tenantID, taskID, execID, projectID string) db.RecoveryExecutionRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rec, err := db.CreateRecoveryExecution(ctx, ttx.Tx, db.RecoveryExecutionRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID, TaskID: taskID,
		FailedExecutionID: execID, TriggerReason: "test",
		Level: domain.RecoveryLevelL3, Status: domain.RecoveryBlocked,
		NeedsHumanApproval: true, TriggeredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create recovery: %v", err)
	}
	plan, err := db.CreateContinuationPlan(ctx, ttx.Tx, db.ContinuationPlanRow{
		ID: db.NewID(), TenantID: tenantID, RecoveryID: rec.ID, Version: 1,
		Completed: []byte("[]"), InProgress: []byte("[]"),
		Remaining: []byte("[]"), Corrections: []byte("[]"), Assumptions: []byte("[]"),
		Status: domain.PlanPending,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	updated, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, tenantID, rec.ID, rec.Version, db.UpdateRecoveryExecutionFields{
		ContinuationPlanID: &plan.ID,
	})
	if err != nil {
		t.Fatalf("link plan: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit recovery: %v", err)
	}
	return updated
}

// auditEventCount counts audit rows matching the exact action + target
// (idempotent across shared-DB runs: unique target ids per test).
func auditEventCount(t *testing.T, pool *db.Pool, tenantID, action, targetType, targetID string) int {
	t.Helper()
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, action, "", targetType, targetID, "", 100)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	return len(rows)
}

// TestAuditServiceTriggerRecovery asserts the manual-trigger RPC writes
// exactly one recovery.triggered row in the engine's transaction (with
// the actor + auth method), zero rows for read-only Get/List, and no row
// for an idempotent re-trigger when a recovery is already active.
func TestAuditServiceTriggerRecovery(t *testing.T) {
	tenantID := "tnt_audit_rec_trig_" + strings.ToLower(db.NewID())
	pool, s, ctx, _, actorID := auditEnv(t, tenantID)
	proj := seedAuditProject(t, pool, tenantID)
	task := seedAuditTask(t, pool, tenantID, proj)
	seedAuditExecution(t, pool, tenantID, task.ID, proj)

	// Manual trigger → 1 recovery.triggered row.
	if _, err := s.TriggerRecovery(ctx, connect.NewRequest(&apiv1.TriggerRecoveryRequest{
		TaskId: task.ID, TriggerReason: "audit test",
	})); err != nil {
		t.Fatalf("TriggerRecovery: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "recovery.triggered", "recovery", task.ID); n != 1 {
		t.Fatalf("recovery.triggered rows = %d, want 1", n)
	}
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "recovery.triggered", "", "recovery", task.ID, "", 10)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if rows[0].ActorType != "user" || rows[0].AuthMethod != "oidc" || rows[0].ActorIdentityID != actorID {
		t.Fatalf("actor fields = type:%s auth:%s id:%s, want user/oidc/%s",
			rows[0].ActorType, rows[0].AuthMethod, rows[0].ActorIdentityID, actorID)
	}
	if rows[0].TargetType != "recovery" || rows[0].TargetID != task.ID {
		t.Fatalf("target = %s/%s, want recovery/%s", rows[0].TargetType, rows[0].TargetID, task.ID)
	}

	// Read-only calls write nothing.
	recs, err := s.ListRecoveries(ctx, connect.NewRequest(&apiv1.ListRecoveriesRequest{TaskId: task.ID}))
	if err != nil {
		t.Fatalf("ListRecoveries: %v", err)
	}
	if len(recs.Msg.Recoveries) != 1 {
		t.Fatalf("ListRecoveries returned %d, want 1 (sanity)", len(recs.Msg.Recoveries))
	}
	if n := auditEventCount(t, pool, tenantID, "recovery.triggered", "recovery", task.ID); n != 1 {
		t.Fatalf("ListRecoveries wrote audit rows: triggered rows = %d, want still 1", n)
	}
	if _, err := s.GetRecovery(ctx, connect.NewRequest(&apiv1.GetRecoveryRequest{Id: recs.Msg.Recoveries[0].Id})); err != nil {
		t.Fatalf("GetRecovery: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "recovery.triggered", "recovery", task.ID); n != 1 {
		t.Fatalf("GetRecovery wrote audit rows: triggered rows = %d, want still 1", n)
	}

	// Idempotent re-trigger (a recovery is already active) is a no-op: no
	// new audit row — an audit row exists iff a mutation committed.
	if _, err := s.TriggerRecovery(ctx, connect.NewRequest(&apiv1.TriggerRecoveryRequest{
		TaskId: task.ID, TriggerReason: "second attempt",
	})); err != nil {
		t.Fatalf("idempotent TriggerRecovery: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "recovery.triggered", "recovery", task.ID); n != 1 {
		t.Fatalf("idempotent re-trigger wrote audit rows: triggered rows = %d, want still 1", n)
	}
}

// TestAuditServiceMarkTaskSucceeded asserts one recovery.task_marked_succeeded
// row per RPC call.
func TestAuditServiceMarkTaskSucceeded(t *testing.T) {
	tenantID := "tnt_audit_rec_ms_" + strings.ToLower(db.NewID())
	pool, s, ctx, _, actorID := auditEnv(t, tenantID)
	proj := seedAuditProject(t, pool, tenantID)
	task := seedAuditTask(t, pool, tenantID, proj)

	reason := "audit test"
	if _, err := s.MarkTaskSucceeded(ctx, connect.NewRequest(&apiv1.MarkTaskSucceededRequest{
		TaskId: task.ID, ActorType: "human", ActorId: actorID, Reason: reason,
	})); err != nil {
		t.Fatalf("MarkTaskSucceeded: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "recovery.task_marked_succeeded", "recovery", task.ID); n != 1 {
		t.Fatalf("recovery.task_marked_succeeded rows = %d, want 1", n)
	}
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "recovery.task_marked_succeeded", "", "recovery", task.ID, "", 10)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if rows[0].ActorType != "user" || rows[0].AuthMethod != "oidc" || rows[0].ActorIdentityID != actorID {
		t.Fatalf("actor fields = type:%s auth:%s id:%s, want user/oidc/%s",
			rows[0].ActorType, rows[0].AuthMethod, rows[0].ActorIdentityID, actorID)
	}
	// before/after reflect the status transition observed by the engine.
	if string(rows[0].Before) == "" || string(rows[0].After) == "" {
		t.Fatalf("task_marked_succeeded row must carry before+after: %+v", rows[0])
	}
}

// TestAuditServiceContinuationPlanDecision asserts Approve and Reject each
// write exactly one audit row for their recovery.
func TestAuditServiceContinuationPlanDecision(t *testing.T) {
	tenantID := "tnt_audit_rec_plan_" + strings.ToLower(db.NewID())
	pool, s, ctx, _, actorID := auditEnv(t, tenantID)
	proj := seedAuditProject(t, pool, tenantID)
	task := seedAuditTask(t, pool, tenantID, proj)
	exec := seedAuditExecution(t, pool, tenantID, task.ID, proj)

	// Approve path.
	recA := seedAuditBlockedRecovery(t, pool, tenantID, task.ID, exec.ID, proj)
	if _, err := s.ApproveContinuationPlan(ctx, connect.NewRequest(&apiv1.ApproveContinuationPlanRequest{
		RecoveryId: recA.ID, Actor: actorID,
	})); err != nil {
		t.Fatalf("ApproveContinuationPlan: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "recovery.continuation_plan_approved", "recovery", recA.ID); n != 1 {
		t.Fatalf("recovery.continuation_plan_approved rows = %d, want 1", n)
	}

	// Reject path (a second, distinct recovery).
	recB := seedAuditBlockedRecovery(t, pool, tenantID, task.ID, exec.ID, proj)
	if _, err := s.RejectContinuationPlan(ctx, connect.NewRequest(&apiv1.RejectContinuationPlanRequest{
		RecoveryId: recB.ID, Actor: actorID, Reason: "audit test",
	})); err != nil {
		t.Fatalf("RejectContinuationPlan: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "recovery.continuation_plan_rejected", "recovery", recB.ID); n != 1 {
		t.Fatalf("recovery.continuation_plan_rejected rows = %d, want 1", n)
	}
}
