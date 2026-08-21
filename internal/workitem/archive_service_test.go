package workitem

// Archive/Restore service tests: ArchiveWorkItem hides a terminal item from
// every normal list read and can be restored to its prior terminal status.
// Skipped unless ORCHICON_TEST_DSN points at a disposable database (repo
// convention — the archive columns are added by the embedded migrations,
// which validateParentTestPool applies on every run).

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// archiveEnv wires the service + audit actor over the test pool and returns
// the pool, service, ctx, tenant id and project id.
func archiveEnv(t *testing.T) (*db.Pool, *Service, context.Context, string, string) {
	t.Helper()
	tenantID := "tnt-archive-" + strings.ToLower(db.NewID())
	pool, s, ctx, _, _ := auditServiceEnv(t, tenantID)
	return pool, s, ctx, tenantID, seedAuditProject(t, pool, tenantID)
}

func archiveItem(t *testing.T, pool *db.Pool, tenantID, projectID, status string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: "task", Title: "Archive test item", Status: status,
		Budgets: []byte("{}"), Results: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit work item: %v", err)
	}
	return w
}

func archiveItemChild(t *testing.T, pool *db.Pool, tenantID, projectID, parentID string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID, ParentID: &parentID,
		Kind: "subtask", Title: "Archive child", Status: domain.WorkItemPending,
		Budgets: []byte("{}"), Results: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create child work item: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit child work item: %v", err)
	}
	return w
}

func TestArchiveWorkItem(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	item := archiveItem(t, pool, tenantID, projectID, domain.WorkItemSucceeded)

	got, err := s.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: item.ID}))
	if err != nil {
		t.Fatalf("archive work item: %v", err)
	}
	if got.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED {
		t.Fatalf("status = %v, want archived", got.Msg.WorkItem.Status)
	}
	if got.Msg.WorkItem.ArchivedAt == nil {
		t.Fatal("archived_at not set")
	}
	if got.Msg.WorkItem.ArchivedFromStatus != domain.WorkItemSucceeded {
		t.Fatalf("archived_from_status = %q, want succeeded", got.Msg.WorkItem.ArchivedFromStatus)
	}
}

func TestArchiveWorkItemRejectsNonTerminal(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	item := archiveItem(t, pool, tenantID, projectID, domain.WorkItemReady)

	_, err := s.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: item.ID}))
	if err == nil {
		t.Fatal("expected archive of a non-terminal item to fail")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("error code = %v, want FailedPrecondition", got)
	}
}

func TestArchiveWorkItemRejectsHasChildren(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	parent := archiveItem(t, pool, tenantID, projectID, domain.WorkItemSucceeded)
	archiveItemChild(t, pool, tenantID, projectID, parent.ID)

	_, err := s.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: parent.ID}))
	if err == nil {
		t.Fatal("expected archive of an item with children to fail")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("error code = %v, want FailedPrecondition", got)
	}
}

func TestListWorkItemsExcludesArchivedByDefault(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	active := archiveItem(t, pool, tenantID, projectID, domain.WorkItemSucceeded)
	archived := archiveItem(t, pool, tenantID, projectID, domain.WorkItemSucceeded)

	if _, err := s.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: archived.ID})); err != nil {
		t.Fatalf("archive item: %v", err)
	}

	activeResp, err := s.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		ProjectId: projectID,
		PageSize:  100,
	}))
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	activeIDs := map[string]bool{}
	for _, w := range activeResp.Msg.WorkItems {
		activeIDs[w.Id] = true
	}
	if !activeIDs[active.ID] {
		t.Fatal("active item missing from default list")
	}
	if activeIDs[archived.ID] {
		t.Fatal("archived item leaked into the default (active) list")
	}

	archResp, err := s.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		ProjectId:       projectID,
		PageSize:        100,
		IncludeArchived: true,
	}))
	if err != nil {
		t.Fatalf("list archived: %v", err)
	}
	if len(archResp.Msg.WorkItems) != 1 || archResp.Msg.WorkItems[0].Id != archived.ID {
		t.Fatalf("archive view returned %d items, want just the archived item", len(archResp.Msg.WorkItems))
	}
}

func TestRestoreWorkItem(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	item := archiveItem(t, pool, tenantID, projectID, domain.WorkItemFailed)
	if _, err := s.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: item.ID})); err != nil {
		t.Fatalf("archive item: %v", err)
	}

	restored, err := s.RestoreWorkItem(ctx, connect.NewRequest(&apiv1.RestoreWorkItemRequest{Id: item.ID}))
	if err != nil {
		t.Fatalf("restore work item: %v", err)
	}
	if restored.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_FAILED {
		t.Fatalf("restored status = %v, want failed (prior terminal status)", restored.Msg.WorkItem.Status)
	}
	if restored.Msg.WorkItem.ArchivedAt != nil {
		t.Fatal("archived_at not cleared on restore")
	}
	if restored.Msg.WorkItem.ArchivedFromStatus != "" {
		t.Fatalf("archived_from_status = %q, want cleared", restored.Msg.WorkItem.ArchivedFromStatus)
	}
}

func TestRestoreWorkItemRejectsActive(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	item := archiveItem(t, pool, tenantID, projectID, domain.WorkItemSucceeded)

	_, err := s.RestoreWorkItem(ctx, connect.NewRequest(&apiv1.RestoreWorkItemRequest{Id: item.ID}))
	if err == nil {
		t.Fatal("expected restore of an active item to fail")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("error code = %v, want FailedPrecondition", got)
	}
}

// TestUpdateWorkItemRejectsArchivedStatus: the generic update path must NOT
// be able to set status='archived' (that is the ArchiveWorkItem RPC's job,
// with its terminal-only + no-children preconditions and archived_at
// bookkeeping). Setting it via update would write status='archived' with
// archived_at NULL — which every active view (archived_at IS NULL filter)
// would wrongly surface.
func TestUpdateWorkItemRejectsArchivedStatus(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	item := archiveItem(t, pool, tenantID, projectID, domain.WorkItemSucceeded)

	st := apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED
	_, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:     item.ID,
		Status: &st,
	}))
	if err == nil {
		t.Fatal("expected setting archived via the generic update to fail")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("error code = %v, want FailedPrecondition", got)
	}
}
