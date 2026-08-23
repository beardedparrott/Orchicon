package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// Sequence engine tests: the derived-cursor model
// (architecture-notes/sequential-multi-workflow-runs.md §2).
//
// The DB-backed tests exercise the engine against a real Postgres
// (ORCHICON_TEST_DSN), using a stub StartWorkflowFn that records the
// armed leaf instead of starting a real run — the sequence engine's
// contract is "arm the child and hand it to its own bound workflow", so
// the workflow machinery itself is not under test here.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon@127.0.0.1:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run TestSequence -v

// sequenceTestEnv is the fixture for the engine tests: a project plus a
// recorded stub start fn.
type sequenceTestEnv struct {
	pool   *db.Pool
	proj   db.ProjectRow
	starts []leafStart
	mu     sync.Mutex
}

func (e *sequenceTestEnv) startFn() StartWorkflowFn {
	return func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.starts = append(e.starts, leafStart{tenantID, workflowID, projectID, workItemID})
		return nil
	}
}

func (e *sequenceTestEnv) armedWorkItems() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []string
	for _, s := range e.starts {
		ids = append(ids, s.itemID)
	}
	return ids
}

func newSequenceTestEnv(t *testing.T) *sequenceTestEnv {
	t.Helper()
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "seq-env",
		Slug: "seq-env-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Each test tears down its own project so the shared tnt_dev DB never
	// accumulates seq-env residue. The blocked-scan tests are sensitive to
	// the total count of blocked/ready tasks in the tenant (the reconciler
	// scan processes a bounded window), so hermetic cleanup is required.
	t.Cleanup(func() { deleteTestProject(t, pool, proj.ID) })
	return &sequenceTestEnv{pool: pool, proj: proj}
}

// seedPublishedWorkflow creates a published workflow template so a leaf's
// workflow_id is real and runnable.
func seedPublishedWorkflow(t *testing.T, pool *db.Pool, projID string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projID,
		Name: "seq-wf-" + db.NewID()[:8], CurrentVersion: 0, Status: domain.WorkflowDraft,
		Type: domain.WorkflowTypeTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: domain.WorkflowVersionDraft, Steps: []byte("[]"),
		Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PublishWorkflowVersion(ctx, ttx.Tx, approvalTestTenant, wf.ID, 1); err != nil {
		t.Fatal(err)
	}
	// Mirror the workflow service's publish: bump the header's
	// current_version + status so StartWorkflowDirect accepts it.
	if _, err := db.UpdateWorkflowCurrentVersion(ctx, ttx.Tx, approvalTestTenant, wf.ID, wf.Version, 1); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return wf.ID
}

