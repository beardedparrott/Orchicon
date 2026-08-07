package workitem

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
)

// TestValidateTopLevelKind verifies the hierarchy invariant that only
// epics are top-level (shared by Create and Update/clear).
func TestValidateTopLevelKind(t *testing.T) {
	if err := validateTopLevelKind(domain.WorkItemKindEpic); err != nil {
		t.Errorf("epic should be allowed top-level, got %v", err)
	}
	for _, kind := range []string{
		domain.WorkItemKindFeature,
		domain.WorkItemKindTask,
		domain.WorkItemKindSubtask,
	} {
		if err := validateTopLevelKind(kind); err == nil {
			t.Errorf("%s should be rejected as top-level", kind)
		} else if msg := err.Error(); !strings.Contains(msg, "only epics can be top-level") {
			t.Errorf("top-level error for %s = %q, want 'only epics can be top-level'", kind, msg)
		}
	}
}

// TestValidateKindDepth verifies the depth rule that a child must be
// strictly deeper than its parent (Epic > Feature > Task > Subtask).
func TestValidateKindDepth(t *testing.T) {
	valid := []struct{ child, parent string }{
		{domain.WorkItemKindFeature, domain.WorkItemKindEpic},
		{domain.WorkItemKindTask, domain.WorkItemKindFeature},
		{domain.WorkItemKindTask, domain.WorkItemKindEpic},
		{domain.WorkItemKindSubtask, domain.WorkItemKindTask},
		{domain.WorkItemKindSubtask, domain.WorkItemKindFeature},
		{domain.WorkItemKindSubtask, domain.WorkItemKindEpic},
	}
	for _, tc := range valid {
		if err := validateKindDepth(tc.child, tc.parent); err != nil {
			t.Errorf("validateKindDepth(%s, %s) unexpected error: %v", tc.child, tc.parent, err)
		}
	}

	invalid := []struct{ child, parent string }{
		{domain.WorkItemKindEpic, domain.WorkItemKindEpic},       // same depth
		{domain.WorkItemKindEpic, domain.WorkItemKindFeature},    // parent deeper than child
		{domain.WorkItemKindFeature, domain.WorkItemKindFeature}, // same depth
		{domain.WorkItemKindFeature, domain.WorkItemKindTask},    // parent deeper than child
		{domain.WorkItemKindTask, domain.WorkItemKindTask},       // same depth
		{domain.WorkItemKindTask, domain.WorkItemKindSubtask},    // parent deeper than child
		{domain.WorkItemKindSubtask, domain.WorkItemKindSubtask}, // same depth
	}
	for _, tc := range invalid {
		if err := validateKindDepth(tc.child, tc.parent); err == nil {
			t.Errorf("validateKindDepth(%s, %s) should have been rejected", tc.child, tc.parent)
		}
	}
}

// TestValidateKindDepthSelfParent verifies the cycle-safety property: a
// node can never be its own parent or its own descendant's child because
// the parent chain is strictly ordered by depth.
func TestValidateKindDepthSelfParent(t *testing.T) {
	for _, kind := range []string{
		domain.WorkItemKindEpic,
		domain.WorkItemKindFeature,
		domain.WorkItemKindTask,
		domain.WorkItemKindSubtask,
	} {
		if err := validateKindDepth(kind, kind); err == nil {
			t.Errorf("a %s must not be its own parent", kind)
		}
	}
}

// ---------------------------------------------------------------------------
// DB-backed integration test for ValidateParent (skipped unless
// ORCHICON_TEST_DSN points at a disposable database, same convention as
// internal/askorchicon/tool_workitems_test.go):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/workitem/ -run TestValidateParent -v
// ---------------------------------------------------------------------------

const validateParentTestTenant = "tnt_validate_parent"

func validateParentTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed workitem tests")
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

func validateParentProject(t *testing.T, ctx context.Context, pool *db.Pool) string {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: validateParentTestTenant,
		Name: "Parent Test", Slug: "parent-test-" + strings.ToLower(db.NewID()),
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

func validateParentItem(t *testing.T, ctx context.Context, pool *db.Pool, projectID, kind string, parentID *string) db.WorkItemRow {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: validateParentTestTenant,
		ProjectID: projectID, ParentID: parentID, Kind: kind,
		Title: "Parent test " + strings.ToLower(db.NewID()),
		Status: domain.WorkItemPending,
	})
	if err != nil {
		t.Fatalf("create %s: %v", kind, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit item: %v", err)
	}
	return item
}

