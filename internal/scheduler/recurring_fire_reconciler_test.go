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

// Recurring-fire reconciler tests: the fire path that scans for
// status='recurring' items with a due next_run_at, fires them, and
// advances next_run_at to the next occurrence.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon@127.0.0.1:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run TestRecurringFire -v

// recurringFireRecorder records which start/sequence calls scanAndFire made.
type recurringFireRecorder struct {
	mu            sync.Mutex
	started       []string // "tenant:workflow:item"
	sequences     []string // "tenant:parent"
	startCalls    int
	sequenceCalls int
}

func (f *recurringFireRecorder) startFn() StartWorkflowFn {
	return func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.startCalls++
		f.started = append(f.started, tenantID+":"+workflowID+":"+workItemID)
		return nil
	}
}

func (f *recurringFireRecorder) sequenceFn() StartSequenceFn {
	return func(ctx context.Context, tenantID, parentID string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sequenceCalls++
		f.sequences = append(f.sequences, tenantID+":"+parentID)
		return nil
	}
}

// createRecurringItem creates a work item with status='recurring' and a
// due next_run_at (in the past, within the 5-minute scan window).
func createRecurringItem(t *testing.T, pool *db.Pool, projID, kind, title string,
	parent, workflowID *string, schedule *apiv1.RecurringSchedule) db.WorkItemRow {

	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	var scheduleJSON []byte
	if schedule != nil {
		b, err := json.Marshal(schedule)
		if err != nil {
			t.Fatal(err)
		}
		scheduleJSON = b
	}
	due := time.Now().Add(-2 * time.Minute) // within the due window
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projID,
		ParentID: parent, Kind: kind, Title: title, Status: domain.WorkItemRecurring,
		WorkflowID: workflowID, RecurringSchedule: scheduleJSON, NextRunAt: &due,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestRecurringFireFiresLeafAndAdvancesNextRunAt verifies the core flow:
// a due recurring leaf item is fired and its next_run_at advances.
func TestRecurringFireFiresLeafAndAdvancesNextRunAt(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-fire",
		Slug: "recur-fire-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfID := seedPublishedWorkflow(t, pool, proj.ID)
	schedule := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-12",
		StartTime: "09:00",
	}
	item := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask,
		"Recurring Leaf", nil, &wfID, schedule)

	rec := &recurringFireRecorder{}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())

	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scanAndFire returned error: %v", res.Error)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.startCalls != 1 || len(rec.started) != 1 {
		t.Fatalf("workflow start calls = %d, want 1", rec.startCalls)
	}
	if rec.started[0] != approvalTestTenant+":"+wfID+":"+item.ID {
		t.Errorf("started = %v, want [%s:%s:%s]", rec.started, approvalTestTenant, wfID, item.ID)
	}

	// Verify next_run_at advanced to the next day occurrence.
	updated := mustGet(t, pool, item.ID)
	if updated.NextRunAt == nil {
		t.Fatal("next_run_at is nil after fire; expected advanced value")
	}
	if !updated.NextRunAt.After(time.Now().Add(-1 * time.Minute)) {
		t.Errorf("next_run_at should be in the future, got %v", updated.NextRunAt)
	}
}

// TestRecurringFireFiresSequenceParent verifies that a due recurring
// sequence parent (has children, no bound workflow) is routed through the
// sequence engine.
func TestRecurringFireFiresSequenceParent(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-seq",
		Slug: "recur-seq-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfID := seedPublishedWorkflow(t, pool, proj.ID)
	schedule := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-12",
		StartTime: "09:00",
	}

	// Sequence parent: no workflow of its own, one child bound to wfID.
	parent := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindEpic,
		"Seq Parent", nil, nil, schedule)
	_ = createWorkItem(t, pool, proj.ID, domain.WorkItemKindTask,
		"Child", &parent.ID, &wfID)

	rec := &recurringFireRecorder{}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())
	reconciler.SetSequenceStarter(rec.sequenceFn())

	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scanAndFire returned error: %v", res.Error)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.sequenceCalls != 1 || len(rec.sequences) != 1 {
		t.Fatalf("sequence calls = %d, want 1", rec.sequenceCalls)
	}
	if rec.sequences[0] != approvalTestTenant+":"+parent.ID {
		t.Errorf("sequence = %v, want [%s:%s]", rec.sequences, approvalTestTenant, parent.ID)
	}
	// No workflow start — parent is a sequence container.
	if rec.startCalls != 0 {
		t.Errorf("workflow start calls = %d, want 0", rec.startCalls)
	}
}