// createWorkItem creates a work item row within the tenant tx.
func createWorkItem(t *testing.T, pool *db.Pool, projID string, kind, title string, parent *string, workflowID *string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projID,
		ParentID: parent, Kind: kind, Title: title, Status: domain.WorkItemPending,
		WorkflowID: workflowID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

func mustGet(t *testing.T, pool *db.Pool, id string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	w, err := db.GetWorkItem(ctx, ttx.Tx, approvalTestTenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func setStatus(t *testing.T, pool *db.Pool, id, status string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	w := mustGet(t, pool, id)
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, id, w.Version, db.UpdateWorkItemFields{
		Status: &status,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// addDependency creates a DAG edge from→to within the test project.
func addDependency(t *testing.T, pool *db.Pool, projID, fromID, toID, depType string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	if _, err := db.CreateDependency(ctx, ttx.Tx, db.DependencyRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projID,
		FromID: fromID, ToID: toID, Type: depType,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDeriveNextChild(t *testing.T) {
	cases := []struct {
		name     string
		statuses []string
		want     int
	}{
		{"all succeeded → done", []string{"succeeded", "succeeded"}, -1},
		{"first pending → arm it", []string{"pending", "pending"}, 0},
		{"succeeded then pending", []string{"succeeded", "pending"}, 1},
		{"running in flight", []string{"succeeded", "running", "pending"}, 1},
		{"failed halts", []string{"succeeded", "failed", "pending"}, 1},
		{"cancelled halts", []string{"succeeded", "cancelled"}, 1},
		{"all skipped → done", []string{"skipped", "skipped"}, -1},
		{"succeeded then skipped → done", []string{"succeeded", "skipped"}, -1},
		{"skipped then pending", []string{"skipped", "pending"}, 1},
		{"skipped then running", []string{"skipped", "running", "pending"}, 1},
		{"empty", []string{}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var children []db.WorkItemRow
			for _, s := range tc.statuses {
				children = append(children, db.WorkItemRow{Status: s})
			}
			if got := deriveNextChild(children); got != tc.want {
				t.Fatalf("deriveNextChild(%v) = %d, want %d", tc.statuses, got, tc.want)
			}
		})
	}
}

// TestStartSequencePreservesSucceededChildren verifies the new contract
// (AC2): parent → running, every NON-terminal descendant reset to pending,
// succeeded/skipped children ALWAYS kept, and only the first NON-succeeded
// child in sort_order arms with its OWN workflow. Prior successes are never
// reset.
func TestStartSequencePreservesSucceededChildren(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf1 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	wf2 := seedPublishedWorkflow(t, env.pool, env.proj.ID)

	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Child One", &parent.ID, &wf1)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Child Two", &parent.ID, &wf2)

	// c1 succeeded from an earlier manual run; c2 is on-deck. START must
	// preserve c1 and arm only c2 — no destructive re-fire.
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("StartSequence: %v", err)
	}

	gotParent := mustGet(t, env.pool, parent.ID)
	if gotParent.Status != domain.WorkItemRunning {
		t.Errorf("parent status = %q, want running", gotParent.Status)
	}
	gotC1 := mustGet(t, env.pool, c1.ID)
	if gotC1.Status != domain.WorkItemSucceeded {
		t.Errorf("child one status = %q, want succeeded (preserved, never re-armed)", gotC1.Status)
	}
	gotC2 := mustGet(t, env.pool, c2.ID)
	if gotC2.Status != domain.WorkItemRunning {
		t.Errorf("child two status = %q, want running (armed)", gotC2.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 1 || armed[0] != c2.ID {
		t.Errorf("armed work items = %v, want exactly [%s]", armed, c2.ID)
	}
	// The armed child ran with its OWN workflow (no config copy).
	env.mu.Lock()
	started := append([]leafStart(nil), env.starts...)
	env.mu.Unlock()
	if len(started) != 1 || started[0].workflowID != wf2 {
		t.Errorf("started workflow = %+v, want child two's own workflow %s", started, wf2)
	}
}

// TestStartSequenceAllTerminalSuccessCompletes: START on a chain whose
// children are ALL terminal-success (succeeded/skipped) completes the
// sequence — the parent transitions to succeeded (nothing armed, nothing
// reset) rather than re-firing completed work.
func TestStartSequenceAllTerminalSuccessCompletes(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	setStatus(t, env.pool, c2.ID, domain.WorkItemSkipped)

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("StartSequence: %v", err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemSucceeded {
		t.Errorf("parent status = %q, want succeeded (every child terminal-success)", got.Status)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemSucceeded {
		t.Errorf("c1 status = %q, want succeeded (preserved)", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemSkipped {
		t.Errorf("c2 status = %q, want skipped (preserved)", got.Status)
	}
	if armed := env.armedWorkItems(); len(armed) != 0 {
		t.Errorf("armed work items = %v, want none", armed)
	}
}

// TestStopSequencePreservesSucceededSibling (AC1, ADR #3): STOP parks a
// non-terminal sibling to pending but leaves a succeeded sibling untouched —
// a succeeded child is never re-armed by STOP.
func TestStopSequencePreservesSucceededSibling(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Succeeded", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Running", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	// Parent running; c1 terminal-success, c2 in flight.
	setField(t, env.pool, parent.ID, func(f *db.UpdateWorkItemFields) {
		s := domain.WorkItemRunning
		f.Status = &s
	})
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	setField(t, env.pool, c2.ID, func(f *db.UpdateWorkItemFields) {
		s := domain.WorkItemRunning
		f.Status = &s
	})

	if err := StopSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("StopSequence: %v", err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemSucceeded {
		t.Errorf("succeeded child status = %q, want succeeded (preserved)", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemPending {
		t.Errorf("running child status = %q, want pending (parked)", got.Status)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemPending {
		t.Errorf("parent status = %q, want pending (parked)", got.Status)
	}
}

// TestOneOffSucceededChildNeverReKicked (AC3, ADR #4): a one-off
// manually-fired succeeded child is never re-kicked by a subsequent START
// or STOP of its parent — it stays succeeded through both.
func TestOneOffSucceededChildNeverReKicked(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	// c1 is a one-off manually-fired success; c2 is on-deck pending.
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)

	// START must preserve c1 and arm c2 (never re-kick c1).
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("StartSequence: %v", err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemSucceeded {
		t.Fatalf("c1 after start = %q, want succeeded (preserved, never re-kicked)", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("c2 after start = %q, want running (armed)", got.Status)
	}

	// STOP must also preserve c1 and park c2.
	if err := StopSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("StopSequence: %v", err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemSucceeded {
		t.Errorf("c1 after stop = %q, want succeeded (never re-kicked)", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemPending {
		t.Errorf("c2 after stop = %q, want pending (parked)", got.Status)
	}
}

// TestSequenceRecurringParentKeepsCadenceWithSucceededChild (AC5, ADR #5):
// a recurring sequence parent whose cycle contains a succeeded child keeps
// its cadence — a refire preserves the succeeded child, arms the next
// non-succeeded child, and the completing cycle returns the parent to
// "recurring" (not "succeeded") with next_run_at intact.
func TestSequenceRecurringParentKeepsCadenceWithSucceededChild(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createRecurringParent(t, env.pool, env.proj.ID, "Recurring Parent")
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	// c1 succeeded in an earlier cycle; c2 is on-deck pending.
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)

	// A refire of the recurring cycle must PRESERVE the succeeded c1 and
	// arm only c2 — the new START semantics.
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("StartSequence: %v", err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemSucceeded {
		t.Fatalf("c1 status after start = %q, want succeeded (preserved)", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("c2 status after start = %q, want running (armed)", got.Status)
	}

	// c2 succeeds → the cycle completes: the recurring parent returns to
	// "recurring" (not "succeeded"), cadence preserved for the next fire.
	setStatus(t, env.pool, c2.ID, domain.WorkItemSucceeded)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, env.pool, parent.ID)
	if got.Status != domain.WorkItemRecurring {
		t.Errorf("parent status = %q, want recurring (cadence kept with a succeeded child in cycle)", got.Status)
	}
	if len(got.RecurringSchedule) == 0 {
		t.Error("parent recurring_schedule was cleared, want intact")
	}
	if got.NextRunAt == nil {
		t.Error("parent next_run_at is nil, want preserved for the next cycle")
	}
}

// TestSequenceAdvanceOnSuccess: a child reaching succeeded advances the
// chain to the next sibling, whose own workflow starts.
func TestSequenceAdvanceOnSuccess(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf1 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	wf2 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf1)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf2)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("StartSequence: %v", err)
	}
	// Child one completes (as the WorkflowReconciler would mark it).
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)

	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	gotC2 := mustGet(t, env.pool, c2.ID)
	if gotC2.Status != domain.WorkItemRunning {
		t.Errorf("child two status = %q, want running after advance", gotC2.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 2 || armed[1] != c2.ID {
		t.Errorf("armed work items = %v, want [c1 c2]", armed)
	}
	// Parent still running until every child succeeds.
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status = %q, want running", got.Status)
	}
}

// TestSequenceCompletion: every child succeeded → parent succeeded.
func TestSequenceCompletion(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	setStatus(t, env.pool, c2.ID, domain.WorkItemSucceeded)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemSucceeded {
		t.Errorf("parent status = %q, want succeeded", got.Status)
	}
}

// TestSequenceRecurringCompletionReturnsToRecurring: when every child of a
// recurring sequence parent succeeds, the parent returns to "recurring"
// (not "succeeded") so the RecurringFireReconciler fires the next cycle.
func TestSequenceRecurringCompletionReturnsToRecurring(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createRecurringParent(t, env.pool, env.proj.ID, "Recurring Parent")
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	// The last child succeeds → the sequence completes.
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	setStatus(t, env.pool, c2.ID, domain.WorkItemSucceeded)

	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, env.pool, parent.ID)
	if got.Status != domain.WorkItemRecurring {
		t.Errorf("parent status = %q, want recurring (a recurring parent must not end succeeded)", got.Status)
	}
	if len(got.RecurringSchedule) == 0 {
		t.Error("parent recurring_schedule was cleared, want intact")
	}
	if got.NextRunAt == nil {
		t.Error("parent next_run_at is nil, want preserved for the next cycle")
	}
}

// TestSequenceRecurringFailureKeepsSchedule: a failed child in a recurring
// sequence parent's cycle halts the CURRENT chain (later siblings never
// arm) but does NOT kill the schedule — the parent stays "recurring" with
// next_run_at intact, so the next occurrence re-fires the chain fresh.
func TestSequenceRecurringFailureKeepsSchedule(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createRecurringParent(t, env.pool, env.proj.ID, "Recurring Parent")
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	setStatus(t, env.pool, c1.ID, domain.WorkItemFailed)

	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, env.pool, parent.ID)
	if got.Status != domain.WorkItemRecurring {
		t.Errorf("parent status = %q, want recurring (a failed cycle must not kill the recurring schedule)", got.Status)
	}
	if len(got.RecurringSchedule) == 0 {
		t.Error("parent recurring_schedule was cleared, want intact")
	}
	if got.NextRunAt == nil {
		t.Error("parent next_run_at is nil, want preserved for the next cycle")
	}
	// The failed cycle halts: the later sibling never arms.
	if gotC2 := mustGet(t, env.pool, c2.ID); gotC2.Status != domain.WorkItemPending {
		t.Errorf("later sibling status = %q, want pending (never arms in the failed cycle)", gotC2.Status)
	}

	// The NEXT occurrence still fires: make the still-recurring parent due
	// and run the RecurringFireReconciler — it must re-fire the sequence.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(-2 * time.Minute)
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, parent.ID, got.Version,
		db.UpdateWorkItemFields{NextRunAt: &due}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rec2 := &recurringFireRecorder{}
	fireRec := NewRecurringFireReconciler(env.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec2.startFn())
	fireRec.SetSequenceStarter(rec2.sequenceFn())
	if res := fireRec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("fire reconciler (next occurrence): %v", res.Error)
	}
	if rec2.sequenceCalls != 1 {
		t.Errorf("sequence calls = %d, want 1 (next occurrence must re-fire the sequence)", rec2.sequenceCalls)
	}
}

// TestSequenceFailureHalts: a failed child halts the chain — later
// siblings stay pending, the parent goes failed.
func TestSequenceFailureHalts(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	setStatus(t, env.pool, c1.ID, domain.WorkItemFailed)

	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemFailed {
		t.Errorf("parent status = %q, want failed", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemPending {
		t.Errorf("later sibling status = %q, want pending (never arms)", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 1 {
		t.Errorf("armed work items = %v, want only [c1]", got)
	}
}

// TestRetryResume: fixing + retrying the failed child to success resumes
// the chain automatically — the next sibling arms, no manual resume.
func TestRetryResume(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	setStatus(t, env.pool, c1.ID, domain.WorkItemFailed)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	_ = rec.reconcileOne(ctx, approvalTestTenant, parent.ID)
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemFailed {
		t.Fatalf("precondition: parent should be failed, got %q", got.Status)
	}
	// Fix + retry the failed child to success (existing retry/recovery).
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("next sibling status = %q, want running (chain resumed)", got.Status)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status = %q, want running again", got.Status)
	}
}

// TestSequenceDependencyPark: an on-deck child with an unsatisfied
// external blocker parks the chain (parent running, child BLOCKED, no
// arm) — the stall is surfaced, not a silent gray pending. When the
// blocker succeeds the chain advances automatically.
func TestSequenceDependencyPark(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)

	// blocker BLOCKS c1.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateDependency(ctx, ttx.Tx, db.DependencyRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: env.proj.ID,
		FromID: blocker.ID, ToID: c1.ID, Type: domain.DependencyBlocks,
	}); err != nil {
		t.Fatal(err)
	}
	ttx.Commit(ctx)

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	// Blocked: parent running, child BLOCKED (surfaced stall), NOT armed.
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status = %q, want running (parked)", got.Status)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("child status = %q, want %q (parked, surfaced)", got.Status, domain.WorkItemBlocked)
	}
	if got := env.armedWorkItems(); len(got) != 0 {
		t.Errorf("armed work items = %v, want none while parked", got)
	}
	// Blocker succeeds → the blocked child clears to pending and arms in
	// the same pass — chain advances without human action.
	setStatus(t, env.pool, blocker.ID, domain.WorkItemSucceeded)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("child status = %q, want running after blocker success", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 1 || got[0] != c1.ID {
		t.Errorf("armed work items = %v, want [c1]", got)
	}
}

// TestSequenceBlockedStaysBlockedWhileBlockerNonTerminal: a dependency-
// gated child whose blocker is still pending/failed stays BLOCKED (never
// armed, never silently parked). Only a terminal-success blocker clears it.
func TestSequenceBlockedStaysBlockedWhileBlockerNonTerminal(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, c1.ID, domain.DependencyBlocks)

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	// A FAILED blocker still blocks (server gate accepts only succeeded):
	// the dependent stays blocked, named and visible for the operator.
	setStatus(t, env.pool, blocker.ID, domain.WorkItemFailed)
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("child status = %q, want %q (failed blocker still blocks)", got.Status, domain.WorkItemBlocked)
	}
	if got := env.armedWorkItems(); len(got) != 0 {
		t.Errorf("armed work items = %v, want none", got)
	}
	// Blocker succeeded → clears + arms.
	setStatus(t, env.pool, blocker.ID, domain.WorkItemSucceeded)
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("child status = %q, want running after blocker success", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 1 || got[0] != c1.ID {
		t.Errorf("armed work items = %v, want [c1]", got)
	}
}

// TestSequenceCancelledBlockerKeepsDependentBlocked: a cancelled blocker
// keeps the dependent blocked (terminal-failure, exactly like failed) —
// recoverable only via the existing recovery flow once the blocker is
// resolved to a terminal-success state.
func TestSequenceCancelledBlockerKeepsDependentBlocked(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, c1.ID, domain.DependencyDependsOn)

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	// A CANCELLED blocker (terminal-failure) still blocks: the dependent
	// stays surfaced-blocked, never armed.
	setStatus(t, env.pool, blocker.ID, domain.WorkItemCancelled)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("child status = %q, want %q (cancelled blocker still blocks)", got.Status, domain.WorkItemBlocked)
	}
	if got := env.armedWorkItems(); len(got) != 0 {
		t.Errorf("armed work items = %v, want none", got)
	}
}

// TestSequenceSkippedBlockerSatisfiesDependencyGate: a skipped external
// blocker satisfies the dependency gate (AC1) — the dependent clears
// blocked and arms in the same pass, exactly as if the blocker had
// succeeded. A skipped dependency never blocks its dependents.
func TestSequenceSkippedBlockerSatisfiesDependencyGate(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, c1.ID, domain.DependencyDependsOn)

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemBlocked {
		t.Fatalf("C1 status = %q, want %q (parked on blocker, surfaced)", got.Status, domain.WorkItemBlocked)
	}
	// Blocker skipped → the dependency gate satisfies; C1 clears and arms.
	setStatus(t, env.pool, blocker.ID, domain.WorkItemSkipped)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("C1 status = %q, want running after blocker skipped", got.Status)
	}
	if armed := env.armedWorkItems(); len(armed) != 1 || armed[0] != c1.ID {
		t.Errorf("armed work items = %v, want [c1]", armed)
	}
}

// TestSequenceAdvanceOnSkippedPredecessor: a skipped in-order predecessor
// releases the next strict child — the chain gate treats skipped as
// terminal-success exactly like succeeded.
func TestSequenceAdvanceOnSkippedPredecessor(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	// C1 is skipped (as the WorkflowReconciler would mark a bound child
	// whose run completed with a skipped step). The strict chain gate must
	// treat it as terminal-success: C2's position is reached.
	setStatus(t, env.pool, c1.ID, domain.WorkItemSkipped)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("C2 status = %q, want running after predecessor skipped", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 2 || got[1] != c2.ID {
		t.Errorf("armed work items = %v, want [c1 c2]", got)
	}
}

// TestSequenceCompletionWithSkippedChild: a sequence whose children are all
// succeeded or skipped is complete — the parent transitions to succeeded
// and a skipped child never wedges the chain on itself.
func TestSequenceCompletionWithSkippedChild(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	setStatus(t, env.pool, c2.ID, domain.WorkItemSkipped)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemSucceeded {
		t.Errorf("parent status = %q, want succeeded (every child terminal-success)", got.Status)
	}
}

// TestSequenceParallelDispatchOnSharedDependency: two dependency-governed
// siblings gated by the same external blocker arm CONCURRENTLY in a single
// pass once the blocker succeeds — the core parallelism this fix enables.
func TestSequenceParallelDispatchOnSharedDependency(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	a := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "A", &parent.ID, &wf)
	b := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "B", &parent.ID, &wf)
	x := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "X", nil, &wf)
	addDependency(t, env.pool, env.proj.ID, x.ID, a.ID, domain.DependencyBlocks)
	addDependency(t, env.pool, env.proj.ID, x.ID, b.ID, domain.DependencyBlocks)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{a.ID, b.ID})

	// Both children are dependency-governed by X; X is not done yet, so
	// the chain parks with nothing armed and both children surfaced as
	// blocked.
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("parent status = %q, want running (parked on X)", got.Status)
	}
	if got := mustGet(t, env.pool, a.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("A status = %q, want %q (parked on X, surfaced)", got.Status, domain.WorkItemBlocked)
	}
	if got := mustGet(t, env.pool, b.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("B status = %q, want %q (parked on X, surfaced)", got.Status, domain.WorkItemBlocked)
	}
	if got := env.armedWorkItems(); len(got) != 0 {
		t.Fatalf("armed work items = %v, want none while parked", got)
	}

	// X succeeds → A and B, unrelated dependency-governed siblings, arm
	// CONCURRENTLY in a single pass.
	setStatus(t, env.pool, x.ID, domain.WorkItemSucceeded)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, a.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("A status = %q, want running (parallel dispatch)", got.Status)
	}
	if got := mustGet(t, env.pool, b.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("B status = %q, want running (parallel dispatch)", got.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 2 || armed[0] != a.ID || armed[1] != b.ID {
		t.Errorf("armed work items = %v, want [A B]", armed)
	}
}

// TestSequenceStrictChainSinglePassArmsOnlyFirst: over [P, P] a single
// reconcileParent pass arms only the first child — the just-armed
// predecessor is non-terminal, so the next strict child's chain gate
// blocks. Full serialization is preserved by construction.
func TestSequenceStrictChainSinglePassArmsOnlyFirst(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	// StartSequence is a single reconcileParent pass over [pending, pending]:
	// only C1 may arm — C2's chain position is behind non-terminal C1.
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("C1 status = %q, want running (chain head armed)", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemPending {
		t.Fatalf("C2 status = %q, want pending (strict chain serialized)", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 1 || got[0] != c1.ID {
		t.Fatalf("armed work items = %v, want [C1]", got)
	}
}

// TestSequenceChainPositionNotReached: over [running, pending] strict
// children the pending sibling's chain position is not reached while its
// predecessor runs; it arms only after the predecessor succeeds.
func TestSequenceChainPositionNotReached(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}

	// [running, pending] strict children: the pending sibling's position is
	// not reached while its predecessor runs.
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemPending {
		t.Fatalf("C2 status = %q, want pending (predecessor still running)", got.Status)
	}

	// Predecessor succeeds → C2's position is reached and it arms.
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("C2 status = %q, want running after predecessor succeeded", got.Status)
	}
}

// TestSequenceDependencyGovernedChildIgnoresChainPosition: a
// dependency-governed child's ordering is its edges, not its chain
// position — it arms alongside a still-running strict predecessor once its
// dependency succeeds.
func TestSequenceDependencyGovernedChildIgnoresChainPosition(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	a := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "A", &parent.ID, &wf)
	b := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "B", &parent.ID, &wf)
	x := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "X", nil, &wf)
	addDependency(t, env.pool, env.proj.ID, x.ID, b.ID, domain.DependencyDependsOn)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{a.ID, b.ID})

	// A (strict, chain head) arms; B is dependency-governed and parked on X.
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, a.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("A status = %q, want running", got.Status)
	}
	if got := mustGet(t, env.pool, b.ID); got.Status != domain.WorkItemBlocked {
		t.Fatalf("B status = %q, want %q (parked on X, surfaced)", got.Status, domain.WorkItemBlocked)
	}

	// X succeeds while A is still running: B is dependency-governed, so its
	// ordering is its edges, not its chain position — B arms alongside A.
	setStatus(t, env.pool, x.ID, domain.WorkItemSucceeded)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, a.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("A status = %q, want running (untouched)", got.Status)
	}
	if got := mustGet(t, env.pool, b.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("B status = %q, want running (dependency-governed ignores chain position)", got.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 2 || armed[1] != b.ID {
		t.Errorf("armed work items = %v, want [A B]", armed)
	}
}

// TestSequenceFailureHaltsAllIncludingDependencyGoverned: a failed child
// ANYWHERE halts the whole chain — a dependency-governed sibling whose
// edges are otherwise satisfied must NOT arm in a pass containing a
// failure, and stays parked even after its blocker succeeds.
func TestSequenceFailureHaltsAllIncludingDependencyGoverned(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	a := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "A", &parent.ID, &wf)
	b := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "B", &parent.ID, &wf)
	x := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "X", nil, &wf)
	addDependency(t, env.pool, env.proj.ID, x.ID, a.ID, domain.DependencyBlocks)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{a.ID, b.ID})

	// A is dependency-governed (parked on X); B is a strict chain child.
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	// B fails → the WHOLE chain halts: the parent goes failed and nothing
	// arms — not even A, whose dependency would otherwise be satisfied next.
	setStatus(t, env.pool, b.ID, domain.WorkItemFailed)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemFailed {
		t.Errorf("parent status = %q, want failed", got.Status)
	}
	if got := mustGet(t, env.pool, a.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("A status = %q, want %q (parked on X, surfaced; nothing arms in a pass containing a failure)", got.Status, domain.WorkItemBlocked)
	}

	// Even after X succeeds, the unfixed failure still halts the chain.
	setStatus(t, env.pool, x.ID, domain.WorkItemSucceeded)
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, a.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("A status = %q, want %q (failure halts all regardless of deps)", got.Status, domain.WorkItemBlocked)
	}
}

