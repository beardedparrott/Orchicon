package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/transcript"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
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
	recoveryDirective = "If you are already done with your work, please print your ORCHICON WORKER SUMMARY. If you already did that, please just print the summary itself again and then finish. When you finish, delete this file (rm .orchicon/worker.recovery) so a future session for a different worker is never confused by it. If `.orchicon/worker.recovery` is missing or unreadable when you start, do NOT redo any work — finish immediately with `ORCHICON WORKER SUMMARY: failure` and reason `recovery seed file missing`."

	// recoveryFailFastDirective is the last line of defense: the composite's
	// ## Recovery block instructs a recovery-resumed worker to fail fast when
	// the seed file it was promised is missing. It mirrors recoveryDirective
	// so the worker sees the contract in BOTH the prompt and the file itself.
	recoveryFailFastDirective = "If the file is missing or unreadable, do NOT redo work — finish immediately with decision `fail` and reason `recovery seed file missing`."

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

// recoveryIdentityFromResult extracts the dead-execution identity recorded
// on a step-run / work-item result. Order of preference mirrors the ADR:
// _failed_execution_id (persisted at the recovering transition) →
// _recovery_execution_id (published by the recovery engine).
func recoveryIdentityFromResult(runResult, wiResults []byte) (failedExecID string) {
	read := func(raw []byte) {
		var m map[string]any
		if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
			return
		}
		if failedExecID == "" {
			if e, ok := m["_failed_execution_id"].(string); ok && e != "" {
				failedExecID = e
			}
		}
		if failedExecID == "" {
			if e, ok := m["_recovery_execution_id"].(string); ok && e != "" {
				failedExecID = e
			}
		}
	}
	read(runResult)
	read(wiResults)
	return failedExecID
}

// resolveRecoverySeed is the SHARED seed resolver used by BOTH the
// workflow dispatch gate (recoveryDispatchReady) and the TaskReconciler's
// file writer, so the two can never disagree about whether a dispatch is a
// same-worker recovery-resumed dispatch:
//
//  1. fast path: recoverySeedFor on the step-run result + work item
//     results (the keys the recovery engine publishes on resume);
//  2. fallback: when no keys are present yet but a failed-execution id is
//     resolvable, build the seed from the most recent recovery row for
//     that execution — only if it is terminal `resumed`. The same-worker
//     gate (failed worker == dispatching worker) still applies.
//
// Returns nil when the dispatch is NOT a resolvable same-worker
// recovery-resumed dispatch.
func resolveRecoverySeed(ctx context.Context, tx pgx.Tx, tenantID, taskID string, runResult, wiResults []byte, workerID string) *recoverySeed {
	if seed := recoverySeedFor(runResult, wiResults, workerID); seed != nil {
		return seed
	}
	return recoverySeedFromRow(ctx, tx, tenantID, taskID, runResult, wiResults, workerID)
}

// recoverySeedFromRow is the DB fallback half of the shared resolver: when
// the engine-published keys are not yet on the step run / work item, build
// the seed from the most recent recovery row for the failed execution — but
// ONLY if it is terminal `resumed`. The same-worker gate (failed worker ==
// dispatching worker) still applies.
func recoverySeedFromRow(ctx context.Context, tx pgx.Tx, tenantID, taskID string, runResult, wiResults []byte, workerID string) *recoverySeed {
	failedExecID := recoveryIdentityFromResult(runResult, wiResults)
	if failedExecID == "" || tx == nil {
		return nil
	}
	rec, err := db.GetLatestRecoveryForExecution(ctx, tx, tenantID, taskID, failedExecID)
	if err != nil || rec.Status != domain.RecoveryResumed {
		return nil
	}
	failedWorkerID := ""
	if fe, err := db.GetExecution(ctx, tx, tenantID, rec.FailedExecutionID); err == nil {
		failedWorkerID = fe.WorkerID
	}
	if failedWorkerID == "" || failedWorkerID != workerID {
		return nil
	}
	return &recoverySeed{Summary: rec.Summary, FailedExecID: rec.FailedExecutionID, FailedWorkerID: failedWorkerID}
}

