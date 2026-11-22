package askorchicon

// recurring_tool_live_test.go — the end-to-end proof that the MCP tool surface can actually create,
// change and clear a recurring work item against a real database.
//
// WHY A DB TEST IS WORTH IT HERE. The pure tests above prove the argument shape and the invariants. They
// cannot prove the WRITE is correct: that recurring_schedule lands in the JSONB column, that next_run_at
// is computed, and that the item's status becomes 'recurring' rather than 'pending'. Those are the parts
// that make a fire actually happen, and every one of them is a database round-trip.
//
// Skips without ORCHICON_TEST_DSN, like the rest of the DB-backed suite.

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

const liveRecurringTenant = "tnt_recurring_tool"

func livePool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed recurring tool test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

// liveFixture creates an active project and a workflow inside the test tenant, so the tool's own
// precondition checks (active project, bound workflow) are satisfied honestly rather than bypassed.
func liveFixture(t *testing.T, pool *db.Pool) (projectID, workflowID string) {
	t.Helper()
	ctx := tenant.WithID(context.Background(), liveRecurringTenant)
	projectID = db.NewID()
	ttx, err := pool.BeginTenantTx(ctx, liveRecurringTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Exec(ctx, `INSERT INTO projects (id, tenant_id, name, slug, status, project_dir, git_strategy)
		VALUES ($1,$2,$3,$4,'active','/tmp/recurring-tool-test','none')`,
		projectID, liveRecurringTenant, "Recurring Tool Test", "recurring-tool-"+projectID[len(projectID)-6:]); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	workflowID = db.NewID()
	if _, err := ttx.Exec(ctx, `INSERT INTO workflows (id, tenant_id, project_id, name, status, type)
		VALUES ($1,$2,$3,'wf-under-test','published','one_shot')`,
		workflowID, liveRecurringTenant, projectID); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return projectID, workflowID
}

// TestCreateWorkItemToolWritesARealRecurrence is the whole point of the feature: after this call the
// item exists, is 'recurring', carries the schedule as JSONB, and has a next fire time.
func TestCreateWorkItemToolWritesARealRecurrence(t *testing.T) {
	pool := livePool(t)
	projectID, workflowID := liveFixture(t, pool)
	ctx := tenant.WithID(context.Background(), liveRecurringTenant)

	args, err := json.Marshal(map[string]any{
		"title":       "Daily digest",
		"project_id":  projectID,
		"kind":        "task",
		"workflow_id": workflowID,
		"recurrence": map[string]any{
			"frequency":  "daily",
			"interval":   1,
			"start_date": "2026-12-01",
			"start_time": "09:00",
			"timezone":   "America/Chicago",
		},
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	out, err := toolCreateWorkItem(ctx, pool, args)
	if err != nil {
		t.Fatalf("create recurring item: %v", err)
	}
	var created db.WorkItemRow
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatalf("decode created item: %v", err)
	}

	// Read it back through a fresh transaction — the write, not the return value, is what matters.
	tx, err := pool.BeginTenantTx(ctx, liveRecurringTenant)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer tx.Rollback(ctx)
	got, err := db.GetWorkItem(ctx, tx.Tx, liveRecurringTenant, created.ID)
	if err != nil {
		t.Fatalf("read back item: %v", err)
	}

	if got.Status != "recurring" {
		t.Errorf("status = %q, want 'recurring' — a pending item never fires", got.Status)
	}
	if len(got.RecurringSchedule) == 0 {
		t.Fatal("recurring_schedule is empty; the recurrence was not persisted")
	}
	var sched map[string]any
	if err := json.Unmarshal(got.RecurringSchedule, &sched); err != nil {
		t.Fatalf("schedule is not JSON: %v", err)
	}
	if sched["frequency"] != "daily" {
		t.Errorf("stored frequency = %v, want daily", sched["frequency"])
	}
	if sched["timezone"] != "America/Chicago" {
		t.Errorf("stored timezone = %v, want America/Chicago (a wall-clock rule needs its zone)",
			sched["timezone"])
	}
	if got.NextRunAt == nil {
		t.Error("next_run_at is NULL; the item would never fire")
	}
}

// TestCreateRejectsRecurrenceWithoutWorkflow proves the invariant is enforced by the TOOL, not merely
// documented in the prompt.
func TestCreateRejectsRecurrenceWithoutWorkflow(t *testing.T) {
	pool := livePool(t)
	projectID, _ := liveFixture(t, pool)
	ctx := tenant.WithID(context.Background(), liveRecurringTenant)

	args, _ := json.Marshal(map[string]any{
		"title":      "Unbound recurrence",
		"project_id": projectID,
		"kind":       "task",
		"recurrence": map[string]any{
			"frequency": "daily", "interval": 1,
			"start_date": "2026-12-01", "start_time": "09:00", "timezone": "UTC",
		},
	})
	if _, err := toolCreateWorkItem(ctx, pool, args); err == nil {
		t.Fatal("a recurring item with no workflow was created; the fire would have nothing to start")
	}
}

// TestUpdateToolClearsARecurrence proves an agent can STOP a recurrence it created. Without this the
// feature is a trap: set it and it runs forever.
func TestUpdateToolClearsARecurrence(t *testing.T) {
	pool := livePool(t)
	projectID, workflowID := liveFixture(t, pool)
	ctx := tenant.WithID(context.Background(), liveRecurringTenant)

	createArgs, _ := json.Marshal(map[string]any{
		"title": "Temporary recurrence", "project_id": projectID, "kind": "task", "workflow_id": workflowID,
		"recurrence": map[string]any{
			"frequency": "hourly", "interval": 2,
			"start_date": "2026-12-01", "start_time": "09:00", "timezone": "UTC",
		},
	})
	out, err := toolCreateWorkItem(ctx, pool, createArgs)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created db.WorkItemRow
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	clearArgs, _ := json.Marshal(map[string]any{"id": created.ID, "clear_recurrence": true})
	if _, err := toolUpdateWorkItem(ctx, pool, clearArgs); err != nil {
		t.Fatalf("clear recurrence: %v", err)
	}

	tx, err := pool.BeginTenantTx(ctx, liveRecurringTenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	got, err := db.GetWorkItem(ctx, tx.Tx, liveRecurringTenant, created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.RecurringSchedule) != 0 {
		t.Errorf("recurring_schedule survived clear_recurrence: %s", got.RecurringSchedule)
	}
	if got.NextRunAt != nil {
		t.Errorf("next_run_at survived clear_recurrence: %v — the item would still fire", got.NextRunAt)
	}
}
