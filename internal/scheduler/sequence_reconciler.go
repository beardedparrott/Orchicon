package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/jackc/pgx/v5"
)

// SequenceReconciler is the sequence engine for sequential multi-workflow
// runs (architecture-notes/sequential-multi-workflow-runs.md §2).
//
// A parent work item with children and NO bound workflow IS a sequence
// run (Option A — no sequence_runs table): the parent's status + its
// children's statuses fully describe the state. "Who's next" is derived,
// never stored: on every pass the engine recomputes the first direct
// child in sort_order whose status is not terminal-success. Because it is
// a pure function of (sort_order, statuses), a crash/restart mid-chain
// resumes correctly (idempotent derived cursor — acceptance criterion
// "Reconciliation stability").
//
//   - fire (scheduled / run-instant): parent → running, every descendant
//     resets to pending, the first child arms (flips straight to running,
//     its OWN bound workflow starts — no ready/assigned dance, no config
//     copy).
//   - advance: child succeeds → next non-succeeded sibling arms.
//   - completion: all children succeeded → parent → succeeded.
//   - failure halts: child fails → that child failed, later siblings stay
//     pending, parent (and each container up the chain) → failed; nothing
//     after the failure ever arms on its own.
//   - retry auto-resumes: fixing + retrying the failed child to success
//     continues with the next sibling (the cursor is derived).
//   - dependency gate: an on-deck child whose external blockers aren't
//     satisfied PARKS the chain (parent stays running, child stays
//     pending) until the blockers succeed — then it advances without
//     human action. Only a human may fail a blocked child.
//   - recursion: arming a container child starts its own nested sequence
//     (its descendants reset to pending); failure propagates up the chain.
type SequenceReconciler struct {
	pool  *db.Pool
	log   *slog.Logger
	start StartWorkflowFn
}

// StartSequenceFn starts a sequence run for a parent work item with
// children: flips the parent to running, resets every descendant to
// pending, and arms the first child in sort_order (whose own bound
// workflow starts). Mirrors StartWorkflowFn for the sequence case.
type StartSequenceFn func(ctx context.Context, tenantID, parentID string) error

// NewSequenceReconciler creates the sequence engine.
func NewSequenceReconciler(pool *db.Pool, log *slog.Logger, start StartWorkflowFn) *SequenceReconciler {
	return &SequenceReconciler{pool: pool, log: log, start: start}
}

// Kind returns the reconciler kind.
func (r *SequenceReconciler) Kind() string { return "sequence" }

// Reconcile processes one sequence parent (key = parent work item id), or
// scans all running sequence parents when the key is empty. Idempotent:
// re-running a pass derives the same next step.
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
		if err := r.reconcileOne(ctx, tenantID, p.ID); err != nil {
			r.log.Warn("sequence: reconcile parent failed", "parent", p.ID, "error", err)
		}
	}
	return reconciler.Result{}
}

// reconcileOne advances a single sequence parent. The state change is
// committed first; leaf workflow starts fire after commit (mirrors
// ScheduledRunReconciler's rollback-before-fire and WorkflowReconciler's
// post-commit dispatch).
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
			r.log.Error("sequence: start child workflow failed",
				"child", s.itemID, "workflow", s.workflowID, "error", err)
			// Self-heal: the child was flipped to running in the committed
			// tx; a failed start must not strand it as running-with-no-run
			// (the derived cursor would wait on it forever). Reset to
			// pending so the next pass re-arms.
			resetArmedChild(ctx, r.pool, s.tenantID, s.itemID)
		}
	}
	return nil
}

// leafStart is a child workflow to start AFTER the reconciler transaction
// commits — the workflow's own transaction would deadlock on the locks we
// hold while the child status flip is uncommitted.
type leafStart struct {
	tenantID   string
	workflowID string
	projectID  string
	itemID     string
}

