package askorchicon

// hard_delete_tool_test.go — THE MISSING HARD DELETE.
//
// The operator: "I think the work item should be hard deleted once the work is complete. I don't want a ton of
// invisible records out there. I think currently there is no MCP tool to do this only cancelling (soft
// delete) so we will need to add that."
//
// They were exactly right, and the shape of the gap is worth recording: the `HardDeleteWorkItem` RPC, its
// dependency cascade and its audit row ALL already existed. What did not exist was an MCP tool, so an agent
// asked to clean up its own ephemeral work could only call `delete_work_item` — a status change to cancelled,
// which is precisely the invisible record the operator was trying to avoid.
//
// The DB-backed cases skip unless ORCHICON_TEST_DSN is set, like the rest of this package's pool tests.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// THE TOOL EXISTS AND SAYS WHAT IT DOES — including that it cannot be undone.
//
// The description is the agent's only warning, so it has to carry the two things that decide whether the call
// is right: that it is IRREVERSIBLE, and that it is not the same act as cancelling.
func TestHardDeleteWorkItemToolIsRegisteredAndHonest(t *testing.T) {
	r := NewToolRegistry(nil, nil, nil)
	td, ok := r.Get("hard_delete_work_item")
	if !ok {
		t.Fatal("hard_delete_work_item is not registered — the agent has no way to remove a row, only to " +
			"cancel it, which is the gap the operator identified")
	}
	if !td.Mutating {
		t.Error("hard_delete_work_item is not marked Mutating, so the confirm-before-mutate discipline would " +
			"not apply to a destructive, irreversible call")
	}
	desc := strings.ToUpper(td.Description)
	for _, want := range []string{"PERMANENTLY", "IRREVERSIBLE", "CANNOT BE RESTORED"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the tool's description does not state %q, which is the only warning an agent gets: %q",
				want, td.Description)
		}
	}
	// And the SOFT delete must point at it, so an agent hunting for a real removal is told the right name
	// rather than settling for a status change.
	soft, ok := r.Get("delete_work_item")
	if !ok {
		t.Fatal("delete_work_item vanished")
	}
	if !strings.Contains(soft.Description, "hard_delete_work_item") {
		t.Error("delete_work_item does not point at the hard delete, so an agent looking for a real removal " +
			"would settle for a status change")
	}
	// THE CHORD THE PROMPT TEACHES MUST RESOLVE — the invariant this package already pins for every tool,
	// asserted here so a rename cannot leave the model calling a name that does not dispatch.
	if _, ok := r.Get(normalizeAskToolName(askToolNamePrefix + "hard_delete_work_item")); !ok {
		t.Error("the advertised `orchicon_hard_delete_work_item` form does not resolve")
	}
}

// THE PROMPT ADVERTISES IT, so the model knows it can call it.
func TestThePromptAdvertisesTheHardDeleteTool(t *testing.T) {
	full := NewToolRegistry(nil, nil, nil)
	if _, ok := full.Get("hard_delete_work_item"); !ok {
		t.Fatal("fixture: the full registry does not carry the new tool")
	}
	p := BuildSystemPrompt(modeQuickWork, testAgentConfig(), full)
	if !strings.Contains(p, "`orchicon_hard_delete_work_item`") {
		t.Error("the prompt does not advertise `orchicon_hard_delete_work_item`, so the model cannot know the " +
			"capability exists")
	}
}

// IT ACTUALLY DELETES THE ROW — not a status change.
func TestHardDeleteWorkItemToolRemovesTheRow(t *testing.T) {
	pool := chatDBTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projID := createProjectForTest(t, ctx, pool)
	item := createWorkItemForTest(t, ctx, pool, projID, "ephemeral job")

	out, err := toolHardDeleteWorkItem(ctx, pool, mustJSON(t, map[string]any{"id": item.ID}))
	if err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	if res["hard_deleted"] != true {
		t.Errorf("result = %v, want hard_deleted true", res)
	}

	// THE ROW IS GONE, which is the whole difference from delete_work_item.
	if workItemExistsForTest(t, pool, item.ID) {
		t.Error("the work item still exists after a hard delete — the tool cancelled it instead of removing it")
	}
}

// AND IT LEAVES AN AUDIT TRAIL. The service records "work_item.hard_deleted"; a destructive tool that audits
// nothing destroys the row that held the evidence and leaves no record that it existed.
func TestHardDeleteWorkItemToolAudits(t *testing.T) {
	pool := chatDBTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projID := createProjectForTest(t, ctx, pool)
	item := createWorkItemForTest(t, ctx, pool, projID, "audited job")

	if _, err := toolHardDeleteWorkItem(ctx, pool, mustJSON(t, map[string]any{"id": item.ID})); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	if !auditRowExistsForTest(t, pool, "work_item.hard_deleted", item.ID) {
		t.Error("no work_item.hard_deleted audit row — the row that held the evidence is gone and nothing " +
			"records that it existed")
	}
}

