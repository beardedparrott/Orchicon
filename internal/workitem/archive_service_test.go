package workitem

// Archive/Restore service tests: ArchiveWorkItem hides a terminal item from
// every normal list read and can be restored to its prior terminal status.
// Skipped unless ORCHICON_TEST_DSN points at a disposable database (repo
// convention — the archive columns are added by the embedded migrations,
// which validateParentTestPool applies on every run).

import (
	"context"
	"errors"
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

// bumpWorkItemVersion commits a concurrent version bump (a separate,
// committed transaction) to simulate the sequence engine mutating a parent
// behind the archive RPC's back mid-flight.
func bumpWorkItemVersion(t *testing.T, pool *db.Pool, tenantID, id string, expectedVersion int) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin bump tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	title := "bumped by sequence engine"
	w, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, id, expectedVersion, db.UpdateWorkItemFields{Title: &title})
	if err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit bump: %v", err)
	}
	return w
}

// TestArchiveWorkItemRetriesOnceOnVersionConflict exercises the one-shot
// retry (docs/09 §5): a concurrent writer (simulating the sequence engine)
// bumps the row's version between ArchiveWorkItem's initial read and its
// archive write. The RPC must re-read, re-validate against the fresh row,
// and retry exactly once — succeeding here since only one bump occurs —
// with exactly one audit event and one outbox event (no double emission).
func TestArchiveWorkItemRetriesOnceOnVersionConflict(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	item := archiveItem(t, pool, tenantID, projectID, domain.WorkItemSucceeded)

	hookCalls := 0
	s.archiveConflictHook = func(attempt int) {
		hookCalls++
		if attempt == 1 {
			// Commit the bump from a SEPARATE, already-committed
			// transaction right before the archive RPC's own transaction
			// attempts its first write — a real Postgres version conflict,
			// not a mock.
			bumpWorkItemVersion(t, pool, tenantID, item.ID, item.Version)
		}
	}

	got, err := s.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: item.ID}))
	if err != nil {
		t.Fatalf("archive work item: %v", err)
	}
	if got.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED {
		t.Fatalf("status = %v, want archived", got.Msg.WorkItem.Status)
	}
	if hookCalls != 2 {
		t.Fatalf("archiveConflictHook calls = %d, want 2 (initial attempt + retry)", hookCalls)
	}
	if n := auditEventCount(t, pool, tenantID, "work_item.archived", "work_item", item.ID); n != 1 {
		t.Fatalf("work_item.archived audit rows = %d, want exactly 1 (no double emission)", n)
	}
	if n := countOutboxEvent(t, pool, ctx, tenantID, "work_item.archived", item.ID); n != 1 {
		t.Fatalf("work_item.archived outbox events = %d, want exactly 1", n)
	}
}

// TestArchiveWorkItemPersistentConflictSurfacesError: a SECOND consecutive
// conflict (on the retry itself) is a real race, not staleness — the
// bounded retry must not loop, and the error must surface to the caller.
func TestArchiveWorkItemPersistentConflictSurfacesError(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	item := archiveItem(t, pool, tenantID, projectID, domain.WorkItemSucceeded)

	s.archiveConflictHook = func(attempt int) {
		// Re-read the current row via a throwaway GetWorkItem call so the
		// bump always targets the row's CURRENT version, then bump again
		// on both the initial attempt and the retry — a persistent race.
		ttx, err := pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			t.Fatalf("begin read tx: %v", err)
		}
		current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, item.ID)
		ttx.Rollback(ctx)
		if err != nil {
			t.Fatalf("read current: %v", err)
		}
		bumpWorkItemVersion(t, pool, tenantID, item.ID, current.Version)
	}

	_, err := s.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: item.ID}))
	if err == nil {
		t.Fatal("expected a persistent version conflict to surface as an error")
	}
	if !errors.Is(err, db.ErrVersionConflict) {
		t.Fatalf("error = %v, want ErrVersionConflict", err)
	}
	if n := auditEventCount(t, pool, tenantID, "work_item.archived", "work_item", item.ID); n != 0 {
		t.Fatalf("work_item.archived audit rows = %d, want 0 (archive never succeeded)", n)
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

// TestArchiveAndRestoreSkippedWorkItem pins AC13: a skipped item stays
// archivable and restorable, confirming domain.WorkItemIsTerminalArchivable
// still covers "skipped" now that it is a normal, user-settable status
// rather than a system-managed one.
func TestArchiveAndRestoreSkippedWorkItem(t *testing.T) {
	pool, s, ctx, tenantID, projectID := archiveEnv(t)
	item := archiveItem(t, pool, tenantID, projectID, domain.WorkItemSkipped)

	archived, err := s.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: item.ID}))
	if err != nil {
		t.Fatalf("archive skipped item: %v", err)
	}
	if archived.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED {
		t.Fatalf("status = %v, want archived", archived.Msg.WorkItem.Status)
	}
	if archived.Msg.WorkItem.ArchivedFromStatus != domain.WorkItemSkipped {
		t.Fatalf("archived_from_status = %q, want skipped", archived.Msg.WorkItem.ArchivedFromStatus)
	}

	restored, err := s.RestoreWorkItem(ctx, connect.NewRequest(&apiv1.RestoreWorkItemRequest{Id: item.ID}))
	if err != nil {
		t.Fatalf("restore skipped item: %v", err)
	}
	if restored.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_SKIPPED {
		t.Fatalf("restored status = %v, want skipped (prior terminal status)", restored.Msg.WorkItem.Status)
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
