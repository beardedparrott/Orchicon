package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// MCP parity mirrors for the update-path auto-start precondition
// (architecture-notes/fix-update-path-auto-start-…): tool_update_work_item
// applies the same startable-status gate as the Connect handler and, when
// an explicit auto_start request is declined, carries the same warning in
// its JSON result. Skipped unless ORCHICON_TEST_DSN is set (same
// convention as the rest of this package):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run 'TestToolUpdateAutoStart' -v

// toolRunsForItem counts the real workflow runs bound to a work item —
// the MCP path calls StartWorkflowDirect directly (no injectable starter),
// so "nothing started" is asserted as zero run rows.
func toolRunsForItem(t *testing.T, pool *db.Pool, itemID string) int {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE work_item_id = $1`, itemID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedToolPublishedWorkflow mirrors workitem.seedPublishedWorkflowForTest
// for the MCP test tenant so a broken guard would produce a REAL run row.
func seedToolPublishedWorkflow(t *testing.T, pool *db.Pool, projID string) string {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, ProjectID: projID,
		Name: "tool-wf-" + db.NewID()[:8], CurrentVersion: 0, Status: domain.WorkflowDraft,
		Type: domain.WorkflowTypeTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := []byte(`[{"id":"step-1","name":"Task","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"config":""}]`)
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: domain.WorkflowVersionDraft,
		Steps: steps, Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PublishWorkflowVersion(ctx, ttx.Tx, workItemKindTestTenant, wf.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkflowCurrentVersion(ctx, ttx.Tx, workItemKindTestTenant, wf.ID, wf.Version, 1); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return wf.ID
}

// toolResult is the subset of the update tool's JSON result (raw DB row +
// the optional auto-start warning sibling field) the tests assert on.
type toolResult struct {
	ID      string `json:"ID"`
	Title   string `json:"Title"`
	Status  string `json:"Status"`
	Warning string `json:"warning"`
}

// TestToolUpdateAutoStartDeclinedOnCancelledDB — cancelled item + legacy
// stale flag + workflow_id edit: the binding is saved, no run is created,
// and no warning is emitted (the user never asked).
func TestToolUpdateAutoStartDeclinedOnCancelledDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	proj := createProjectForTest(t, ctx, pool)
	wf := seedToolPublishedWorkflow(t, pool, proj)
	item := createWorkItemForTest(t, ctx, pool, proj, "Tool Cancelled")
	forceToolState(t, pool, item.ID, domain.WorkItemCancelled, true)

	res, err := toolUpdateWorkItem(ctx, pool,
		json.RawMessage(fmt.Sprintf("{\"id\":%q,\"workflow_id\":%q}", item.ID, wf)))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var out toolResult
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Warning != "" {
		t.Fatalf("non-explicit decline must stay silent, got %q", out.Warning)
	}
	if out.Status != domain.WorkItemCancelled {
		t.Fatalf("status = %q, want cancelled", out.Status)
	}
	if n := toolRunsForItem(t, pool, item.ID); n != 0 {
		t.Fatalf("%d runs created for cancelled item, want 0", n)
	}
}

// TestToolUpdateAutoStartExplicitWarnsDB — explicit auto_start on a failed
// item: no run, warning carried in the JSON result listing the required
// statuses.
func TestToolUpdateAutoStartExplicitWarnsDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	proj := createProjectForTest(t, ctx, pool)
	wf := seedToolPublishedWorkflow(t, pool, proj)
	item := createWorkItemForTest(t, ctx, pool, proj, "Tool Failed")
	forceToolState(t, pool, item.ID, domain.WorkItemFailed, false)

	res, err := toolUpdateWorkItem(ctx, pool,
		json.RawMessage(fmt.Sprintf(`{"id":%q,"workflow_id":%q,"auto_start_workflow":true}`, item.ID, wf)))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var out toolResult
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(out.Warning, "NOT applied") ||
		!strings.Contains(out.Warning, "pending, scheduled, ready, or assigned") {
		t.Fatalf("warning missing markers: %q", out.Warning)
	}
	if n := toolRunsForItem(t, pool, item.ID); n != 0 {
		t.Fatalf("%d runs created for failed item, want 0", n)
	}
}

// TestToolUpdateAutoStartStaleFlagNoFireDB — stale stored flag on a
// succeeded row with a bound workflow stays inert under a title edit.
func TestToolUpdateAutoStartStaleFlagNoFireDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	proj := createProjectForTest(t, ctx, pool)
	wf := seedToolPublishedWorkflow(t, pool, proj)
	item := createWorkItemForTest(t, ctx, pool, proj, "Tool Stale")
	// Bind the workflow + stale flag directly (legacy row simulation).
	forceToolBoundStale(t, pool, item.ID, wf)

	res, err := toolUpdateWorkItem(ctx, pool,
		json.RawMessage(fmt.Sprintf("{\"id\":%q,\"title\":\"Renamed Tool Stale\"}", item.ID)))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var out toolResult
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Warning != "" {
		t.Fatalf("stale-flag decline is silent, got %q", out.Warning)
	}
	if n := toolRunsForItem(t, pool, item.ID); n != 0 {
		t.Fatalf("%d runs created from stale flag, want 0", n)
	}
}

