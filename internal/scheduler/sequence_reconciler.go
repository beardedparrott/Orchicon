package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

const (
	sequenceMaxAttempts          = 5
	sequenceMaxConsecutiveErrors = 5
	sequenceStallThreshold       = 5 * time.Minute
	sequencePerParentTimeout     = 10 * time.Second
)

func sequenceBackoff(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	d := time.Duration(5*(1<<(attempts-1))) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// SequenceReconciler is the sequence engine for sequential multi-workflow runs
// (architecture-notes/sequential-multi-workflow-runs.md §2).
type SequenceReconciler struct {
	pool  *db.Pool
	log   *slog.Logger
	start StartWorkflowFn
}

// StartSequenceFn starts a sequence run for a parent work item with children: flips the parent to
// running, resets every descendant to pending.
type StartSequenceFn func(ctx context.Context, tenantID, parentID string) error

// NewSequenceReconciler creates the sequence engine.
func NewSequenceReconciler(pool *db.Pool, log *slog.Logger, start StartWorkflowFn) *SequenceReconciler {
	return &SequenceReconciler{pool: pool, log: log, start: start}
}

// Kind returns the reconciler kind.
func (r *SequenceReconciler) Kind() string { return "sequence" }

// Reconcile processes one sequence parent (key = parent work item id), or scans all running
// sequence parents when the key is empty.
func (r *SequenceReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	// v0.1: single dev tenant (mirrors TaskReconciler/WorkflowReconciler).
	tenantID := "tnt_dev"
	if key == "" {
		return r.scan(ctx, tenantID)
	}
	if err := r.reconcileOne(ctx, tenantID, key); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

// scan finds every in-flight sequence parent and advances it.
func (r *SequenceReconciler) scan(ctx context.Context, tenantID string) reconciler.Result {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return reconciler.Result{Error: err}
	}
	parents, err := db.ListSequenceActiveParents(ctx, ttx.Tx, tenantID)
	ttx.Rollback(ctx)
	if err != nil {
		return reconciler.Result{Error: fmt.Errorf("scan sequence parents: %w", err)}
	}
	// Batch cap mirrors TaskReconciler's scan so one pass can't monopolize
	// the reconciler goroutine.
	for i, p := range parents {
		if i >= 16 {
			break
		}
		pCtx, cancel := context.WithTimeout(ctx, sequencePerParentTimeout)
		err := r.reconcileOne(pCtx, tenantID, p.ID)
		cancel()
		if err != nil {
			if r.log != nil {
				r.log.Warn("sequence: reconcile parent failed", "parent", p.ID, "error", err)
			}
			// Version-conflict / CAS races are transient and must not count as wedge heartbeat.
			if !errors.Is(err, db.ErrNotFound) && !reIsCASConflict(err) {
				r.recordScanError(ctx, tenantID, p.ID, err)
			} else if r.log != nil {
				r.log.Warn("sequence: transient reconcile conflict, not counting as wedge", "parent", p.ID, "error", err)
			}
		} else {
			r.clearScanError(ctx, tenantID, p.ID)
		}
	}
	// Stalled-parent liveness guard (E): parent with no progress for 5m and no in-flight/blocked child → halt.
	r.checkStalledParents(ctx, tenantID, parents)
	return reconciler.Result{}
}

func (r *SequenceReconciler) recordScanError(ctx context.Context, tenantID, parentID string, recErr error) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	parent, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, parentID)
	if err != nil {
		return
	}
	// Heartbeat: always bump updated_at even on error so frozen detection is reliable.
	newCount := parent.SequenceConsecutiveScanErrors + 1
	now := time.Now().UTC()
	// Use direct SQL to atomically bump counters without version race: we already hold fresh parent.
	// Update via helper that bypasses optimistic check for system heartbeat: use fresh version.
	if newCount >= sequenceMaxConsecutiveErrors {
		// Halt parent as failed with reason.
		reason := fmt.Sprintf("reconcile wedged: %v", recErr)
		results := parent.Results
		if len(results) == 0 {
			results = []byte("{}")
		}
		var m map[string]any
		_ = json.Unmarshal(results, &m)
		if m == nil {
			m = map[string]any{}
		}
		m["sequence_failure_reason"] = reason
		b, _ := json.Marshal(m)
		status := domain.WorkItemFailed
		if len(parent.RecurringSchedule) > 0 {
			status = domain.WorkItemRecurring
		}
		// Direct update with fresh version.
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, parentID, parent.Version, db.UpdateWorkItemFields{Status: &status, Results: &b}); err == nil {
			_ = ttx.Commit(ctx)
			if r.log != nil {
				r.log.Warn("sequence: parent halted after consecutive errors", "parent", parentID, "consecutive", newCount, "error", recErr)
			}
			return
		}
	}
	// Otherwise just bump error counter and heartbeat.
	_, _ = ttx.Tx.Exec(ctx, `UPDATE work_items SET sequence_consecutive_scan_errors=$1, updated_at=now(), version=version+1 WHERE id=$2 AND tenant_id=$3`, newCount, parentID, tenantID)
	_ = ttx.Commit(ctx)
	_ = now
}

