package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestStopSequenceSucceedsWhileChildrenTransitioning reproduces the live race:
// a concurrent version bump between the walk and the park must not abort STOP
// with misleading "db: not found". With bounded retry + ParkWorkItem fallback
// STOP commits and parks the subtree.
func TestStopSequenceSucceedsWhileChildrenTransitioning(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)

	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Race Parent", nil, nil)
	c1 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wf)
	c2 := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{c1.ID, c2.ID})
	setStatus(t, env.pool, parent.ID, domain.WorkItemRunning)

	// Concurrent bumper: hammer c1/c2 versions while STOP runs, simulating the
	// sequence engine transitioning children (succeeded -> next arming).
	stopCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		stopCh <- StopSequence(ctx, env.pool, logger, approvalTestTenant, parent.ID)
	}()
	// Bump versions in parallel; use direct SQL to force version increments
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			ttx, _ := env.pool.BeginTenantTx(ctx, approvalTestTenant)
			if ttx != nil {
				ttx.Tx.Exec(ctx, `UPDATE work_items SET updated_at=now(), version=version+1 WHERE id=$1 AND tenant_id=$2`, c1.ID, approvalTestTenant)
				ttx.Commit(ctx)
			}
			ttx2, _ := env.pool.BeginTenantTx(ctx, approvalTestTenant)
			if ttx2 != nil {
				ttx2.Tx.Exec(ctx, `UPDATE work_items SET updated_at=now(), version=version+1 WHERE id=$1 AND tenant_id=$2`, c2.ID, approvalTestTenant)
				ttx2.Commit(ctx)
			}
		}
	}()
	wg.Wait()
	err := <-stopCh
	if err != nil {
		t.Fatalf("StopSequence with concurrent bumps: %v", err)
	}
	if errors.Is(err, db.ErrNotFound) {
		t.Fatalf("STOP incorrectly returned db: not found")
	}
	// STOP must have parked the subtree (idempotent pending, recurring keeps cadence)
	for _, id := range []string{parent.ID, c1.ID, c2.ID} {
		got := mustGet(t, env.pool, id)
		if got.Status != domain.WorkItemPending && got.Status != domain.WorkItemRecurring {
			t.Errorf("item %s status %q after STOP, want pending/recurring", id, got.Status)
		}
	}
	// Idempotent second STOP must no-op cleanly
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := StopSequence(ctx, env.pool, logger, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("second StopSequence (idempotent): %v", err)
	}
}

// TestParkWorkItemUnconditionalButSchedulerGuardsSucceeded ensures tolerant
// ParkWorkItem does not overwrite terminal-success via scheduler guard.
func TestParkWorkItemUnconditionalButSchedulerGuardsSucceeded(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()
	wf := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Guard Parent", nil, nil)
	succeeded := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Succeeded", &parent.ID, &wf)
	pending := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Pending", &parent.ID, &wf)
	reorder(t, env.pool, env.proj.ID, parent.ID, []string{succeeded.ID, pending.ID})
	setStatus(t, env.pool, succeeded.ID, domain.WorkItemSucceeded)
	setStatus(t, env.pool, parent.ID, domain.WorkItemRunning)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := StopSequence(ctx, env.pool, logger, approvalTestTenant, parent.ID); err != nil {
		t.Fatalf("StopSequence: %v", err)
	}
	if got := mustGet(t, env.pool, succeeded.ID); got.Status != domain.WorkItemSucceeded {
		t.Errorf("succeeded child status after STOP = %q, want succeeded (guarded)", got.Status)
	}
	if got := mustGet(t, env.pool, pending.ID); got.Status == domain.WorkItemSucceeded {
		t.Errorf("pending child incorrectly succeeded")
	}
}