// recoveryFileReferenceBlock is the ## Recovery prompt block that tells a
// recovery-resumed worker to read the seed file. It is the shared text
// the workflow + standalone composite builders emit natively and
// ensureRecoveryFileReference appends to legacy composites (idempotent via
// recoveryFileReferenceMarker).
func recoveryFileReferenceBlock(seed *recoverySeed) string {
	var sb strings.Builder
	sb.WriteString("## Recovery\n\n")
	sb.WriteString("A previous execution of this step stalled and was recovered. This is your recovery-resumed session. " + recoveryFileReferenceMarker + " in the project working directory — it contains the dead session's transcript tail and instructions. Do NOT redo work that is already done. If you are already done with your work, print your ORCHICON WORKER SUMMARY (or re-print it if you already did) and finish. " + recoveryFailFastDirective + "\n\n")
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

// seedRecoveryFile is the dispatch-time hard gate. It writes (or verifies)
// .orchicon/worker.recovery when this dispatch is a same-worker
// recovery-resumed dispatch, and returns the composite with the ## Recovery
// reference block ensured. A nil seed (fresh / different-worker dispatch)
// proceeds unchanged — and never sweeps an existing file, which may belong
// to another in-flight recovery (a fresh worker is never pointed at it).
//
// This is NOT best-effort anymore: if the seed is resolvable but the file
// cannot be written+verified, an error is returned and the caller fails the
// dispatch (failed_to_start) instead of launching the session cold.
func (r *TaskReconciler) seedRecoveryFile(ctx context.Context, exec db.ExecutionRow, task db.WorkItemRow, version db.WorkerVersionRow, projectDir string, stepRunResult []byte, systemPrompt string) (string, error) {
	if projectDir == "" {
		return systemPrompt, nil
	}
	// Resolve the seed with the SHARED resolver (fast-path keys first —
	// no DB needed; fallback to the terminal-resumed recovery row in a
	// short read tx) so the gate and the file writer can never disagree.
	seed := recoverySeedFor(stepRunResult, task.Results, version.WorkerID)
	if seed == nil && exec.TenantID != "" {
		if ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID); err == nil {
			seed = recoverySeedFromRow(ctx, ttx.Tx, exec.TenantID, task.ID, stepRunResult, task.Results, version.WorkerID)
			_ = ttx.Rollback(ctx)
		}
	}
	if seed == nil {
		return systemPrompt, nil
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

	content := buildRecoveryFileContent(workerName, seed, tail)
	if err := r.writeRecoveryFileGate(projectDir, seed, content, exec); err != nil {
		recoverySeedMetricsSingleton.recordWriteFailure()
		return systemPrompt, err
	}
	recoverySeedMetricsSingleton.recordWritten()
	r.log.Info("recovery seed written", "execution", exec.ID, "failed_execution", seed.FailedExecID, "worker", seed.FailedWorkerID)
	return ensureRecoveryFileReference(systemPrompt, seed), nil
}

// writeRecoveryFileGate enforces the file half of the recovery-resume
// invariant: .orchicon/worker.recovery must exist, be non-empty, and carry
// the footer for THIS recovery execution + worker before the dispatch may
// proceed. Bounded retries (3 × short backoff) on write/verify failure.
func (r *TaskReconciler) writeRecoveryFileGate(projectDir string, seed *recoverySeed, content string, exec db.ExecutionRow) error {
	const attempts = 3
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		lastErr = r.writeRecoveryFileOnce(projectDir, seed, content, exec)
		if lastErr == nil {
			return nil
		}
		if attempt < attempts {
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
	}
	return lastErr
}

// writeRecoveryFileOnce performs one write/verify cycle: idempotent reuse
// when the file already carries the same footer, a foreign-seed ownership
// check before overwriting a different footer, then an atomic write +
// verify. Any failure returns an error so the dispatch can fail loud.
func (r *TaskReconciler) writeRecoveryFileOnce(projectDir string, seed *recoverySeed, content string, exec db.ExecutionRow) error {
	orchDir := filepath.Join(projectDir, recoveryFileDir)
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		return fmt.Errorf("recovery seed file could not be written: %w", err)
	}
	path := filepath.Join(orchDir, recoveryFileName)

	// Pre-existing file handling.
	if existing, err := os.ReadFile(path); err == nil {
		existingStr := string(existing)
		sameFooter := strings.Contains(existingStr, recoveryFooterExecLine+seed.FailedExecID) &&
			strings.Contains(existingStr, recoveryFooterWorkerLine+seed.FailedWorkerID)
		if sameFooter && len(existing) > 0 {
			// Same recovery owns the file → reuse idempotently (verify it).
			recoverySeedMetricsSingleton.recordSkipped("same_footer")
			return r.verifyRecoveryFile(projectDir, seed)
		}
		// Different footer → foreign seed. Never clobber a LIVE foreign
		// recovery's file; only overwrite when the owning recovery is
		// provably terminal (the prior replacement finished/aborted, so its
		// file is stale).
		ownerExec, _ := parseRecoveryFileFooter(existingStr)
		if ownerExec == "" || !r.recoveryOwnerTerminal(exec, ownerExec) {
			recoverySeedMetricsSingleton.recordSkipped("foreign_seed")
			return fmt.Errorf("recovery seed file could not be written: existing file belongs to another recovery (%s)", ownerExec)
		}
		r.log.Warn("recovery seed: overwriting stale foreign file", "owner_execution", ownerExec)
	}

	if err := r.writeRecoveryFile(projectDir, content); err != nil {
		return fmt.Errorf("recovery seed file could not be written: %w", err)
	}
	return r.verifyRecoveryFile(projectDir, seed)
}

