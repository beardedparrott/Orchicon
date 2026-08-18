package workitem

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5/pgconn"
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

// dependsOnAll returns every dependency edge row for a project.
func dependsOnAll(t *testing.T, pool *db.Pool, tenantID, projectID string) []db.DependencyRow {
	t.Helper()
	ttx, err := pool.BeginTenantTx(t.Context(), tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(t.Context())
	deps, err := db.ListDependencies(t.Context(), ttx.Tx, tenantID, projectID)
	if err != nil {
		t.Fatalf("list dependencies: %v", err)
	}
	return deps
}

// dependsOnAdd is a test helper for adding a dependency edge via the
// service RPC.
func dependsOnAdd(t *testing.T, ctx context.Context, s *Service, projectID, fromID, toID string, depType apiv1.DependencyType) (*apiv1.WorkItemDependency, error) {
	t.Helper()
	res, err := s.AddDependency(ctx, connect.NewRequest(&apiv1.AddDependencyRequest{
		ProjectId: projectID,
		FromId:    fromID,
		ToId:      toID,
		Type:      depType,
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.Dependency, nil
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

// TestDependsOnAddCycleDirectDB verifies AddDependency rejects a direct
// 2-node cycle (A→B then B→A) with a FailedPrecondition whose message
// names the offending edge, and that nothing persists.
func TestDependsOnAddCycleDirectDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)

	if _, err := dependsOnAdd(t, ctx, s, proj, a.Id, b.Id, apiv1.DependencyType_DEPENDENCY_TYPE_DEPENDS_ON); err != nil {
		t.Fatalf("add a→b: %v", err)
	}
	_, err := dependsOnAdd(t, ctx, s, proj, b.Id, a.Id, apiv1.DependencyType_DEPENDENCY_TYPE_DEPENDS_ON)
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("direct cycle: got %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), b.Id+" -> "+a.Id) {
		t.Fatalf("direct cycle: error must name the offending edge, got %q", err)
	}
	// Rollback: b has no outgoing edges, a→b survived.
	if deps := dependsOnDB(t, pool, validateParentTestTenant, b.Id); len(deps) != 0 {
		t.Fatalf("direct cycle rollback: b has %d edges, want 0", len(deps))
	}
	if deps := dependsOnDB(t, pool, validateParentTestTenant, a.Id); len(deps) != 1 || deps[0].ToID != b.Id {
		t.Fatalf("direct cycle rollback: a edges = %v, want [b]", deps)
	}
}

// TestDependsOnAddMultiHopCycleDB verifies AddDependency detects a
// multi-hop cycle (A→B→C→A) and rolls the edge back.
func TestDependsOnAddMultiHopCycleDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)
	c := dependsOnCreate(t, ctx, s, proj, nil)

	for _, edge := range [][2]string{{a.Id, b.Id}, {b.Id, c.Id}} {
		if _, err := dependsOnAdd(t, ctx, s, proj, edge[0], edge[1], apiv1.DependencyType_DEPENDENCY_TYPE_DEPENDS_ON); err != nil {
			t.Fatalf("add %s→%s: %v", edge[0], edge[1], err)
		}
	}
	_, err := dependsOnAdd(t, ctx, s, proj, c.Id, a.Id, apiv1.DependencyType_DEPENDENCY_TYPE_DEPENDS_ON)
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("multi-hop cycle: got %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), c.Id+" -> "+a.Id) {
		t.Fatalf("multi-hop cycle: error must name the offending edge, got %q", err)
	}
	// Rollback: c has no edges; a→b and b→c survived.
	if deps := dependsOnDB(t, pool, validateParentTestTenant, c.Id); len(deps) != 0 {
		t.Fatalf("multi-hop cycle rollback: c has %d edges, want 0", len(deps))
	}
	if deps := dependsOnDB(t, pool, validateParentTestTenant, a.Id); len(deps) != 1 || deps[0].ToID != b.Id {
		t.Fatalf("multi-hop cycle rollback: a edges = %v, want [b]", deps)
	}
	if deps := dependsOnDB(t, pool, validateParentTestTenant, b.Id); len(deps) != 1 || deps[0].ToID != c.Id {
		t.Fatalf("multi-hop cycle rollback: b edges = %v, want [c]", deps)
	}
}

// TestDependsOnUpdateMultiHopCycleDB verifies a set-replace that would
// close a multi-hop cycle (A→B→C→A) is rejected (FailedPrecondition)
// and rolls the whole tx back.
func TestDependsOnUpdateMultiHopCycleDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)
	c := dependsOnCreate(t, ctx, s, proj, nil)

	// b→a, c→b, then a→c closes A→B→C→A.
	for _, upd := range []struct{ item, dep string }{{b.Id, a.Id}, {c.Id, b.Id}} {
		if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
			Id:        upd.item,
			DependsOn: &apiv1.DependencyIds{Ids: []string{upd.dep}},
		})); err != nil {
			t.Fatalf("set %s→%s: %v", upd.item, upd.dep, err)
		}
	}
	_, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:        a.Id,
		DependsOn: &apiv1.DependencyIds{Ids: []string{c.Id}},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("multi-hop cycle via update: got %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), a.Id+" -> "+c.Id) {
		t.Fatalf("multi-hop cycle via update: error must name the offending edge, got %q", err)
	}
	// Rollback: a still has no edges; b→a and c→b survived.
	if deps := dependsOnDB(t, pool, validateParentTestTenant, a.Id); len(deps) != 0 {
		t.Fatalf("multi-hop cycle rollback: a has %d edges, want 0", len(deps))
	}
	if deps := dependsOnDB(t, pool, validateParentTestTenant, b.Id); len(deps) != 1 || deps[0].ToID != a.Id {
		t.Fatalf("multi-hop cycle rollback: b edges = %v, want [a]", deps)
	}
	if deps := dependsOnDB(t, pool, validateParentTestTenant, c.Id); len(deps) != 1 || deps[0].ToID != b.Id {
		t.Fatalf("multi-hop cycle rollback: c edges = %v, want [b]", deps)
	}
}

