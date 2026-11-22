package workitem

// Idea Cloud epic anchoring tests (the structural fix for the promoted-idea
// orphan: a promoted child whose parent was an idea-state row and therefore
// invisible to every normal list query).

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// resolveEpic is the small helper every test below needs.
func resolveEpic(t *testing.T, pool *db.Pool, tenantID, projectID string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	epic, err := ResolveIdeaCloudEpic(ctx, ttx.Tx, tenantID, projectID)
	if err != nil {
		t.Fatalf("resolve idea cloud epic: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return epic
}

// anchor performs the anchoring in its own tenant transaction, the way the create
// paths do.
func anchor(t *testing.T, pool *db.Pool, tenantID string, row *db.WorkItemRow) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if err := AnchorIdeaToCloudEpic(ctx, ttx.Tx, row); err != nil {
		t.Fatalf("anchor idea: %v", err)
	}
	created, err := db.CreateWorkItem(ctx, ttx.Tx, *row)
	if err != nil {
		t.Fatalf("create anchored idea: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return created
}

func TestResolveIdeaCloudEpicCreatesOneAndReusesIt(t *testing.T) {
	pool, _, _, tenantID, projectID := ideaEnv(t)

	first := resolveEpic(t, pool, tenantID, projectID)
	if first.Kind != domain.WorkItemKindEpic {
		t.Fatalf("idea cloud epic kind = %q, want epic", first.Kind)
	}
	if first.Title != IdeaCloudEpicTitle {
		t.Fatalf("title = %q, want %q", first.Title, IdeaCloudEpicTitle)
	}
	// PENDING, not idea: the container must be visible in every normal view, or a
	// promoted child's parent is unresolvable again.
	if first.Status != domain.WorkItemPending {
		t.Fatalf("idea cloud epic status = %q, want pending (it must be visible in normal views)", first.Status)
	}
	if first.ParentID != nil {
		t.Fatalf("idea cloud epic must be top-level, got parent %v", *first.ParentID)
	}

	// A second resolve must reuse it — two epics would split triage.
	second := resolveEpic(t, pool, tenantID, projectID)
	if second.ID != first.ID {
		t.Fatalf("second resolve created a new epic (%s vs %s): the anchor must be a singleton per project",
			second.ID, first.ID)
	}
}

func TestResolveIdeaCloudEpicIsPerProject(t *testing.T) {
	pool, _, _, tenantID, projectID := ideaEnv(t)
	otherProject := seedAuditProject(t, pool, tenantID)

	a := resolveEpic(t, pool, tenantID, projectID)
	b := resolveEpic(t, pool, tenantID, otherProject)
	if a.ID == b.ID {
		t.Fatal("two projects shared one idea cloud epic; the anchor is per-project")
	}
}

func TestAnchorIdeaToCloudEpicForceTasksUnderTheEpic(t *testing.T) {
	pool, _, _, tenantID, projectID := ideaEnv(t)

	// The spawn proposes a FEATURE with no parent. Anchoring must still land it as a
	// task child of the epic: the operator's promotion decision is what promotes an
	// item to a feature, not the model's choice at spawn time.
	row := db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: "feature", Title: "Metric rows", Status: domain.WorkItemIdea,
	}
	created := anchor(t, pool, tenantID, &row)

	if created.Kind != domain.WorkItemKindTask {
		t.Fatalf("anchored idea kind = %q, want task", created.Kind)
	}
	epic := resolveEpic(t, pool, tenantID, projectID)
	if created.ParentID == nil || *created.ParentID != epic.ID {
		t.Fatalf("anchored idea parent = %v, want the idea cloud epic %s", created.ParentID, epic.ID)
	}
	// Anchoring must not disturb the idea state itself.
	if created.Status != domain.WorkItemIdea {
		t.Fatalf("anchoring changed status to %q; it must leave idea state alone", created.Status)
	}
}

func TestPromotedIdeaKeepsAResolvableParent(t *testing.T) {
	// THE REGRESSION THIS FIXES, end to end: spawn an idea, promote it, and require
	// that its parent is still visible to a normal (idea-excluding) list query — the
	// property whose absence made the item vanish from the tree and print a bare ULID
	// on its detail page.
	pool, s, ctx, tenantID, projectID := ideaEnv(t)

	row := db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: "task", Title: "Promote me", Status: domain.WorkItemIdea,
	}
	idea := anchor(t, pool, tenantID, &row)

	got, err := s.PromoteIdea(ctx, connect.NewRequest(&apiv1.PromoteIdeaRequest{Id: idea.ID}))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	promoted := got.Msg.GetWorkItem()

	// The promoted item is a task under the epic.
	if promoted.GetParentId() == "" {
		t.Fatal("promoted item lost its parent")
	}
	if idea.ParentID != nil && *idea.ParentID != promoted.GetParentId() {
		t.Fatalf("promoted item parent = %q, want %q", promoted.GetParentId(), *idea.ParentID)
	}

	// THE ASSERTION THAT MATTERS: the parent resolves in the NORMAL scope.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	normal, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
		TenantID:  tenantID,
		ProjectID: projectID,
		PageSize:  1000,
		IdeaScope: "exclude", // what every normal work-items view asks for
	})
	if err != nil {
		t.Fatalf("list normal scope: %v", err)
	}
	byID := make(map[string]db.WorkItemRow, len(normal))
	for _, w := range normal {
		byID[w.ID] = w
	}
	if _, ok := byID[promoted.GetId()]; !ok {
		t.Fatal("promoted item absent from the normal scope")
	}
	parentID := promoted.GetParentId()
	if _, ok := byID[parentID]; !ok {
		t.Fatalf("promoted item's parent %s is NOT in the normal scope — this is the orphan that "+
			"disappeared from the tree and rendered as a bare ULID on the detail page", parentID)
	}
}