// TestSequenceStrictChildBehindDepParkedSiblingStaysParked: the chain gate
// sees a dep-parked predecessor as non-terminal, so a strict child behind
// it stays parked even though the child itself has no blockers — it arms
// only once the parked predecessor reaches success.
func TestSequenceStrictChildBehindDepParkedSiblingStaysParked(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	a := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "A", &parent.ID, &wf)
	b := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "B", &parent.ID, &wf)
	x := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "X", nil, &wf)
	addDependency(t, env.pool, env.proj.ID, x.ID, a.ID, domain.DependencyBlocks)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{a.ID, b.ID})

	// A is dependency-governed and parked on X; B is strict and sits behind
	// A. B's chain position is not reached while A is non-terminal, so B
	// stays parked even though B itself has no blockers.
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, b.ID); got.Status != domain.WorkItemPending {
		t.Fatalf("B status = %q, want pending (behind dep-parked A)", got.Status)
	}

	// X succeeds → A arms, but B still stays parked (A is running now).
	setStatus(t, env.pool, x.ID, domain.WorkItemSucceeded)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, a.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("A status = %q, want running after X succeeded", got.Status)
	}
	if got := mustGet(t, env.pool, b.ID); got.Status != domain.WorkItemPending {
		t.Fatalf("B status = %q, want pending (predecessor A not yet succeeded)", got.Status)
	}

	// A succeeds → B's position is reached and it arms.
	setStatus(t, env.pool, a.ID, domain.WorkItemSucceeded)
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, b.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("B status = %q, want running after predecessor A succeeded", got.Status)
	}
}