// TestDependsOnBlocksCycleDB verifies a cycle formed entirely of
// `blocks` edges (A blocks B, B blocks A) is rejected too.
func TestDependsOnBlocksCycleDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)

	if _, err := dependsOnAdd(t, ctx, s, proj, a.Id, b.Id, apiv1.DependencyType_DEPENDENCY_TYPE_BLOCKS); err != nil {
		t.Fatalf("add a blocks b: %v", err)
	}
	_, err := dependsOnAdd(t, ctx, s, proj, b.Id, a.Id, apiv1.DependencyType_DEPENDENCY_TYPE_BLOCKS)
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("blocks cycle: got %v, want FailedPrecondition", err)
	}
	if deps := dependsOnAll(t, pool, validateParentTestTenant, proj); len(deps) != 1 || deps[0].FromID != a.Id || deps[0].ToID != b.Id {
		t.Fatalf("blocks cycle rollback: edges = %v, want only [a blocks b]", deps)
	}
}

// TestDependsOnRelatesToSymmetricDB verifies a symmetric relates_to pair
// (A relates_to B and B relates_to A) is allowed — relates_to is not a
// DAG edge and must never be flagged as a cycle.
func TestDependsOnRelatesToSymmetricDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)

	for _, edge := range [][2]string{{a.Id, b.Id}, {b.Id, a.Id}} {
		if _, err := dependsOnAdd(t, ctx, s, proj, edge[0], edge[1], apiv1.DependencyType_DEPENDENCY_TYPE_RELATES_TO); err != nil {
			t.Fatalf("add %s relates_to %s: %v", edge[0], edge[1], err)
		}
	}
	if deps := dependsOnAll(t, pool, validateParentTestTenant, proj); len(deps) != 2 {
		t.Fatalf("relates_to: %d edges, want 2", len(deps))
	}
}

// TestDependsOnRelatesToMixedCaseDB verifies the edge-type gate: with an
// existing A depends_on B, adding B relates_to A must be ALLOWED (the
// traversal filter alone would falsely walk A→B, reach B = `from`, and
// report a cycle). Also pins that a relates_to self-loop is still
// rejected by the service's self-dependency rule.
func TestDependsOnRelatesToMixedCaseDB(t *testing.T) {
	ctx, _, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)

	if _, err := dependsOnAdd(t, ctx, s, proj, a.Id, b.Id, apiv1.DependencyType_DEPENDENCY_TYPE_DEPENDS_ON); err != nil {
		t.Fatalf("add a→b: %v", err)
	}
	// B relates_to A must not be falsely rejected as a cycle.
	if _, err := dependsOnAdd(t, ctx, s, proj, b.Id, a.Id, apiv1.DependencyType_DEPENDENCY_TYPE_RELATES_TO); err != nil {
		t.Fatalf("mixed case (B relates_to A after A depends_on B): %v", err)
	}
	// relates_to self-loop is still rejected (self-dependency, any type).
	_, err := dependsOnAdd(t, ctx, s, proj, a.Id, a.Id, apiv1.DependencyType_DEPENDENCY_TYPE_RELATES_TO)
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("relates_to self-loop: got %v, want InvalidArgument", err)
	}
}

