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

func TestAnchorAdoptsTheFiresEarlierChildren(t *testing.T) {
	// THE ORDERING HAZARD. A spawning run creates its OWN parent first and its ideas under
	// it, so by the time any idea is anchored there are already siblings sitting under the
	// run's parent. Anchoring only the current row would split one fire's output across two
	// containers. The Idea Cloud epic must end up authoritative for the whole fire.
	pool, _, _, tenantID, projectID := ideaEnv(t)
	spawn := db.NewID()

	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	// The run's own speculative parent, created first.
	runEpic, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: domain.WorkItemKindEpic, Title: "Weatherapp: close the market-capability gap",
		Status: domain.WorkItemIdea, SpawnedByWorkItemID: &spawn,
	})
	if err != nil {
		t.Fatalf("create run epic: %v", err)
	}
	runEpicID := runEpic.ID
	// Two earlier ideas already parented under it.
	for _, title := range []string{"Earlier idea 1", "Earlier idea 2"} {
		if _, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
			ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
			Kind: domain.WorkItemKindFeature, Title: title,
			Status: domain.WorkItemIdea, ParentID: &runEpicID, SpawnedByWorkItemID: &spawn,
		}); err != nil {
			t.Fatalf("create earlier idea: %v", err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Now anchor a THIRD idea of the same fire.
	row := db.WorkItemRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: projectID,
		Kind: "feature", Title: "Third idea", Status: domain.WorkItemIdea,
		ParentID: &runEpicID, SpawnedByWorkItemID: &spawn,
	}
	created := anchor(t, pool, tenantID, &row)
	epic := resolveEpic(t, pool, tenantID, projectID)

	if created.ParentID == nil || *created.ParentID != epic.ID {
		t.Fatalf("current idea parent = %v, want the container", created.ParentID)
	}
	// THE ASSERTION: the earlier two moved too, so the whole fire sits in one container.
	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx2.Rollback(ctx)
	sibs, err := db.ListWorkItems(ctx, ttx2.Tx, db.ListWorkItemsFilter{
		TenantID: tenantID, ProjectID: projectID, PageSize: 1000, IdeaScope: "only",
	})
	if err != nil {
		t.Fatalf("list ideas: %v", err)
	}
	var underContainer int
	for _, it := range sibs {
		if it.ParentID != nil && *it.ParentID == epic.ID {
			underContainer++
		}
	}
	if underContainer != 3 {
		t.Fatalf("ideas under the container = %d, want 3 (the fire's whole output, not just the current row)", underContainer)
	}
	// And nothing idea-state is still parked under the run's own parent.
	for _, it := range sibs {
		if it.ParentID != nil && *it.ParentID == runEpic.ID {
			t.Fatalf("idea %q still parented to the run's own epic; the fire was split", it.Title)
		}
	}
	// The emptied speculative container is retired, not left as a husk.
	runEpicNow, err := db.GetWorkItem(ctx, ttx2.Tx, tenantID, runEpic.ID)
	if err != nil {
		t.Fatalf("get run epic: %v", err)
	}
	if runEpicNow.Status != domain.WorkItemCancelled {
		t.Fatalf("speculative container status = %q, want cancelled (superseded)", runEpicNow.Status)
	}
}

func TestPromoteIdeaCreatesContainerAndParentsTheTask(t *testing.T) {
	// THE OPERATOR'S REQUIREMENT, end to end:
	//   1. approving an idea creates the "Idea Cloud" epic if it does not exist
	//   2. ... reuses it if it does
	//   3. the promoted item becomes a TASK
	//   4. ... parented to that epic
	//   5. both are visible in the active (non-idea) views
	pool, s, ctx, tenantID, projectID := ideaEnv(t)

	// An idea from before the container existed: no parent at all.
	orphan := createIdeaItem(t, pool, tenantID, projectID, "Legacy idea", "", "")

	got, err := s.PromoteIdea(ctx, connect.NewRequest(&apiv1.PromoteIdeaRequest{Id: orphan.ID}))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	promoted := got.Msg.GetWorkItem()

	// (1) the container was created.
	epic := resolveEpic(t, pool, tenantID, projectID)
	if epic.Title != IdeaCloudEpicTitle {
		t.Fatalf("container title = %q, want %q", epic.Title, IdeaCloudEpicTitle)
	}
	// (3) the promoted item is a task.
	if promoted.GetKind() != apiv1.WorkItemKind_WORK_ITEM_KIND_TASK {
		t.Fatalf("promoted kind = %v, want TASK", promoted.GetKind())
	}
	// (4) parented to the container.
	if promoted.GetParentId() != epic.ID {
		t.Fatalf("promoted parent = %q, want the container %s", promoted.GetParentId(), epic.ID)
	}

	// (5) BOTH appear in the active scope — the query the work-items page issues.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	active, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
		TenantID: tenantID, ProjectID: projectID, PageSize: 1000, IdeaScope: "exclude",
	})
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	seen := map[string]bool{}
	for _, w := range active {
		seen[w.ID] = true
	}
	if !seen[epic.ID] {
		t.Fatal("the Idea Cloud epic is NOT in the active work items — the operator cannot see the parent")
	}
	if !seen[promoted.GetId()] {
		t.Fatal("the promoted task is NOT in the active work items")
	}

	// (2) a SECOND promotion reuses the same container rather than creating another.
	second := createIdeaItem(t, pool, tenantID, projectID, "Second idea", "", "")
	got2, err := s.PromoteIdea(ctx, connect.NewRequest(&apiv1.PromoteIdeaRequest{Id: second.ID}))
	if err != nil {
		t.Fatalf("promote second: %v", err)
	}
	if got2.Msg.GetWorkItem().GetParentId() != epic.ID {
		t.Fatalf("second promotion parented to %q, want the SAME container %s",
			got2.Msg.GetWorkItem().GetParentId(), epic.ID)
	}
	again := resolveEpic(t, pool, tenantID, projectID)
	if again.ID != epic.ID {
		t.Fatal("a second promotion created a SECOND idea cloud epic")
	}
}

func TestPromoteIdeaIsVisibleInTheTreeShape(t *testing.T) {
	// The tree needs the parent present in the SAME list it renders, or the child becomes an
	// orphan root. This asserts the shape the frontend's buildTreeData sees: a parentless
	// epic plus a task pointing at it.
	pool, s, ctx, tenantID, projectID := ideaEnv(t)
	idea := createIdeaItem(t, pool, tenantID, projectID, "Promote me", "", "")
	got, err := s.PromoteIdea(ctx, connect.NewRequest(&apiv1.PromoteIdeaRequest{Id: idea.ID}))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	parentID := got.Msg.GetWorkItem().GetParentId()

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	active, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
		TenantID: tenantID, ProjectID: projectID, PageSize: 1000, IdeaScope: "exclude",
	})
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	var parentFound, childFound bool
	for _, w := range active {
		if w.ID == parentID {
			parentFound = true
			if w.ParentID != nil {
				t.Fatalf("the container is not top-level (parent %v)", *w.ParentID)
			}
		}
		if w.ID == idea.ID {
			childFound = true
		}
	}
	if !parentFound || !childFound {
		t.Fatalf("tree shape incomplete: parent present=%v child present=%v", parentFound, childFound)
	}
}
