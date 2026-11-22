package scheduler

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// Scheduled-run reconciler tests: the fire path that admits sequence
// parents (NULL workflow_id + children) WITHOUT breaking the pre-existing
// bound-workflow path (QA regression: scanning a sequence parent's NULL
// workflow_id into a plain string poisoned rows.Err() and aborted the
// whole pass, so co-due bound items never fired).
//
//	export ORCHICON_TEST_DSN='postgres://orchicon@127.0.0.1:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run TestScheduledRun -v

// fireRecorder records which start/sequence calls scanAndFire made.
type fireRecorder struct {
	mu            sync.Mutex
	started       []string // "tenant:workflow:item"
	sequences     []string // "tenant:parent"
	startCalls    int
	sequenceCalls int
}

func (f *fireRecorder) startFn() StartWorkflowFn {
	return func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.startCalls++
		f.started = append(f.started, tenantID+":"+workflowID+":"+workItemID)
		return nil
	}
}

func (f *fireRecorder) sequenceFn() StartSequenceFn {
	return func(ctx context.Context, tenantID, parentID string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sequenceCalls++
		f.sequences = append(f.sequences, tenantID+":"+parentID)
		return nil
	}
}

// createScheduledItem creates a work item with a past-due scheduled start
// (inside the reconciler's now()-5m..now() window) and status 'scheduled'.
func createScheduledItem(t *testing.T, pool *db.Pool, projID, kind, title string, parent, workflowID *string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	start := time.Now().Add(-2 * time.Minute)
	w, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projID,
		ParentID: parent, Kind: kind, Title: title, Status: domain.WorkItemScheduled,
		WorkflowID: workflowID, ScheduledStartAt: &start,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestScheduledRunFiresSequenceParentAndBoundItem is the regression test for QA BLOCKER #1: a due
// sequence parent (NULL workflow_id.
func TestScheduledRunFiresSequenceParentAndBoundItem(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "sched-run",
		Slug: "sched-run-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
		ExecutionMode: db.ExecutionModeRuntime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Self-cleaning: see db.CleanupProject — the fixture tears its project down
	// through the same cascade the product uses, so it cannot leave residue behind.
	db.CleanupProject(t, pool, approvalTestTenant, proj.ID)

	wfID := seedPublishedWorkflow(t, pool, proj.ID)

	// Sequence parent: no workflow of its own, one child bound to wfID.
	parent := createScheduledItem(t, pool, proj.ID, domain.WorkItemKindEpic, "Seq Parent", nil, nil)
	_ = createWorkItem(t, pool, proj.ID, domain.WorkItemKindTask, "Child", &parent.ID, &wfID)

	// Co-due bound-workflow item (the path that must NOT be suppressed).
	bound := createScheduledItem(t, pool, proj.ID, domain.WorkItemKindTask, "Bound Item", nil, &wfID)

	rec := &fireRecorder{}
	reconciler := NewScheduledRunReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), rec.startFn())
	reconciler.SetSequenceStarter(rec.sequenceFn())

	if res := reconciler.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scanAndFire returned error (whole pass aborted): %v", res.Error)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	// ASSERT ON THIS TEST'S ITEMS, NOT ON GLOBAL CALL COUNTS.
	//
	// scanAndFire has NO tenant predicate by design — it is a background reconciler that scans every
	// tenant's due items. A `startCalls == 1` assertion therefore also counts the scheduled items any
	// OTHER tenant left behind, which is why this test failed against a shared database (the shared
	// test DB had 20 due `tnt_dev` rows and 24 from another package's tenant). The behaviour under test
	// is "did THIS parent start a sequence, and did THIS bound item start its workflow while the
	// sequence parent did not" — so assert exactly that, in both directions.
	if !sliceContains(rec.sequences, approvalTestTenant+":"+parent.ID) {
		t.Errorf("sequence starter calls = %v, want it to include %s:%s",
			rec.sequences, approvalTestTenant, parent.ID)
	}
	if !sliceContains(rec.started, approvalTestTenant+":"+wfID+":"+bound.ID) {
		t.Errorf("workflow start calls = %v, want it to include %s:%s:%s",
			rec.started, approvalTestTenant, wfID, bound.ID)
	}
	// The suppression this test exists for: the SEQUENCE parent must not start a workflow of its own.
	// Previously implied by the global count being exactly 1; asserted directly so it survives the
	// presence of unrelated tenants' rows.
	for _, started := range rec.started {
		if started == approvalTestTenant+":"+parent.ID || strings.HasSuffix(started, ":"+parent.ID) {
			t.Errorf("the sequence parent started a workflow of its own (%q); it must only sequence",
				started)
		}
	}
}

// sliceContains reports whether want is in have.
func sliceContains(have []string, want string) bool {
	for _, h := range have {
		if h == want {
			return true
		}
	}
	return false
}
