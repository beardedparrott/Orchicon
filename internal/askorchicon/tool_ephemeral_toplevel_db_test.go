package askorchicon

// tool_ephemeral_toplevel_db_test.go — the top-level exemption for ephemeral
// work items, end to end through the real create_work_item tool against
// Postgres.
//
// Quick Work creates ONE ephemeral item per job. Two rules collide on it:
// ephemeral items are TOP-LEVEL ONLY (validateEphemeralPlacement refuses a
// parent, because a child would be invisible in the list yet rendered inside
// its real parent's tree), and workitem.ValidateParent refuses a top-level
// non-epic ("a task must have a parent; only epics can be top-level").
// Together they made an ephemeral TASK impossible, so every ephemeral item
// had to be created with kind=epic — which mislabels a transient unit of
// work as a planning container. These tests pin the exemption that removes
// the contradiction, and pin that it is scoped to ephemeral items only.
//
// The child half of the contract (an ephemeral item WITH a parent is still
// refused) is pure and already pinned by
// TestCreateWorkItemRefusesAnEphemeralChildBeforeTouchingThePool and
// TestEphemeralPlacementRefusesAChildAndPermitsTopLevel in
// tool_ephemeral_test.go; the placement guard is untouched by this change, so
// those tests staying green is the assertion.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// AN EPHEMERAL TASK WITH NO PARENT IS CREATED. This is the acceptance
// criterion: refused today with "a task must have a parent; only epics can be
// top-level", created after the exemption.
func TestCreateWorkItemEphemeralTopLevelTaskSucceeds(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	raw, err := json.Marshal(map[string]any{
		"project_id": projectID,
		"title":      "Quick Work transient " + strings.ToLower(db.NewID()),
		"kind":       domain.WorkItemKindTask,
		"ephemeral":  true,
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := toolCreateWorkItem(ctx, pool, raw)
	if err != nil {
		t.Fatalf("create_work_item refused a top-level EPHEMERAL TASK: %v\n"+
			"A Quick Work dispatch is a transient unit of work, not an epic. Refusing it forces "+
			"every ephemeral item to be created with kind=epic.", err)
	}
	var item db.WorkItemRow
	if err := json.Unmarshal(res, &item); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if item.Kind != domain.WorkItemKindTask {
		t.Errorf("created kind = %q, want %q — the exemption must not re-type the item to make it legal",
			item.Kind, domain.WorkItemKindTask)
	}
	if !item.Ephemeral {
		t.Error("created item came back with Ephemeral=false — the exemption must create a real " +
			"ephemeral item, not an ordinary one with the flag dropped")
	}
	if item.ParentID != nil {
		t.Errorf("created item has parent %v, want none — ephemeral items stay top-level only", *item.ParentID)
	}

	// Reload from the table: the row must really carry the flag (not be a
	// value the tool only echoed back in its response).
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	reloaded, err := db.GetWorkItem(ctx, ttx.Tx, workItemKindTestTenant, item.ID)
	if err != nil {
		t.Fatalf("reload created item: %v", err)
	}
	if !reloaded.Ephemeral || reloaded.ParentID != nil || reloaded.Kind != domain.WorkItemKindTask {
		t.Errorf("reloaded item = {ephemeral:%v parent:%v kind:%s}, want {true <nil> task}",
			reloaded.Ephemeral, reloaded.ParentID, reloaded.Kind)
	}
}

// A NON-EPHEMERAL TASK WITH NO PARENT IS STILL REFUSED, with the unchanged
// message. The exemption is scoped to ephemeral items; the ordinary orphan
// task — the exact shape the hierarchy invariant forbids — is untouched.
func TestCreateWorkItemNonEphemeralTopLevelTaskStillRefused(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	raw, err := json.Marshal(map[string]any{
		"project_id": projectID,
		"title":      "Ordinary orphan task " + strings.ToLower(db.NewID()),
		"kind":       domain.WorkItemKindTask,
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	_, err = toolCreateWorkItem(ctx, pool, raw)
	if err == nil {
		t.Fatal("create_work_item accepted an ordinary (non-ephemeral) top-level task — the exemption " +
			"leaked out of the ephemeral scope and the planning hierarchy is no longer enforced for real work")
	}
	const want = "a task must have a parent; only epics can be top-level"
	if err.Error() != want {
		t.Errorf("refusal = %q, want the unchanged message %q (clients surface it verbatim)", err.Error(), want)
	}
}
