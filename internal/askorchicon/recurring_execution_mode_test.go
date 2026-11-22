package askorchicon

// recurring_execution_mode_test.go — a recurring item must be able to run LOCALLY, and the tool surface
// must expose that choice.
//
// THE BUG. Every resolution path already handled a per-item mode (db.EffectiveExecutionMode is
// item -> project -> tenant default, and the scheduler, the workflow reconciler and the runtime
// lifecycle all read it). The HANDLERS even applied it: create_work_item and update_work_item parse
// execution_mode and clear_execution_mode.
//
// What was missing was the SCHEMA: neither tool listed the parameter, so no model ever sent it, so every
// recurring item was created with execution_mode NULL and inherited its project's mode. "A recurring item
// cannot run locally" was therefore true in practice and false in principle — the same shape as the
// recurrence gap itself, where the capability existed and the tool surface could not express it.
//
// These tests pin both halves: the schema advertises the parameter, and the value a caller sends actually
// lands on the item and resolves correctly at dispatch time.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestBothWorkItemToolsExposeExecutionMode is the direct regression guard for the missing schema entry.
func TestBothWorkItemToolsExposeExecutionMode(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	for _, name := range []string{"create_work_item", "update_work_item"} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		prop, ok := tool.Properties["execution_mode"]
		if !ok {
			t.Errorf("%s has no execution_mode parameter — an agent cannot make any item, recurring or not, "+
				"run locally", name)
			continue
		}
		// The description must say the two values, because the agent has to choose between them.
		desc := strings.ToLower(prop.Description)
		for _, want := range []string{"local", "runtime"} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s execution_mode description does not name %q", name, want)
			}
		}
		// AND IT MUST SAY IT APPLIES TO RECURRING ITEMS. That is the specific belief that was wrong.
		if !strings.Contains(desc, "recurring") {
			t.Errorf("%s execution_mode description does not mention recurring items, so an agent may "+
				"assume the choice does not apply to them", name)
		}
	}
	// update can also drop the override, or a locally-pinned item could never be returned to inheriting.
	upd, _ := reg.Get("update_work_item")
	if _, ok := upd.Properties["clear_execution_mode"]; !ok {
		t.Error("update_work_item has no clear_execution_mode")
	}
}

// TestRecurringItemRunsLocallyEndToEnd is the actual complaint, proven: create a RECURRING item with
// execution_mode=local against a real database whose project says runtime, then resolve the mode the way
// the dispatch path does.
func TestRecurringItemRunsLocallyEndToEnd(t *testing.T) {
	pool := livePool(t)
	projectID, workflowID := liveFixture(t, pool)
	ctx := tenant.WithID(context.Background(), liveRecurringTenant)

	// The project says RUNTIME — so a local resolution cannot be inheritance in disguise.
	setProjectMode(t, pool, projectID, "runtime")

	args, err := json.Marshal(map[string]any{
		"title":          "Daily digest, locally",
		"project_id":     projectID,
		"kind":           "task",
		"workflow_id":    workflowID,
		"execution_mode": "local",
		"recurrence": map[string]any{
			"frequency": "daily", "interval": 1,
			"start_date": "2026-12-01", "start_time": "09:00", "timezone": "America/Chicago",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := toolCreateWorkItem(ctx, pool, args)
	if err != nil {
		t.Fatalf("create recurring item with execution_mode=local: %v", err)
	}
	var created db.WorkItemRow
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatalf("decode: %v", err)
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

	// 1. It really is recurring...
	if len(got.RecurringSchedule) == 0 {
		t.Fatal("the item is not recurring")
	}
	if got.NextRunAt == nil {
		t.Fatal("no next_run_at — it would never fire")
	}
	// 2. ...and it really carries the local override...
	if got.ExecutionMode == nil || *got.ExecutionMode != "local" {
		t.Fatalf("execution_mode = %v, want local — the recurring item inherited the project's runtime mode",
			got.ExecutionMode)
	}
	// 3. ...and the DISPATCH resolution agrees, which is what the scheduler/reconciler/lifecycle all use.
	schedTx, err := pool.BeginTenantTx(ctx, liveRecurringTenant)
	if err != nil {
		t.Fatalf("begin dispatch tx: %v", err)
	}
	defer schedTx.Rollback(ctx)
	eff := db.EffectiveExecutionMode(ctx, schedTx.Tx, liveRecurringTenant, got.ExecutionMode, got.ProjectID)
	if eff != db.ExecutionModeLocal {
		t.Fatalf("dispatch resolved to %q, want local — a recurring item still cannot run locally", eff)
	}
}

// TestUpdateCanFlipARecurringItemLocalThenBack proves the change is reversible: an agent that pinned an
// item to runtime by mistake can put it back.
func TestUpdateCanFlipARecurringItemLocalThenBack(t *testing.T) {
	pool := livePool(t)
	projectID, workflowID := liveFixture(t, pool)
	ctx := tenant.WithID(context.Background(), liveRecurringTenant)
	setProjectMode(t, pool, projectID, "local")

	createArgs, _ := json.Marshal(map[string]any{
		"title": "Flip me", "project_id": projectID, "kind": "task", "workflow_id": workflowID,
		"recurrence": map[string]any{
			"frequency": "hourly", "interval": 1,
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

	// Pin it to runtime.
	upd, _ := json.Marshal(map[string]any{"id": created.ID, "execution_mode": "runtime"})
	if _, err := toolUpdateWorkItem(ctx, pool, upd); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	if mode := readMode(t, pool, created.ID); mode != "runtime" {
		t.Fatalf("after update, execution_mode = %q, want runtime", mode)
	}
	// And back to inheriting.
	clr, _ := json.Marshal(map[string]any{"id": created.ID, "clear_execution_mode": true})
	if _, err := toolUpdateWorkItem(ctx, pool, clr); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if mode := readMode(t, pool, created.ID); mode != "" {
		t.Fatalf("after clear, execution_mode = %q, want empty (inherit)", mode)
	}
	// The recurrence survived both edits — flipping the mode must not disturb the schedule.
	tx, _ := pool.BeginTenantTx(ctx, liveRecurringTenant)
	defer tx.Rollback(ctx)
	got, err := db.GetWorkItem(ctx, tx.Tx, liveRecurringTenant, created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.RecurringSchedule) == 0 {
		t.Error("editing execution_mode dropped the recurrence")
	}
}

// --- small helpers -----------------------------------------------------------------------------

func readMode(t *testing.T, pool *db.Pool, itemID string) string {
	t.Helper()
	tx, err := pool.BeginTenantTx(context.Background(), liveRecurringTenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	w, err := db.GetWorkItem(context.Background(), tx.Tx, liveRecurringTenant, itemID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if w.ExecutionMode == nil {
		return ""
	}
	return *w.ExecutionMode
}

func setProjectMode(t *testing.T, pool *db.Pool, projectID, mode string) {
	t.Helper()
	tx, err := pool.BeginTenantTx(context.Background(), liveRecurringTenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(),
		`UPDATE projects SET execution_mode = $1 WHERE id = $2`, mode, projectID); err != nil {
		t.Fatalf("set project mode: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