// TestSequenceNestedDepthFirst: epic → feature → tasks runs depth-first;
// a leaf failure fails the whole ancestor chain.
func TestSequenceNestedDepthFirst(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	epic := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Epic", nil, nil)
	feature := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindFeature, "Feature", &epic.ID, nil)
	t1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "T1", &feature.ID, &wf)
	t2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "T2", &feature.ID, &wf)
	reorder(t, env.pool, env.proj.ID, epic.ID, []string{feature.ID})
	reorder(t, env.pool, env.proj.ID, feature.ID, []string{t1.ID, t2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, epic.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	// Firing the epic arms the feature (container) as a nested sequence.
	if got := mustGet(t, env.pool, feature.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("feature status = %q, want running (nested sequence armed)", got.Status)
	}
	// The feature's own chain arms its first task.
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, feature.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, t1.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("t1 status = %q, want running", got.Status)
	}
	if got := mustGet(t, env.pool, t2.ID); got.Status != domain.WorkItemPending {
		t.Errorf("t2 status = %q, want pending", got.Status)
	}
	// Depth-first: t1 succeeds, then t2 arms.
	setStatus(t, env.pool, t1.ID, domain.WorkItemSucceeded)
	_ = rec.reconcileOne(ctx, approvalTestTenant, feature.ID)
	if got := mustGet(t, env.pool, t2.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("t2 status = %q, want running after t1 succeeded", got.Status)
	}
	// t2 fails → feature failed → epic failed (propagation up the chain).
	setStatus(t, env.pool, t2.ID, domain.WorkItemFailed)
	_ = rec.reconcileOne(ctx, approvalTestTenant, feature.ID)
	if got := mustGet(t, env.pool, feature.ID); got.Status != domain.WorkItemFailed {
		t.Errorf("feature status = %q, want failed", got.Status)
	}
	if got := mustGet(t, env.pool, epic.ID); got.Status != domain.WorkItemFailed {
		t.Errorf("epic status = %q, want failed (ancestor propagated)", got.Status)
	}
}