// AN ITEM WITH CHILDREN IS REFUSED.
//
// `work_items.parent_id` has NO foreign key (the hierarchy is enforced in the service layer), so deleting a
// parent does not fail — it ORPHANS the children, leaving rows whose parent no longer exists. That is silent
// corruption, and an agent cannot see the children it would strand, so the tool refuses and says what to do.
func TestHardDeleteWorkItemToolRefusesAnItemWithChildren(t *testing.T) {
	pool := chatDBTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projID := createProjectForTest(t, ctx, pool)
	parent := createWorkItemForTest(t, ctx, pool, projID, "parent")
	createChildWorkItemForTest(t, ctx, pool, projID, parent.ID, "child")

	_, err := toolHardDeleteWorkItem(ctx, pool, mustJSON(t, map[string]any{"id": parent.ID}))
	if err == nil {
		t.Fatal("a work item with children was hard-deleted — its children now point at a row that no longer " +
			"exists")
	}
	if !strings.Contains(err.Error(), "child work item") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
	// The parent must still be there: a refused call must not have half-deleted anything.
	if !workItemExistsForTest(t, pool, parent.ID) {
		t.Error("the refused delete removed the parent anyway")
	}
}

// AN IDEA IS REFUSED. Destroying an idea takes its provenance with it, and provenance is the entire record of
// where an idea came from.
func TestHardDeleteWorkItemToolRefusesAnIdea(t *testing.T) {
	pool := chatDBTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projID := createProjectForTest(t, ctx, pool)
	idea := createWorkItemForTest(t, ctx, pool, projID, "spawned idea")
	setWorkItemStatusForTest(t, pool, idea.ID, domain.WorkItemIdea)

	_, err := toolHardDeleteWorkItem(ctx, pool, mustJSON(t, map[string]any{"id": idea.ID}))
	if err == nil {
		t.Fatal("an idea-state item was hard-deleted, taking its provenance with it")
	}
	if !workItemExistsForTest(t, pool, idea.ID) {
		t.Error("the refused delete removed the idea anyway")
	}
}

// A BAD CALL FAILS BEFORE TOUCHING ANYTHING: a missing id is refused rather than read as "removed nothing,
// successfully".
func TestHardDeleteWorkItemToolRequiresAnID(t *testing.T) {
	pool := chatDBTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	if _, err := toolHardDeleteWorkItem(ctx, pool, mustJSON(t, map[string]any{})); err == nil {
		t.Error("an empty id was accepted")
	}
	if _, err := toolHardDeleteWorkItem(ctx, pool, json.RawMessage(`not json`)); err == nil {
		t.Error("malformed args were accepted")
	}
}

// --- helpers ----------------------------------------------------------------

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

// workItemExistsForTest reports whether the row is still there, in its own transaction so a delete's commit
// cannot hide behind the caller's tx.
func workItemExistsForTest(t *testing.T, pool *db.Pool, id string) bool {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	_, err = db.GetWorkItem(ctx, ttx.Tx, workItemKindTestTenant, id)
	return err == nil
}

// createChildWorkItemForTest adds a work item under a parent, which is what makes the orphan guard testable.
func createChildWorkItemForTest(t *testing.T, ctx context.Context, pool *db.Pool, projID, parentID, title string) db.WorkItemRow {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	pid := parentID
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, ProjectID: projID,
		ParentID: &pid, Kind: domain.WorkItemKindFeature, Title: title, Status: domain.WorkItemPending,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit child: %v", err)
	}
	return w
}

// setWorkItemStatusForTest moves an item to a status the fixture cannot create directly (an idea).
func setWorkItemStatusForTest(t *testing.T, pool *db.Pool, id, status string) {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	cur, err := db.GetWorkItem(ctx, ttx.Tx, workItemKindTestTenant, id)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, workItemKindTestTenant, id, cur.Version,
		db.UpdateWorkItemFields{Status: &status}); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit status: %v", err)
	}
}

// auditRowExistsForTest reads the audit trail for one target+action.
func auditRowExistsForTest(t *testing.T, pool *db.Pool, action, targetID string) bool {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var n int
	if err := ttx.Tx.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		workItemKindTestTenant, action, targetID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n > 0
}
