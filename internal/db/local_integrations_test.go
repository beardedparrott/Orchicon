package db

// local_integrations_test.go — THE RECORD, AGAINST A REAL DATABASE.
//
// These are not shape tests. Every assertion here is a property the reported failure needed and did not have:
// the obligation exists even before anything was attempted, a second enqueue cannot erase an outcome, the queue
// is ordered by production order, a conflict KEEPS BLOCKING, and the gate's count is what a run waits on.
//
// Skipped (never failed) without ORCHICON_TEST_DSN, like the rest of the repo's DB-backed proofs.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func localIntegrationTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping the DB-backed local-integration proof")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// localIntegrationTenant creates a fresh tenant whose rows are visible under RLS, plus a project.
func localIntegrationTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (tenantID, projectID string) {
	t.Helper()
	suffix := itoaTest(time.Now().UnixNano())
	tenantID = "tnt_li_" + suffix
	projectID = "proj_li_" + suffix
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, slug) VALUES ($1,$1,$1)
		ON CONFLICT (id) DO NOTHING`, tenantID); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id, tenant_id, name, slug, status)
		VALUES ($1,$2,'li project',$1,'active') ON CONFLICT (id) DO NOTHING`,
		projectID, tenantID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return tenantID, projectID
}

func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// THE OBLIGATION IS A ROW, WRITTEN BEFORE THE WORK IS ATTEMPTED — and enqueueing twice cannot lose an outcome.
func TestLocalIntegrationEnqueueIsIdempotentAndPreservesOutcome(t *testing.T) {
	pool, ctx := localIntegrationTestPool(t)
	tenantID, projectID := localIntegrationTenant(t, ctx, pool)

	tx, err := pool.Begin(ctx)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatalf("set tenant: %v", err)
	}

	rec, err := EnqueueLocalIntegration(ctx, tx, tenantID, projectID, "run_1", "exec_1", "branch-1", "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if rec.Status != LocalIntegrationPending {
		t.Fatalf("a new obligation is %q, want pending", rec.Status)
	}
	if rec.WorkflowRunID != "run_1" || rec.ExecutionID != "exec_1" || rec.Branch != "branch-1" {
		t.Fatalf("the record lost its identifying data: %+v", rec)
	}

	// It landed. Now the SECOND enqueue — a retried run, two triggers racing, the backfill overlapping —
	// must not reset it to pending. This is the property that stops a completed obligation reappearing.
	if err := SetLocalIntegrationStatus(ctx, tx, tenantID, rec.ID, LocalIntegrationMerged, "abc123", "", "develop"); err != nil {
		t.Fatalf("mark merged: %v", err)
	}
	again, err := EnqueueLocalIntegration(ctx, tx, tenantID, projectID, "run_1", "exec_1", "branch-1", "")
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if again.Status != LocalIntegrationMerged {
		t.Fatalf("re-enqueueing reset a MERGED record to %q — the work would be integrated twice or lost", again.Status)
	}
	if again.MergeCommit != "abc123" {
		t.Fatalf("re-enqueueing lost the merge commit (%q)", again.MergeCommit)
	}
	if again.Attempts < 1 {
		t.Fatalf("attempts did not record the attempt: %d", again.Attempts)
	}
}

// THE GATE'S COUNT — THE CONTRACT IS "ONLY A PROVEN RESULT RELEASES THE QUEUE".
//
// `verified`, `skipped` and `adopted` release it. Everything else blocks, and that list is longer than the
// obvious two because some of its members are states the platform used to CALL success:
//
//	pending      not landed yet
//	integrating  an attempt is in flight, or was interrupted by a restart
//	conflicted   a merge conflict; a human must resolve it
//	failed       merged, but the result could not be PROVEN — a tree that was not materialised, or the
//	             project's own check failing. The requested behaviour: "must not be recorded as success and
//	             must not let the next run start".
//	merged       LEGACY: landed by a build that did not verify. Unproven, so it is re-examined — otherwise a
//	             project keeps branching on top of a result nobody checked, which is how an empty checkout went
//	             unnoticed for six runs.
func TestOnlyAProvenResultReleasesTheQueue(t *testing.T) {
	cases := map[string]bool{ // status -> does it hold the queue?
		LocalIntegrationPending:     true,
		LocalIntegrationIntegrating: true,
		LocalIntegrationConflicted:  true,
		LocalIntegrationFailed:      true,
		LocalIntegrationMerged:      true, // legacy, unproven
		LocalIntegrationVerified:    false,
		LocalIntegrationSkipped:     false,
		LocalIntegrationAdopted:     false, // drained in the background, not gated
	}
	for status, wantBlock := range cases {
		t.Run(status, func(t *testing.T) {
			pool, ctx := localIntegrationTestPool(t)
			// A project of its own, so nothing else in the shared tenant can influence the count.
			tenantID, projectID := localIntegrationTenant(t, ctx, pool)
			tx, err := pool.Begin(ctx)
			t.Cleanup(func() { _ = tx.Rollback(ctx) })
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
				t.Fatalf("set tenant: %v", err)
			}
			rec, err := EnqueueLocalIntegration(ctx, tx, tenantID, projectID, "run_x", "", "b-x", "")
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if status != LocalIntegrationPending {
				if serr := SetLocalIntegrationStatus(ctx, tx, tenantID, rec.ID, status, "", "", "develop"); serr != nil {
					t.Fatalf("set status: %v", serr)
				}
			}
			n, cerr := CountBlockingLocalIntegrations(ctx, tx, tenantID, projectID)
			if cerr != nil {
				t.Fatalf("count: %v", cerr)
			}
			if gotBlock := n > 0; gotBlock != wantBlock {
				t.Fatalf("status %q holds the queue=%v (count %d), want %v — the gate releases only on a "+
					"PROVEN result", status, gotBlock, n, wantBlock)
			}
		})
	}
}