// TestRecurringFireSkipsNonDueItems verifies that items with next_run_at
// in the future or past the 5-minute window are not fired.
func TestRecurringFireSkipsNonDueItems(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-skip",
		Slug: "recur-skip-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfID := seedPublishedWorkflow(t, pool, proj.ID)

	// Item 1: next_run_at in the future (should not fire).
	futureTime := time.Now().Add(1 * time.Hour)
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	futureItem, err := db.CreateWorkItem(ctx, ttx2.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Future Item",
		Status: domain.WorkItemRecurring, WorkflowID: &wfID, NextRunAt: &futureTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Item 2: next_run_at far in the past (>5 minutes ago, should not fire).
	pastTime := time.Now().Add(-10 * time.Minute)
	ttx3, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	pastItem, err := db.CreateWorkItem(ctx, ttx3.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Past Item",
		Status: domain.WorkItemRecurring, WorkflowID: &wfID, NextRunAt: &pastTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx3.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rec := &recurringFireRecorder{}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())

	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scanAndFire returned error: %v", res.Error)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.startCalls != 0 {
		t.Errorf("workflow start calls = %d, want 0 (future and past items skipped)", rec.startCalls)
	}

	// Verify items unchanged.
	futureGot := mustGet(t, pool, futureItem.ID)
	if futureGot.Status != domain.WorkItemRecurring {
		t.Errorf("future item status = %s, want recurring", futureGot.Status)
	}
	pastGot := mustGet(t, pool, pastItem.ID)
	if pastGot.Status != domain.WorkItemRecurring {
		t.Errorf("past item status = %s, want recurring", pastGot.Status)
	}
}

// TestRecurringFireIdempotency verifies that firing the same item twice
// (simulating a reconcile retry) does not double-fire thanks to the
// version lock on next_run_at advance.
func TestRecurringFireIdempotency(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-ide",
		Slug: "recur-ide-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfID := seedPublishedWorkflow(t, pool, proj.ID)
	schedule := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-12",
		StartTime: "09:00",
	}
	item := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask,
		"Idempotent Item", nil, &wfID, schedule)

	rec := &recurringFireRecorder{}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())

	// First pass: fires and advances next_run_at.
	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("first pass error: %v", res.Error)
	}
	// Second pass: item should not appear (next_run_at advanced past window).
	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("second pass error: %v", res.Error)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.startCalls != 1 {
		t.Errorf("workflow start calls = %d, want 1 (idempotent)", rec.startCalls)
	}
	_ = item // used for creation; idempotency verified by start call count
}

// TestRecurringFireSkipsNoWorkflow verifies that a recurring leaf with no
// bound workflow is skipped with a warning.
func TestRecurringFireSkipsNoWorkflow(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-nowf",
		Slug: "recur-nowf-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	schedule := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-12",
		StartTime: "09:00",
	}
	_ = createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask,
		"No Workflow", nil, nil, schedule)

	rec := &recurringFireRecorder{}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())

	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scanAndFire returned error: %v", res.Error)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.startCalls != 0 {
		t.Errorf("workflow start calls = %d, want 0 (no workflow)", rec.startCalls)
	}
}

// TestRecurringAdvanceNextRunAt verifies the advanceNextRunAt function
// correctly computes and persists the next occurrence.
func TestRecurringAdvanceNextRunAt(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-adv",
		Slug: "recur-adv-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	schedule := &apiv1.RecurringSchedule{
		Frequency: "hourly",
		Interval:  2,
		StartDate: "2026-08-12",
		StartTime: "08:00",
	}
	scheduleJSON, _ := json.Marshal(schedule)
	due := time.Now().Add(-1 * time.Minute)

	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateWorkItem(ctx, ttx2.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Advance Item",
		Status: domain.WorkItemRecurring, RecurringSchedule: scheduleJSON, NextRunAt: &due,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rec := &recurringFireRecorder{}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())

	// Manually call advanceNextRunAt.
	err = reconciler.advanceNextRunAt(ctx, approvalTestTenant, item.ID, item.Version, scheduleJSON)
	if err != nil {
		t.Fatalf("advanceNextRunAt: %v", err)
	}

	updated := mustGet(t, pool, item.ID)
	if updated.NextRunAt == nil {
		t.Fatal("next_run_at is nil after advance")
	}
	// The next occurrence must be strictly in the future — the whole
	// point of advanceNextRunAt. (Asserting ">30min in the future" would
	// be time-of-day flaky: for an hourly/interval-2 schedule the next
	// occurrence can land as little as ~1 minute from now.)
	if !updated.NextRunAt.After(time.Now()) {
		t.Errorf("next_run_at %v should be in the future after advance", updated.NextRunAt)
	}
}

