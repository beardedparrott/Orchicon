package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/workflow"
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
//   - fire (scheduled / run-instant): parent → running, every NON-terminal
//     descendant resets to pending (succeeded/skipped are always kept),
//     the first non-succeeded child arms (flips straight to running,
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
// created_at. On each pass every PENDING child is evaluated for arming —
// it arms when its dependency gate passes (all incoming depends_on/blocks
// edges satisfied) AND, for a strict sequence child (no incoming
// dependency edges), when its chain position is reached (its immediate
// predecessor in sort_order is succeeded). Unrelated siblings arm
// concurrently; strict chains stay serialized by construction (a just-armed
// predecessor is non-terminal). Every decision is a pure function of
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
		// Recurring sequence parents stay "recurring" so the
		// RecurringFireReconciler can fire the next cycle; the
		// next_run_at was pre-advanced before the fire.
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

	// Failure pre-scan: a failed/cancelled child ANYWHERE in the sequence
	// halts the WHOLE chain — the parent (and every container up the
	// ancestor chain) goes failed, and NOTHING arms in this pass, even a
	// dependency-governed sibling whose edges are otherwise satisfied.
	// Status-based, not position-based, so a mid-run reorder cannot skip an
	// unfixed failure. This preserves the strict-chain halt/retry semantics
	// while the per-child arming loop below relaxes serialization for every
	// non-failure case.
	for _, c := range children {
		if c.Status == domain.WorkItemFailed || c.Status == domain.WorkItemCancelled {
			if err := failSequenceChain(ctx, tx, tenantID, c); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}

	// Retry/resume: a parent that was FAILED (chain halted on a failed
	// child) is revived to running as soon as its children are no longer
	// halted — the derived cursor resumes with the next sibling, no
	// explicit "resume sequence" action. The scan includes failed parents
	// so this converges automatically. The failure pre-scan above
	// guarantees no failed/cancelled child remains when this fires.
	if parent.Status == domain.WorkItemFailed {
		status := domain.WorkItemRunning
		if _, err := db.UpdateWorkItem(ctx, tx, tenantID, parentID, parent.Version, db.UpdateWorkItemFields{
			Status: &status,
		}); err != nil {
			return nil, fmt.Errorf("revive sequence parent: %w", err)
		}
	}

	// Classify each child as strict-sequence (no incoming dependency edges)
	// vs dependency-governed (has incoming edges) in one query. A strict
	// child's ordering is its chain position; a dependency-governed child's
	// ordering is its edges — it has no chain gate.
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

	// Per-child arming loop over ALL children in sort_order. Each pending
	// child arms independently when its gates pass, so unrelated siblings
	// dispatch concurrently (parallelism) while strict chains keep their
	// order:
	//
	//   - Dependency gate (every child): all incoming depends_on/blocks
	//     edges satisfied (CheckDependenciesSatisfied). Unsatisfied → park
	//     (stay pending, never fail — the existing gate semantics).
	//   - Chain gate (strict children only): a strict child arms only when
	//     its immediate predecessor in sort_order is terminal-success. The
	//     loop's statuses are the pre-pass snapshot, so a just-armed
	//     predecessor still shows pending (non-terminal) and blocks the
	//     next strict child even within this same pass — strict chains stay
	//     serialized by construction.
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
			// Chain gate (strict children only): the child's position in
			// the chain is reached only when its immediate predecessor is
			// terminal-success (succeeded or skipped). Dependency-governed
			// children are ordered by their edges, so they skip this gate.
			// (A blocked child is always dependency-governed by
			// construction — it only got blocked via an unsatisfied edge.)
			if !depGoverned[c.ID] && i > 0 && !domain.WorkItemIsTerminalSuccess(children[i-1].Status) {
				continue // chain position not reached — wait for the predecessor
			}
			// Dependency gate: an unsatisfied external blocker parks this
			// child until the blockers succeed — then the next pass arms
			// automatically. The park is now SURFACED: a pending child
			// with unsat deps flips to blocked (persisted) so operators
			// see WHY nothing is dispatching, instead of a silent gray
			// pending pill. A blocked child stays blocked; when the gate
			// satisfies it flips back to pending and falls through to arm
			// in this same pass.
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
			// Gate satisfied: a blocked child clears back to pending (the
			// on-deck set) before arming below. Carry the fresh version
			// forward so the arm's CAS still matches (this pass updates the
			// child twice).
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

// deriveNextChild returns the index of the first direct child in the
// (already sort_order-ordered) slice whose status is not terminal-success,
// or -1 when every child is terminal-success (succeeded or skipped). This
// is the sequence cursor: a pure function of (sort_order, statuses),
// recomputed on every reconcile pass. A skipped child counts as "past" so
// it never wedges the chain on itself.
func deriveNextChild(children []db.WorkItemRow) int {
	for i, c := range children {
		if !domain.WorkItemIsTerminalSuccess(c.Status) {
			return i
		}
	}
	return -1
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
				fields := db.UpdateWorkItemFields{}
				if len(parent.RecurringSchedule) > 0 {
					// Recurring sequence parents: a failed cycle does NOT
					// kill the schedule — the parent stays "recurring" with
					// next_run_at intact so the RecurringFireReconciler
					// fires the next occurrence, which resets the subtree
					// and re-runs the chain fresh.
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

// resetSubtree sets every non-terminal descendant of parentID to pending,
// recursively. Succeeded/skipped descendants are ALWAYS terminal and are
// never reset (their subtree is already done too) — a START re-arms the
// first non-succeeded child and preserves prior successes.
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

// StartSequence implements StartSequenceFn: it fires a sequence run for a
// parent work item with children — parent → running, every NON-terminal
// descendant reset to pending (succeeded/skipped are always preserved),
// first non-succeeded child armed. Validation of the subtree
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

	// Parent → running; every NON-terminal descendant resets to pending
	// (succeeded/skipped are always preserved). Also clear a
	// stale workflow binding on the parent: a parent with children IS a
	// sequence container (its own workflow_id is ignored — children each
	// run their own workflows), and leaving the stale binding would keep
	// the row contradicting the mode (a bound-run ticket vs a sequence).
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

// ResumeSequence resumes a sequence parent from its current state —
// parent → running, and the engine derives the first non-succeeded child
// and arms it (reusing reconcileParent). Unlike StartSequence it does NOT
// reset the subtree: prior child successes are preserved, and the chain
// continues from where it left off. This is the manual counterpart to the
// auto-resume path (a failed parent whose children are no longer halted
// revives on the next scan); it exists for the two cases the derived
// cursor cannot act on alone:
//
//   - Parked: a parent parked by StopSequence (status pending) is never
//     picked up by the scan, so Resume is how it re-enters the chain
//     without destroying history.
//   - Halted: a parent failed by failSequenceChain (first non-succeeded
//     child failed/cancelled) would re-halt if handed to reconcileParent
//     unchanged, so Resume first re-arms that child to pending — the same
//     input change the manual "set failed child to pending" produces —
//     which lets the auto-revive + arm path continue the chain.
//
// Validation of the subtree (workflows bound, no one-shots) is the
// CALLER's responsibility and runs before this (mirrors StartSequence).
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

	// Halted-chain resume: a parent whose first non-succeeded child is
	// failed/cancelled has halted (failSequenceChain marked the parent
	// failed, and reconcileParent's failed-child branch re-halts it). A
	// manual resume must re-arm that child — set it back to pending so the
	// engine's derived cursor treats it as on-deck and the auto-revive
	// branch (parent failed + first child no longer halted) re-arms it.
	// This is byte-for-byte the same input change the manual "set failed
	// child to pending" produces, so parent-level Resume ≡ child-level
	// pending by construction. Only the FIRST non-succeeded child is
	// reset: reconcileParent's failure pre-scan keeps multi-failure chains
	// parked on the remaining failures (each must be resolved in turn), and
	// a container child re-arms its own subtree through the existing
	// container arm path.
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

	// Parent → running; clear a stale workflow binding on the parent
	// (same rationale as StartSequence — a parent with children IS a
	// sequence container, its own workflow_id is ignored). No subtree
	// reset: resume keeps history.
	status := domain.WorkItemRunning
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, parentID, parent.Version, db.UpdateWorkItemFields{
		Status:     &status,
		WorkflowID: strPtr(""),
	}); err != nil {
		return fmt.Errorf("resume sequence parent: %w", err)
	}
	// Arm the first non-succeeded child in sort_order (reuses the
	// engine's advance logic; succeeded children are already terminal, so
	// the derived cursor passes over them).
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

// StopSequence halts a work item and, when it is a sequence parent, its
// whole subtree: every descendant → pending (a RECURRING item keeps its
// cadence), scheduled starts cleared, and any in-flight workflow run bound
// to a descendant aborted (run → aborted, step runs → failed, worker
// executions → terminated). A leaf (no children) is halted on its own —
// its bound run (if any) is aborted and it is parked to pending.
//
// The parked state is deliberately NOT running/failed: the sequence scan
// only advances running/failed parents and the auto-revive path only
// resurrects a FAILED parent, so a stopped chain (or leaf) stays stopped
// until explicitly STARTed/RESUMEd. This is how a chain is halted without
// destroying history — children can then be run standalone, and Resume
// re-enters from the first non-succeeded child.
func StopSequence(ctx context.Context, pool *db.Pool, log *slog.Logger, tenantID, parentID string) error {
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	if err := haltWorkItem(ctx, ttx.Tx, tenantID, parentID); err != nil {
		return err
	}
	return ttx.Commit(ctx)
}

// haltWorkItem parks a single work item (pending, schedule cleared) and,
// for a sequence parent, recursively halts every descendant. Any in-flight
// workflow run bound to the item or a descendant is aborted via
// AbortRunInTx (run → aborted, step runs → failed, executions →
// terminated) with the bound work item left pending so it can be re-run.
func haltWorkItem(ctx context.Context, tx pgx.Tx, tenantID, itemID string) error {
	item, err := db.GetWorkItem(ctx, tx, tenantID, itemID)
	if err != nil {
		return err
	}
	// Terminal-success descendants are ALWAYS terminal — STOP must never
	// reset or re-arm a succeeded/skipped item (or re-recurse into its
	// subtree). Parks only non-terminal items.
	if domain.WorkItemIsTerminalSuccess(item.Status) {
		return nil
	}
	// Abort any in-flight bound run on this item (a leaf task with a live
	// run, or a bound container). Idempotent: terminal runs are skipped.
	// The bound work item is left PENDING so it can be re-run standalone.
	// A bound run id that no longer exists (stale binding, or a run that
	// was already cleaned up) is tolerated — there is nothing to abort, and
	// the binding is cleared below when the item is parked.
	if item.WorkflowRunID != "" {
		aborted, err := workflow.AbortRunInTx(ctx, tx, tenantID, item.WorkflowRunID,
			domain.WorkItemPending)
		switch {
		case err == nil:
			// The executions are now terminal; the sequence reconciler runs on
			// the dispatch path, so session abort happens asynchronously via
			// the terminal-execution handling. The IDs are returned for
			// completeness.
			_ = aborted
		case errors.Is(err, db.ErrNotFound):
			// The run row is gone — nothing in flight to abort. Fall through
			// and park the item (clearing the stale binding).
		default:
			return fmt.Errorf("abort bound run %s: %w", item.WorkflowRunID, err)
		}
	}
	// Recurse into children (a parent IS a sequence container).
	children, err := db.ListDirectChildren(ctx, tx, tenantID, itemID)
	if err != nil {
		return fmt.Errorf("list children: %w", err)
	}
	for _, c := range children {
		if err := haltWorkItem(ctx, tx, tenantID, c.ID); err != nil {
			return err
		}
	}

	// Park this item: pending (a RECURRING item keeps its cadence armed),
	// schedule cleared, stale run binding cleared so a later START/RESUME
	// can dispatch fresh. Re-read for a FRESH version: aborting a bound run
	// on THIS very item (AbortRunInTx) updated its status and bumped its
	// version, so the version captured at the top of this function is
	// stale — updating with it would fail the optimistic concurrency check.
	status := domain.WorkItemPending
	if len(item.RecurringSchedule) > 0 {
		status = domain.WorkItemRecurring
	}
	fresh, err := db.GetWorkItem(ctx, tx, tenantID, itemID)
	if err != nil {
		return err
	}
	empty := ""
	if _, err := db.UpdateWorkItem(ctx, tx, tenantID, itemID, fresh.Version, db.UpdateWorkItemFields{
		Status:                &status,
		ClearScheduledStartAt: true,
		WorkflowRunID:         &empty,
	}); err != nil {
		return fmt.Errorf("park work item %s: %w", itemID, err)
	}
	return nil
}

// fireLeafStarts starts the armed leaf workflows after the reconciler
// transaction commits, self-healing any child whose start fails (reset to
// pending so the next pass re-arms). Shared by StartSequence and
// ResumeSequence.
func fireLeafStarts(ctx context.Context, pool *db.Pool, log *slog.Logger, starts []leafStart, start StartWorkflowFn) {
	for _, s := range starts {
		if err := start(ctx, s.tenantID, s.workflowID, s.projectID, s.itemID); err != nil {
			log.Warn("sequence: start child workflow failed",
				"child", s.itemID, "workflow", s.workflowID, "error", err)
			// Self-heal (same rationale as the reconciler path): a failed
			// start must not strand the child as running-with-no-run.
			resetArmedChild(ctx, pool, s.tenantID, s.itemID)
		}
	}
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
