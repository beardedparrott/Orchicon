package workitem

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// dependsOnTestEnv sets up the service + a fresh project, mirroring the
// other DB-backed workitem tests (skipped unless ORCHICON_TEST_DSN).
func dependsOnTestEnv(t *testing.T) (context.Context, *db.Pool, *Service, string) {
	t.Helper()
	pool := validateParentTestPool(t)
	ctx := tenant.WithID(context.Background(), validateParentTestTenant)
	s := New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	proj := validateParentProject(t, ctx, pool)
	return ctx, pool, s, proj
}

// dependsOnCreate is a test helper for creating an epic via the service.
func dependsOnCreate(t *testing.T, ctx context.Context, s *Service, projectID string, depIDs []string) *apiv1.WorkItem {
	t.Helper()
	res, err := s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: projectID,
		Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		Title:     "Depends-on " + strings.ToLower(db.NewID()),
		DependsOn: depIDs,
	}))
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	return res.Msg.WorkItem
}

// dependsOnDB returns the outgoing depends_on edge rows for an item.
func dependsOnDB(t *testing.T, pool *db.Pool, tenantID, itemID string) []db.DependencyRow {
	t.Helper()
	ttx, err := pool.BeginTenantTx(t.Context(), tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(t.Context())
	deps, err := db.ListDependenciesForItems(t.Context(), ttx.Tx, tenantID, []string{itemID}, domain.DependencyDependsOn)
	if err != nil {
		t.Fatalf("list dependencies: %v", err)
	}
	return deps
}

// TestDependsOnCreateDB verifies create accepts a dependency list, the
// payload round-trips it, and the edges land in the dependency table
// (independent of the parent/child hierarchy).
func TestDependsOnCreateDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	targetA := dependsOnCreate(t, ctx, s, proj, nil)
	targetB := dependsOnCreate(t, ctx, s, proj, nil)
	item := dependsOnCreate(t, ctx, s, proj, []string{targetA.Id, targetB.Id})

	// Payload returns the dependencies on create.
	if got := item.DependsOn; len(got) != 2 || got[0] != targetA.Id || got[1] != targetB.Id {
		t.Fatalf("create: depends_on = %v, want [%s %s]", got, targetA.Id, targetB.Id)
	}
	// Edges are in the DB.
	deps := dependsOnDB(t, pool, validateParentTestTenant, item.Id)
	if len(deps) != 2 {
		t.Fatalf("create: %d dependency edges, want 2", len(deps))
	}
	// Edges never touch the hierarchy: no parent links were created.
	if item.ParentId != "" || targetA.ParentId != "" || targetB.ParentId != "" {
		t.Fatalf("depends_on must not set parent_id: item=%q a=%q b=%q", item.ParentId, targetA.ParentId, targetB.ParentId)
	}
	// Dedup: the same target listed twice creates one edge.
	dup := dependsOnCreate(t, ctx, s, proj, []string{targetA.Id, targetA.Id})
	if got := dup.DependsOn; len(got) != 1 || got[0] != targetA.Id {
		t.Fatalf("dedup: depends_on = %v, want [%s]", got, targetA.Id)
	}
	if deps := dependsOnDB(t, pool, validateParentTestTenant, dup.Id); len(deps) != 1 {
		t.Fatalf("dedup: %d edges, want 1", len(deps))
	}
}

// TestDependsOnCreateValidationDB verifies create rejects blank ids,
// self-dependencies, missing targets (NotFound), and cross-project targets.
func TestDependsOnCreateValidationDB(t *testing.T) {
	ctx, _, s, proj := dependsOnTestEnv(t)
	otherProj := validateParentProject(t, ctx, s.pool)

	target := dependsOnCreate(t, ctx, s, proj, nil)
	otherTarget := dependsOnCreate(t, ctx, s, otherProj, nil)

	cases := []struct {
		name string
		ids  []string
		code connect.Code
	}{
		{"self dependency", []string{""}, connect.CodeInvalidArgument}, // filled below
		{"blank id", []string{"  "}, connect.CodeInvalidArgument},
		{"missing target", []string{"does-not-exist"}, connect.CodeNotFound},
		{"cross-project target", []string{otherTarget.Id}, connect.CodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Self-dependency needs the item's own id, which only exists
			// after create — build it as a two-step case.
			if tc.name == "self dependency" {
				item := dependsOnCreate(t, ctx, s, proj, nil)
				_, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
					Id:        item.Id,
					DependsOn: &apiv1.DependencyIds{Ids: []string{item.Id}},
				}))
				if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("self dependency: got %v, want InvalidArgument", err)
				}
				return
			}
			_, err := s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
				ProjectId: proj,
				Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
				Title:     "Validation " + strings.ToLower(db.NewID()),
				DependsOn: tc.ids,
			}))
			if err == nil || connect.CodeOf(err) != tc.code {
				t.Fatalf("%s: got %v, want code %s", tc.name, err, tc.code)
			}
		})
	}
	_ = target
}

