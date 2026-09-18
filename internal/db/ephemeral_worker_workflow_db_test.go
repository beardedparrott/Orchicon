package db_test

// ephemeral_worker_workflow_db_test.go — the gate and the sweep for the two
// tables that joined the ephemeral mechanism after work_items: workers and
// workflows.
//
// The operator: "Yeah I would like ephemeral only on the workers and workflows
// Quick Work creates." Quick Work builds a throwaway worker (pinned to the
// agent's own model_ref) and a throwaway workflow per job, and both must be
// invisible in the Workers and Workflows screens while the job runs and gone
// when it ends.
//
// This file exists because the pure predicate tests CANNOT see the wiring. If
// `ephemeral` were missing from a RETURNING list or landed in the wrong scan
// slot, the predicate would filter on a value that is never loaded and every
// pure test would still pass. Only a real row proves the round-trip.
//
// Guarded by ORCHICON_TEST_DSN like the other DB-backed tests in this package.

import (
	"context"
	"os"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// ephWorkerWorkflowFixture migrates, opens a tenant transaction, and creates one
// ordinary and one ephemeral worker and workflow.
func ephWorkerWorkflowFixture(t *testing.T, ctx context.Context, tenant string) (*db.Pool, *db.TenantTx, string, string) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed ephemeral worker/workflow test")
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatal(err)
	}
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant, Name: "eph-wf", Slug: "eph-wf-" + db.NewID(),
		Status: domain.ProjectActive, Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	db.CleanupProject(t, pool, tenant, proj.ID)

	mkWorker := func(name string, ephemeral bool) db.WorkerRow {
		w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
			ID: db.NewID(), TenantID: tenant, Name: name, Slug: name + "-" + db.NewID(),
			Description: "d", Purpose: "p", Status: domain.WorkerDraft, Ephemeral: ephemeral,
		})
		if err != nil {
			t.Fatalf("create worker %s: %v", name, err)
		}
		return w
	}
	mkWorkflow := func(name string, ephemeral bool) db.WorkflowRow {
		wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
			ID: db.NewID(), TenantID: tenant, ProjectID: proj.ID, Name: name,
			Type: domain.WorkflowTypeOneShot, Status: domain.WorkflowDraft, Ephemeral: ephemeral,
		})
		if err != nil {
			t.Fatalf("create workflow %s: %v", name, err)
		}
		return wf
	}

	ordinaryWorker := mkWorker("ordinary-worker", false)
	ephWorker := mkWorker("quick-work-worker", true)
	ordinaryWorkflow := mkWorkflow("ordinary-workflow", false)
	ephWorkflow := mkWorkflow("quick-work-workflow", true)

	// THE ROUND-TRIP. If either of these fails, the create path wrote the flag
	// somewhere the read path cannot see it, and a transient would be VISIBLE
	// despite being marked — the exact leak the flag exists to prevent.
	if ephWorker.Ephemeral != true {
		t.Fatalf("CreateWorker returned Ephemeral=false for an ephemeral worker — the flag is not round-tripping "+
			"through INSERT ... RETURNING (worker id %s)", ephWorker.ID)
	}
	if ephWorkflow.Ephemeral != true {
		t.Fatalf("CreateWorkflow returned Ephemeral=false for an ephemeral workflow — the flag is not "+
			"round-tripping through INSERT ... RETURNING (workflow id %s)", ephWorkflow.ID)
	}
	if ordinaryWorker.Ephemeral || ordinaryWorkflow.Ephemeral {
		t.Fatal("an ordinary create came back marked ephemeral — the flag defaults true somewhere, which would " +
			"hide real workers and workflows from the operator")
	}

	// GET must also carry the flag: the delete guards read it by id, and a false
	// here would let the guard wave through a workflow it should protect.
	gotW, err := db.GetWorker(ctx, ttx.Tx, tenant, ephWorker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !gotW.Ephemeral {
		t.Error("GetWorker reports Ephemeral=false for an ephemeral worker — the by-id read lost the flag")
	}
	gotWF, err := db.GetWorkflow(ctx, ttx.Tx, tenant, ephWorkflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !gotWF.Ephemeral {
		t.Error("GetWorkflow reports Ephemeral=false for an ephemeral workflow — the by-id read lost the flag")
	}

	return pool, ttx, ordinaryWorker.ID, ephWorker.ID
}

// THE WORKERS VIEW HIDES TRANSIENTS BY DEFAULT, and only the opt-in reveals them.
func TestEphemeralWorkerIsHiddenFromTheDefaultList(t *testing.T) {
	ctx := context.Background()
	_, ttx, ordinaryID, ephID := ephWorkerWorkflowFixture(t, ctx, "tnt_eph_worker_gate")

	byID := func(ids []db.WorkerRow) map[string]bool {
		m := make(map[string]bool, len(ids))
		for _, w := range ids {
			m[w.ID] = true
		}
		return m
	}

	def, err := db.ListWorkers(ctx, ttx.Tx, db.ListWorkersFilter{TenantID: "tnt_eph_worker_gate", PageSize: 500})
	if err != nil {
		t.Fatal(err)
	}
	d := byID(def)
	if d[ephID] {
		t.Error("an ephemeral worker appeared in the DEFAULT ListWorkers — it would render in the Workers " +
			"screen, which is the leak the flag exists to prevent")
	}
	if !d[ordinaryID] {
		t.Error("the default ListWorkers dropped an ORDINARY worker — the gate is over-filtering, which would " +
			"hide real workers from the operator")
	}

	inc, err := db.ListWorkers(ctx, ttx.Tx, db.ListWorkersFilter{
		TenantID: "tnt_eph_worker_gate", PageSize: 500, EphemeralScope: "include"})
	if err != nil {
		t.Fatal(err)
	}
	if !byID(inc)[ephID] {
		t.Error("the ephemeral opt-in did not return the ephemeral worker, so Quick Work could not enumerate " +
			"the workers it created in order to clean them up")
	}

	only, err := db.ListWorkers(ctx, ttx.Tx, db.ListWorkersFilter{
		TenantID: "tnt_eph_worker_gate", PageSize: 500, EphemeralScope: "only"})
	if err != nil {
		t.Fatal(err)
	}
	o := byID(only)
	if !o[ephID] || o[ordinaryID] {
		t.Errorf("scope \"only\" returned the wrong partition (ephemeral present=%v, ordinary present=%v)",
			o[ephID], o[ordinaryID])
	}
}

// THE JOINED LIST — the query the Workers pane actually calls — is gated on the
// QUALIFIED column, and this is the test that proves the qualifier is right: an
// unqualified `ephemeral` in that join is ambiguous and Postgres rejects the
// statement outright, so a green result here means the SQL is unambiguous.
func TestEphemeralWorkerIsHiddenFromTheJoinedListThePaneUses(t *testing.T) {
	ctx := context.Background()
	_, ttx, ordinaryID, ephID := ephWorkerWorkflowFixture(t, ctx, "tnt_eph_worker_join")

	rows, err := db.ListWorkersWithActiveVersion(ctx, ttx.Tx, db.ListWorkersFilter{
		TenantID: "tnt_eph_worker_join", PageSize: 500})
	if err != nil {
		t.Fatalf("the joined list (which the Workers pane calls) failed: %v", err)
	}
	for _, r := range rows {
		if r.ID == ephID {
			t.Error("an ephemeral worker reached the Workers pane's own query — the gate is applied to " +
				"ListWorkers but not to the list the pane renders from")
		}
	}
	_ = ordinaryID

	// And on request, the joined list carries it too.
	rows2, err := db.ListWorkersWithActiveVersion(ctx, ttx.Tx, db.ListWorkersFilter{
		TenantID: "tnt_eph_worker_join", PageSize: 500, EphemeralScope: "include"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows2 {
		if r.ID == ephID {
			found = true
		}
	}
	if !found {
		t.Error("the ephemeral opt-in did not return the ephemeral worker from the joined list")
	}
}

// THE WORKFLOWS VIEW HIDES TRANSIENTS BY DEFAULT.
func TestEphemeralWorkflowIsHiddenFromTheDefaultList(t *testing.T) {
	ctx := context.Background()
	_, ttx, _, _ := ephWorkerWorkflowFixture(t, ctx, "tnt_eph_workflow_gate")
	tenant := "tnt_eph_workflow_gate"

	all, err := db.ListWorkflows(ctx, ttx.Tx, db.ListWorkflowsFilter{TenantID: tenant, PageSize: 500})
	if err != nil {
		t.Fatal(err)
	}
	visible := map[string]bool{}
	for _, w := range all {
		visible[w.Name] = true
		if w.Ephemeral {
			t.Errorf("workflow %q came back Ephemeral=true from a DEFAULT list — the flag reached the row but "+
				"the gate did not filter it", w.Name)
		}
	}
	if !visible["ordinary-workflow"] {
		t.Error("the default workflow list dropped an ORDINARY workflow")
	}
	if visible["quick-work-workflow"] {
		t.Error("an ephemeral workflow appeared in the DEFAULT ListWorkflows — it would render in the " +
			"Workflows screen")
	}

	inc, err := db.ListWorkflows(ctx, ttx.Tx, db.ListWorkflowsFilter{
		TenantID: tenant, PageSize: 500, EphemeralScope: "include"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range inc {
		if w.Name == "quick-work-workflow" {
			found = true
		}
	}
	if !found {
		t.Error("the ephemeral opt-in did not return the ephemeral workflow")
	}
}

// THE SWEEP REMOVES ABANDONED TRANSIENTS AND NOTHING ELSE.
//
// This is the backstop test: an agent that dies leaves its records behind, and
// the sweep is the only thing that ever collects them. So it has to remove
// exactly the stale transients — no real work, and no live job.
func TestSweepRemovesOnlyAbandonedEphemeralRecords(t *testing.T) {
	ctx := context.Background()
	tenant := "tnt_eph_sweep"
	pool, ttx, _, _ := ephWorkerWorkflowFixture(t, ctx, tenant)

	// THE ORDER MATTERS: the old records are created and then BACKDATED, and only
	// afterwards are the live ones created. Backdating after creating a "fresh"
	// row would age it too, which is how the first version of this test managed to
	// delete records it was asserting should survive.
	oldItem, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenant, Kind: domain.WorkItemKindTask,
		Title: "old real work", Description: "d", AcceptanceCriteria: "ac",
		Status: domain.WorkItemPending, Priority: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Backdate EVERYTHING that exists now — ephemeral and ordinary alike. The
	// ordinary rows are backdated too on purpose: age alone must not be enough,
	// or the sweep would eventually delete records a human created.
	for _, stmt := range []string{
		`UPDATE work_items SET created_at = now() - interval '7 hours' WHERE tenant_id = $1`,
		`UPDATE workers SET created_at = now() - interval '7 hours' WHERE tenant_id = $1`,
		`UPDATE workflows SET created_at = now() - interval '7 hours' WHERE tenant_id = $1`,
	} {
		if _, err := ttx.Tx.Exec(ctx, stmt, tenant); err != nil {
			t.Fatal(err)
		}
	}

	// NOW the live job's records, so they carry a real "now" timestamp.
	freshItem, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenant, Kind: domain.WorkItemKindTask,
		Title: "live job", Description: "d", AcceptanceCriteria: "ac",
		Status: domain.WorkItemPending, Priority: 1, Ephemeral: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	freshWorker, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
		ID: db.NewID(), TenantID: tenant, Name: "live-worker", Slug: "live-worker-" + db.NewID(),
		Status: domain.WorkerDraft, Ephemeral: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cutoff 6h back: only records older than that are abandoned.
	res, err := db.SweepAbandonedEphemeral(ctx, ttx.Tx, tenant, time.Now().Add(-6*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Total() == 0 {
		t.Fatal("the sweep removed nothing, even though an out-of-window ephemeral item, worker and workflow " +
			"were all present — a sweep that never deletes is not a backstop")
	}

	count := func(table, where string) int {
		var n int
		if err := ttx.Tx.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE tenant_id = $1 AND `+where, tenant).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The stale transients are gone…
	if n := count("work_items", "ephemeral"); n != 1 {
		t.Errorf("expected 1 ephemeral work item to survive (the fresh one), got %d — the sweep either missed a "+
			"stale record or took a live one", n)
	}
	if n := count("workers", "ephemeral"); n != 1 {
		t.Errorf("expected 1 ephemeral worker to survive (the fresh one), got %d", n)
	}
	// …the fresh transient survived (the window is the guard)…
	if _, err := db.GetWorkItem(ctx, ttx.Tx, tenant, freshItem.ID); err != nil {
		t.Errorf("the sweep deleted a FRESH ephemeral item — the window is the only thing standing between "+
			"the sweep and a live job's records: %v", err)
	}
	if _, err := db.GetWorker(ctx, ttx.Tx, tenant, freshWorker.ID); err != nil {
		t.Errorf("the sweep deleted a FRESH ephemeral worker: %v", err)
	}
	// …and no real work was touched, even though it was just as old.
	if n := count("work_items", "NOT ephemeral"); n != 1 {
		t.Errorf("the sweep removed real work (ordinary items left: %d, expected 1 — the backdated one)", n)
	}
	if _, err := db.GetWorkItem(ctx, ttx.Tx, tenant, oldItem.ID); err != nil {
		t.Errorf("the sweep deleted an ORDINARY work item that was every bit as old as the transients — age "+
			"alone must never be enough: %v", err)
	}
	if n := count("workers", "NOT ephemeral"); n != 1 {
		t.Errorf("the sweep removed a real worker (ordinary workers left: %d, expected 1)", n)
	}
	if n := count("workflows", "NOT ephemeral"); n != 1 {
		t.Errorf("the sweep removed a real workflow (ordinary workflows left: %d, expected 1)", n)
	}
	if n := count("workflows", "ephemeral"); n != 0 {
		t.Errorf("the stale ephemeral workflow survived the sweep (%d left) — the backstop did not collect it", n)
	}
	_ = pool
}

// A WORKFLOW WITH RUN HISTORY SWEEPS CLEANLY — the ordering claim.
//
// workflow_runs.work_item_id references work_items, so deleting the item first
// would make the database write NULL into a run that is about to be deleted
// anyway. The sweep deletes workflows FIRST for that reason, and this test
// creates the exact shape that would expose getting it wrong.
func TestSweepRemovesAnAbandonedWorkflowWithItsRuns(t *testing.T) {
	ctx := context.Background()
	tenant := "tnt_eph_sweep_runs"
	_, ttx, _, _ := ephWorkerWorkflowFixture(t, ctx, tenant)

	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenant, Kind: domain.WorkItemKindTask,
		Title: "job", Description: "d", AcceptanceCriteria: "ac",
		Status: domain.WorkItemPending, Priority: 1, Ephemeral: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: item.ProjectID, Name: "job-wf",
		Type: domain.WorkflowTypeOneShot, Status: domain.WorkflowDraft, Ephemeral: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: tenant, WorkflowID: wf.ID, WorkflowVersion: 1,
		ProjectID: item.ProjectID, Status: domain.WorkflowRunPending, WorkItemID: item.ID,
		// run_context is NOT NULL; the column is mandatory even when empty.
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: tenant, WorkflowRunID: run.ID, StepID: "s1",
		StepName: "step", StepKind: "TASK", Status: domain.StepRunPending,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := ttx.Tx.Exec(ctx, `UPDATE workflows SET created_at = now() - interval '7 hours' WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := ttx.Tx.Exec(ctx, `UPDATE work_items SET created_at = now() - interval '7 hours' WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatal(err)
	}

	res, err := db.SweepAbandonedEphemeral(ctx, ttx.Tx, tenant, time.Now().Add(-6*time.Hour))
	if err != nil {
		t.Fatalf("the sweep failed on a workflow that had a run bound to a work item — the delete ordering is "+
			"wrong (workflows own the runs that reference items, so they must go first): %v", err)
	}
	if res.Workflows == 0 || res.WorkItems == 0 {
		t.Errorf("expected both the workflow and the item to be swept, got workflows=%d items=%d",
			res.Workflows, res.WorkItems)
	}
	var runs, stepRuns int
	if err := ttx.Tx.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE tenant_id = $1`, tenant).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Tx.QueryRow(ctx, `SELECT count(*) FROM workflow_step_runs WHERE tenant_id = $1`, tenant).Scan(&stepRuns); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || stepRuns != 0 {
		t.Errorf("the sweep left run rows behind (runs=%d step_runs=%d) — an abandoned workflow must not leave "+
			"orphaned history", runs, stepRuns)
	}
}
