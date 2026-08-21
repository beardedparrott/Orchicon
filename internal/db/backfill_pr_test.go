package db_test

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestListBackfillPRRuns verifies the one-shot PR-backfill scan: it must
// return git-backed terminal runs (completed/failed/aborted) that have a
// branch recorded and NO captured pr_url yet, and must exclude in-flight
// runs, non-git-backed runs, and runs that already have a pr_url.
func TestListBackfillPRRuns(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_dev"

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant,
		Name: "Backfill PR Test", Slug: "backfill-pr-" + db.NewID()[:8],
		Status: "active", Goals: []byte("{}"),
		RepoSlug: strPtr("beardedparrott/Orchicon"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		tx, err := pool.BeginTenantTx(c, tenant)
		if err != nil {
			return
		}
		defer tx.Rollback(c)
		_ = db.DeleteProject(c, tx.Tx, tenant, proj.ID)
		_ = tx.Commit(c)
	})

	mkRun := func(status, wts, branch, runContext string) db.WorkflowRunRow {
		r, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
			ID: db.NewID(), TenantID: tenant,
			WorkflowID: "wf-pr", WorkflowVersion: 1,
			ProjectID:  proj.ID,
			Status:     status,
			RunContext: []byte(runContext),
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		// CreateWorkflowRun does not write the worktree fields on insert;
		// set them via the partial update (as the WorktreeReconciler does).
		_, err = db.UpdateWorkflowRun(ctx, ttx.Tx, tenant, r.ID, r.Version, db.UpdateWorkflowRunFields{
			WorktreeStatus: &wts,
			WorktreePath:   strPtr("/wt"),
			WorktreeBranch: &branch,
		})
		if err != nil {
			t.Fatalf("update run worktree: %v", err)
		}
		return r
	}

	want := mkRun("completed", "pruned", "branch-merged", "{}")
	mkRun("completed", "pruned", "branch-done", `{"pr_url":"https://github.com/a/b/pull/9","pr_state":"merged"}`)
	mkRun("completed", "pruned", "", "{}")                // no branch → not git-backed target
	mkRun("completed", "skipped", "branch-skipped", "{}") // worktree skipped → no branch
	mkRun("running", "ready", "branch-running", "{}")     // in-flight → excluded

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	readTtx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer readTtx.Rollback(ctx)

	runs, err := db.ListBackfillPRRuns(ctx, readTtx.Tx, tenant)
	if err != nil {
		t.Fatalf("ListBackfillPRRuns: %v", err)
	}

	if len(runs) != 1 {
		t.Fatalf("ListBackfillPRRuns returned %d runs, want exactly 1", len(runs))
	}
	if runs[0].ID != want.ID {
		t.Fatalf("ListBackfillPRRuns id = %q, want %q", runs[0].ID, want.ID)
	}
}

func strPtr(s string) *string { return &s }
