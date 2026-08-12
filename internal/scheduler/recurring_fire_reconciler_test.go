package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
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
	// The next occurrence should be at least 1 hour from now (hourly, interval 2).
	if updated.NextRunAt.Before(time.Now().Add(30 * time.Minute)) {
		t.Errorf("next_run_at %v should be >30min in the future for 2-hourly schedule", updated.NextRunAt)
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
