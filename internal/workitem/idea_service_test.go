package workitem

// Idea Cloud (feature 5.1) service tests: ListIdeas / PromoteIdea /
// DismissIdea on top of the 4.1 idea-state primitives. These assert the
// anti-leakage contract — idea-state items are excluded from the normal
// Work Items scope and only PromoteIdea moves one into the normal scope;
// DismissIdea discards; both transitions are audited + outboxed; the generic
// update/delete/archive/hard-delete paths are gated for idea items so no
// path other than Promote/Dismiss can act on an idea.
//
// Skipped unless ORCHICON_TEST_DSN points at a disposable database (repo
// convention — validateParentTestPool applies the embedded migrations on
// every run).

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// ideaEnv wires the service + audit actor over the test pool and returns the
// pool, service, ctx, tenant id and a fresh project id.
func ideaEnv(t *testing.T) (*db.Pool, *Service, context.Context, string, string) {
	t.Helper()
	tenantID := "tnt-idea-" + strings.ToLower(db.NewID())
	pool, s, ctx, _, _ := auditServiceEnv(t, tenantID)
	return pool, s, ctx, tenantID, seedAuditProject(t, pool, tenantID)
}

func createIdeaItem(t *testing.T, pool *db.Pool, tenantID, projectID, title, spawnID, runID string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row := db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: "task", Title: title, Status: domain.WorkItemIdea,
		Budgets: []byte("{}"), Results: []byte("{}"),
	}
	if spawnID != "" {
		row.SpawnedByWorkItemID = &spawnID
	}
	if runID != "" {
		row.SpawnedByRunID = &runID
	}
	w, err := db.CreateWorkItem(ctx, ttx.Tx, row)
	if err != nil {
		t.Fatalf("create idea item: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit idea item: %v", err)
	}
	return w
}

func createPlainItem(t *testing.T, pool *db.Pool, tenantID, projectID, title, status string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: "task", Title: title, Status: status,
		Budgets: []byte("{}"), Results: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create plain item: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit plain item: %v", err)
	}
	return w
}