// TestRecurringFireBothLeafAndSequenceParent verifies that a due
// recurring leaf AND a co-due sequence parent both fire in the same pass.
func TestRecurringFireBothLeafAndSequenceParent(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-both",
		Slug: "recur-both-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfID := seedPublishedWorkflow(t, pool, proj.ID)
	schedule := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-12",
		StartTime: "09:00",
	}

	// Sequence parent: no workflow of its own, one child.
	parent := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindEpic,
		"Seq Parent", nil, nil, schedule)
	_ = createWorkItem(t, pool, proj.ID, domain.WorkItemKindTask,
		"Child", &parent.ID, &wfID)

	// Co-due bound-workflow leaf.
	bound := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask,
		"Bound Leaf", nil, &wfID, schedule)

	rec := &recurringFireRecorder{}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())
	reconciler.SetSequenceStarter(rec.sequenceFn())

	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scanAndFire returned error: %v", res.Error)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.sequenceCalls != 1 {
		t.Errorf("sequence calls = %d, want 1", rec.sequenceCalls)
	}
	if rec.startCalls != 1 {
		t.Errorf("workflow start calls = %d, want 1", rec.startCalls)
	}
	if len(rec.started) == 1 && rec.started[0] != approvalTestTenant+":"+wfID+":"+bound.ID {
		t.Errorf("started = %v, want [%s:%s:%s]", rec.started, approvalTestTenant, wfID, bound.ID)
	}
}

// TestRecurringFireLifecycle_CompletionReturnsToRecurring verifies the
// critical lifecycle: fire a recurring item → run completes → item returns
// to "recurring" status (not "succeeded"). This catches the bug where the
// completion handler checked wi.Status == "recurring" but StartWorkflow
// had already set it to "running".
func TestRecurringFireLifecycle_CompletionReturnsToRecurring(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()

	// Setup: project + workflow with one step.
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-lifecycle",
		Slug: "recur-lifecycle-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Seed a workflow with a REAL task step (not the empty step DAG that
	// seedPublishedWorkflow publishes) so reconcileRun can actually
	// complete the run below — an empty DAG fails the run at start
	// (workflow_reconciler.go:258) and the item would end "failed".
	steps := []workflow.StepWire{
		{ID: "step-1", Name: "Do work", Kind: domain.StepKindTask, DependsOn: []string{}},
	}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))
	schedule := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-12",
		StartTime: "09:00",
	}

	// Create recurring leaf item with a due next_run_at.
	item := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask,
		"Lifecycle Item", nil, &wfID, schedule)

	// Step 1: Fire the item via RecurringFireReconciler.
	rec := &recurringFireRecorder{}
	fireRec := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())

	if res := fireRec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("fire reconciler: %v", res.Error)
	}
	if rec.startCalls != 1 {
		t.Fatalf("expected 1 start call, got %d", rec.startCalls)
	}

	// After fire: next_run_at advanced, item still "recurring" (fire
	// reconciler doesn't change status — it only advances next_run_at).
	fired := mustGet(t, pool, item.ID)
	if fired.Status != domain.WorkItemRecurring {
		t.Fatalf("after fire: status=%s, want recurring", fired.Status)
	}
	if fired.NextRunAt == nil || !fired.NextRunAt.After(time.Now().Add(-1*time.Minute)) {
		t.Fatalf("after fire: next_run_at=%v, want future", fired.NextRunAt)
	}

	// Step 2: Simulate what StartWorkflowDirect does — create a run and
	// transition the item to "running" (the bug trigger: status is no
	// longer "recurring" when the completion handler fires).
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx2.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: wfID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: item.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Flip the runtime-serve readiness gate (what the headless
	// reconcileRun path does) so the reconciler doesn't bump the run's
	// version mid-pass and leave the completion update with a stale
	// version ("db: not found").
	if _, err := db.UpdateWorkflowRun(ctx, ttx2.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	}); err != nil {
		t.Fatal(err)
	}
	// Create a step run for the workflow.
	stepRun, err := db.CreateWorkflowStepRun(ctx, ttx2.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "step-1",
		StepName: "Do work", StepKind: domain.StepKindTask,
		Status: domain.StepRunSucceeded,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Transition the item to "running" (mimics StartWorkflow behavior).
	runningStatus := domain.WorkItemRunning
	if _, err := db.UpdateWorkItem(ctx, ttx2.Tx, approvalTestTenant, item.ID, fired.Version,
		db.UpdateWorkItemFields{Status: &runningStatus, WorkflowRunID: &run.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Step 3: Run the WorkflowReconciler — it should see all steps
	// succeeded and transition the item back to "recurring" (not "succeeded").
	wfRec := NewWorkflowReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil, nil, nil, nil)
	if res := wfRec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("workflow reconciler: %v", res.Error)
	}

	// Verify: item must be "recurring" with next_run_at intact.
	final := mustGet(t, pool, item.ID)
	if final.Status != domain.WorkItemRecurring {
		t.Errorf("after completion: status=%s, want recurring (bug: StartWorkflow sets running, completion must not override to succeeded)", final.Status)
	}
	if final.NextRunAt == nil {
		t.Error("after completion: next_run_at is nil, want it preserved for next cycle")
	}

	_ = stepRun // used in setup; verified via allSucceeded path
}

