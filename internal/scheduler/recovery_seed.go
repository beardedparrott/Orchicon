package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/transcript"
)

// Recovery seeding (.orchicon/worker.recovery).
//
// A recovery-resumed dispatch starts almost from zero: the goal is just
// the work item title and the only injected recovery context is a
// metadata-only summary. That makes the replacement worker redo a large
// share of the dead session's work. The fix: when a recovery-resumed
// dispatch runs for the SAME worker that died, the TaskReconciler writes
// a gitignored file <project_dir>/.orchicon/worker.recovery carrying the
// dead session's rendered transcript tail + the "already done" directive,
// and the composite (system prompt) references the file. A genuinely new
// session for a different worker never sees the reference or the file.
//
// The file is written at dispatch time (not by the recovery engine) so
// the same-worker check is exact and no stale file survives a recovery
// that never re-dispatches. Cleanup is two-layer: the worker deletes the
// file itself before printing its summary (the file's own directive), and
// the scheduler deletes it on success when the file's footer matches the
// recovery execution that just completed.

const (
	// recoveryFileDir is the gitignored per-project directory that holds
	// recovery seeding (and per-run step results).
	recoveryFileDir = ".orchicon"
	// recoveryFileName is the single recovery seed file; it is
	// last-write-wins per project dir and each worker only trusts a file
	// whose header/footer names its own worker + execution.
	recoveryFileName = "worker.recovery"

	// recoveryTailMaxParts is how many transcript parts the dead session's
	// tail seeds (operator: "last ~50+ parts").
	recoveryTailMaxParts = 60
	// recoveryTailMaxBytes caps the rendered tail (operator band 16-32KB;
	// 24KB keeps the file cheap to ingest even on a 32K-context local
	// model).
	recoveryTailMaxBytes = 24 * 1024

	// recoveryHeaderPrefix is the mandatory first line of the file.
	recoveryHeaderPrefix = "This is for '%s' worker only because you stalled in a previous session. If you are not this worker, do not read the rest of this file."

	// recoveryDirective tells the worker it may terminate immediately by
	// re-printing its summary; it also instructs the worker to delete the
	// file, which is the primary cleanup mechanism for the fast path.
	recoveryDirective = "If you are already done with your work, please print your ORCHICON WORKER SUMMARY. If you already did that, please just print the summary itself again and then finish. When you finish, delete this file (rm .orchicon/worker.recovery) so a future session for a different worker is never confused by it."

	// recoveryFooterExecLine / recoveryFooterWorkerLine are the LAST lines
	// of the file. The system-side cleanup matcher reads them so it never
	// deletes a NEWER recovery's file.
	recoveryFooterExecLine   = "# recovery-execution-id: "
	recoveryFooterWorkerLine = "# worker-id: "

	// recoveryFileReferenceMarker is the exact text the composite's
	// ## Recovery block uses to point at the file. It doubles as the
	// idempotency marker for ensureRecoveryFileReference: the workflow and
	// standalone composite builders emit the block natively (same marker),
	// so the dispatch-time append is a no-op for them and only patches
	// legacy direct-dispatch composites built without the block.
	recoveryFileReferenceMarker = "Read the file `.orchicon/worker.recovery`"
)

// recoverySeed is the recovery context the dispatch path acts on. It is
// nil when the dispatch is NOT a recovery-resumed dispatch for the same
// worker — the single predicate both composite builders and the file
// writer share, so they can never disagree.
type recoverySeed struct {
	Summary        string // the recovery engine's one-line narrative
	FailedExecID   string // the dead execution's id
	FailedWorkerID string // the worker the dead execution ran on
}

// recoverySeedFor returns the recovery seed for a dispatch, or nil when
// this dispatch is NOT a recovery-resumed dispatch for the SAME worker.
// The seed's keys come from the step-run result (workflow path) or the
// work item's Results (standalone path). All three _recovery_* keys must
// be present and FailedWorkerID must equal the dispatching worker id —
// otherwise a different worker assigned to the same task gets no seed (no
// file, no composite reference).
func recoverySeedFor(runResult, wiResults []byte, workerID string) *recoverySeed {
	var seed recoverySeed
	found := false
	read := func(raw []byte) {
		var m map[string]any
		if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
			return
		}
		if seed.Summary == "" {
			if s, ok := m["_recovery_summary"].(string); ok && s != "" {
				seed.Summary = s
			}
		}
		if seed.FailedExecID == "" {
			if e, ok := m["_recovery_execution_id"].(string); ok && e != "" {
				seed.FailedExecID = e
				found = true
			}
		}
		if seed.FailedWorkerID == "" {
			if w, ok := m["_recovery_worker_id"].(string); ok && w != "" {
				seed.FailedWorkerID = w
			}
		}
	}
	read(runResult)
	read(wiResults)
	if !found || seed.FailedExecID == "" || seed.FailedWorkerID == "" || seed.FailedWorkerID != workerID {
		return nil
	}
	return &seed
}