func (r *SequenceReconciler) clearScanError(ctx context.Context, tenantID, parentID string) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	parent, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, parentID)
	if err != nil {
		return
	}
	if parent.SequenceConsecutiveScanErrors == 0 && parent.SequenceLastProgressAt != nil {
		return
	}
	now := time.Now().UTC()
	// Reset error counter and mark progress. Use versioned update; failure is benign.
	_, _ = ttx.Tx.Exec(ctx, `UPDATE work_items SET sequence_consecutive_scan_errors=0, sequence_last_progress_at=$1, updated_at=now(), version=version+1 WHERE id=$2 AND tenant_id=$3`, now, parentID, tenantID)
	_ = ttx.Commit(ctx)
}

func (r *SequenceReconciler) checkStalledParents(ctx context.Context, tenantID string, parents []db.WorkItemRow) {
	now := time.Now().UTC()
	for _, p := range parents {
		// Re-read fresh snapshot: the scan's parents slice is stale by the time per-parent reconciles ran,
		// so threshold + status must be evaluated on fresh row (review fix: stale snapshot).
		freshSnap, err := func() (db.WorkItemRow, error) {
			ttx2, err := r.pool.BeginTenantTx(ctx, tenantID)
			if err != nil {
				return db.WorkItemRow{}, err
			}
			defer ttx2.Rollback(ctx)
			return db.GetWorkItem(ctx, ttx2.Tx, tenantID, p.ID)
		}()
		if err != nil {
			continue
		}
		lastProg := freshSnap.SequenceLastProgressAt
		ref := freshSnap.UpdatedAt
		if lastProg != nil {
			ref = *lastProg
		}
		if now.Sub(ref) < sequenceStallThreshold {
			continue
		}
		if freshSnap.Status != domain.WorkItemRunning {
			continue
		}
		ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			continue
		}
		children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, p.ID)
		if err != nil {
			ttx.Rollback(ctx)
			continue
		}
		hasInFlight := false
		hasBlocked := false
		for _, c := range children {
			switch c.Status {
			case domain.WorkItemRunning, domain.WorkItemAssigned, domain.WorkItemReady, domain.WorkItemCheckpointing, domain.WorkItemRecovering, domain.WorkItemScheduled, domain.WorkItemRecurring:
				hasInFlight = true
			case domain.WorkItemBlocked:
				hasBlocked = true
			}
		}
		if hasInFlight || hasBlocked {
			ttx.Rollback(ctx)
			continue
		}
		// No in-flight nor blocked but still running with no progress → stalled.
		fresh, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, p.ID)
		if err != nil {
			ttx.Rollback(ctx)
			continue
		}
		results := fresh.Results
		if len(results) == 0 {
			results = []byte("{}")
		}
		var m map[string]any
		_ = json.Unmarshal(results, &m)
		if m == nil {
			m = map[string]any{}
		}
		m["sequence_failure_reason"] = "stalled: no progress for 5m"
		b, _ := json.Marshal(m)
		status := domain.WorkItemFailed
		if len(fresh.RecurringSchedule) > 0 {
			status = domain.WorkItemRecurring
		}
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, p.ID, fresh.Version, db.UpdateWorkItemFields{Status: &status, Results: &b}); err == nil {
			_ = ttx.Commit(ctx)
			if r.log != nil {
				r.log.Warn("sequence: parent stalled, halting", "parent", p.ID, "last_progress", ref)
			}
		} else {
			ttx.Rollback(ctx)
		}
	}
}