// TestDependsOnCreateCannotCycleDB documents the create-path invariant:
// a fresh item has no incoming edges, so an outgoing item→target edge can
// never close a cycle no matter what the graph around it looks like.
func TestDependsOnCreateCannotCycleDB(t *testing.T) {
	ctx, _, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)
	if _, err := dependsOnAdd(t, ctx, s, proj, a.Id, b.Id, apiv1.DependencyType_DEPENDENCY_TYPE_DEPENDS_ON); err != nil {
		t.Fatalf("add a→b: %v", err)
	}
	// c → a is fine even though a sits in a chain: c is fresh.
	if _, err := s.CreateWorkItem(ctx, connect.NewRequest(&apiv1.CreateWorkItemRequest{
		ProjectId: proj,
		Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		Title:     "Create cannot cycle " + strings.ToLower(db.NewID()),
		DependsOn: []string{a.Id},
	})); err != nil {
		t.Fatalf("create with depends_on into an existing chain: %v", err)
	}
}

// TestDependsOnTriggerBackstopDB proves the DB trigger enforces the DAG
// invariant for writes that bypass the service layer entirely (the bulk
// import / raw-SQL path): a cyclic edge inserted directly is rejected
// and never persists, a relates_to self-loop is rejected on every type,
// and the relates_to exemption holds at the DB layer too.
func TestDependsOnTriggerBackstopDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)
	c := dependsOnCreate(t, ctx, s, proj, nil)

	rawInsert := func(t *testing.T, fromID, toID, depType string) error {
		t.Helper()
		ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer ttx.Rollback(ctx)
		_, err = db.CreateDependency(ctx, ttx.Tx, db.DependencyRow{
			ID: db.NewID(), TenantID: validateParentTestTenant, ProjectID: proj,
			FromID: fromID, ToID: toID, Type: depType,
		})
		if err != nil {
			return err
		}
		return ttx.Commit(ctx)
	}

	// Valid DAG edges via raw SQL (the bulk-import path) succeed.
	if err := rawInsert(t, a.Id, b.Id, domain.DependencyDependsOn); err != nil {
		t.Fatalf("raw insert a→b: %v", err)
	}
	if err := rawInsert(t, b.Id, c.Id, domain.DependencyDependsOn); err != nil {
		t.Fatalf("raw insert b→c: %v", err)
	}

	// Raw-inserting c→a closes A→B→C→A: the trigger must reject it and
	// nothing may persist.
	if err := rawInsert(t, c.Id, a.Id, domain.DependencyDependsOn); err == nil {
		t.Fatal("raw cyclic insert c→a: want error, got nil")
	}
	edges := dependsOnAll(t, pool, validateParentTestTenant, proj)
	if len(edges) != 2 {
		t.Fatalf("trigger backstop rollback: %d edges, want 2 (a→b, b→c)", len(edges))
	}

	// Self-loops are rejected by the trigger on every edge type.
	if err := rawInsert(t, a.Id, a.Id, domain.DependencyDependsOn); err == nil {
		t.Fatal("raw self-loop depends_on: want error, got nil")
	}
	if err := rawInsert(t, a.Id, a.Id, domain.DependencyRelatesTo); err == nil {
		t.Fatal("raw self-loop relates_to: want error, got nil")
	}

	// Mixed case at the DB layer: with a→b depends_on existing, a raw
	// b relates_to a must succeed (relates_to is exempt).
	if err := rawInsert(t, b.Id, a.Id, domain.DependencyRelatesTo); err != nil {
		t.Fatalf("raw relates_to after depends_on chain: %v", err)
	}
	edges = dependsOnAll(t, pool, validateParentTestTenant, proj)
	if len(edges) != 3 {
		t.Fatalf("trigger relates_to exemption: %d edges, want 3", len(edges))
	}
}