// reconcileParent advances one sequence parent to its next derived step,
// performing all state changes in a single transaction and returning the
// leaf workflow starts to fire after commit. Idempotent: a repeated pass
// for the same parent converges to the same state.
//
// The derived cursor: children are ordered by sort_order NULLS LAST,
// created_at; the first child whose status is not terminal-success is the
// on-deck child. Every decision below is a pure function of
// (sort_order, statuses, dependencies) — there is no cursor row to drift.
func reconcileParent(ctx context.Context, tx pgx.Tx, tenantID, parentID string, start StartWorkflowFn) ([]leafStart, error) {
	parent, err := db.GetWorkItem(ctx, tx, tenantID, parentID)
	if err != nil {
		return nil, err // ErrNotFound → parent deleted; nothing to advance
	}
	// Sequence-parent guard: reconcileParent must only advance a work item
	// that IS a sequence run — status running (or failed, for retry-resume)
	// with NO bound workflow run and at least one child. This mirrors the
	// ListSequenceActiveParents scan predicate. The notifier path
	// (workflow_reconciler.go) fires for the parent of ANY terminal bound
	// work item; a non-sequence parent — a bound-run ticket, a never-fired
	// parent, or a terminal parent — must be a no-op, never force-arm its
	// children or spuriously mark it succeeded.
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
		if _, err := db.UpdateWorkItem(ctx, tx, tenantID, parentID, parent.Version, db.UpdateWorkItemFields{
			Status: &status,
		}); err != nil {
			return nil, fmt.Errorf("mark sequence parent succeeded: %w", err)
		}
		return nil, nil
	}
	first := children[idx]
	// Retry/resume: a parent that was FAILED (chain halted on a failed
	// child) is revived to running as soon as its children are no longer
	// halted — the derived cursor resumes with the next sibling, no
	// explicit "resume sequence" action. The scan includes failed parents
	// so this converges automatically.
	if parent.Status == domain.WorkItemFailed &&
		first.Status != domain.WorkItemFailed && first.Status != domain.WorkItemCancelled {
		status := domain.WorkItemRunning
		if _, err := db.UpdateWorkItem(ctx, tx, tenantID, parentID, parent.Version, db.UpdateWorkItemFields{
			Status: &status,
		}); err != nil {
			return nil, fmt.Errorf("revive sequence parent: %w", err)
		}
	}
	switch first.Status {
	case domain.WorkItemFailed, domain.WorkItemCancelled:
		// Failure halts the chain: the parent and every container up the
		// ancestor chain go failed; nothing after the failure arms.
		if err := failSequenceChain(ctx, tx, tenantID, first); err != nil {
			return nil, err
		}
		return nil, nil
	case domain.WorkItemRunning, domain.WorkItemAssigned, domain.WorkItemReady,
		domain.WorkItemCheckpointing, domain.WorkItemRecovering, domain.WorkItemScheduled:
		// In flight (or human-managed): wait for the current child.
		return nil, nil
	case domain.WorkItemPending:
		// On deck. Strict one-at-a-time: a mid-run drag (ReorderWorkItems)
		// may have sorted this pending sibling ahead of an in-flight child.
		// Never arm while any sibling is in flight — that would run two
		// sequence children concurrently (sequential execution is strict).
		// The derived cursor keeps waiting until the in-flight child
		// reaches a terminal state, then re-derives who's next.
		if anySiblingBlocksArming(children) {
			return nil, nil
		}
		// The dependency gate composes as a gate, not a halt: an
		// unsatisfied external blocker parks the chain on this child
		// (parent stays running, child stays pending) until the blockers
		// succeed — then the next pass arms automatically.
		satisfied, err := db.CheckDependenciesSatisfied(ctx, tx, tenantID, first.ID)
		if err != nil {
			return nil, fmt.Errorf("check deps: %w", err)
		}
		if !satisfied {
			return nil, nil // parked — do not arm, do not requeue, do not fail
		}
		// Arm. A container child starts a nested sequence; a leaf starts
		// its own bound workflow. No ready/assigned dance, no config copy.
		grandchildren, err := db.ListDirectChildren(ctx, tx, tenantID, first.ID)
		if err != nil {
			return nil, fmt.Errorf("list grandchildren: %w", err)
		}
		if len(grandchildren) > 0 {
			status := domain.WorkItemRunning
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, first.ID, first.Version, db.UpdateWorkItemFields{
				Status: &status,
			}); err != nil {
				return nil, fmt.Errorf("arm container child: %w", err)
			}
			// Its own chain resets to pending when the container arms
			// (nested sequence). The next scan pass reconciles it.
			if err := resetSubtree(ctx, tx, tenantID, first.ID); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if first.WorkflowID == nil || *first.WorkflowID == "" {
			// Config error — schedule-time validation should have
			// prevented this. Mark the child failed and cascade.
			status := domain.WorkItemFailed
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, first.ID, first.Version, db.UpdateWorkItemFields{
				Status: &status,
			}); err != nil {
				return nil, fmt.Errorf("mark unbound child failed: %w", err)
			}
			if err := failSequenceChain(ctx, tx, tenantID, first); err != nil {
				return nil, err
			}
			return nil, nil
		}
		status := domain.WorkItemRunning
		if _, err := db.UpdateWorkItem(ctx, tx, tenantID, first.ID, first.Version, db.UpdateWorkItemFields{
			Status: &status,
		}); err != nil {
			return nil, fmt.Errorf("arm leaf child: %w", err)
		}
		return []leafStart{{tenantID, *first.WorkflowID, first.ProjectID, first.ID}}, nil
	}
	return nil, nil
}