// recoveryFileReferenceBlock is the ## Recovery prompt block that tells a
// recovery-resumed worker to read the seed file. It is the shared text
// the workflow + standalone composite builders emit natively and
// ensureRecoveryFileReference appends to legacy composites (idempotent via
// recoveryFileReferenceMarker).
func recoveryFileReferenceBlock(seed *recoverySeed) string {
	var sb strings.Builder
	sb.WriteString("## Recovery\n\n")
	sb.WriteString("A previous execution of this step stalled and was recovered. This is your recovery-resumed session. " + recoveryFileReferenceMarker + " in the project working directory — it contains the dead session's transcript tail and instructions. Do NOT redo work that is already done. If you are already done with your work, print your ORCHICON WORKER SUMMARY (or re-print it if you already did) and finish.\n\n")
	if seed != nil && seed.Summary != "" {
		sb.WriteString("Summary: " + seed.Summary + "\n")
	}
	return sb.String()
}

// ensureRecoveryFileReference appends the ## Recovery reference block to a
// composite that lacks it (legacy direct-dispatch composites built without
// the block). Idempotent: if the marker line is already present the
// composite is returned unchanged.
func ensureRecoveryFileReference(systemPrompt string, seed *recoverySeed) string {
	if strings.Contains(systemPrompt, recoveryFileReferenceMarker) {
		return systemPrompt
	}
	if seed == nil {
		return systemPrompt
	}
	var sb strings.Builder
	sb.WriteString(systemPrompt)
	if systemPrompt != "" && !strings.HasSuffix(systemPrompt, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(recoveryFileReferenceBlock(seed))
	return sb.String()
}

// buildRecoveryFileContent assembles the .orchicon/worker.recovery file:
// the mandatory header line, the ## Recovery section (summary + the
// already-done directive), the capped transcript tail, and the footer
// (LAST lines) the system-side cleanup matcher uses.
func buildRecoveryFileContent(workerName string, seed *recoverySeed, tail string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, recoveryHeaderPrefix+"\n\n", workerName)
	sb.WriteString("## Recovery\n\n")
	if seed.Summary != "" {
		sb.WriteString(seed.Summary + "\n\n")
	}
	sb.WriteString(recoveryDirective + "\n")
	if tail != "" {
		sb.WriteString("\n## Dead session transcript (tail)\n\n")
		sb.WriteString(tail)
		if !strings.HasSuffix(tail, "\n") {
			sb.WriteString("\n")
		}
	}
	// Footer must be the last lines of the file.
	sb.WriteString("\n")
	sb.WriteString(recoveryFooterExecLine + seed.FailedExecID + "\n")
	sb.WriteString(recoveryFooterWorkerLine + seed.FailedWorkerID + "\n")
	return sb.String()
}

// seedRecoveryFile is the dispatch-time hook. It writes the seed file when
// this dispatch is a same-worker recovery-resumed dispatch, removes any
// stale file otherwise (safety net for a worker reassignment), and returns
// the composite with the ## Recovery reference block ensured. Best-effort:
// a failure to write must never fail dispatch — the caller logs and
// continues. The returned prompt replaces the one passed in.
func (r *TaskReconciler) seedRecoveryFile(ctx context.Context, exec db.ExecutionRow, task db.WorkItemRow, version db.WorkerVersionRow, projectDir string, stepRunResult []byte, systemPrompt string) string {
	if projectDir == "" {
		return systemPrompt
	}
	seed := recoverySeedFor(stepRunResult, task.Results, version.WorkerID)
	if seed == nil {
		r.removeRecoveryFile(projectDir)
		return systemPrompt
	}

	// Fetch the failed worker's display name (header) + the dead session's
	// transcript tail, all in one short read tx. Best-effort — missing data
	// degrades the file, never the dispatch.
	workerName := seed.FailedWorkerID
	tail := ""
	if exec.TenantID != "" {
		if ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID); err == nil {
			if w, err := db.GetWorker(ctx, ttx.Tx, exec.TenantID, seed.FailedWorkerID); err == nil && w.Name != "" {
				workerName = w.Name
			}
			if parts, err := db.ListExecutionSessionPartsTail(ctx, ttx.Tx, exec.TenantID, seed.FailedExecID, recoveryTailMaxParts); err == nil {
				tail = transcript.RenderTail(parts, recoveryTailMaxBytes)
			} else {
				r.log.Warn("recovery seed: read transcript tail", "execution", seed.FailedExecID, "error", err)
			}
			_ = ttx.Rollback(ctx)
		}
	}

	if err := r.writeRecoveryFile(projectDir, buildRecoveryFileContent(workerName, seed, tail)); err != nil {
		r.log.Warn("recovery seed: write file", "error", err)
	}
	r.log.Info("recovery seed written", "execution", exec.ID, "failed_execution", seed.FailedExecID, "worker", seed.FailedWorkerID)
	return ensureRecoveryFileReference(systemPrompt, seed)
}