// TestBlockedByAttachNamesBlockersDB verifies the server names blocking
// edges on every read path (AC #1, server authority). The batch attach
// (ListWorkItems / GetDependencyGraph) must put each blocker on the item it
// actually blocks — a regression where the batch attached rows by the
// blocker's id (dropping the edges, or attaching a self-blocker when the
// blocker shares the page).
func TestBlockedByAttachNamesBlockersDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	// Create blocker→dependent edges directly in the direction the dispatch
	// gate uses (from_id = blocker, to_id = dependent — the same direction
	// ListUnsatisfiedDependencies / CheckDependenciesSatisfied read).
	addBlock := func(blockerID, depID string) {
		t.Helper()
		ttx, err := pool.BeginTenantTx(ctx, validateParentTestTenant)
		if err != nil {
			t.Fatal(err)
		}
		defer ttx.Rollback(ctx)
		if _, err := db.CreateDependency(ctx, ttx.Tx, db.DependencyRow{
			ID: db.NewID(), TenantID: validateParentTestTenant, ProjectID: proj,
			FromID: blockerID, ToID: depID, Type: domain.DependencyDependsOn,
		}); err != nil {
			t.Fatal(err)
		}
		if err := ttx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	blockerA := dependsOnCreate(t, ctx, s, proj, nil)
	blockerB := dependsOnCreate(t, ctx, s, proj, nil)
	dep1 := dependsOnCreate(t, ctx, s, proj, nil)
	dep2 := dependsOnCreate(t, ctx, s, proj, nil)
	addBlock(blockerA.Id, dep1.Id)
	addBlock(blockerB.Id, dep2.Id)

	// ListWorkItems (batch attach) must name each dep's own blocker.
	list, err := s.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		ProjectId: proj,
		PageSize:  100,
	}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]*apiv1.WorkItem{}
	for _, w := range list.Msg.WorkItems {
		byID[w.Id] = w
	}
	if got := byID[dep1.Id].BlockedBy; len(got) != 1 || got[0].Id != blockerA.Id {
		t.Fatalf("list: dep1 blocked_by = %v, want [%s]", got, blockerA.Id)
	}
	if got := byID[dep2.Id].BlockedBy; len(got) != 1 || got[0].Id != blockerB.Id {
		t.Fatalf("list: dep2 blocked_by = %v, want [%s]", got, blockerB.Id)
	}
	// The blockers themselves have no incoming unsatisfied edges.
	if got := byID[blockerA.Id].BlockedBy; len(got) != 0 {
		t.Fatalf("list: blockerA blocked_by = %v, want none (no self-blocker)", got)
	}

	// GetDependencyGraph (batch attach over all nodes) must agree.
	graph, err := s.GetDependencyGraph(ctx, connect.NewRequest(&apiv1.GetDependencyGraphRequest{ProjectId: proj}))
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	nodeByID := map[string]*apiv1.WorkItem{}
	for _, n := range graph.Msg.Graph.Nodes {
		nodeByID[n.Id] = n
	}
	if got := nodeByID[dep1.Id].BlockedBy; len(got) != 1 || got[0].Id != blockerA.Id {
		t.Fatalf("graph: dep1 blocked_by = %v, want [%s]", got, blockerA.Id)
	}
	if got := nodeByID[dep2.Id].BlockedBy; len(got) != 1 || got[0].Id != blockerB.Id {
		t.Fatalf("graph: dep2 blocked_by = %v, want [%s]", got, blockerB.Id)
	}
	if got := nodeByID[blockerA.Id].BlockedBy; len(got) != 0 {
		t.Fatalf("graph: blockerA blocked_by = %v, want none", got)
	}

	// Once a blocker succeeds, the dependent's blocked_by empties at read time.
	succ := apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED
	if _, err := s.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:     blockerA.Id,
		Status: &succ,
	})); err != nil {
		t.Fatalf("succeed blockerA: %v", err)
	}
	list2, err := s.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		ProjectId: proj,
		PageSize:  100,
	}))
	if err != nil {
		t.Fatalf("list after unblock: %v", err)
	}
	for _, w := range list2.Msg.WorkItems {
		if w.Id == dep1.Id {
			if got := w.BlockedBy; len(got) != 0 {
				t.Fatalf("list after unblock: dep1 blocked_by = %v, want empty (read-time, no staleness)", got)
			}
		}
		if w.Id == dep2.Id {
			if got := w.BlockedBy; len(got) != 1 || got[0].Id != blockerB.Id {
				t.Fatalf("list after unblock: dep2 blocked_by = %v, want [%s]", got, blockerB.Id)
			}
		}
	}
	_ = pool
}