// reconcileOne advances a single sequence parent.
func (r *SequenceReconciler) reconcileOne(ctx context.Context, tenantID, parentID string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	starts, err := reconcileParent(ctx, ttx.Tx, tenantID, parentID, r.start)
	if err != nil {
		return err
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	for _, s := range starts {
		if err := r.start(ctx, s.tenantID, s.workflowID, s.projectID, s.itemID); err != nil {
			if r.log != nil {
				r.log.Error("sequence: start child workflow failed",
					"child", s.itemID, "workflow", s.workflowID, "error", err)
			}
			// Self-heal: the child was flipped to running in the committed tx; a failed start must not strand
			// it as running-with-no-run (the derived cursor would wait on it forever).
			resetArmedChild(ctx, r.pool, s.tenantID, s.itemID)
		}
	}
	return nil
}

// leafStart is a child workflow to start AFTER the reconciler transaction commits — the workflow's
// own transaction would deadlock on the locks we hold while the child status flip is uncommitted.
type leafStart struct {
	tenantID   string
	workflowID string
	projectID  string
	itemID     string
}

// reconcileParent advances one sequence parent to its next derived step, performing all state
// changes in a single transaction and returning the leaf workflow starts to fire after commit.
func reconcileParent(ctx context.Context, tx pgx.Tx, tenantID, parentID string, start StartWorkflowFn) ([]leafStart, error) {
	parent, err := db.GetWorkItem(ctx, tx, tenantID, parentID)
	if err != nil {
		return nil, err // ErrNotFound → parent deleted; nothing to advance
	}
	// Sequence-parent guard: reconcileParent must only advance a work item that IS a sequence run —
	// status running (or failed, for retry-resume) with NO bound workflow run and at least one child.
	if parent.WorkflowRunID != "" ||
		(parent.Status != domain.WorkItemRunning && parent.Status != domain.WorkItemFailed) {
		return nil, nil
	}
	children, err := db.ListDirectChildren(ctx, tx, tenantID, parentID)
	if err != nil {
		return nil, fmt.Errorf("list children: %w", err)
	}
	if len(children) == 0 {
		// No children → not a sequence parent (a running leaf). A scan
		// would never pick it up; the notifier must not either.
		return nil, nil
	}
	idx := deriveNextChild(children)
	if idx < 0 {
		// Every child terminal-success → the sequence is complete.
		status := domain.WorkItemSucceeded
		fields := db.UpdateWorkItemFields{}
		if len(parent.RecurringSchedule) > 0 {
			status = domain.WorkItemRecurring
			// If the cursor was cleared mid-cycle, recompute it so the
			// next occurrence still fires on schedule.
			fields.NextRunAt = ensureRecurringNextRun(parent.RecurringSchedule, parent.NextRunAt, time.Now().UTC())
		}
		fields.Status = &status
		if _, err := db.UpdateWorkItem(ctx, tx, tenantID, parentID, parent.Version, fields); err != nil {
			return nil, fmt.Errorf("complete sequence parent: %w", err)
		}
		return nil, nil
	}

	// Failure pre-scan: a failed/cancelled child ANYWHERE in the sequence halts the WHOLE chain — the
	// parent (and every container up the ancestor chain) goes failed, and NOTHING arms in this pass.
	for _, c := range children {
		if c.Status == domain.WorkItemFailed || c.Status == domain.WorkItemCancelled {
			if err := failSequenceChain(ctx, tx, tenantID, c); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}

	// Retry/resume: a parent that was FAILED (chain halted on a failed child) is revived to running as
	// soon as its children are no longer halted — the derived cursor resumes with the next sibling.
	if parent.Status == domain.WorkItemFailed {
		fresh, err := db.GetWorkItem(ctx, tx, tenantID, parentID)
		if err != nil {
			return nil, err
		}
		if fresh.Status == domain.WorkItemFailed {
			status := domain.WorkItemRunning
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, parentID, fresh.Version, db.UpdateWorkItemFields{
				Status: &status,
			}); err != nil {
				if errors.Is(err, db.ErrNotFound) {
					return nil, fmt.Errorf("revive CAS conflict: %w", err)
				}
				return nil, fmt.Errorf("revive sequence parent: %w", err)
			}
		}
	}

	// Classify each child as strict-sequence (no incoming dependency edges) vs dependency-governed (has
	// incoming edges) in one query.
	depGoverned := map[string]bool{}
	ids := make([]string, 0, len(children))
	for _, c := range children {
		ids = append(ids, c.ID)
	}
	targeting, err := db.ListDependenciesTargetingItems(ctx, tx, tenantID, ids,
		[]string{domain.DependencyBlocks, domain.DependencyDependsOn})
	if err != nil {
		return nil, fmt.Errorf("list targeting deps: %w", err)
	}
	for _, d := range targeting {
		depGoverned[d.ToID] = true
	}

	// Per-child arming loop over ALL children in sort_order.
	var starts []leafStart
	for i, c := range children {
		switch c.Status {
		case domain.WorkItemSucceeded:
			continue // past
		case domain.WorkItemSkipped:
			// A skipped child is terminal-success: it satisfies edges,
			// counts toward sequence completion, and is never re-armed.
			continue // past
		case domain.WorkItemRunning, domain.WorkItemAssigned, domain.WorkItemReady,
			domain.WorkItemCheckpointing, domain.WorkItemRecovering, domain.WorkItemScheduled,
			domain.WorkItemRecurring:
			// In flight (or human-managed): wait for the current child.
			continue
		case domain.WorkItemPending, domain.WorkItemBlocked:
			// Backoff gate (P1): cap check BEFORE backoff gate so halt is not delayed by ~80s (review fix).
			if c.SequenceAttempts >= sequenceMaxAttempts {
				// Cap exceeded → mark child failed so chain halts visibly (no backoff delay).
				reason := fmt.Sprintf("start failed %d times", c.SequenceAttempts)
				results := c.Results
				if len(results) == 0 {
					results = []byte("{}")
				}
				var m map[string]any
				_ = json.Unmarshal(results, &m)
				if m == nil {
					m = map[string]any{}
				}
				m["sequence_failure_reason"] = reason
				b, _ := json.Marshal(m)
				status := domain.WorkItemFailed
				if _, err := db.UpdateWorkItem(ctx, tx, tenantID, c.ID, c.Version, db.UpdateWorkItemFields{Status: &status, Results: &b}); err != nil {
					return nil, fmt.Errorf("cap child failed: %w", err)
				}
				if err := failSequenceChain(ctx, tx, tenantID, c); err != nil {
					return nil, err
				}
				return nil, nil
			}
			if c.SequenceAttempts > 0 && c.SequenceLastAttemptAt != nil {
				if time.Since(*c.SequenceLastAttemptAt) < sequenceBackoff(c.SequenceAttempts) {
					continue
				}
			}
			// Chain gate (strict children only): the child's position in the chain is reached only when its
			// immediate predecessor is terminal-success (succeeded or skipped).
			if !depGoverned[c.ID] && i > 0 && !domain.WorkItemIsTerminalSuccess(children[i-1].Status) {
				continue // chain position not reached — wait for the predecessor
			}
			// Dependency gate: an unsatisfied external blocker parks this child until the blockers succeed —
			// then the next pass arms automatically.
			satisfied, err := db.CheckDependenciesSatisfied(ctx, tx, tenantID, c.ID)
			if err != nil {
				return nil, fmt.Errorf("check deps: %w", err)
			}
			if !satisfied {
				if c.Status == domain.WorkItemPending {
					status := domain.WorkItemBlocked
					if _, err := db.UpdateWorkItem(ctx, tx, tenantID, c.ID, c.Version, db.UpdateWorkItemFields{
						Status: &status,
					}); err != nil {
						return nil, fmt.Errorf("park child blocked: %w", err)
					}
				}
				continue // parked — do not arm, do not requeue, do not fail
			}
			// Gate satisfied: a blocked child clears back to pending (the on-deck set) before arming below.
			if c.Status == domain.WorkItemBlocked {
				status := domain.WorkItemPending
				updated, err := db.UpdateWorkItem(ctx, tx, tenantID, c.ID, c.Version, db.UpdateWorkItemFields{
					Status: &status,
				})
				if err != nil {
					return nil, fmt.Errorf("clear child blocked: %w", err)
				}
				c = updated
			}
			// Arm. A container child starts a nested sequence; a leaf starts
			// its own bound workflow. No ready/assigned dance, no config copy.
			grandchildren, err := db.ListDirectChildren(ctx, tx, tenantID, c.ID)
			if err != nil {
				return nil, fmt.Errorf("list grandchildren: %w", err)
			}
			if len(grandchildren) > 0 {
				status := domain.WorkItemRunning
				if _, err := db.UpdateWorkItem(ctx, tx, tenantID, c.ID, c.Version, db.UpdateWorkItemFields{
					Status: &status,
				}); err != nil {
					return nil, fmt.Errorf("arm container child: %w", err)
				}
				// Its own chain resets to pending when the container arms
				// (nested sequence). The next scan pass reconciles it.
				if err := resetSubtree(ctx, tx, tenantID, c.ID); err != nil {
					return nil, err
				}
				continue
			}
			if c.WorkflowID == nil || *c.WorkflowID == "" {
				// Config error — schedule-time validation should have
				// prevented this. Mark the child failed and cascade.
				status := domain.WorkItemFailed
				if _, err := db.UpdateWorkItem(ctx, tx, tenantID, c.ID, c.Version, db.UpdateWorkItemFields{
					Status: &status,
				}); err != nil {
					return nil, fmt.Errorf("mark unbound child failed: %w", err)
				}
				if err := failSequenceChain(ctx, tx, tenantID, c); err != nil {
					return nil, err
				}
				return nil, nil
			}
			status := domain.WorkItemRunning
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, c.ID, c.Version, db.UpdateWorkItemFields{
				Status: &status,
			}); err != nil {
				return nil, fmt.Errorf("arm leaf child: %w", err)
			}
			starts = append(starts, leafStart{tenantID, *c.WorkflowID, c.ProjectID, c.ID})
		}
	}
	return starts, nil
}