// TestStartSequenceStartsRealRun verifies the engine hands the armed leaf
// to the REAL workflow service: a workflow run is created and the child's
// workflow_run_id is stamped (no config copy — the child's own bound
// workflow is used).
func TestStartSequenceStartsRealRun(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf1 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	wf2 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf1)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf2)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	realStart := func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		return workflow.StartWorkflowDirect(ctx, env.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), tenantID, workflowID, projectID, workItemID)
	}
	if err := StartSequence(ctx, env.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), approvalTestTenant, parent.ID, realStart); err != nil {
		t.Fatalf("StartSequence: %v", err)
	}
	gotC1 := mustGet(t, env.pool, c1.ID)
	if gotC1.WorkflowRunID == "" {
		t.Fatal("child one has no workflow_run_id — the real workflow was not started")
	}
	// The run must belong to the child's OWN workflow.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, gotC1.WorkflowRunID)
	ttx.Rollback(ctx)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.WorkflowID != wf1 {
		t.Errorf("run workflow = %s, want child one's own %s", run.WorkflowID, wf1)
	}
	// Child two is untouched (still pending, no run).
	gotC2 := mustGet(t, env.pool, c2.ID)
	if gotC2.WorkflowRunID != "" {
		t.Errorf("child two should not have a run yet, got %s", gotC2.WorkflowRunID)
	}
}

// setField applies a partial update via a field-mask callback (test
// helper mirroring setStatus for non-status fields).
func setField(t *testing.T, pool *db.Pool, id string, apply func(f *db.UpdateWorkItemFields)) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	w := mustGet(t, pool, id)
	f := db.UpdateWorkItemFields{}
	apply(&f)
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, id, w.Version, f); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestStopSequenceCascadesAndAborts is the regression test for the
// "stop didn't stop" bug. StopSequence previously parked ONLY the node it
// was called on: a sequence parent's descendant containers stayed
// running/failed and kept arming children (and a FAILED parent auto-revived
// on the next scan), so stopping an epic left the whole chain running.
//
// The fix: STOP is recursive. Stopping a parent parks the ENTIRE subtree —
// every descendant → pending, scheduled starts cleared, stale run bindings
// cleared, and any in-flight workflow run bound to a descendant is ABORTED
// (run → aborted, step runs → failed, linked executions → terminated).
// This test seeds a parent with a running child (real workflow run) and a
// running grandchild, stops the parent, and asserts:
//   - the child's bound run is aborted and its work item parked to pending,
//   - the grandchild is parked to pending,
//   - no member of the subtree is running or failed (so the scan and the
//     auto-revive path have nothing to pick up).
func TestStopSequenceCascadesAndAborts(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)

	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	child := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Child", &parent.ID, &wf)
	grandchild := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Grandchild", &child.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{child.ID})
	reorder(t, env.pool, env.proj.ID, child.ID, []string{grandchild.ID})

	// Fire the child's REAL workflow so it has an in-flight bound run, then
	// give the grandchild a live run to prove recursion aborts deeper nodes.
	realStart := func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		return workflow.StartWorkflowDirect(ctx, env.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), tenantID, workflowID, projectID, workItemID)
	}
	if err := StartSequence(ctx, env.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), approvalTestTenant, parent.ID, realStart); err != nil {
		t.Fatalf("StartSequence: %v", err)
	}
	setField(t, env.pool, grandchild.ID, func(f *db.UpdateWorkItemFields) {
		runID := db.NewID()
		f.WorkflowRunID = &runID
		status := domain.WorkItemRunning
		f.Status = &status
	})

	// Stop the PARENT — must cascade to the whole subtree and abort runs.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := StopSequence(ctx, env.pool, logger, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("StopSequence: %v", err)
	}

	for name, id := range map[string]string{
		"parent":     parent.ID,
		"child":      child.ID,
		"grandchild": grandchild.ID,
	} {
		got := mustGet(t, env.pool, id)
		if got.Status == domain.WorkItemRunning || got.Status == domain.WorkItemFailed {
			t.Errorf("%s status = %s after stop, want parked (not running/failed)", name, got.Status)
		}
		if got.WorkflowRunID != "" {
			t.Errorf("%s still has workflow_run_id %s after stop", name, got.WorkflowRunID)
		}
	}

	// The child's bound run must be ABORTED.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	childRun, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, child.WorkflowRunID)
	ttx.Rollback(ctx)
	if err != nil {
		t.Fatalf("get child run after stop: %v", err)
	}
	if childRun.Status != domain.WorkflowRunAborted {
		t.Errorf("child run status = %s after stop, want aborted", childRun.Status)
	}

	// A stopped chain must NOT auto-revive on a reconcile pass: the scan
	// only picks up running/failed parents, and the auto-revive path only
	// resurrects a FAILED parent. Assert a fresh reconcile pass does nothing.
	rec := NewSequenceReconciler(env.pool, logger, env.startFn())
	if res := rec.Reconcile(ctx, parent.ID); res.Error != nil {
		t.Fatalf("reconcile after stop: %v", res.Error)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemPending {
		t.Errorf("parent status after post-stop reconcile = %s, want pending (must not auto-revive)", got.Status)
	}
	if armed := env.armedWorkItems(); len(armed) != 0 {
		t.Errorf("stop should not arm anything; got %v", armed)
	}
}