// deriveNextChild returns the index of the first direct child in the
// (already sort_order-ordered) slice whose status is not terminal-success,
// or -1 when every child succeeded. This is the sequence cursor: a pure
// function of (sort_order, statuses), recomputed on every reconcile pass.
func deriveNextChild(children []db.WorkItemRow) int {
	for i, c := range children {
		if c.Status != domain.WorkItemSucceeded {
			return i
		}
	}
	return -1
}

// anySiblingBlocksArming reports whether any child is in a state that
// must resolve before the on-deck pending child may arm: still executing
// (running/checkpointing/recovering), awaiting dispatch (assigned/ready),
// or halted (failed/cancelled). Guards two invariants:
//
//  1. Strict sequential execution: a mid-run ReorderWorkItems can sort a
//     pending sibling ahead of an in-flight child; arming it would start
//     two sequence children concurrently.
//  2. Failure halts the chain: a failed/cancelled child anywhere in the
//     sequence parks it — nothing after the failure ever arms on its own,
//     and the only way forward is fixing + retrying that child to success
//     (the derived cursor then re-derives who's next). A reorder that puts
//     a pending child before a failed sibling must not skip the unfixed
//     failure.
//
// Only terminal-success siblings are "past"; anything else still in flight
// or halted is waited on regardless of sort_order.
func anySiblingBlocksArming(children []db.WorkItemRow) bool {
	for _, c := range children {
		switch c.Status {
		case domain.WorkItemRunning, domain.WorkItemCheckpointing,
			domain.WorkItemRecovering, domain.WorkItemAssigned, domain.WorkItemReady,
			domain.WorkItemFailed, domain.WorkItemCancelled:
			return true
		}
	}
	return false
}

// failSequenceChain marks the failed child's parent — and every ancestor
// container up the chain that is sequence-running (status running, no
// bound workflow run, has children) — failed. Nothing after the failure
// ever arms on its own; the derived cursor keeps later siblings pending.
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
				if _, err := db.UpdateWorkItem(ctx, tx, tenantID, parent.ID, parent.Version, db.UpdateWorkItemFields{
					Status: &status,
				}); err != nil {
					return fmt.Errorf("fail sequence ancestor: %w", err)
				}
			}
		}
		cur = parent
	}
	return nil
}

// resetSubtree sets every descendant of parentID to pending, recursively.
// Prior successes from earlier manual runs are reset too (fire semantics).
// Descendants with an IN-FLIGHT bound run (running/checkpointing/
// recovering) are skipped with their whole subtree — the derived cursor
// waits for them instead of double-arming.
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

// StartSequence implements StartSequenceFn: it fires a sequence run for a
// parent work item with children — parent → running, every descendant
// reset to pending, first child armed. Validation of the subtree
// (workflows bound, no one-shots) is the CALLER's responsibility and runs
// before this (schedule-time validation, architecture-notes §3).
//
// Idempotent in the reconcile sense: re-invoking it for an already-running
// parent re-arms the first non-succeeded child (harmless), but callers
// should only fire once per schedule.
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

	// Parent → running; every descendant resets to pending.
	status := domain.WorkItemRunning
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, parentID, parent.Version, db.UpdateWorkItemFields{
		Status: &status,
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
	for _, s := range starts {
		if err := start(ctx, s.tenantID, s.workflowID, s.projectID, s.itemID); err != nil {
			log.Warn("sequence: start child workflow failed",
				"child", s.itemID, "workflow", s.workflowID, "error", err)
			// Self-heal (same rationale as the reconciler path): a failed
			// start must not strand the child as running-with-no-run.
			resetArmedChild(ctx, pool, s.tenantID, s.itemID)
		}
	}
	return nil
}

// resetArmedChild resets a leaf child back to pending after its workflow
// start failed, so the derived cursor re-arms it on the next pass. Only
// touches a child that is still running with no bound workflow run (i.e.
// the failed start left it mid-arm) — a child whose run actually started
// is left alone.
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
	status := domain.WorkItemPending
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, childID, child.Version, db.UpdateWorkItemFields{
		Status: &status,
	}); err == nil {
		_ = ttx.Commit(ctx)
	}
}