// deriveNextChild returns the index of the first direct child in the (already sort_order-ordered)
// slice whose status is not terminal-success.
func deriveNextChild(children []db.WorkItemRow) int {
	for i, c := range children {
		if !domain.WorkItemIsTerminalSuccess(c.Status) {
			return i
		}
	}
	return -1
}

// failSequenceChain marks the failed child's parent — and every ancestor container up the chain
// that is sequence-running (status running, no bound workflow run, has children) — failed.
func failSequenceChain(ctx context.Context, tx pgx.Tx, tenantID string, failed db.WorkItemRow) error {
	cur := failed
	for cur.ParentID != nil {
		parent, err := db.GetWorkItem(ctx, tx, tenantID, *cur.ParentID)
		if err != nil {
			return nil // ancestor gone; stop the walk
		}
		if parent.Status == domain.WorkItemRunning && parent.WorkflowRunID == "" {
			children, err := db.ListDirectChildren(ctx, tx, tenantID, parent.ID)
			if err == nil && len(children) > 0 {
				status := domain.WorkItemFailed
				fields := db.UpdateWorkItemFields{}
				if len(parent.RecurringSchedule) > 0 {
					// Recurring sequence parents: a failed cycle does NOT kill the schedule — the parent stays
					// "recurring" with next_run_at intact so the RecurringFireReconciler fires the next occurrence.
					status = domain.WorkItemRecurring
					// If the cursor was cleared mid-cycle, recompute it so
					// the next occurrence still fires on schedule.
					fields.NextRunAt = ensureRecurringNextRun(parent.RecurringSchedule, parent.NextRunAt, time.Now().UTC())
				}
				fields.Status = &status
				if _, err := db.UpdateWorkItem(ctx, tx, tenantID, parent.ID, parent.Version, fields); err != nil {
					return fmt.Errorf("fail sequence ancestor: %w", err)
				}
			}
		}
		cur = parent
	}
	return nil
}