// TestStopLeafIsIndividual verifies STOP on a leaf (no children) parks
// only that item and aborts its run — it must NOT touch the parent.
func TestStopLeafIsIndividual(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)

	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	leaf := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Leaf", &parent.ID, &wf)

	realStart := func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		return workflow.StartWorkflowDirect(ctx, env.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), tenantID, workflowID, projectID, workItemID)
	}
	if err := realStart(ctx, approvalTestTenant, wf, env.proj.ID, leaf.ID); err != nil {
		t.Fatalf("start leaf: %v", err)
	}
	if got := mustGet(t, env.pool, leaf.ID); got.WorkflowRunID == "" {
		t.Fatal("leaf has no run after start")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := StopSequence(ctx, env.pool, logger, approvalTestTenant, leaf.ID); err != nil {
		t.Fatalf("StopSequence leaf: %v", err)
	}
	// Leaf parked; parent untouched (stays pending, no run binding).
	if got := mustGet(t, env.pool, leaf.ID); got.Status != domain.WorkItemPending || got.WorkflowRunID != "" {
		t.Errorf("leaf after stop = status %s run %q, want pending + no run", got.Status, got.WorkflowRunID)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemPending {
		t.Errorf("parent status after leaf stop = %s, want pending (leaf stop is individual)", got.Status)
	}
}

// TestReconcileParentNoOpForNonSequenceParents is the regression test for
// the notifier-path guard: reconcileParent must only advance a work item
// that IS a sequence run (status running/failed, no bound workflow run,
// has children). The WorkflowReconciler notifier fires for the parent of
// ANY terminal bound work item — a parent that is a bound-run ticket, a
// never-fired pending parent, or a terminal parent must be a no-op. Before
// the guard, a never-scheduled parent's pending child flipped to running
// and its workflow started, and a bound-run parent with all children
// succeeded was spuriously marked succeeded.
func TestReconcileParentNoOpForNonSequenceParents(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	_ = wf

	t.Run("never-fired pending parent is a no-op", func(t *testing.T) {
		parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Pending Parent", nil, nil)
		c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
		reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID})

		rec := NewSequenceReconciler(env.pool, nil, env.startFn())
		if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
			t.Fatalf("reconcileOne: %v", err)
		}
		// Nothing armed, child untouched, parent untouched.
		if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemPending {
			t.Errorf("child status = %q, want pending (never-scheduled parent must not arm)", got.Status)
		}
		if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemPending {
			t.Errorf("parent status = %q, want pending", got.Status)
		}
		if got := env.armedWorkItems(); len(got) != 0 {
			t.Errorf("armed work items = %v, want none", got)
		}
	})

	t.Run("bound-run parent with all children succeeded is not marked succeeded", func(t *testing.T) {
		// A normal bound-run ticket that happens to have children.
		parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Bound Parent", nil, nil)
		c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
		reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID})

		// Stamp a bound run on the parent (it is a bound ticket, not a
		// sequence parent) and mark every child succeeded.
		setField(t, env.pool, parent.ID, func(f *db.UpdateWorkItemFields) {
			rid := "run-" + db.NewID()
			f.WorkflowRunID = &rid
			s := domain.WorkItemRunning
			f.Status = &s
		})
		setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)

		rec := NewSequenceReconciler(env.pool, nil, env.startFn())
		if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
			t.Fatalf("reconcileOne: %v", err)
		}
		// The bound ticket must NOT be flipped to succeeded by the
		// sequence engine, and its children must not be force-armed.
		if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
			t.Errorf("bound-run parent status = %q, want running (sequence engine must not touch it)", got.Status)
		}
		if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemSucceeded {
			t.Errorf("child status = %q, want succeeded (untouched)", got.Status)
		}
		if got := env.armedWorkItems(); len(got) != 0 {
			t.Errorf("armed work items = %v, want none", got)
		}
	})

	t.Run("terminal succeeded parent is a no-op", func(t *testing.T) {
		parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Done Parent", nil, nil)
		c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
		reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID})
		setStatus(t, env.pool, parent.ID, domain.WorkItemSucceeded)

		rec := NewSequenceReconciler(env.pool, nil, env.startFn())
		if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
			t.Fatalf("reconcileOne: %v", err)
		}
		if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemPending {
			t.Errorf("child status = %q, want pending (succeeded parent must not arm)", got.Status)
		}
		if got := env.armedWorkItems(); len(got) != 0 {
			t.Errorf("armed work items = %v, want none", got)
		}
	})
}

// TestStartSequenceFailedStartResetsChild: a leaf whose workflow start
// fails must not be stranded running-with-no-run — it resets to pending
// so the next pass re-arms.
func TestStartSequenceFailedStartResetsChild(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID})

	failingStart := func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		return errors.New("workflow gone")
	}
	_ = StartSequence(ctx, env.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), approvalTestTenant, parent.ID, failingStart)
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemPending {
		t.Errorf("child status after failed start = %q, want pending (re-armable)", got.Status)
	}
	// Parent stays running — the chain is parked, not failed.
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status = %q, want running", got.Status)
	}
}