// TestToolUpdateAutoStartGoodPathDB — regression: explicit auto-start on a
// pending leaf with a bound published workflow DOES start a real run.
func TestToolUpdateAutoStartGoodPathDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	proj := createProjectForTest(t, ctx, pool)
	wf := seedToolPublishedWorkflow(t, pool, proj)
	item := createWorkItemForTest(t, ctx, pool, proj, "Tool Good Path")
	bindToolWorkflow(t, pool, item.ID, wf)

	if _, err := toolUpdateWorkItem(ctx, pool,
		json.RawMessage(`{"id":"`+item.ID+`","auto_start_workflow":true}`)); err != nil {
		t.Fatalf("update: %v", err)
	}
	if n := toolRunsForItem(t, pool, item.ID); n != 1 {
		t.Fatalf("%d runs created for good path, want 1", n)
	}
}

// TestToolUpdateAutoStartStaleFlagUnboundStartableDB is the UNBOUND LEAF
// case: a startable row carrying the stored auto_start_workflow flag with NO
// workflow bound and NO children, edited by a plain (non-auto-start) update.
//
// The sequence validation that rejects an unbound leaf runs only when the
// REQUEST asks to start, so this edit skipped it and reached the post-commit
// auto-start with a nil binding — `*updated.WorkflowID` dereferenced nil and
// the panic killed the whole process. The RPC handler has always guarded this
// (updated.WorkflowID != nil && *updated.WorkflowID != "",
// internal/workitem/service.go); this pins the tool path to the same
// behaviour: no panic, the edit saved, nothing started.
//
// This is NOT the shape that crashed production — that row was a sequence
// PARENT (see TestToolUpdateAutoStartSequenceParentFiresChainDB). It is the
// same nil deref reached from the other side of the branch, and it is the
// guard's own regression pin.
func TestToolUpdateAutoStartStaleFlagUnboundStartableDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	proj := createProjectForTest(t, ctx, pool)
	item := createWorkItemForTest(t, ctx, pool, proj, "Tool Unbound Stale")
	// Startable status + legacy flag, and workflow_id stays NULL
	// (createWorkItemForTest never binds one).
	forceToolState(t, pool, item.ID, domain.WorkItemPending, true)

	res, err := toolUpdateWorkItem(ctx, pool,
		json.RawMessage(fmt.Sprintf(`{"id":%q,"title":"Renamed Unbound Stale"}`, item.ID)))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var out toolResult
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Title != "Renamed Unbound Stale" {
		t.Fatalf("title = %q, want the edit saved", out.Title)
	}
	if out.Status != domain.WorkItemPending {
		t.Fatalf("status = %q, want pending", out.Status)
	}
	if n := toolRunsForItem(t, pool, item.ID); n != 0 {
		t.Fatalf("%d runs created for an unbound item, want 0", n)
	}
}

// TestToolUpdateAutoStartExplicitUnboundRejectsDB pins the OTHER entry to the
// same unbound branch: an explicit auto_start_workflow=true on an unbound
// leaf is rejected by schedule-time validation (nothing to run) BEFORE the
// post-commit start. It must be an error, never a panic.
func TestToolUpdateAutoStartExplicitUnboundRejectsDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	proj := createProjectForTest(t, ctx, pool)
	item := createWorkItemForTest(t, ctx, pool, proj, "Tool Unbound Explicit")

	_, err := toolUpdateWorkItem(ctx, pool,
		json.RawMessage(fmt.Sprintf(`{"id":%q,"auto_start_workflow":true}`, item.ID)))
	if err == nil {
		t.Fatal("expected a rejection for auto-starting an unbound leaf, got nil error")
	}
	if !strings.Contains(err.Error(), "no workflow is set") {
		t.Fatalf("error = %v, want the no-workflow rejection", err)
	}
}

