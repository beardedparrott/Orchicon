package workitem

// Service-level audit tests (design §5 item 3): every mutating RPC writes
// exactly one audit_events row in the same transaction as the mutation
// (transactional outbox), read-only RPCs write zero, and the actor fields
// (actor_type=user, auth_method) round-trip. Skipped unless
// ORCHICON_TEST_DSN points at a disposable database (repo convention):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/workitem/ -run TestAuditService -v

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

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

// auditServiceEnv wires the service over a migration-applied test pool and
// returns a context carrying BOTH the tenant and a resolved identity (the
// middleware-equivalent for the audit actor), plus the acting identity id.
// Each caller passes a unique tenant id so sibling tests never share audit
// rows (a mutation test's rows would otherwise leak into a "no rows" test).
func auditServiceEnv(t *testing.T, tenantID string) (*db.Pool, *Service, context.Context, string, string) {
	t.Helper()
	pool := validateParentTestPool(t)
	ident := seedAuditIdentity(t, pool, tenantID, "audit-wi-"+strings.ToLower(db.NewID()))
	ctx := tenant.WithID(context.Background(), tenantID)
	ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
		IdentityID: ident.ID,
		TenantID:   tenantID,
		Subject:    ident.Subject,
		AuthMethod: "oidc",
		IsAdmin:    true,
	})
	s := New(pool, slog.New(slog.DiscardHandler))
	return pool, s, ctx, tenantID, ident.ID
}

// seedAuditProject creates an active project row in the given tenant (the
// work-item service requires an active project on create).
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
	// The fixture cleans up after itself, through the SAME cascade the product uses — so a fixture
	// can never leave a project (or its executions/usage/recoveries) behind in the dev tenant.
	db.CleanupProject(t, pool, tenantID, proj.ID)
	return proj.ID
}

// auditEventCount counts audit rows matching the exact action + target
// (idempotent across shared-DB runs: unique target ids per test).
func auditEventCount(t *testing.T, pool *db.Pool, tenantID, action, targetType, targetID string) int {
	t.Helper()
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, action, "", targetType, targetID, "", 100, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	return len(rows)
}

// TestAuditServiceWorkItemMutations asserts the transactional-outbox
// contract through the real handler surface: one row per mutation, zero
// rows for Get/List, correct actor + auth_method + before/after.
func TestAuditServiceWorkItemMutations(t *testing.T) {
	pool, s, ctx, tenantID, actorID := auditServiceEnv(t, "tnt_audit_wi_mut")

	proj := seedAuditProject(t, pool, tenantID)

	// Create → 1 work_item.created row.
	created, err := s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: proj,
		Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		Title:     "Audit target " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	wiID := created.Msg.WorkItem.Id
	if n := auditEventCount(t, pool, tenantID, "work_item.created", "work_item", wiID); n != 1 {
		t.Fatalf("work_item.created rows = %d, want 1", n)
	}

	// The row carries the actor: user + oidc + the identity id.
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "work_item.created", "", "work_item", wiID, "", 10, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if rows[0].ActorType != "user" || rows[0].AuthMethod != "oidc" || rows[0].ActorIdentityID != actorID {
		t.Fatalf("actor fields = type:%s auth:%s id:%s, want user/oidc/%s",
			rows[0].ActorType, rows[0].AuthMethod, rows[0].ActorIdentityID, actorID)
	}

	// GetWorkItem + ListWorkItems write nothing (read-only).
	if _, err := s.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: wiID})); err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "work_item.created", "work_item", wiID); n != 1 {
		t.Fatalf("GetWorkItem wrote audit rows: created rows = %d, want still 1", n)
	}
	if _, err := s.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{ProjectId: proj})); err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "work_item.created", "work_item", wiID); n != 1 {
		t.Fatalf("ListWorkItems wrote audit rows: created rows = %d, want still 1", n)
	}

	// Update → 1 work_item.updated row with before/after populated.
	newTitle := "Updated audit target"
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:    wiID,
		Title: &newTitle,
	})); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "work_item.updated", "work_item", wiID); n != 1 {
		t.Fatalf("work_item.updated rows = %d, want 1", n)
	}
	updRows, err := pool.ListAuditEvents(context.Background(), tenantID, "work_item.updated", "", "work_item", wiID, "", 10, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(updRows) != 1 || string(updRows[0].Before) == "" || string(updRows[0].After) == "" {
		t.Fatalf("update row must carry before+after: %+v", updRows)
	}

	// Delete → 1 work_item.deleted row (after omitted, before = last state).
	if _, err := s.DeleteWorkItem(ctx, connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: wiID})); err != nil {
		t.Fatalf("DeleteWorkItem: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "work_item.deleted", "work_item", wiID); n != 1 {
		t.Fatalf("work_item.deleted rows = %d, want 1", n)
	}
}