// TestValidateParentDB exercises the shared helper against a real DB:
// valid reparenting, depth violations, cross-project parents, clearing
// rules, and the cross-tenant NotFound guarantee.
func TestValidateParentDB(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)

	projectA := validateParentProject(t, ctx, pool)
	projectB := validateParentProject(t, ctx, pool)

	epic := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	feature := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindFeature, &epic.ID)
	task := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &feature.ID)
	subtask := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindSubtask, &task.ID)
	otherProjectEpic := validateParentItem(t, ctx, pool, projectB, domain.WorkItemKindEpic, nil)

	ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	// Valid: same project + strictly shallower kinds (skip-level ok).
	for _, tc := range []struct {
		child  db.WorkItemRow
		parent db.WorkItemRow
	}{
		{task, feature},
		{task, epic},
		{subtask, epic},
		{subtask, feature},
	} {
		if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, tc.parent.ID, tc.child.Kind, projectA); err != nil {
			t.Errorf("ValidateParent(%s under %s) unexpected error: %v", tc.child.ID, tc.parent.ID, err)
		}
	}

	// Invalid: same-depth or deeper parent.
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, task.ID, domain.WorkItemKindFeature, projectA); err == nil {
		t.Error("feature under task (deeper parent) should be rejected")
	}
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, feature.ID, domain.WorkItemKindFeature, projectA); err == nil {
		t.Error("feature under feature (same depth) should be rejected")
	}
	// Self-parent is rejected by the depth check.
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, task.ID, domain.WorkItemKindTask, projectA); err == nil {
		t.Error("task as its own parent should be rejected")
	}

	// Invalid: cross-project parent.
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, otherProjectEpic.ID, domain.WorkItemKindFeature, projectA); err == nil {
		t.Error("cross-project parent should be rejected")
	} else if msg := err.Error(); !strings.Contains(msg, "same project") {
		t.Errorf("cross-project error = %q, want 'same project'", msg)
	}

	// Clearing: only epics may go top-level.
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, "", domain.WorkItemKindTask, projectA); err == nil {
		t.Error("clearing a task's parent should be rejected")
	}
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, "", domain.WorkItemKindEpic, projectA); err != nil {
		t.Errorf("clearing an epic's (absent) parent should be a no-op, got %v", err)
	}

	// Unknown parent id → ErrNotFound (cross-tenant parents are the same
	// shape: the row never resolves inside the tenant tx).
	if err := ValidateParent(ctx, ttx.Tx, validateParentTestTenant, "does-not-exist", domain.WorkItemKindFeature, projectA); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("unknown parent should be ErrNotFound, got %v", err)
	}
}

// TestUpdateWorkItemReparent exercises the full UpdateWorkItem handler:
// explicit reparenting (same-project, depth rules), clearing rules, and
// the carried-parent guard that rejects moving a child to another project
// when its parent does not already live there.
func TestUpdateWorkItemReparent(t *testing.T) {
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	projectA := validateParentProject(t, ctx, pool)
	projectB := validateParentProject(t, ctx, pool)

	epicA1 := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	epicA2 := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindEpic, nil)
	feature := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindFeature, &epicA1.ID)
	task := validateParentItem(t, ctx, pool, projectA, domain.WorkItemKindTask, &feature.ID)
	epicB := validateParentItem(t, ctx, pool, projectB, domain.WorkItemKindEpic, nil)

	update := func(item db.WorkItemRow, mutate func(req *apiv1.UpdateWorkItemRequest)) error {
		req := connect.NewRequest(&apiv1.UpdateWorkItemRequest{Id: item.ID})
		mutate(req.Msg)
		_, err := s.UpdateWorkItem(ctx, req)
		return err
	}

	// Explicit reparent within the same project: feature moves from
	// epicA1 to epicA2.
	err := update(feature, func(m *apiv1.UpdateWorkItemRequest) {
		m.ParentId = strPtr(epicA2.ID)
	})
	if err != nil {
		t.Fatalf("reparent feature to epicA2: %v", err)
	}
	cur, err := db.GetWorkItem(ctx, mustTx(t, pool), validateParentTestTenant, feature.ID)
	if err != nil {
		t.Fatalf("reload feature: %v", err)
	}
	if cur.ParentID == nil || *cur.ParentID != epicA2.ID {
		t.Fatalf("feature parent = %v, want %s", cur.ParentID, epicA2.ID)
	}

	// A deeper parent (feature under its own task) is rejected — this is
	// also the cycle-safety check (feature → task → feature would be a
	// 2-cycle, impossible because the depth rule forbids it).
	err = update(feature, func(m *apiv1.UpdateWorkItemRequest) { m.ParentId = strPtr(task.ID) })
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("feature under task (deeper parent) should be rejected, got %v", err)
	}

	// Clearing a non-epic's parent is rejected.
	err = update(feature, func(m *apiv1.UpdateWorkItemRequest) { m.ParentId = strPtr("") })
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("clearing a feature's parent should be rejected, got %v", err)
	}

	// Moving a child cross-project WITHOUT reparenting is rejected by the
	// carried-parent guard (the parent still lives in project A).
	err = update(task, func(m *apiv1.UpdateWorkItemRequest) { m.ProjectId = strPtr(projectB) })
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("cross-project move without reparent should be rejected, got %v", err)
	}

	// Moving a child cross-project WITH an explicit parent in the target
	// project succeeds.
	err = update(task, func(m *apiv1.UpdateWorkItemRequest) {
		m.ProjectId = strPtr(projectB)
		m.ParentId = strPtr(epicB.ID)
	})
	if err != nil {
		t.Fatalf("cross-project move with reparent: %v", err)
	}
	cur, err = db.GetWorkItem(ctx, mustTx(t, pool), validateParentTestTenant, task.ID)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if cur.ProjectID != projectB || cur.ParentID == nil || *cur.ParentID != epicB.ID {
		t.Fatalf("task after cross-project move = project %s parent %v, want project %s parent %s",
			cur.ProjectID, cur.ParentID, projectB, epicB.ID)
	}

	// Moving an epic (no parent) cross-project succeeds without a parent.
	err = update(epicA1, func(m *apiv1.UpdateWorkItemRequest) { m.ProjectId = strPtr(projectB) })
	if err != nil {
		t.Fatalf("move epic cross-project: %v", err)
	}

	// Unknown parent id → NotFound.
	err = update(feature, func(m *apiv1.UpdateWorkItemRequest) { m.ParentId = strPtr("does-not-exist") })
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("reparent to unknown parent should be NotFound, got %v", err)
	}
}

func mustTx(t *testing.T, pool *db.Pool) pgx.Tx {
	t.Helper()
	ttx, err := pool.BeginTenantTx(context.Background(), validateParentTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = ttx.Rollback(context.Background()) })
	return ttx.Tx
}