// resetSubtree sets every non-terminal descendant of parentID to pending, recursively.
func resetSubtree(ctx context.Context, tx pgx.Tx, tenantID, parentID string) error {
	children, err := db.ListDirectChildren(ctx, tx, tenantID, parentID)
	if err != nil {
		return fmt.Errorf("list children for reset: %w", err)
	}
	for _, c := range children {
		switch c.Status {
		case domain.WorkItemPending:
		case domain.WorkItemRunning, domain.WorkItemCheckpointing, domain.WorkItemRecovering:
			continue // in-flight bound run — leave it and its subtree
		case domain.WorkItemSucceeded, domain.WorkItemSkipped:
			// Terminal-success descendants are always kept — never reset to
			// pending, never recursed (their subtree is already done too).
			continue
		default:
			status := domain.WorkItemPending
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, c.ID, c.Version, db.UpdateWorkItemFields{
				Status: &status,
			}); err != nil {
				return fmt.Errorf("reset child: %w", err)
			}
		}
		if err := resetSubtree(ctx, tx, tenantID, c.ID); err != nil {
			return err
		}
	}
	return nil
}

// StartSequence implements StartSequenceFn: it fires a sequence run for a parent work item with
// children — parent → running.
func StartSequence(ctx context.Context, pool *db.Pool, log *slog.Logger, tenantID, parentID string, start StartWorkflowFn) error {
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	parent, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, parentID)
	if err != nil {
		return err
	}
	children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, parentID)
	if err != nil {
		return fmt.Errorf("list children: %w", err)
	}
	if len(children) == 0 {
		return errors.New("cannot start a sequence on a work item with no children")
	}

	// Parent → running; every NON-terminal descendant resets to pending (succeeded/skipped are always
	// preserved).
	status := domain.WorkItemRunning
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, parentID, parent.Version, db.UpdateWorkItemFields{
		Status:     &status,
		WorkflowID: strPtr(""),
	}); err != nil {
		return fmt.Errorf("fire sequence parent: %w", err)
	}
	if err := resetSubtree(ctx, ttx.Tx, tenantID, parentID); err != nil {
		return err
	}
	// Arm the first child in sort_order (reuses the engine's advance logic).
	starts, err := reconcileParent(ctx, ttx.Tx, tenantID, parentID, start)
	if err != nil {
		return err
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	fireLeafStarts(ctx, pool, log, starts, start)
	return nil
}