// TestMapDBErrorDependencyCycle verifies the trigger's cycle rejection —
// which the service surfaces when the concurrent serialization path catches
// an app write (the app-layer check ran against stale state) — is mapped to
// a FailedPrecondition naming the offending edge, not an opaque Internal.
// Pure unit test: no DB required.
func TestMapDBErrorDependencyCycle(t *testing.T) {	pgErr := &pgconn.PgError{Code: "P0001", Message: "cannot add dependency w_a -> w_b: would create a cycle in the work DAG"}
	err := mapDBError(pgErr)
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Fatalf("mapDBError(cycle): got %s, want FailedPrecondition", code)
	}
	if msg := err.Error(); !strings.Contains(msg, "w_a -> w_b") || !strings.Contains(msg, "would create a cycle") {
		t.Fatalf("mapDBError(cycle): message must name the edge and the cycle, got %q", msg)
	}
	// Non-cycle DB errors keep the Internal mapping.
	if code := connect.CodeOf(mapDBError(fmt.Errorf("disk on fire"))); code != connect.CodeInternal {
		t.Fatalf("mapDBError(generic): got %s, want Internal", code)
	}
	// A pgconn error that is not the cycle rejection stays Internal.
	other := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	if code := connect.CodeOf(mapDBError(other)); code != connect.CodeInternal {
		t.Fatalf("mapDBError(non-cycle pg error): got %s, want Internal", code)
	}
}

// TestDependsOnConcurrentRaceClosedDB proves the DB trigger's per-project
// transactional advisory lock closes the concurrent-write TOCTOU race:
// two transactions inserting A→B and B→A at the same time cannot both
// pass the reachability walk and commit a live cycle. tx1 commits A→B;
// tx2, queued on the advisory lock until then, re-walks the graph after
// tx1 commits, sees A→B, and is rejected — the cycle never persists.
func TestDependsOnConcurrentRaceClosedDB(t *testing.T) {
	ctx, pool, s, proj := dependsOnTestEnv(t)

	a := dependsOnCreate(t, ctx, s, proj, nil)
	b := dependsOnCreate(t, ctx, s, proj, nil)
	tenant := validateParentTestTenant

	// tx1 inserts a→b and holds the project's advisory lock uncommitted.
	tx1, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer tx1.Rollback(ctx)
	if _, err := db.CreateDependency(ctx, tx1.Tx, db.DependencyRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: proj,
		FromID: a.Id, ToID: b.Id, Type: domain.DependencyDependsOn,
	}); err != nil {
		t.Fatalf("tx1 insert a→b: %v", err)
	}

	// tx2 tries to insert b→a concurrently; the trigger must queue it on
	// the advisory lock until tx1 commits, then re-walk and reject it.
	tx2, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	pidCh := make(chan int32, 1)
	resCh := make(chan error, 1)
	go func() {
		var pid int32
		if err := tx2.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			resCh <- fmt.Errorf("tx2 backend pid: %w", err)
			return
		}
		pidCh <- pid
		_, err := db.CreateDependency(ctx, tx2.Tx, db.DependencyRow{
			ID: db.NewID(), TenantID: tenant, ProjectID: proj,
			FromID: b.Id, ToID: a.Id, Type: domain.DependencyDependsOn,
		})
		if err == nil {
			err = tx2.Commit(ctx)
		} else {
			_ = tx2.Rollback(ctx)
		}
		resCh <- err
	}()
	pid := <-pidCh

	// Barrier: wait until tx2 is visibly queued on the advisory lock
	// (granted=false) before releasing tx1 — otherwise the interleaving is
	// sequential (tx2 after tx1 committed) and the race we mean to test
	// was not exercised.
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted = false AND pid = $1`,
			pid).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatalf("tx2 never queued on the advisory lock: race not exercised (concurrency fix regression), pid=%d", pid)
	}

	// Release tx1: tx2 unblocks, re-walks, sees a→b, and must be rejected.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}
	if err := <-resCh; err == nil {
		t.Fatal("concurrent b→a insert must be rejected, got nil")
	} else if !strings.Contains(err.Error(), "would create a cycle in the work DAG") {
		t.Fatalf("concurrent b→a insert: got error %q, want the DAG-cycle rejection", err)
	}

	// Only a→b persisted — the graph stayed acyclic.
	edges := dependsOnAll(t, pool, tenant, proj)
	if len(edges) != 1 || edges[0].FromID != a.Id || edges[0].ToID != b.Id {
		t.Fatalf("concurrent race: edges = %v, want only [a→b]", edges)
	}
}