// TestRecurringRunFailureKeepsItemRecurring verifies the failure semantics
// for a recurring leaf: when the bound run FAILS, the item returns to
// "recurring" (not "failed") with its schedule + next_run_at intact, so the
// NEXT occurrence still fires. The failed occurrence is recorded (run →
// failed) and recoverable via the existing per-step recovery flow; the
// cycle itself is never killed.
func TestRecurringRunFailureKeepsItemRecurring(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()

	// Setup: project + workflow with one real task step.
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-failure",
		Slug: "recur-failure-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	steps := []workflow.StepWire{
		{ID: "step-1", Name: "Do work", Kind: domain.StepKindTask, DependsOn: []string{}},
	}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))
	schedule := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-12",
		StartTime: "09:00",
	}
	item := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask,
		"Failure Item", nil, &wfID, schedule)

	// Step 1: Fire the item — next_run_at advances to the next occurrence.
	rec := &recurringFireRecorder{}
	fireRec := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())
	if res := fireRec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("fire reconciler: %v", res.Error)
	}
	if rec.startCalls != 1 {
		t.Fatalf("expected 1 start call, got %d", rec.startCalls)
	}
	fired := mustGet(t, pool, item.ID)
	if fired.NextRunAt == nil {
		t.Fatal("after fire: next_run_at is nil")
	}

	// Step 2: Create a run whose step FAILS; bind + transition item to
	// "running" (mimics StartWorkflow behavior).
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx2.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: wfID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: item.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx2.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowStepRun(ctx, ttx2.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "step-1",
		StepName: "Do work", StepKind: domain.StepKindTask,
		Status: domain.StepRunFailed,
	}); err != nil {
		t.Fatal(err)
	}
	runningStatus := domain.WorkItemRunning
	if _, err := db.UpdateWorkItem(ctx, ttx2.Tx, approvalTestTenant, item.ID, fired.Version,
		db.UpdateWorkItemFields{Status: &runningStatus, WorkflowRunID: &run.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Step 3: Run the WorkflowReconciler — the run fails; the recurring
	// item must return to "recurring", NOT "failed".
	wfRec := NewWorkflowReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil, nil, nil, nil)
	if res := wfRec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("workflow reconciler: %v", res.Error)
	}

	final := mustGet(t, pool, item.ID)
	if final.Status != domain.WorkItemRecurring {
		t.Errorf("after failed run: status=%s, want recurring (a failed cycle must not kill the schedule)", final.Status)
	}
	if len(final.RecurringSchedule) == 0 {
		t.Error("after failed run: recurring_schedule was cleared, want intact")
	}
	if final.NextRunAt == nil {
		t.Error("after failed run: next_run_at is nil, want the next occurrence preserved")
	}

	// The failed occurrence itself is recorded as a failed run.
	ttx3, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	runRow, err := db.GetWorkflowRun(ctx, ttx3.Tx, approvalTestTenant, run.ID)
	_ = ttx3.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runRow.Status != domain.WorkflowRunFailed {
		t.Errorf("run status = %s, want failed (the failed occurrence is recorded)", runRow.Status)
	}

	// Step 4: The NEXT occurrence still fires — make the item due again
	// and re-run the fire reconciler.
	ttx4, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(-2 * time.Minute)
	if _, err := db.UpdateWorkItem(ctx, ttx4.Tx, approvalTestTenant, item.ID, final.Version,
		db.UpdateWorkItemFields{NextRunAt: &due}); err != nil {
		t.Fatal(err)
	}
	if err := ttx4.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if res := fireRec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("fire reconciler (next occurrence): %v", res.Error)
	}
	if rec.startCalls != 2 {
		t.Errorf("start calls = %d, want 2 (next occurrence must still fire after a failed cycle)", rec.startCalls)
	}
}

