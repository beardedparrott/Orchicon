package db_test

// ephemeral_gate_db_test.go — the ephemeral gate against a REAL table.

import (
	"context"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// setupEphemeralFixture migrates, opens a tenant transaction, and returns a
// project plus one ordinary and one ephemeral work item in it.
func setupEphemeralFixture(t *testing.T, ctx context.Context) (*db.Pool, *db.TenantTx, string, string, string) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed ephemeral gate test")
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatal(err)
	}
	tenant := "tnt_eph"
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant, Name: "eph-test", Slug: "eph-test-" + db.NewID(),
		Status: domain.ProjectActive, Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Self-cleaning: see db.CleanupProject.
	db.CleanupProject(t, pool, tenant, proj.ID)

	ordinary, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: proj.ID, Kind: domain.WorkItemKindTask,
		Title: "ordinary", Description: "d", AcceptanceCriteria: "ac", Status: domain.WorkItemPending, Priority: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	eph, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: proj.ID, Kind: domain.WorkItemKindTask,
		Title: "quick work job", Description: "d", AcceptanceCriteria: "ac",
		Status: domain.WorkItemPending, Priority: 1, Ephemeral: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The flag must survive the INSERT/RETURNING round-trip: if it does not, the
	// gate is filtering on a column the create path never wrote.
	if !eph.Ephemeral {
		t.Fatal("CreateWorkItem returned Ephemeral=false for an ephemeral item — the flag is not round-tripping " +
			"through INSERT ... RETURNING, so a created transient would be VISIBLE despite being marked")
	}
	return pool, ttx, tenant, ordinary.ID, eph.ID
}

// THE ROW IS HIDDEN FROM EVERY DEFAULT READ, and visible only on request.
func TestEphemeralItemIsHiddenFromDefaultListAndVisibleOnRequest(t *testing.T) {
	ctx := context.Background()
	pool, ttx, tenant, ordinaryID, ephID := setupEphemeralFixture(t, ctx)
	defer pool.Close()
	defer ttx.Rollback(ctx)

	ids := func(scope string) map[string]bool {
		items, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
			TenantID: tenant, PageSize: 1000, EphemeralScope: scope,
		})
		if err != nil {
			t.Fatalf("ListWorkItems(scope=%q): %v", scope, err)
		}
		out := map[string]bool{}
		for _, it := range items {
			out[it.ID] = true
		}
		return out
	}

	// The default — what every human view gets with no scope set at all.
	def := ids("")
	if def[ephID] {
		t.Error("an ephemeral item appeared in the DEFAULT work-item list — this is exactly the invisible-record " +
			"leak the operator objected to, and the board/tree/sequence/counts all read through here")
	}
	if !def[ordinaryID] {
		t.Error("the ordinary item is missing from the default list — the gate is hiding real work, not just transients")
	}

	// The agent's opt-in, which is the only way to see what Quick Work created.
	if !ids("include")[ephID] {
		t.Error("an ephemeral item is invisible even with EphemeralScope=include, so the Quick Work agent cannot " +
			"enumerate the items it created and could never clean them up")
	}
	only := ids("only")
	if !only[ephID] || only[ordinaryID] {
		t.Error("EphemeralScope=only must return exactly the ephemeral partition")
	}

	// The flag must also come back correctly ON THE ROW — the runtime proof that
	// the column is in WorkItemSelectCols and in the right scan position.
	items, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
		TenantID: tenant, PageSize: 1000, EphemeralScope: "only",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if !it.Ephemeral {
			t.Errorf("row %s came back from the ephemeral partition with Ephemeral=false — the column is loaded "+
				"into the wrong field or missing from the SELECT list", it.ID)
		}
	}
}

// AN EPHEMERAL ITEM STILL DISPATCHES.
func TestEphemeralItemStillReachesTheDispatchScan(t *testing.T) {
	ctx := context.Background()
	pool, ttx, tenant, _, ephID := setupEphemeralFixture(t, ctx)
	defer pool.Close()
	defer ttx.Rollback(ctx)

	// Make the ephemeral item dispatchable: ready, with a worker bound (the two
	// conditions ListReadyTasks requires).
	status := domain.WorkItemReady
	ref := []byte(`{"worker_id":"wk_quick","version":1}`)
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenant, ephID, 1, db.UpdateWorkItemFields{
		Status: &status, AssignedWorkerRef: &ref,
	}); err != nil {
		t.Fatal(err)
	}

	ready, err := db.ListReadyTasks(ctx, ttx.Tx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range ready {
		if r.ID == ephID {
			found = true
		}
	}
	if !found {
		t.Error("an ephemeral item in ready status with a bound worker is NOT returned by ListReadyTasks — the " +
			"dispatch scan has been made to filter ephemeral rows, which would make every Quick Work job invisible " +
			"AND inert with no error anywhere")
	}
}

// REORDER STILL WORKS WITH AN EPHEMERAL SIBLING PRESENT.
func TestSiblingListAgreesWithTheVisibleBoard(t *testing.T) {
	ctx := context.Background()
	pool, ttx, tenant, ordinaryID, ephID := setupEphemeralFixture(t, ctx)
	defer pool.Close()
	defer ttx.Rollback(ctx)

	items, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{TenantID: tenant, PageSize: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var projID string
	for _, it := range items {
		if it.ID == ordinaryID {
			projID = it.ProjectID
		}
	}
	if projID == "" {
		t.Fatal("fixture: could not find the project for the ordinary item")
	}

	sibs, err := db.ListSiblingsForReorder(ctx, ttx.Tx, tenant, projID, "")
	if err != nil {
		t.Fatal(err)
	}
	var visibleSiblings int
	for _, s := range sibs {
		if s.ID == ephID {
			t.Error("ListSiblingsForReorder returned the ephemeral item, which the board does not show — " +
				"ReorderWorkItems demands an exact permutation of this list, so every top-level drag in this " +
				"project would fail with 'not a permutation' and nothing on screen would explain it")
		}
		if s.ParentID == nil {
			visibleSiblings++
		}
	}
	// THE AGREEMENT ITSELF: the two queries must count the same children.
	var visibleTopLevel int
	for _, it := range items {
		if it.ProjectID == projID && it.ParentID == nil {
			visibleTopLevel++
		}
	}
	if visibleSiblings != visibleTopLevel {
		t.Errorf("ListSiblingsForReorder returned %d top-level children but the board shows %d — the filter that "+
			"hides ephemeral items and the filter that builds the reorder permutation have drifted apart",
			visibleSiblings, visibleTopLevel)
	}
}