func TestAnchorIsIdempotent(t *testing.T) {
	pool, _, _, tenantID, projectID := ideaEnv(t)
	epic := resolveEpic(t, pool, tenantID, projectID)

	epicID := epic.ID
	row := db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: domain.WorkItemKindTask, Title: "Already anchored", Status: domain.WorkItemIdea,
		ParentID: &epicID,
	}
	created := anchor(t, pool, tenantID, &row)
	if created.ParentID == nil || *created.ParentID != epic.ID {
		t.Fatalf("re-anchoring moved an already-anchored idea: parent = %v", created.ParentID)
	}
}

func TestAnchorReparentsASpeculativePendingParent(t *testing.T) {
	// The reported run invented a PENDING epic to hold its ideas. Anchoring must not
	// leave that husk behind as normal-state, visible, meaningless work — it becomes a
	// child of the Idea Cloud epic instead.
	pool, _, _, tenantID, projectID := ideaEnv(t)
	epic := resolveEpic(t, pool, tenantID, projectID)

	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	husk, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: domain.WorkItemKindEpic, Title: "Weatherapp: close the gap",
		Status: domain.WorkItemPending,
	})
	if err != nil {
		t.Fatalf("create speculative epic: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	huskID := husk.ID
	row := db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: domain.WorkItemKindFeature, Title: "An idea", Status: domain.WorkItemIdea,
		ParentID: &huskID,
	}
	created := anchor(t, pool, tenantID, &row)

	if created.ParentID == nil || *created.ParentID != epic.ID {
		t.Fatalf("idea parent = %v, want the idea cloud epic", created.ParentID)
	}
	// The husk is now a child of the epic rather than a second dangling root.
	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx2.Rollback(ctx)
	moved, err := db.GetWorkItem(ctx, ttx2.Tx, tenantID, husk.ID)
	if err != nil {
		t.Fatalf("get husk: %v", err)
	}
	if moved.ParentID == nil || *moved.ParentID != epic.ID {
		t.Fatalf("speculative parent %s parent = %v, want the idea cloud epic %s",
			husk.ID, moved.ParentID, epic.ID)
	}
}

func TestAnchorLeavesAnIdeaStateParentAlone(t *testing.T) {
	// An idea-state parent is triage material in its own right — the reported epic was
	// one. Flattening it would destroy the run's grouping before an operator ever saw
	// it, so it is left where it is.
	pool, _, _, tenantID, projectID := ideaEnv(t)
	epic := resolveEpic(t, pool, tenantID, projectID)

	ideaParent := createIdeaItem(t, pool, tenantID, projectID, "Idea state epic", "", "")
	parentID := ideaParent.ID
	row := db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: domain.WorkItemKindTask, Title: "Child idea", Status: domain.WorkItemIdea,
		ParentID: &parentID,
	}
	anchor(t, pool, tenantID, &row)

	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	untouched, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, ideaParent.ID)
	if err != nil {
		t.Fatalf("get idea parent: %v", err)
	}
	if untouched.ParentID != nil {
		t.Fatalf("idea-state parent was moved to %v; it must be left alone", *untouched.ParentID)
	}
	_ = epic
}