// TestRecurringRunFailureAtStartKeepsItemRecurring covers the failRunAtStart
// path: a recurring item whose run can never progress (here: the run
// references a workflow version that does not exist) is failed at start by
// the WorkflowReconciler. The item must return to "recurring" — a structural
// failure of one occurrence is still just a failed cycle, never a terminal
// one, and the next occurrence must fire on schedule.
func TestRecurringRunFailureAtStartKeepsItemRecurring(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-fail-start",
		Slug: "recur-fail-start-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	steps := []workflow.StepWire{
		{ID: "step-1", Name: "Do work", Kind: domain.StepKindTask, DependsOn: []string{}},
	}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))
	schedule := &apiv1.RecurringSchedule{
		Frequency: "daily",
		Interval:  1,
		StartDate: "2026-08-12",
		StartTime: "09:00",
	}
	item := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask,
		"Failure-at-start Item", nil, &wfID, schedule)

	// Fire the item so next_run_at is advanced (as in a real cycle).
	rec := &recurringFireRecorder{}
	fireRec := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())
	if res := fireRec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("fire reconciler: %v", res.Error)
	}
	if rec.startCalls != 1 {
		t.Fatalf("expected 1 start call, got %d", rec.startCalls)
	}
	fired := mustGet(t, pool, item.ID)
	if fired.NextRunAt == nil {
		t.Fatal("after fire: next_run_at is nil")
	}

	// Create a run bound to the item that references a NON-EXISTENT
	// workflow version — reconcileRun fails it at start.
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx2.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: wfID, WorkflowVersion: 999,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: item.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	runningStatus := domain.WorkItemRunning
	if _, err := db.UpdateWorkItem(ctx, ttx2.Tx, approvalTestTenant, item.ID, fired.Version,
		db.UpdateWorkItemFields{Status: &runningStatus, WorkflowRunID: &run.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfRec := NewWorkflowReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil, nil, nil, nil)
	if res := wfRec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("workflow reconciler: %v", res.Error)
	}

	final := mustGet(t, pool, item.ID)
	if final.Status != domain.WorkItemRecurring {
		t.Errorf("after fail-at-start run: status=%s, want recurring (a failed occurrence is never terminal)", final.Status)
	}
	if len(final.RecurringSchedule) == 0 {
		t.Error("after fail-at-start run: recurring_schedule was cleared, want intact")
	}
	if final.NextRunAt == nil {
		t.Error("after fail-at-start run: next_run_at is nil, want the next occurrence preserved")
	}

	ttx3, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	runRow, err := db.GetWorkflowRun(ctx, ttx3.Tx, approvalTestTenant, run.ID)
	_ = ttx3.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runRow.Status != domain.WorkflowRunFailed {
		t.Errorf("run status = %s, want failed (the failed occurrence is recorded)", runRow.Status)
	}
}

