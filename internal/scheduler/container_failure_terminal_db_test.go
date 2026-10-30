package scheduler

// container_failure_terminal_db_test.go — A CONTAINER THAT CANNOT BE CREATED MUST FAIL THE RUN, ONCE, VISIBLY.
//
// Requested as regression coverage for the behaviour an operator observed AFTER the hot-loop fix, on a real
// Windows host. Before the fix the same failure produced 22 minutes of retries with nothing shown; after it,
// the run failed within a second:
//
//	workflow run failed at start  reason="runtime container could not be created: ... mount source outside
//	allowed roots: C:\projects\weatherapp ..."
//	status=failed, started 19:22:19.313, ended 19:22:20.147   (0.8s)
//
// Four properties, and the hot loop is the one worth pinning hardest: a failure that terminates is a
// diagnosis, a failure that retries is a black hole.
//
// Skipped without ORCHICON_TEST_DSN (approvalTestPool).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// unavailableLifecycle models a daemon that refuses every container create — the reported mount refusal, and
// any other reason a container cannot be built.
type unavailableLifecycle struct {
	stubLifecycle
	creates int // how many times a create was attempted, so "once, not in a loop" is measurable
	reason  string
}

func (u *unavailableLifecycle) EnsureForRun(context.Context, db.WorkflowRunRow) error {
	u.creates++
	return errors.New(u.reason)
}
func (u *unavailableLifecycle) EnsureServing(context.Context, db.WorkflowRunRow, bool) error {
	return errors.New(u.reason)
}

func TestContainerCreateFailureFailsTheRunOnceAndVisibly(t *testing.T) {
	ctx := context.Background()
	pool := approvalTestPool(t)

	// A RUNTIME-MODE project: this is the configuration that actually attempts a container.
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	slug := "cfail-" + strings.ToLower(db.NewID())
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "Ctr Fail", Slug: slug,
		Status: "active", Goals: []byte("[]"), ProjectDir: "/tmp/orchicon/" + slug,
		ExecutionMode: db.ExecutionModeRuntime,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	suffix := strings.ToLower(db.NewID())[:12]
	w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
		ID: "w-" + suffix, TenantID: approvalTestTenant, Name: "cf-" + suffix[:8],
		Slug: "w-" + suffix, Status: domain.WorkerPublished,
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkerID: w.ID, Version: 1,
		Status: domain.WorkerVersionPublished, ModelRef: "opencode/some/model",
	}); err != nil {
		t.Fatalf("create worker version: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, `[
		{"id":"s1","name":"Container","kind":"task","ref":"`+w.ID+`","depends_on":[]}
	]`)

	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	// BOUND TO A WORK ITEM, because that is where the operator-facing reason lands: failRunAtStart writes
	// the narrative and an acceptance review naming the cause, and marks the item failed. Asserting the run
	// row alone would miss the half a person actually reads.
	item, err := db.CreateWorkItem(ctx, ttx2.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Container step",
		Status: domain.WorkItemReady,
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx2.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		RunContext: []byte("{}"), WorkflowID: wfID, WorkflowVersion: 1,
		WorkItemID: item.ID,
		Status:     domain.WorkflowRunPending,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	lc := &unavailableLifecycle{reason: "runtime daemon: mount source outside allowed roots: C:\\projects\\weatherapp"}
	r := &WorkflowReconciler{
		pool: pool, runtime: lc,
		log: slog.New(slog.DiscardHandler), warming: map[string]bool{},
	}

	// THE PASS MUST NOT RETURN AN ERROR. A container that cannot be built is a RUN failure — terminal and
	// recorded — not an infrastructure error for the reconciler to retry.
	if err := r.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun returned an error instead of failing the run: %v\n"+
			"An error here is RETRYABLE, which is the hot loop this suite exists to prevent.", err)
	}

	ttx3, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetWorkflowRun(ctx, ttx3.Tx, approvalTestTenant, run.ID)
	_ = ttx3.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// (a) TERMINATES. Not pending, not running — failed.
	if got.Status != domain.WorkflowRunFailed {
		t.Errorf("run status = %q, want failed — a container that cannot be created must terminate the run, "+
			"not leave it pending for ever", got.Status)
	}
	if got.EndedAt == nil {
		t.Error("ended_at is unset on a failed run — 'it failed' needs a when")
	}
	// (b) PROPAGATES THE REASON, to the WORK ITEM — the surface the operator reads. "failed" with no reason
	// is only marginally better than the hang it replaced.
	ttx4, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	wi, err := db.GetWorkItem(ctx, ttx4.Tx, approvalTestTenant, item.ID)
	_ = ttx4.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wi.Status != domain.WorkItemFailed {
		t.Errorf("work item status = %q, want failed", wi.Status)
	}
	carried := string(wi.Results) + string(wi.AcceptanceReview)
	if !strings.Contains(carried, "mount source outside allowed roots") {
		t.Errorf("the reason did not reach the work item. A failed run that does not say WHY is the black box "+
			"this coverage exists to prevent.\nresults=%s\nreview=%s", wi.Results, wi.AcceptanceReview)
	}

	// (c) DOES NOT HOT-LOOP. A second pass over a terminal run must be a no-op that does not re-attempt the
	// create. This is the assertion that distinguishes "failed" from "failed and keeps trying".
	before := lc.creates
	if err := r.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("the second pass errored: %v", err)
	}
	if lc.creates != before {
		t.Errorf("a container create was re-attempted on an already-failed run (%d -> %d) — a terminal run "+
			"must not be retried", before, lc.creates)
	}
}