// TestMidRunReorderArmsNewChainHeadNotBehindInFlightPredecessor is the
// reordered-chain regression test under the parallel-arm semantics. A
// ReorderWorkItems call while a child is running drags a pending sibling
// to the front: the new chain head (no in-flight predecessor) IS armable
// even while a later sibling is in flight — arming is gated per-child by
// the child's OWN chain position and dependency edges, not by every
// sibling. The chain-position protection still applies to a strict child
// sitting BEHIND the in-flight child: it stays parked until its immediate
// predecessor is succeeded.
func TestMidRunReorderArmsNewChainHeadNotBehindInFlightPredecessor(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf1 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	wf2 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	wf3 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "One", &parent.ID, &wf1)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Two", &parent.ID, &wf2)
	c3 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Three", &parent.ID, &wf3)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID, c3.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	// Child one is armed and in flight; two and three are pending.
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("precondition: child one should be running, got %q", got.Status)
	}

	// Mid-run drag: move Three to the front (QA repro: [Three, One, Two]).
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c3.ID, c1.ID, c2.ID})

	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	// Three is the new chain head — its position IS reached, so it arms
	// even while One (now a later sibling) is in flight.
	if got := mustGet(t, env.pool, c3.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("child three status = %q, want running (new chain head arms)", got.Status)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("child one status = %q, want running (untouched)", got.Status)
	}
	// Two sits BEHIND in-flight One — its chain position is not reached,
	// so it must NOT arm.
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemPending {
		t.Fatalf("child two status = %q, want pending (behind in-flight One)", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 2 || got[1] != c3.ID {
		t.Fatalf("armed work items = %v, want [c1 c3]", got)
	}

	// One reaches a terminal success → the chain advances: Three stays
	// running, and Two (now behind succeeded One) arms.
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c3.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("child three status = %q, want running (already in flight)", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("child two status = %q, want running after One succeeded", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 3 || got[2] != c2.ID {
		t.Errorf("armed work items = %v, want [c1 c3 c2]", got)
	}
}

// TestResumeSequenceParksThenResumes: a chain parked by StopSequence
// (parent → pending) is picked up by ResumeSequence and continues from the
// first non-succeeded child, keeping prior successes.
func TestResumeSequenceParksThenResumes(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf1 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	wf2 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf1)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf2)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	// Run the chain to c1 succeeded, c2 pending, parent running.
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)

	// Park the chain: parent → pending, schedule cleared.
	setField(t, env.pool, parent.ID, func(f *db.UpdateWorkItemFields) {
		status := domain.WorkItemPending
		f.Status = &status
		scheduled := time.Now().Add(time.Hour)
		f.ScheduledStartAt = &scheduled
	})
	if err := StopSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("StopSequence: %v", err)
	}
	gotParent := mustGet(t, env.pool, parent.ID)
	if gotParent.Status != domain.WorkItemPending {
		t.Fatalf("parent status after stop = %q, want pending", gotParent.Status)
	}
	if gotParent.ScheduledStartAt != nil {
		t.Errorf("parent scheduled_start_at after stop = %v, want cleared", gotParent.ScheduledStartAt)
	}

	// Resume: parent → running, c2 (first non-succeeded) arms, c1 kept.
	if err := ResumeSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("ResumeSequence: %v", err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status after resume = %q, want running", got.Status)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemSucceeded {
		t.Errorf("c1 status after resume = %q, want succeeded (history kept)", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("c2 status after resume = %q, want running (armed)", got.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 2 || armed[1] != c2.ID {
		t.Errorf("armed work items = %v, want [c1 c2]", armed)
	}
}

// TestResumeSequenceHaltedChainReArmsFailedChild is the regression test
// for the "Resume button on a sequence parent does not resume the
// sequence" bug. A chain halted on a failed child (parent failed) resumes
// through ResumeSequence: the failed child is re-armed to running — re-run,
// not skipped — and prior sibling state is kept. Before the fix,
// ResumeSequence handed the still-failed child to reconcileParent, whose
// failed-child branch re-halted the chain (a resume no-op).
func TestResumeSequenceHaltedChainReArmsFailedChild(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	// Run the chain, then halt it: c1 fails → parent failed, c2 stays pending.
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	setStatus(t, env.pool, c1.ID, domain.WorkItemFailed)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemFailed {
		t.Fatalf("precondition: parent should be failed (halted), got %q", got.Status)
	}

	// Resume the halted chain: c1 re-arms (re-run, not skipped), c2 untouched.
	if err := ResumeSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("ResumeSequence: %v", err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status after resume = %q, want running", got.Status)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("c1 status after resume = %q, want running (re-armed)", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemPending {
		t.Errorf("c2 status after resume = %q, want pending (not yet)", got.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 2 || armed[0] != c1.ID || armed[1] != c1.ID {
		t.Errorf("armed work items = %v, want [c1 c1] (c1 re-dispatched, c2 never armed)", armed)
	}
}

// TestResumeSequenceHaltedCancelledChild: a chain halted on a CANCELLED
// first non-succeeded child resumes exactly like a failed one — the
// cancelled child is reset to pending and re-armed (cancellation is the
// same halt semantics as failure).
func TestResumeSequenceHaltedCancelledChild(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	setStatus(t, env.pool, c1.ID, domain.WorkItemCancelled)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemFailed {
		t.Fatalf("precondition: parent should be failed (halted), got %q", got.Status)
	}

	if err := ResumeSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("ResumeSequence: %v", err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status after resume = %q, want running", got.Status)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("c1 status after resume = %q, want running (re-armed)", got.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 2 || armed[1] != c1.ID {
		t.Errorf("armed work items = %v, want [c1 c1]", armed)
	}
}

// TestResumeSequenceHaltedContainerChild: resuming a chain halted on a
// FAILED container child re-arms the container, which resets its subtree —
// the nested chain re-runs from its own first non-succeeded child through
// the existing container arm path (no recursion added).
func TestResumeSequenceHaltedContainerChild(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	feature := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindFeature, "Feature", &parent.ID, nil)
	t1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "T1", &feature.ID, &wf)
	t2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "T2", &feature.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{feature.ID})
	reorder(t, env.pool, env.proj.ID, feature.ID, []string{t1.ID, t2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, feature.ID); err != nil {
		t.Fatal(err)
	}
	// t1 fails → feature fails → parent fails (whole chain halted).
	setStatus(t, env.pool, t1.ID, domain.WorkItemFailed)
	_ = rec.reconcileOne(ctx, approvalTestTenant, feature.ID)
	_ = rec.reconcileOne(ctx, approvalTestTenant, parent.ID)
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemFailed {
		t.Fatalf("precondition: parent should be failed (halted), got %q", got.Status)
	}

	if err := ResumeSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("ResumeSequence: %v", err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status after resume = %q, want running", got.Status)
	}
	// The container child is re-armed as a container, and its subtree
	// resets to pending — the nested chain re-runs from t1.
	if got := mustGet(t, env.pool, feature.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("feature status after resume = %q, want running (container re-armed)", got.Status)
	}
	if got := mustGet(t, env.pool, t1.ID); got.Status != domain.WorkItemPending {
		t.Errorf("t1 status after resume = %q, want pending (subtree reset for re-run)", got.Status)
	}
	if got := mustGet(t, env.pool, t2.ID); got.Status != domain.WorkItemPending {
		t.Errorf("t2 status after resume = %q, want pending", got.Status)
	}
}

// TestResumeSequenceNoChildren: resume/stop on a leaf (no children) is
// rejected — only sequence parents can be controlled.
func TestResumeSequenceNoChildren(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	leaf := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Leaf", nil, &wf)

	if err := ResumeSequence(ctx, env.pool, nil, approvalTestTenant, leaf.ID, env.startFn()); err == nil {
		t.Error("ResumeSequence on a leaf should fail (no children)")
	}
	if err := StopSequence(ctx, env.pool, nil, approvalTestTenant, leaf.ID); err == nil {
		t.Error("StopSequence on a leaf should fail (no children)")
	}
}

// TestStopSequenceInFlightChildInert: STOP parks the parent while an
// in-flight child keeps running; the child's later success does NOT revive
// the parked parent (completion is inert because the engine only advances
// running/failed parents).
func TestStopSequenceInFlightChildInert(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("precondition: c1 should be running, got %q", got.Status)
	}
	if err := StopSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("StopSequence: %v", err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemPending {
		t.Fatalf("parent status after stop = %q, want pending", got.Status)
	}
	// The in-flight child finishes; its completion must NOT revive the
	// parked parent (reconcileOne's sequence-parent guard is inert on a
	// pending parent).
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	rec := NewSequenceReconciler(env.pool, nil, env.startFn())
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemPending {
		t.Errorf("parent status after in-flight child success = %q, want pending (completion inert)", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 1 {
		t.Errorf("armed work items = %v, want only the original [c1]", got)
	}
}

// TestStopSequenceRecurringParentKeepsCadence: STOP on a RECURRING sequence
// parent parks it "recurring" (not "pending"), so the cadence stays armed and
// the RecurringFireReconciler re-fires the next occurrence on schedule — a
// pending parent with a schedule would be a dead state nothing ever re-fires.
func TestStopSequenceRecurringParentKeepsCadence(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createRecurringParent(t, env.pool, env.proj.ID, "Recurring Parent")
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if err := StopSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("StopSequence: %v", err)
	}
	got := mustGet(t, env.pool, parent.ID)
	if got.Status != domain.WorkItemRecurring {
		t.Fatalf("parent status after stop = %q, want recurring (cadence stays armed)", got.Status)
	}
	if len(got.RecurringSchedule) == 0 {
		t.Error("parent recurring_schedule was cleared, want intact")
	}
	if got.NextRunAt == nil {
		t.Error("parent next_run_at is nil, want preserved for the next occurrence")
	}
	if got.ScheduledStartAt != nil {
		t.Errorf("parent scheduled_start_at after stop = %v, want cleared", got.ScheduledStartAt)
	}
}

// reorder sets sort_order on the given sibling ids in order, mirroring
// ReorderWorkItems' 1..N numbering (test helper).
func reorder(t *testing.T, pool *db.Pool, projectID, parentID string, ordered []string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	for i, id := range ordered {
		w, err := db.GetWorkItem(ctx, ttx.Tx, approvalTestTenant, id)
		if err != nil {
			t.Fatal(err)
		}
		o := float64(i + 1)
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, id, w.Version, db.UpdateWorkItemFields{
			SortOrder: &o,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// createRecurringParent creates a sequence parent with a recurring schedule
// and a FUTURE next_run_at — mirroring the RecurringFireReconciler, which
// pre-advances the cursor before it fires an occurrence. A future cursor
// also keeps tests isolated: no test leaves a still-due recurring item in
// the shared test DB that a later fire-reconciler invocation would pick up.
func createRecurringParent(t *testing.T, pool *db.Pool, projID, title string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	scheduleJSON, err := json.Marshal(&apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-12",
		StartTime: "09:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(24 * time.Hour)
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projID,
		Kind: domain.WorkItemKindEpic, Title: title, Status: domain.WorkItemRecurring,
		RecurringSchedule: scheduleJSON, NextRunAt: &future,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestSequenceStopParksBlockedChild: StopSequence on a parent whose first
// child is blocked must park EVERY descendant to pending — a blocked child
// falls into haltWorkItem's default branch (it is not in-flight), so the
// stopped chain is fully at rest and re-runnable.
func TestSequenceStopParksBlockedChild(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, c1.ID, domain.DependencyBlocks)

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemBlocked {
		t.Fatalf("child status = %q, want %q (parked on blocker)", got.Status, domain.WorkItemBlocked)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := StopSequence(ctx, env.pool, logger, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("StopSequence: %v", err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemPending {
		t.Errorf("child status = %q, want pending (stopped)", got.Status)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemPending {
		t.Errorf("parent status = %q, want pending (stopped)", got.Status)
	}
}

// TestSequenceResumeClearsBlockedFirstChild: ResumeSequence routes the
// first blocked child through reconcileParent, which clears it back to
// pending (and arms it once its blocker is terminal-success).
func TestSequenceResumeClearsBlockedFirstChild(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, c1.ID, domain.DependencyBlocks)

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemBlocked {
		t.Fatalf("child status = %q, want %q", got.Status, domain.WorkItemBlocked)
	}
	// Park the parent (STOP) so RESUME is a valid action.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := StopSequence(ctx, env.pool, logger, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("StopSequence: %v", err)
	}
	// Blocker succeeds before the resume.
	setStatus(t, env.pool, blocker.ID, domain.WorkItemSucceeded)

	if err := ResumeSequence(ctx, env.pool, logger, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("ResumeSequence: %v", err)
	}
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status = %q, want running (resumed)", got.Status)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("child status = %q, want running (blocked cleared and armed by resume)", got.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 1 || armed[0] != c1.ID {
		t.Errorf("armed work items = %v, want [c1]", armed)
	}
}

// TestSequenceStartResetsBlockedSubtree: a recurring fire (StartSequence)
// resets every descendant to pending — a previously blocked child must be
// re-armed fresh for the new cycle, not left blocked.
func TestSequenceStartResetsBlockedSubtree(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, c1.ID, domain.DependencyBlocks)

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemBlocked {
		t.Fatalf("child status = %q, want %q", got.Status, domain.WorkItemBlocked)
	}
	// Re-fire the chain (a recurring cycle start): the subtree resets to
	// pending, so the new cycle's first child is on-deck again.
	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("StartSequence (re-fire): %v", err)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("child status = %q, want %q (re-parked on still-unsatisfied blocker)", got.Status, domain.WorkItemBlocked)
	}
}