// recoveryOwnerTerminal reports whether the recovery for a failed execution
// (identified by its footer's exec id) has reached a TERMINAL state. A file
// only exists because its recovery reached resumed and dispatched; a
// terminal owner therefore means the file is stale and safe to overwrite.
func (r *TaskReconciler) recoveryOwnerTerminal(exec db.ExecutionRow, failedExecID string) bool {
	if exec.TenantID == "" || failedExecID == "" {
		return false
	}
	if ttx, err := r.pool.BeginTenantTx(context.Background(), exec.TenantID); err == nil {
		defer ttx.Rollback(context.Background())
		rec, err := db.GetLatestRecoveryForFailedExecution(context.Background(), ttx.Tx, exec.TenantID, failedExecID)
		if err != nil {
			return false
		}
		return rec.Status == domain.RecoveryResumed || rec.Status == domain.RecoveryFailed || rec.Status == domain.RecoveryCancelled
	}
	return false
}

// parseRecoveryFileFooter reads the footer lines of a seed file back out.
func parseRecoveryFileFooter(content string) (execID, workerID string) {
	for _, line := range strings.Split(content, "\n") {
		if execID == "" && strings.HasPrefix(line, recoveryFooterExecLine) {
			execID = strings.TrimPrefix(line, recoveryFooterExecLine)
		}
		if workerID == "" && strings.HasPrefix(line, recoveryFooterWorkerLine) {
			workerID = strings.TrimPrefix(line, recoveryFooterWorkerLine)
		}
	}
	return
}

// verifyRecoveryFile stat-checks the file (exists, non-empty) and
// read-checks the footer against the expected seed. This is the final gate
// before bridge.Start: the worker must be able to trust the file exists
// with its own footer.
func (r *TaskReconciler) verifyRecoveryFile(projectDir string, seed *recoverySeed) error {
	path := filepath.Join(projectDir, recoveryFileDir, recoveryFileName)
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("recovery seed file could not be verified: %w", err)
	}
	if st.Size() == 0 {
		return errors.New("recovery seed file could not be verified: file is empty")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("recovery seed file could not be verified: %w", err)
	}
	content := string(b)
	if !strings.Contains(content, recoveryFooterExecLine+seed.FailedExecID) ||
		!strings.Contains(content, recoveryFooterWorkerLine+seed.FailedWorkerID) {
		return errors.New("recovery seed file could not be verified: footer does not match the recovery execution + worker")
	}
	recoverySeedMetricsSingleton.recordVerified()
	return nil
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