// writeRecoveryFile writes the seed file atomically (temp + rename).
func (r *TaskReconciler) writeRecoveryFile(projectDir, content string) error {
	orchDir := filepath.Join(projectDir, recoveryFileDir)
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(orchDir, recoveryFileName)
	tmp, err := os.CreateTemp(orchDir, recoveryFileName+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// removeRecoveryFile deletes the seed file for a project dir, ignoring a
// missing file.
func (r *TaskReconciler) removeRecoveryFile(projectDir string) {
	if projectDir == "" {
		return
	}
	if err := os.Remove(filepath.Join(projectDir, recoveryFileDir, recoveryFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.log.Warn("recovery seed: remove stale file", "error", err)
	}
}

// removeRecoveryFileMatching deletes the seed file ONLY when its footer
// matches the given recovery execution + worker, so a success never
// removes a NEWER recovery's file. Mismatch or missing file → no-op.
func (r *TaskReconciler) removeRecoveryFileMatching(projectDir, execID, workerID string) {
	if projectDir == "" || execID == "" || workerID == "" {
		return
	}
	path := filepath.Join(projectDir, recoveryFileDir, recoveryFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(b)
	if !strings.Contains(content, recoveryFooterExecLine+execID) || !strings.Contains(content, recoveryFooterWorkerLine+workerID) {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.log.Warn("recovery seed: remove on success", "error", err)
	}
}

// recoverySeedForSuccess resolves the recovery execution + worker a
// completed execution was resuming. Standalone: the keys ride in the
// results map (carried forward from the work item's Results). Workflow:
// the keys live on the step run's result (recovery engine + re-dispatch
// preservation); the results map here is the ticket's, so read the step
// run directly.
func (r *TaskReconciler) recoverySeedForSuccess(ctx context.Context, exec db.ExecutionRow, results map[string]any) (execID, workerID string) {
	execID, _ = results["_recovery_execution_id"].(string)
	workerID, _ = results["_recovery_worker_id"].(string)
	if execID != "" && workerID != "" {
		return
	}
	if exec.TenantID == "" || exec.WorkflowRunID == "" || exec.WorkflowStepID == "" {
		return
	}
	if ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID); err == nil {
		if sr, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, exec.TenantID, exec.WorkflowRunID, exec.WorkflowStepID); err == nil {
			var m struct {
				ExecID   string `json:"_recovery_execution_id"`
				WorkerID string `json:"_recovery_worker_id"`
			}
			_ = json.Unmarshal(sr.Result, &m)
			execID, workerID = m.ExecID, m.WorkerID
		}
		_ = ttx.Rollback(ctx)
	}
	return
}

// removeRecoveryFileForSuccess is the system-side cleanup: when a
// recovery-resumed execution succeeds, delete the seed file whose footer
// matches this execution (so the file never lingers across workflows or
// projects, and never deletes a newer recovery's file).
func (r *TaskReconciler) removeRecoveryFileForSuccess(ctx context.Context, exec db.ExecutionRow, results map[string]any, succeeded bool) {
	if !succeeded {
		return
	}
	execID, workerID := r.recoverySeedForSuccess(ctx, exec, results)
	if execID == "" || workerID == "" {
		return // not a recovery-resumed success (or keys lost) — leave the file
	}
	projectDir := r.lookupProjectDir(ctx, exec.ProjectID)
	if projectDir == "" {
		return
	}
	r.removeRecoveryFileMatching(projectDir, execID, workerID)
}
