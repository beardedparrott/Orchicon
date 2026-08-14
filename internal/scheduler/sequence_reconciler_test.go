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

// TestStartSequenceArmsFirstChild verifies the fire semantics: parent →
// running, every descendant reset to pending (prior successes included),
// only the FIRST child in sort_order arms with its OWN workflow.
func TestStartSequenceArmsFirstChild(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf1 := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	wf2 := seedPublishedWorkflow(t, env.pool, env.proj.ID)

	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Child One", &parent.ID, &wf1)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Child Two", &parent.ID, &wf2)

	// Prior successes from an earlier manual run — must be reset to pending.
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	setStatus(t, env.pool, c2.ID, domain.WorkItemSucceeded)

	// Establish sort_order so child order is unambiguous.
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})

	if err := StartSequence(ctx, env.pool, nil, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatalf("StartSequence: %v", err)
	}

	gotParent := mustGet(t, env.pool, parent.ID)
	if gotParent.Status != domain.WorkItemRunning {
		t.Errorf("parent status = %q, want running", gotParent.Status)
	}
	gotC1 := mustGet(t, env.pool, c1.ID)
	if gotC1.Status != domain.WorkItemRunning {
		t.Errorf("child one status = %q, want running (armed)", gotC1.Status)
	}
	gotC2 := mustGet(t, env.pool, c2.ID)
	if gotC2.Status != domain.WorkItemPending {
		t.Errorf("child two status = %q, want pending (not yet armed)", gotC2.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 1 || armed[0] != c1.ID {
		t.Errorf("armed work items = %v, want exactly [%s]", armed, c1.ID)
	}
	// The armed child ran with its OWN workflow (no config copy).
	env.mu.Lock()
	started := append([]leafStart(nil), env.starts...)
	env.mu.Unlock()
	if len(started) != 1 || started[0].workflowID != wf1 {
		t.Errorf("started workflow = %+v, want child one's own workflow %s", started, wf1)
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
// external blocker parks the chain (parent running, child pending, no
// arm). When the blocker succeeds the chain advances automatically.
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
	// Blocked: parent running, child pending, NOT armed.
	if got := mustGet(t, env.pool, parent.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("parent status = %q, want running (parked)", got.Status)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemPending {
		t.Errorf("child status = %q, want pending (parked)", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 0 {
		t.Errorf("armed work items = %v, want none while parked", got)
	}
	// Blocker succeeds → chain advances without human action.
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

// TestMidRunReorderNeverArmsAheadOfInFlightChild is the regression test
// for QA MAJOR #2: a ReorderWorkItems call while a child is running drags
// a pending sibling to the front, but the engine must NOT arm it while the
// earlier child is still in flight — two sequence children would run
// concurrently (sequential execution is strict). The chain only advances
// after the in-flight child reaches a terminal state; the reorder shifts
// only FUTURE arming.
func TestMidRunReorderNeverArmsAheadOfInFlightChild(t *testing.T) {
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
	// Three must NOT be armed while One is still running.
	if got := mustGet(t, env.pool, c3.ID); got.Status != domain.WorkItemPending {
		t.Fatalf("child three status = %q, want pending (must not arm ahead of in-flight One)", got.Status)
	}
	if got := mustGet(t, env.pool, c1.ID); got.Status != domain.WorkItemRunning {
		t.Fatalf("child one status = %q, want running (untouched)", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 1 {
		t.Fatalf("armed work items = %v, want exactly [c1] while One is in flight", got)
	}

	// One reaches a terminal success → the chain advances to the NEW
	// first child (Three) — the reorder shifts future arming.
	setStatus(t, env.pool, c1.ID, domain.WorkItemSucceeded)
	if err := rec.reconcileOne(ctx, approvalTestTenant, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, env.pool, c3.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("child three status = %q, want running after One succeeded", got.Status)
	}
	if got := mustGet(t, env.pool, c2.ID); got.Status != domain.WorkItemPending {
		t.Errorf("child two status = %q, want pending (not yet)", got.Status)
	}
	if got := env.armedWorkItems(); len(got) != 2 || got[1] != c3.ID {
		t.Errorf("armed work items = %v, want [c1 c3]", got)
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