// listLedger returns the per-fire run-history ledger for a work item.
func listLedger(t *testing.T, pool *db.Pool, itemID string) []db.RecurringRunHistoryEntry {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	entries, err := db.ListRecurringRunHistory(ctx, ttx.Tx, approvalTestTenant, itemID)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// TestRecurringFireRecordsLedgerOnFire verifies a successful fire writes a
// 'fired' run-history entry on the recurring item (4.2 AC: each fire records
// a run-history entry). A fire never auto-creates a work item — the dispatch
// goes through the normal start path and the item remains "recurring".
func TestRecurringFireRecordsLedgerOnFire(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-ledger",
		Slug: "recur-ledger-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wfID := seedPublishedWorkflow(t, pool, proj.ID)
	schedule := &apiv1.RecurringSchedule{Frequency: "daily", Interval: 1, StartDate: "2026-08-12", StartTime: "09:00"}
	item := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask, "Ledger Item", nil, &wfID, schedule)

	rec := &recurringFireRecorder{}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())
	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}
	if rec.startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", rec.startCalls)
	}

	entries := listLedger(t, pool, item.ID)
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	if entries[0].Status != "fired" {
		t.Errorf("ledger status = %q, want fired", entries[0].Status)
	}
	if entries[0].Error != "" {
		t.Errorf("ledger error = %q, want empty on a successful fire", entries[0].Error)
	}
	if entries[0].FireAt.IsZero() || !entries[0].FireAt.Before(time.Now().Add(1*time.Minute)) {
		t.Errorf("fire_at = %v, want a recent timestamp", entries[0].FireAt)
	}
	if got := mustGet(t, pool, item.ID); got.Status != domain.WorkItemRecurring {
		t.Errorf("item status = %q, want recurring after fire", got.Status)
	}
}

// TestRecurringFireFailedFireRecordsLedgerAndAdvances verifies the failure
// semantics (4.2 AC): a fire whose dispatch fails writes a 'failed' ledger
// entry carrying the error, next_run_at is still advanced (advance-before-
// fire idempotency), and the item stays "recurring" — a failed fire must not
// wedge the scheduler nor corrupt next_run_at.
func TestRecurringFireFailedFireRecordsLedgerAndAdvances(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-failfire",
		Slug: "recur-failfire-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wfID := seedPublishedWorkflow(t, pool, proj.ID)
	schedule := &apiv1.RecurringSchedule{Frequency: "daily", Interval: 1, StartDate: "2026-08-12", StartTime: "09:00"}
	item := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask, "Fail-Fire Item", nil, &wfID, schedule)

	failStart := func(ctx context.Context, _, _, _, _ string) error {
		return errors.New("dispatch failed")
	}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), failStart)
	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}

	entries := listLedger(t, pool, item.ID)
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	if entries[0].Status != "failed" {
		t.Errorf("ledger status = %q, want failed", entries[0].Status)
	}
	if entries[0].Error != "dispatch failed" {
		t.Errorf("ledger error = %q, want %q", entries[0].Error, "dispatch failed")
	}

	got := mustGet(t, pool, item.ID)
	if got.Status != domain.WorkItemRecurring {
		t.Errorf("item status = %q, want recurring after a failed fire", got.Status)
	}
	if got.NextRunAt == nil || !got.NextRunAt.After(time.Now()) {
		t.Errorf("next_run_at = %v, want advanced to a future occurrence after a failed fire", got.NextRunAt)
	}
}

// TestRecurringFireSkipsPausedItem verifies the enable/pause lifecycle
// (4.2 AC): a paused recurring item (recurring_enabled=false) keeps its
// schedule + next_run_at but is excluded from the due scan — it does NOT fire
// and records no ledger entry.
func TestRecurringFireSkipsPausedItem(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "recur-paused",
		Slug: "recur-paused-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wfID := seedPublishedWorkflow(t, pool, proj.ID)
	schedule := &apiv1.RecurringSchedule{Frequency: "daily", Interval: 1, StartDate: "2026-08-12", StartTime: "09:00"}
	item := createRecurringItem(t, pool, proj.ID, domain.WorkItemKindTask, "Paused Item", nil, &wfID, schedule)

	// Pause: recurring_enabled=false. Schedule + next_run_at must be preserved.
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := db.UpdateWorkItem(ctx, ttx2.Tx, approvalTestTenant, item.ID, item.Version,
		db.UpdateWorkItemFields{RecurringEnabled: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if paused.RecurringEnabled {
		t.Fatal("recurring_enabled = true, want false after pause")
	}
	if len(paused.RecurringSchedule) == 0 || paused.NextRunAt == nil {
		t.Fatal("pause cleared the schedule or next_run_at — pause must preserve them")
	}

	rec := &recurringFireRecorder{}
	reconciler := NewRecurringFireReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())
	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}
	if rec.startCalls != 0 {
		t.Errorf("start calls = %d, want 0 (paused item must not fire)", rec.startCalls)
	}
	if entries := listLedger(t, pool, item.ID); len(entries) != 0 {
		t.Errorf("ledger entries = %d, want 0 (paused item records no fire)", len(entries))
	}
}