// recoverySeedMetrics holds the OTel counters for the recovery-seed
// lifecycle (ADR fix-recovery-seed-race §6). Best-effort: the ensure
// pattern swallows instrument errors and ORCHICON_TELEMETRY=none makes
// Meter() a no-op, so metrics are never a control-flow dependency.
type recoverySeedMetrics struct {
	initOnce        sync.Once
	written         otelmetric.Int64Counter
	skipped         otelmetric.Int64Counter
	verified        otelmetric.Int64Counter
	writeFailure    otelmetric.Int64Counter
	dispatchHeld    otelmetric.Int64Counter
	dispatchBlocked otelmetric.Int64Counter
}

func (m *recoverySeedMetrics) ensure() {
	m.initOnce.Do(func() {
		// NOTE: no units on these instruments — the Prometheus exporter
		// appends "_<unit>" to metric names, which would break the canonical
		// names queried downstream (same pattern as internal/aigateway).
		if c, err := telemetry.Meter().Int64Counter("orchicon_recovery_seed_written",
			otelmetric.WithDescription("Recovery seed files written at dispatch time")); err == nil {
			m.written = c
		}
		if c, err := telemetry.Meter().Int64Counter("orchicon_recovery_seed_skipped",
			otelmetric.WithDescription("Recovery seed actions skipped by reason")); err == nil {
			m.skipped = c
		}
		if c, err := telemetry.Meter().Int64Counter("orchicon_recovery_seed_verified",
			otelmetric.WithDescription("Recovery seed files verified (exists, non-empty, footer matches)")); err == nil {
			m.verified = c
		}
		if c, err := telemetry.Meter().Int64Counter("orchicon_recovery_seed_write_failure",
			otelmetric.WithDescription("Recovery seed file write/verify failures that blocked a dispatch")); err == nil {
			m.writeFailure = c
		}
		if c, err := telemetry.Meter().Int64Counter("orchicon_recovery_dispatch_held",
			otelmetric.WithDescription("Recovery re-dispatches held by the gate (recovery not yet ready)")); err == nil {
			m.dispatchHeld = c
		}
		if c, err := telemetry.Meter().Int64Counter("orchicon_recovery_dispatch_blocked",
			otelmetric.WithDescription("Recovery dispatches failed loud instead of starting cold")); err == nil {
			m.dispatchBlocked = c
		}
	})
}

func (m *recoverySeedMetrics) recordWritten() {
	m.ensure()
	if m.written != nil {
		m.written.Add(context.Background(), 1)
	}
}

func (m *recoverySeedMetrics) recordSkipped(reason string) {
	m.ensure()
	if m.skipped != nil {
		m.skipped.Add(context.Background(), 1, otelmetric.WithAttributes(attribute.String("reason", reason)))
	}
}

func (m *recoverySeedMetrics) recordVerified() {
	m.ensure()
	if m.verified != nil {
		m.verified.Add(context.Background(), 1)
	}
}

func (m *recoverySeedMetrics) recordWriteFailure() {
	m.ensure()
	if m.writeFailure != nil {
		m.writeFailure.Add(context.Background(), 1)
	}
}

func (m *recoverySeedMetrics) recordHeld() {
	m.ensure()
	if m.dispatchHeld != nil {
		m.dispatchHeld.Add(context.Background(), 1)
	}
}

func (m *recoverySeedMetrics) recordBlocked() {
	m.ensure()
	if m.dispatchBlocked != nil {
		m.dispatchBlocked.Add(context.Background(), 1)
	}
}

// recoverySeedMetricsSingleton is the shared instance for the recovery-seed
// counters (ADR fix-recovery-seed-race §6).
var recoverySeedMetricsSingleton = &recoverySeedMetrics{}