// ResumeSequence resumes a sequence parent from its current state — parent → running, and the
// engine derives the first non-succeeded child and arms it (reusing reconcileParent).
func ResumeSequence(ctx context.Context, pool *db.Pool, log *slog.Logger, tenantID, parentID string, start StartWorkflowFn) error {
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	parent, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, parentID)
	if err != nil {
		return err
	}
	children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, parentID)
	if err != nil {
		return fmt.Errorf("list children: %w", err)
	}
	if len(children) == 0 {
		return errors.New("cannot resume a sequence on a work item with no children")
	}

	// Halted-chain resume: a parent whose first non-succeeded child is failed/cancelled has halted
	// (failSequenceChain marked the parent failed.
	if idx := deriveNextChild(children); idx >= 0 {
		switch children[idx].Status {
		case domain.WorkItemFailed, domain.WorkItemCancelled:
			status := domain.WorkItemPending
			if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, children[idx].ID,
				children[idx].Version, db.UpdateWorkItemFields{Status: &status}); err != nil {
				return fmt.Errorf("re-arm halted child on resume: %w", err)
			}
		}
	}

	// Parent → running; clear a stale workflow binding on the parent (same rationale as StartSequence —
	// a parent with children IS a sequence container, its own workflow_id is ignored).
	status := domain.WorkItemRunning
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, parentID, parent.Version, db.UpdateWorkItemFields{
		Status:     &status,
		WorkflowID: strPtr(""),
	}); err != nil {
		return fmt.Errorf("resume sequence parent: %w", err)
	}
	// Arm the first non-succeeded child in sort_order (reuses the engine's advance logic; succeeded
	// children are already terminal, so the derived cursor passes over them).
	starts, err := reconcileParent(ctx, ttx.Tx, tenantID, parentID, start)
	if err != nil {
		return err
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	fireLeafStarts(ctx, pool, log, starts, start)
	return nil
}