// TestUpdateWorkItemAcceptsSkippedStatus pins the Connect UpdateWorkItem
// contract for the new user-settable "skipped" status (this item's actual
// remaining work): the generic status write accepts
// WORK_ITEM_STATUS_SKIPPED exactly like succeeded/cancelled with no new
// guard, persists "skipped", bumps the version, and emits one
// work_item.updated audit row + one outbox event in the same transaction.
// The same test proves the archived/idea guards at service.go:852-863 are
// untouched: both are still rejected by this path.
func TestUpdateWorkItemAcceptsSkippedStatus(t *testing.T) {
	pool, s, ctx, tenantID, _ := auditServiceEnv(t, "tnt_audit_wi_skip")
	proj := seedAuditProject(t, pool, tenantID)

	created, err := s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: proj,
		Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		Title:     "Skip target " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	wiID := created.Msg.WorkItem.Id
	wantVersion := created.Msg.WorkItem.Version + 1

	skipped := apiv1.WorkItemStatus_WORK_ITEM_STATUS_SKIPPED
	updated, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:     wiID,
		Status: &skipped,
	}))
	if err != nil {
		t.Fatalf("UpdateWorkItem(status=skipped): %v", err)
	}
	if updated.Msg.WorkItem.Status != apiv1.WorkItemStatus_WORK_ITEM_STATUS_SKIPPED {
		t.Fatalf("status = %v, want SKIPPED", updated.Msg.WorkItem.Status)
	}
	if updated.Msg.WorkItem.Version != wantVersion {
		t.Fatalf("version = %d, want %d", updated.Msg.WorkItem.Version, wantVersion)
	}

	ttx, err := pool.BeginTenantTx(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	row, err := db.GetWorkItem(context.Background(), ttx.Tx, tenantID, wiID)
	ttx.Rollback(context.Background())
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	if row.Status != "skipped" {
		t.Fatalf("stored status = %q, want \"skipped\"", row.Status)
	}

	if n := auditEventCount(t, pool, tenantID, "work_item.updated", "work_item", wiID); n != 1 {
		t.Fatalf("work_item.updated audit rows = %d, want 1", n)
	}
	if n := countOutboxEvent(t, pool, ctx, tenantID, "work_item.updated", wiID); n != 1 {
		t.Fatalf("work_item.updated outbox events = %d, want 1", n)
	}

	// Pre-existing guards are untouched: archived and idea are still
	// rejected by this same generic status path — no new skipped guard
	// was added alongside them.
	archived := apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: wiID, Status: &archived,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("status=archived: err = %v, want FailedPrecondition", err)
	}
	idea := apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: wiID, Status: &idea,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("status=idea: err = %v, want FailedPrecondition", err)
	}
}

// TestAuditServiceWorkItemAtomicRollback pins atomicity the other way: a
// mutation that fails validation must NOT leave an audit row (nothing to
// record — the row exists iff the mutation committed).
func TestAuditServiceWorkItemAtomicRollback(t *testing.T) {
	pool, s, ctx, tenantID, _ := auditServiceEnv(t, "tnt_audit_wi_rb_"+strings.ToLower(db.NewID()))
	proj := seedAuditProject(t, pool, tenantID)

	// Invalid title → rejected before any tx; no audit row.
	_, err := s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: proj,
		Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		Title:     strings.Repeat("x", 600),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("over-long title: err = %v, want InvalidArgument", err)
	}
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "", "", "", "", "", 100, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	// The env seeds an identity (identity rows are not audit events), so any
	// audit row here would be a mutation record — assert none for the tenant
	// beyond what the seeding itself never writes. Filter by action pattern.
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "work_item.") {
			t.Fatalf("failed create left an audit row: %+v", r)
		}
	}
}