// TestDependsOnUpdateReplaceDB verifies set-replace semantics: an update
// fully replaces the outgoing depends_on set, an empty list clears all
// edges, and an absent field leaves them unchanged. Get/List return the
// field too.
func TestDependsOnUpdateReplaceDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)
	c := dependsOnCreate(t, ctx, s, proj, nil)
	item := dependsOnCreate(t, ctx, s, proj, []string{a.Id})

	// Replace a → b, c.
	upd, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        item.Id,
		DependsOn: &apiv1.DependencyIds{Ids: []string{b.Id, c.Id}},
	}))
	if err != nil {
		t.Fatalf("update depends_on: %v", err)
	}
	if got := upd.Msg.WorkItem.DependsOn; len(got) != 2 || got[0] != b.Id || got[1] != c.Id {
		t.Fatalf("update: depends_on = %v, want [%s %s]", got, b.Id, c.Id)
	}
	deps := dependsOnDB(t, pool, validateParentTestTenant, item.Id)
	if len(deps) != 2 {
		t.Fatalf("update: %d edges, want 2", len(deps))
	}
	for _, d := range deps {
		if d.ToID == a.Id {
			t.Fatalf("update: edge to %s should have been removed", a.Id)
		}
	}

	// Absent field = unchanged.
	same, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:    item.Id,
		Title: strPtr("renamed without touching deps"),
	}))
	if err != nil {
		t.Fatalf("update without depends_on: %v", err)
	}
	if got := same.Msg.WorkItem.DependsOn; len(got) != 2 {
		t.Fatalf("absent field: depends_on = %v, want 2 entries unchanged", got)
	}

	// Empty list = clear all.
	cleared, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        item.Id,
		DependsOn: &apiv1.DependencyIds{},
	}))
	if err != nil {
		t.Fatalf("clear depends_on: %v", err)
	}
	if got := cleared.Msg.WorkItem.DependsOn; len(got) != 0 {
		t.Fatalf("clear: depends_on = %v, want empty", got)
	}
	if deps := dependsOnDB(t, pool, validateParentTestTenant, item.Id); len(deps) != 0 {
		t.Fatalf("clear: %d edges, want 0", len(deps))
	}

	// Get and List return the field.
	got, err := s.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: a.Id}))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Msg.WorkItem.DependsOn) != 0 {
		t.Fatalf("get: a.depends_on = %v, want empty", got.Msg.WorkItem.DependsOn)
	}
	// b has no depends_on either; recreate a set on b to verify Get/List.
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        b.Id,
		DependsOn: &apiv1.DependencyIds{Ids: []string{a.Id}},
	})); err != nil {
		t.Fatalf("set b.depends_on: %v", err)
	}
	gotB, err := s.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: b.Id}))
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if got := gotB.Msg.WorkItem.DependsOn; len(got) != 1 || got[0] != a.Id {
		t.Fatalf("get b: depends_on = %v, want [%s]", got, a.Id)
	}
	list, err := s.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		ProjectId: proj,
		PageSize:  100,
	}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, w := range list.Msg.WorkItems {
		if w.Id == b.Id {
			found = true
			if got := w.DependsOn; len(got) != 1 || got[0] != a.Id {
				t.Fatalf("list: b.depends_on = %v, want [%s]", got, a.Id)
			}
		}
	}
	if !found {
		t.Fatal("list: b not present in page")
	}
}

// TestDependsOnUpdateCycleDB verifies a set-replace that would close a
// cycle is rejected (FailedPrecondition) and rolls the whole tx back.
func TestDependsOnUpdateCycleDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)

	// b depends on a.
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        b.Id,
		DependsOn: &apiv1.DependencyIds{Ids: []string{a.Id}},
	})); err != nil {
		t.Fatalf("set b→a: %v", err)
	}
	// Now make a depend on b → a→b + b→a is a cycle.
	_, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        a.Id,
		DependsOn: &apiv1.DependencyIds{Ids: []string{b.Id}},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("cycle: got %v, want FailedPrecondition", err)
	}
	// Rollback: a still has no edges, b still depends on a.
	if deps := dependsOnDB(t, pool, validateParentTestTenant, a.Id); len(deps) != 0 {
		t.Fatalf("cycle rollback: a has %d edges, want 0", len(deps))
	}
	if deps := dependsOnDB(t, pool, validateParentTestTenant, b.Id); len(deps) != 1 || deps[0].ToID != a.Id {
		t.Fatalf("cycle rollback: b edges = %v, want [a]", deps)
	}
}

// TestDependsOnUpdateProjectMoveGuardDB verifies an item that participates
// in any dependency edge (outgoing or incoming) cannot be moved to another
// project, while an edge-free item still can.
func TestDependsOnUpdateProjectMoveGuardDB(t *testing.T) {
	ctx, _, s, projA := dependsOnTestEnv(t)
	projB := validateParentProject(t, ctx, s.pool)

	a := dependsOnCreate(t, ctx, s, projA, nil)
	b := dependsOnCreate(t, ctx, s, projA, []string{a.Id})

	// b has an outgoing edge → move rejected.
	_, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        b.Id,
		ProjectId: strPtr(projB),
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("move with outgoing edge: got %v, want FailedPrecondition", err)
	}
	// a has an incoming edge → move rejected too.
	_, err = s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        a.Id,
		ProjectId: strPtr(projB),
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("move with incoming edge: got %v, want FailedPrecondition", err)
	}
	// Remove the edge, then the move succeeds.
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        b.Id,
		DependsOn: &apiv1.DependencyIds{},
	})); err != nil {
		t.Fatalf("clear b edges: %v", err)
	}
	moved, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        b.Id,
		ProjectId: strPtr(projB),
	}))
	if err != nil {
		t.Fatalf("move after clearing edges: %v", err)
	}
	if moved.Msg.WorkItem.ProjectId != projB {
		t.Fatalf("move: project_id = %q, want %q", moved.Msg.WorkItem.ProjectId, projB)
	}
}