// TestToolUpdateAutoStartSequenceParentFiresChainDB pins the FIRE PATH for the
// shape that actually crashed production on 2026-09-28: an armed SEQUENCE
// PARENT, not an unbound leaf.
//
// The row that killed the host plane was feature 01KYQXRABZC7JAX65FF5GWG7S1 —
// status pending, auto_start_workflow true, workflow_id NULL, SIX children —
// edited by a plain (non-auto-start) update during a bulk "update them, then
// schedule them" request. Its UpdatedAt (08:23:10.717584) is 2.7 ms before the
// panic at tool_workitems.go:755, and its scheduled_start_at is still NULL
// because the plane died before it could schedule.
//
// A parent's NULL binding is that mode's NORMAL state, not a legacy flag:
// StartSequence clears the parent's own workflow_id ("a parent with children
// IS a sequence container, its own workflow_id is ignored") and the children
// each run their own. The Connect handler routes exactly this shape to the
// chain (itemHasChildren → maybeStartSequence); the tool path lacked the
// branch and fell through to the leaf deref, so it panicked the whole plane
// instead of firing. This pins parity: no panic, the edit saved, the chain
// actually fires. Declining silently here would be the same divergence in the
// other direction.
func TestToolUpdateAutoStartSequenceParentFiresChainDB(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	proj := createProjectForTest(t, ctx, pool)
	wfID := seedToolPublishedWorkflow(t, pool, proj)

	parent := createWorkItemForTest(t, ctx, pool, proj, "Tool Armed Sequence Parent")
	child := createChildWorkItemForTest(t, ctx, pool, proj, parent.ID, "Tool Armed Sequence Child")
	bindToolWorkflow(t, pool, child.ID, wfID)
	// The armed shape: startable, stored flag on, and no binding of its own.
	forceToolState(t, pool, parent.ID, domain.WorkItemPending, true)

	res, err := toolUpdateWorkItem(ctx, pool,
		json.RawMessage(fmt.Sprintf(`{"id":%q,"title":"Renamed Armed Chain"}`, parent.ID)))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var out toolResult
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Title != "Renamed Armed Chain" {
		t.Fatalf("title = %q, want the edit saved", out.Title)
	}
	// StartSequence stamps the parent running and arms the first child, which
	// starts the child's own bound run.
	var parentStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM work_items WHERE id = $1`, parent.ID).Scan(&parentStatus); err != nil {
		t.Fatal(err)
	}
	if parentStatus != domain.WorkItemRunning {
		t.Fatalf("parent status = %q, want %q — the chain must FIRE, not decline", parentStatus, domain.WorkItemRunning)
	}
	if n := toolRunsForItem(t, pool, child.ID); n != 1 {
		t.Fatalf("%d runs on the armed child, want 1 — the chain must actually start the child", n)
	}
}

// --- direct-SQL helpers -----------------------------------------------------

func createWorkItemForTest(t *testing.T, ctx context.Context, pool *db.Pool, projID, title string) db.WorkItemRow {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, ProjectID: projID,
		Kind: domain.WorkItemKindEpic, Title: title, Status: domain.WorkItemPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

func forceToolState(t *testing.T, pool *db.Pool, itemID, status string, autoStart bool) {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Tx.Exec(ctx,
		`UPDATE work_items SET status = $1, auto_start_workflow = $2 WHERE id = $3`,
		status, autoStart, itemID); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func forceToolBoundStale(t *testing.T, pool *db.Pool, itemID, workflowID string) {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Tx.Exec(ctx,
		`UPDATE work_items SET workflow_id = $1, auto_start_workflow = true, status = $2 WHERE id = $3`,
		workflowID, domain.WorkItemSucceeded, itemID); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func bindToolWorkflow(t *testing.T, pool *db.Pool, itemID, workflowID string) {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Tx.Exec(ctx, `UPDATE work_items SET workflow_id = $1 WHERE id = $2`, workflowID, itemID); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