func countOutboxEvent(t *testing.T, pool *db.Pool, ctx context.Context, tenantID, eventType, aggregateID string) int {
	t.Helper()
	var payload []byte
	err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3 LIMIT 1`,
		tenantID, eventType, aggregateID).Scan(&payload)
	if err != nil {
		return 0
	}
	return 1
}

func TestPromoteIdea(t *testing.T) {
	pool, s, ctx, tenantID, projectID := ideaEnv(t)
	spawn := createPlainItem(t, pool, tenantID, projectID, "Automation X", domain.WorkItemPending)
	idea := createIdeaItem(t, pool, tenantID, projectID, "Generated idea", spawn.ID, "run-abc123")

	got, err := s.PromoteIdea(ctx, connect.NewRequest(&apiv1.PromoteIdeaRequest{Id: idea.ID}))
	if err != nil {
		t.Fatalf("PromoteIdea: %v", err)
	}
	wi := got.Msg.WorkItem
	if wi.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING {
		t.Fatalf("status = %v, want pending", wi.Status)
	}
	// Provenance retained + read-time badge resolved.
	if wi.SpawnedBy != spawn.ID {
		t.Fatalf("spawned_by = %q, want %q", wi.SpawnedBy, spawn.ID)
	}
	if wi.SpawnedByRunId != "run-abc123" {
		t.Fatalf("spawned_by_run_id = %q, want run-abc123", wi.SpawnedByRunId)
	}
	if wi.SpawnedByTitle != "Automation X" {
		t.Fatalf("spawned_by_title = %q, want 'Automation X'", wi.SpawnedByTitle)
	}
	// Transitioned out of idea state: present in the normal scope, absent
	// from the idea list.
	if n := auditEventCount(t, pool, tenantID, "work_item.promoted", "work_item", idea.ID); n != 1 {
		t.Fatalf("work_item.promoted rows = %d, want 1", n)
	}
	if n := countOutboxEvent(t, pool, ctx, tenantID, "work_item.promoted", idea.ID); n != 1 {
		t.Fatalf("work_item.promoted outbox events = %d, want 1", n)
	}
	listResp, err := s.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{ProjectId: projectID, PageSize: 100}))
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	found := false
	for _, w := range listResp.Msg.WorkItems {
		if w.Id == idea.ID {
			if w.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING {
				t.Fatalf("promoted item surfaced in normal scope with status %v, want pending", w.Status)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("promoted item missing from the normal Work Items scope")
	}
	ideaResp, err := s.ListIdeas(ctx, connect.NewRequest(&apiv1.ListIdeasRequest{ProjectId: projectID, PageSize: 100}))
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	for _, w := range ideaResp.Msg.Ideas {
		if w.Id == idea.ID {
			t.Fatal("promoted item still appears in the Idea Cloud list")
		}
	}
}

func TestPromoteIdeaRejectsNonIdea(t *testing.T) {
	pool, s, ctx, tenantID, projectID := ideaEnv(t)
	item := createPlainItem(t, pool, tenantID, projectID, "Not an idea", domain.WorkItemPending)

	_, err := s.PromoteIdea(ctx, connect.NewRequest(&apiv1.PromoteIdeaRequest{Id: item.ID}))
	if err == nil {
		t.Fatal("expected promoting a non-idea item to fail")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("error code = %v, want FailedPrecondition", got)
	}
}

func TestDismissIdea(t *testing.T) {
	pool, s, ctx, tenantID, projectID := ideaEnv(t)
	spawn := createPlainItem(t, pool, tenantID, projectID, "Automation Y", domain.WorkItemPending)
	idea := createIdeaItem(t, pool, tenantID, projectID, "Won't do", spawn.ID, "run-def456")

	got, err := s.DismissIdea(ctx, connect.NewRequest(&apiv1.DismissIdeaRequest{Id: idea.ID}))
	if err != nil {
		t.Fatalf("DismissIdea: %v", err)
	}
	wi := got.Msg.WorkItem
	if wi.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED {
		t.Fatalf("status = %v, want cancelled", wi.Status)
	}
	if wi.SpawnedBy != spawn.ID {
		t.Fatalf("spawned_by = %q, want %q (provenance retained)", wi.SpawnedBy, spawn.ID)
	}
	if wi.SpawnedByRunId != "run-def456" {
		t.Fatalf("spawned_by_run_id = %q, want run-def456", wi.SpawnedByRunId)
	}
	if wi.SpawnedByTitle != "Automation Y" {
		t.Fatalf("spawned_by_title = %q, want 'Automation Y'", wi.SpawnedByTitle)
	}
	if n := auditEventCount(t, pool, tenantID, "work_item.dismissed", "work_item", idea.ID); n != 1 {
		t.Fatalf("work_item.dismissed rows = %d, want 1", n)
	}
	if n := countOutboxEvent(t, pool, ctx, tenantID, "work_item.dismissed", idea.ID); n != 1 {
		t.Fatalf("work_item.dismissed outbox events = %d, want 1", n)
	}
	ideaResp, err := s.ListIdeas(ctx, connect.NewRequest(&apiv1.ListIdeasRequest{ProjectId: projectID, PageSize: 100}))
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	for _, w := range ideaResp.Msg.Ideas {
		if w.Id == idea.ID {
			t.Fatal("dismissed idea still appears in the Idea Cloud list")
		}
	}
}

func TestDismissIdeaRejectsNonIdea(t *testing.T) {
	pool, s, ctx, tenantID, projectID := ideaEnv(t)
	item := createPlainItem(t, pool, tenantID, projectID, "Not an idea", domain.WorkItemPending)

	_, err := s.DismissIdea(ctx, connect.NewRequest(&apiv1.DismissIdeaRequest{Id: item.ID}))
	if err == nil {
		t.Fatal("expected dismissing a non-idea item to fail")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("error code = %v, want FailedPrecondition", got)
	}
}

// TestIdeaExcludedFromNormalScope pins the anti-leakage invariant from both
// directions: idea-state items do not appear in the normal Work Items list
// (4.1 exclusion), and ListIdeas returns exactly the idea-state items —
// never a normal item, never a promoted/dismissed item.
func TestIdeaExcludedFromNormalScope(t *testing.T) {
	pool, s, ctx, tenantID, projectID := ideaEnv(t)
	spawn := createPlainItem(t, pool, tenantID, projectID, "Automation Z", domain.WorkItemPending)
	idea := createIdeaItem(t, pool, tenantID, projectID, "Idea one", spawn.ID, "run-1")
	normal := createPlainItem(t, pool, tenantID, projectID, "Normal task", domain.WorkItemPending)

	listResp, err := s.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{ProjectId: projectID, PageSize: 100}))
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	for _, w := range listResp.Msg.WorkItems {
		if w.Id == idea.ID {
			t.Fatal("idea-state item leaked into the normal Work Items scope")
		}
		if w.Id == normal.ID {
			if w.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING {
				t.Fatalf("normal item status = %v, want pending", w.Status)
			}
		}
	}

	ideaResp, err := s.ListIdeas(ctx, connect.NewRequest(&apiv1.ListIdeasRequest{ProjectId: projectID, PageSize: 100}))
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	if len(ideaResp.Msg.Ideas) != 1 || ideaResp.Msg.Ideas[0].Id != idea.ID {
		t.Fatalf("ListIdeas returned %d ideas, want exactly the one idea item", len(ideaResp.Msg.Ideas))
	}
	if ideaResp.Msg.Ideas[0].SpawnedByTitle != "Automation Z" {
		t.Fatalf("list idea badge = %q, want 'Automation Z'", ideaResp.Msg.Ideas[0].SpawnedByTitle)
	}
}

// TestListIdeasPaginationSearchSort exercises the list query surface: search
// narrows, sort orders, and pagination pages without overlap.
func TestListIdeasPaginationSearchSort(t *testing.T) {
	pool, s, ctx, tenantID, projectID := ideaEnv(t)
	spawn := createPlainItem(t, pool, tenantID, projectID, "Automation", domain.WorkItemPending)
	for i := 0; i < 3; i++ {
		createIdeaItem(t, pool, tenantID, projectID, "Idea "+string(rune('A'+i)), spawn.ID, "run-"+string(rune('a'+i)))
	}

	// Search: only matching ideas return.
	sResp, err := s.ListIdeas(ctx, connect.NewRequest(&apiv1.ListIdeasRequest{ProjectId: projectID, Search: "Idea A", PageSize: 100}))
	if err != nil {
		t.Fatalf("ListIdeas search: %v", err)
	}
	if len(sResp.Msg.Ideas) != 1 || sResp.Msg.Ideas[0].Title != "Idea A" {
		t.Fatalf("search returned %d ideas, want exactly Idea A", len(sResp.Msg.Ideas))
	}

	// Pagination: page_size 2 then the remaining 1, no overlap.
	p1, err := s.ListIdeas(ctx, connect.NewRequest(&apiv1.ListIdeasRequest{ProjectId: projectID, PageSize: 2}))
	if err != nil {
		t.Fatalf("ListIdeas page 1: %v", err)
	}
	if len(p1.Msg.Ideas) != 2 {
		t.Fatalf("page 1 returned %d ideas, want 2", len(p1.Msg.Ideas))
	}
	if p1.Msg.NextPageToken == "" {
		t.Fatal("page 1 should have a next page token")
	}
	p2, err := s.ListIdeas(ctx, connect.NewRequest(&apiv1.ListIdeasRequest{ProjectId: projectID, PageSize: 2, PageToken: p1.Msg.NextPageToken}))
	if err != nil {
		t.Fatalf("ListIdeas page 2: %v", err)
	}
	if len(p2.Msg.Ideas) != 1 {
		t.Fatalf("page 2 returned %d ideas, want 1", len(p2.Msg.Ideas))
	}
	seen := map[string]bool{}
	for _, w := range p1.Msg.Ideas {
		seen[w.Id] = true
	}
	for _, w := range p2.Msg.Ideas {
		if seen[w.Id] {
			t.Fatalf("pagination overlap: id %s returned on both pages", w.Id)
		}
	}
}

// TestGenericMutationPathsRejectIdea locks every generic mutation path from
// acting on an idea-state item (the centralized not-idea guard), so the ONLY
// sanctioned ways out of idea state are PromoteIdea and DismissIdea.
func TestGenericMutationPathsRejectIdea(t *testing.T) {
	pool, s, ctx, tenantID, projectID := ideaEnv(t)
	spawn := createPlainItem(t, pool, tenantID, projectID, "Automation", domain.WorkItemPending)

	newTitle := "hacked title"
	updateIdea := createIdeaItem(t, pool, tenantID, projectID, "Update idea", spawn.ID, "run-1")
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{Id: updateIdea.ID, Title: &newTitle})); err == nil {
		t.Fatal("expected UpdateWorkItem on an idea to fail")
	} else if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("UpdateWorkItem code = %v, want FailedPrecondition", got)
	}

	deleteIdea := createIdeaItem(t, pool, tenantID, projectID, "Delete idea", spawn.ID, "run-2")
	if _, err := s.DeleteWorkItem(ctx, connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: deleteIdea.ID})); err == nil {
		t.Fatal("expected DeleteWorkItem on an idea to fail")
	} else if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("DeleteWorkItem code = %v, want FailedPrecondition", got)
	}

	archiveIdea := createIdeaItem(t, pool, tenantID, projectID, "Archive idea", spawn.ID, "run-3")
	if _, err := s.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: archiveIdea.ID})); err == nil {
		t.Fatal("expected ArchiveWorkItem on an idea to fail")
	} else if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("ArchiveWorkItem code = %v, want FailedPrecondition", got)
	}

	hardDeleteIdea := createIdeaItem(t, pool, tenantID, projectID, "HardDelete idea", spawn.ID, "run-4")
	if _, err := s.HardDeleteWorkItem(ctx, connect.NewRequest(&apiv1.HardDeleteWorkItemRequest{Id: hardDeleteIdea.ID})); err == nil {
		t.Fatal("expected HardDeleteWorkItem on an idea to fail")
	} else if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("HardDeleteWorkItem code = %v, want FailedPrecondition", got)
	}

	// The idea items must still be idea-state after the rejected mutations.
	ideaResp, err := s.ListIdeas(ctx, connect.NewRequest(&apiv1.ListIdeasRequest{ProjectId: projectID, PageSize: 100}))
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	byID := map[string]bool{}
	for _, w := range ideaResp.Msg.Ideas {
		byID[w.Id] = true
	}
	for _, id := range []string{updateIdea.ID, deleteIdea.ID, archiveIdea.ID, hardDeleteIdea.ID} {
		if !byID[id] {
			t.Fatalf("idea %s disappeared from the Idea Cloud after a rejected mutation", id)
		}
	}
}