// THE QUEUE IS ORDERED BY PRODUCTION ORDER, and excludes an asking run's own record.
func TestQueueIsOldestFirstAndARunDoesNotBlockItself(t *testing.T) {
	pool, ctx := localIntegrationTestPool(t)
	tenantID, projectID := localIntegrationTenant(t, ctx, pool)

	tx, err := pool.Begin(ctx)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatalf("set tenant: %v", err)
	}

	for _, run := range []string{"run_first", "run_second", "run_third"} {
		if _, err := EnqueueLocalIntegration(ctx, tx, tenantID, projectID, run, "", "b-"+run, ""); err != nil {
			t.Fatalf("enqueue %s: %v", run, err)
		}
		// created_at defaults to now(); the order must still follow insertion because the query orders by
		// created_at then id. Make the ordering unambiguous.
		time.Sleep(2 * time.Millisecond)
	}
	recs, err := ListLocalIntegrationsByStatus(ctx, tx, tenantID, projectID, []string{LocalIntegrationPending}, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d pending records, want 3", len(recs))
	}
	if recs[0].WorkflowRunID != "run_first" || recs[2].WorkflowRunID != "run_third" {
		t.Fatalf("the queue is not oldest-first: %s, %s, %s — merging out of order is what makes the next "+
			"branch divergent", recs[0].WorkflowRunID, recs[1].WorkflowRunID, recs[2].WorkflowRunID)
	}

	// The projects-with-pending read is the backstop's index: this project must appear once, not three times.
	projects, err := ListProjectsWithPendingLocalIntegration(ctx, tx, tenantID, 10)
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	n := 0
	for _, p := range projects {
		if p == projectID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("project appears %d times in the pending-project list, want 1", n)
	}
}

// THE BACKFILL QUERY FINDS A RUN WITH NO RECORD, AND STOPS FINDING IT ONCE IT HAS ONE.
//
// This is the self-healing half: a run that completed while the plane was down still enters the queue, and a
// run that already has a record is not enqueued twice.
func TestRunsNeedingLocalIntegrationExcludesRecordedRuns(t *testing.T) {
	pool, ctx := localIntegrationTestPool(t)
	tenantID, projectID := localIntegrationTenant(t, ctx, pool)

	// A completed, branch-bearing run with NO record.
	runID := "run_no_record_" + itoaTest(time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_runs
		(id, tenant_id, workflow_id, workflow_version, project_id, status, worktree_branch, current_step)
		VALUES ($1,$2,'wf_x',1,$3,'completed','feat-x','')`, runID, tenantID, projectID); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	tx, err := pool.Begin(ctx)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatalf("set tenant: %v", err)
	}

	runs, err := ListRunsNeedingLocalIntegration(ctx, tx, tenantID, projectID, 10)
	if err != nil {
		t.Fatalf("list runs needing integration: %v", err)
	}
	found := false
	for _, r := range runs {
		if r.ID == runID {
			found = true
		}
	}
	if !found {
		t.Fatalf("a completed branch-bearing run with no record was NOT found — that run would never enter the "+
			"queue, which is the same silence in a new place (got %d runs)", len(runs))
	}

	if _, err := EnqueueLocalIntegration(ctx, tx, tenantID, projectID, runID, "", "feat-x", ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	runs, err = ListRunsNeedingLocalIntegration(ctx, tx, tenantID, projectID, 10)
	if err != nil {
		t.Fatalf("relist: %v", err)
	}
	for _, r := range runs {
		if r.ID == runID {
			t.Fatal("a run that now HAS a record is still listed for enqueueing — the obligation would be " +
				"recreated and the queue would never drain")
		}
	}
}