// stopAbortHook is the post-commit live-session abort hook for StopSequence.
var stopAbortHook func(ctx context.Context, execID, reason string) error
var stopRuntime RuntimeLifecycle

// SetStopAbortHook injects the live-session abort hook used by StopSequence.
func SetStopAbortHook(fn func(ctx context.Context, execID, reason string) error) { stopAbortHook = fn }

// SetStopRuntime injects the runtime lifecycle used to reap containers after Stop.
func SetStopRuntime(rl RuntimeLifecycle) { stopRuntime = rl }

// StopSequence halts a work item and, when it is a sequence parent, its whole subtree: every
// descendant → pending (a RECURRING item keeps its cadence), scheduled starts cleared.
func StopSequence(ctx context.Context, pool *db.Pool, log *slog.Logger, tenantID, parentID string) error {
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	var abortedExecs []string
	var abortedRunIDs []string
	if err := haltWorkItemCollect(ctx, ttx.Tx, tenantID, parentID, &abortedExecs, &abortedRunIDs); err != nil {
		return err
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if stopAbortHook != nil {
		for _, eid := range abortedExecs {
			if err := stopAbortHook(context.Background(), eid, "sequence stopped"); err != nil && log != nil {
				log.Warn("stop: abort live session failed", "execution", eid, "error", err)
			}
		}
	}
	if stopRuntime != nil {
		for _, rid := range abortedRunIDs {
			if err := stopRuntime.ReapForRun(context.Background(), rid); err != nil && log != nil {
				log.Warn("stop: reap runtime failed", "run", rid, "error", err)
			}
		}
	}
	return nil
}

// haltWorkItem is the legacy non-collecting wrapper kept for tests.
func haltWorkItem(ctx context.Context, tx pgx.Tx, tenantID, itemID string) error {
	var dummyExecs []string
	var dummyRuns []string
	return haltWorkItemCollect(ctx, tx, tenantID, itemID, &dummyExecs, &dummyRuns)
}

// haltWorkItemCollect parks a single work item and recursively halts every descendant.
func haltWorkItemCollect(ctx context.Context, tx pgx.Tx, tenantID, itemID string, execs *[]string, runIDs *[]string) error {
	item, err := db.GetWorkItem(ctx, tx, tenantID, itemID)
	if err != nil {
		return err
	}
	if domain.WorkItemIsTerminalSuccess(item.Status) {
		return nil
	}
	if item.WorkflowRunID != "" {
		aborted, err := workflow.AbortRunInTx(ctx, tx, tenantID, item.WorkflowRunID, domain.WorkItemPending)
		switch {
		case err == nil:
			*execs = append(*execs, aborted...)
			*runIDs = append(*runIDs, item.WorkflowRunID)
		case errors.Is(err, db.ErrNotFound):
		default:
			return fmt.Errorf("abort bound run %s: %w", item.WorkflowRunID, err)
		}
	}
	children, err := db.ListDirectChildren(ctx, tx, tenantID, itemID)
	if err != nil {
		return fmt.Errorf("list children: %w", err)
	}
	for _, c := range children {
		if err := haltWorkItemCollect(ctx, tx, tenantID, c.ID, execs, runIDs); err != nil {
			return err
		}
	}
	return parkWorkItemResilient(ctx, tx, tenantID, itemID)
}

// parkWorkItemResilient parks a single item with bounded optimistic-lock retry and an unconditional
// fallback.
func parkWorkItemResilient(ctx context.Context, tx pgx.Tx, tenantID, itemID string) error {
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		fresh, err := db.GetWorkItem(ctx, tx, tenantID, itemID)
		if err != nil {
			return err
		}
		if domain.WorkItemIsTerminalSuccess(fresh.Status) {
			return nil
		}
		status := domain.WorkItemPending
		if len(fresh.RecurringSchedule) > 0 {
			status = domain.WorkItemRecurring
		}
		empty := ""
		_, err = db.UpdateWorkItem(ctx, tx, tenantID, itemID, fresh.Version, db.UpdateWorkItemFields{
			Status:                &status,
			ClearScheduledStartAt: true,
			WorkflowRunID:         &empty,
		})
		if err == nil {
			return nil
		}
		if errors.Is(err, db.ErrVersionConflict) {
			continue
		}
		if errors.Is(err, db.ErrNotFound) {
			return err
		}
		return fmt.Errorf("park work item %s: %w", itemID, err)
	}
	// Fallback: unconditional park without version (tolerant path).
	fresh, err := db.GetWorkItem(ctx, tx, tenantID, itemID)
	if err != nil {
		return err
	}
	if domain.WorkItemIsTerminalSuccess(fresh.Status) {
		return nil
	}
	if _, err := db.ParkWorkItem(ctx, tx, tenantID, itemID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return err
		}
		return fmt.Errorf("park work item %s: %w", itemID, err)
	}
	return nil
}

