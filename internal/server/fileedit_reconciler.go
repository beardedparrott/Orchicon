package server

import (
	"context"
	"log/slog"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/fileedit"
)

// fileEditReconciler is the server's implementation of
// scheduler.FileEditReconciler: at an execution's terminal transition it
// reconciles the durable ledger against the run worktree's git state
// (AC 4). It resolves the reconciliation dir from the execution row —
// WorktreePath when the run had a provisioned worktree, else ProjectDir
// (in-place / Ask-local runs) — then delegates to fileedit.ReconcileGit.
// Every failure is logged, never propagated: reconciliation must never
// block or fail the terminal writeback (same posture as usage recording).
type fileEditReconciler struct {
	pool *db.Pool
	log  *slog.Logger
}

// newFileEditReconciler wires the completion-time ledger reconciler.
func newFileEditReconciler(pool *db.Pool, log *slog.Logger) *fileEditReconciler {
	return &fileEditReconciler{pool: pool, log: log}
}

// ReconcileExecutionGit implements scheduler.FileEditReconciler.
func (f *fileEditReconciler) ReconcileExecutionGit(ctx context.Context, tenantID, execID string) {
	dir := f.reconcileDir(ctx, tenantID, execID)
	if dir == "" {
		return // no worktree / no project dir: nothing to reconcile against
	}
	store := fileedit.NewPGStore(f.pool)
	if err := fileedit.ReconcileGit(ctx, store, dir, tenantID, db.FileEditOwnerExecution, execID, f.log); err != nil {
		f.log.Warn("file edit ledger git reconciliation failed", "execution", execID, "dir", dir, "error", err)
	}
}

// reconcileDir resolves the directory the run's git truth lives in: the
// provisioned worktree when present, else the project dir (in-place runs).
// Returns "" when neither exists (non-filesystem runs) — reconciliation is
// then a no-op by contract.
func (f *fileEditReconciler) reconcileDir(ctx context.Context, tenantID, execID string) string {
	ttx, err := f.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		f.log.Warn("file edit reconcile: begin tx", "execution", execID, "error", err)
		return ""
	}
	defer ttx.Rollback(ctx)
	exec, err := db.GetExecution(ctx, ttx.Tx, tenantID, execID)
	if err != nil {
		f.log.Warn("file edit reconcile: get execution", "execution", execID, "error", err)
		return ""
	}
	if exec.WorktreePath != nil && *exec.WorktreePath != "" {
		return *exec.WorktreePath
	}
	proj, err := db.GetProject(ctx, ttx.Tx, tenantID, exec.ProjectID)
	if err != nil {
		f.log.Warn("file edit reconcile: get project", "execution", execID, "error", err)
		return ""
	}
	return proj.ProjectDir
}