// fireLeafStarts starts the armed leaf workflows after the reconciler transaction commits,
// self-healing any child whose start fails (reset to pending so the next pass re-arms).
func fireLeafStarts(ctx context.Context, pool *db.Pool, log *slog.Logger, starts []leafStart, start StartWorkflowFn) {
	for _, s := range starts {
		if err := start(ctx, s.tenantID, s.workflowID, s.projectID, s.itemID); err != nil {
			if log != nil {
				log.Warn("sequence: start child workflow failed",
					"child", s.itemID, "workflow", s.workflowID, "error", err)
			}
			// Self-heal (same rationale as the reconciler path): a failed
			// start must not strand the child as running-with-no-run.
			resetArmedChild(ctx, pool, s.tenantID, s.itemID)
		}
	}
}

// resetArmedChild resets a leaf child back to pending after its workflow start failed, so the
// derived cursor re-arms it on the next pass.
func resetArmedChild(ctx context.Context, pool *db.Pool, tenantID, childID string) {
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	child, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, childID)
	if err != nil {
		return
	}
	if child.Status != domain.WorkItemRunning || child.WorkflowRunID != "" {
		return
	}
	// Increment attempt counter + heartbeat via direct SQL (avoid version dance for system field).
	now := time.Now().UTC()
	_, _ = ttx.Tx.Exec(ctx, `UPDATE work_items SET status='pending', sequence_attempts=sequence_attempts+1, sequence_last_attempt_at=$1, updated_at=now(), version=version+1 WHERE id=$2 AND tenant_id=$3 AND status='running' AND workflow_run_id=''`, now, childID, tenantID)
	_ = ttx.Commit(ctx)
}

// reIsCASConflict reports whether err is an optimistic-concurrency version conflict
// (db.ErrNotFound from UpdateWorkItem version check or the explicit revive CAS string).
func reIsCASConflict(err error) bool {
	if errors.Is(err, db.ErrVersionConflict) {
		return true
	}
	if errors.Is(err, db.ErrNotFound) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "CAS conflict") || strings.Contains(s, "version") && strings.Contains(s, "concurrency")
}
