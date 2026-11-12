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

const (
	// recoveryFileDir is the gitignored per-project directory that holds
	// recovery seeding (and per-run step results).
	recoveryFileDir = ".orchicon"
	// recoveryFileName is the single recovery seed file; it is last-write-wins per project dir and each
	// worker only trusts a file whose header/footer names its own worker + execution.
	recoveryFileName = "worker.recovery"

	// recoveryTailMaxParts is how many transcript parts the dead session's
	// tail seeds (operator: "last ~50+ parts").
	recoveryTailMaxParts = 60
	// recoveryTailMaxBytes caps the rendered tail (operator band 16-32KB; 24KB keeps the file cheap to
	// ingest even on a 32K-context local model).
	recoveryTailMaxBytes = 24 * 1024

	// recoveryHeaderPrefix is the mandatory first line of the file.
	recoveryHeaderPrefix = "This is for '%s' worker only because you stalled in a previous session. If you are not this worker, do not read the rest of this file."

	// recoveryDirective tells the worker it may terminate immediately by re-printing its summary; it
	// also instructs the worker to delete the file.
	recoveryDirective = "If you are already done with your work, please print your ORCHICON WORKER SUMMARY. If you already did that, please just print the summary itself again and then finish. When you finish, delete this file (rm .orchicon/worker.recovery) so a future session for a different worker is never confused by it. If `.orchicon/worker.recovery` is missing or unreadable when you start, do NOT redo any work — finish immediately with `ORCHICON WORKER SUMMARY: failure` and reason `recovery seed file missing`."

	// recoveryFailFastDirective is the last line of defense: the composite's ## Recovery block
	// instructs a recovery-resumed worker to fail fast when the seed file it was promised is missing.
	recoveryFailFastDirective = "If the file is missing or unreadable, do NOT redo work — finish immediately with decision `fail` and reason `recovery seed file missing`."

	// recoveryFooterExecLine / recoveryFooterWorkerLine are the LAST lines of the file.
	recoveryFooterExecLine   = "# recovery-execution-id: "
	recoveryFooterWorkerLine = "# worker-id: "

	// recoveryFileReferenceMarker is the exact text the composite's ## Recovery block uses to point at
	// the file.
	recoveryFileReferenceMarker = "Read the file `.orchicon/worker.recovery`"

	// recoveryReasonSectionHeading is the heading of the WHY section inside both the seed file and the
	// composite's ## Recovery block.
	recoveryReasonSectionHeading = "## Why the last attempt was rejected"

	// recoveryRedoSupersedesAlreadyDone is the correction to the "do not redo work" directive, applied
	// when the failure was the PLATFORM rejecting the work rather than a stall or a crash.
	recoveryRedoSupersedesAlreadyDone = "**The work from the last attempt was REJECTED by the platform, not interrupted.** " +
		"The usual recovery instruction not to redo completed work does NOT apply here: what was delivered was examined and refused, so leaving it unchanged will produce the identical rejection. " +
		"Read the reason above, change the work accordingly, then report."

	// recoveryTailSectionHeading is the heading of the transcript section inside the seed file.
	recoveryTailSectionHeading = "## Dead session transcript (tail)"

	// recoveryChurnScanParts is how deep the productive-window walkback scans when the raw tail is
	// bootstrap churn.
	recoveryChurnScanParts = 1000

	// recoverySubstantiveTextMinChars is the minimum length a non-opener text part needs to count as
	// substantive assistant text (short acknowledgements between openers are churn, not work).
	recoverySubstantiveTextMinChars = 80
)

// recoverySeed is the recovery context the dispatch path acts on.
type recoverySeed struct {
	Summary        string // the recovery engine's one-line narrative
	FailedExecID   string // the dead execution's id
	FailedWorkerID string // the worker the dead execution ran on
	// FailureReason is WHY the dead execution failed, verbatim from its execution row's error_message —
	// the one thing the resumed agent needs in order to behave differently this time.
	FailureReason string
}

// recoverySeedFor returns the recovery seed for a dispatch, or nil when this dispatch is NOT a
// recovery-resumed dispatch for the SAME worker.
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
		if seed.FailureReason == "" {
			if r, ok := m["_recovery_failure_reason"].(string); ok && r != "" {
				seed.FailureReason = r
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

// recoveryIdentityFromResult extracts the dead-execution identity recorded on a step-run /
// work-item result.
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

// resolveRecoverySeed is the SHARED seed resolver used by BOTH the workflow dispatch gate
// (recoveryDispatchReady) and the TaskReconciler's file writer.
func resolveRecoverySeed(ctx context.Context, tx pgx.Tx, tenantID, taskID string, runResult, wiResults []byte, workerID string) *recoverySeed {
	if seed := recoverySeedFor(runResult, wiResults, workerID); seed != nil {
		return seed
	}
	return recoverySeedFromRow(ctx, tx, tenantID, taskID, runResult, wiResults, workerID)
}

// recoverySeedFromRow is the DB fallback half of the shared resolver: when the engine-published
// keys are not yet on the step run / work item.
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
	return &recoverySeed{
		Summary:        rec.Summary,
		FailedExecID:   rec.FailedExecutionID,
		FailedWorkerID: failedWorkerID,
		// The DB fallback reads the row directly, so the reason is available even
		// when the engine's published key has not landed on the step run yet.
		FailureReason: failureReasonForExecution(ctx, tx, tenantID, rec.FailedExecutionID),
	}
}

// recoveryFileReferenceBlock is the ## Recovery prompt block that tells a recovery-resumed worker
// to read the seed file.
func recoveryFileReferenceBlock(seed *recoverySeed) string {
	var sb strings.Builder
	sb.WriteString("## Recovery\n\n")
	sb.WriteString("A previous execution of this step did not complete and was recovered. This is your recovery-resumed session. " + recoveryFileReferenceMarker + " in the project working directory — it carries the reason the last attempt was rejected, the dead session's transcript tail, and instructions. " + recoveryFailFastDirective + "\n\n")
	if seed != nil && seed.Summary != "" {
		sb.WriteString("Summary: " + seed.Summary + "\n")
	}
	// THE REASON IS STATED IN THE PROMPT AS WELL AS THE FILE, because the file is not always read and this
	// is the one fact that changes behaviour. It is short and it is the whole point of the resume.
	if seed != nil && strings.TrimSpace(seed.FailureReason) != "" {
		sb.WriteString("\n" + recoveryReasonSectionHeading + "\n\n")
		sb.WriteString(strings.TrimSpace(seed.FailureReason) + "\n\n")
		sb.WriteString(recoveryRedoSupersedesAlreadyDone + "\n")
	} else {
		// No reason available. Say so, so the agent knows it is resuming BLIND rather than assuming the
		// silence means nothing was wrong.
		sb.WriteString("\nThe platform did not record a machine-readable reason for the previous failure, so you are resuming without one. " +
			"Treat your previous report as UNCONFIRMED: re-examine what you produced before reporting again, rather than repeating it.\n")
	}
	return sb.String()
}

// ensureRecoveryFileReference appends the ## Recovery reference block to a composite that lacks it
// (legacy direct-dispatch composites built without the block).
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

// buildRecoveryFileContent assembles the .orchicon/worker.recovery file: the mandatory header line,
// the ## Recovery section (summary + the already-done directive), the capped transcript tail.
func buildRecoveryFileContent(workerName string, seed *recoverySeed, tail string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, recoveryHeaderPrefix+"\n\n", workerName)
	sb.WriteString("## Recovery\n\n")
	if seed.Summary != "" {
		sb.WriteString(seed.Summary + "\n\n")
	}
	// WHY BEFORE WHAT: the reason is placed ahead of the transcript, because the transcript of an agent
	// that believed it had finished reads as a completed job and invites the identical report.
	if strings.TrimSpace(seed.FailureReason) != "" {
		sb.WriteString(recoveryReasonSectionHeading + "\n\n")
		sb.WriteString(strings.TrimSpace(seed.FailureReason) + "\n\n")
		sb.WriteString(recoveryRedoSupersedesAlreadyDone + "\n\n")
	}
	sb.WriteString(recoveryDirective + "\n")
	if tail != "" {
		sb.WriteString("\n" + recoveryTailSectionHeading + "\n\n")
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

// seedRecoveryFile is the dispatch-time hard gate.
func (r *TaskReconciler) seedRecoveryFile(ctx context.Context, exec db.ExecutionRow, task db.WorkItemRow, version db.WorkerVersionRow, projectDir string, stepRunResult []byte, systemPrompt string) (string, error) {
	if projectDir == "" {
		return systemPrompt, nil
	}
	// Resolve the seed with the SHARED resolver (fast-path keys first — no DB needed; fallback to the
	// terminal-resumed recovery row in a short read tx) so the gate and the file writer can never.
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

	// Fetch the failed worker's display name (header) + the dead session's transcript tail, all in one
	// short read tx.
	workerName := seed.FailedWorkerID
	tail := ""
	churnOnly := false
	if exec.TenantID != "" {
		if ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID); err == nil {
			if w, err := db.GetWorker(ctx, ttx.Tx, exec.TenantID, seed.FailedWorkerID); err == nil && w.Name != "" {
				workerName = w.Name
			}
			if parts, err := db.ListExecutionSessionPartsTail(ctx, ttx.Tx, exec.TenantID, seed.FailedExecID, recoveryTailMaxParts); err == nil {
				var full []db.SessionPart
				if tailIsBootstrapChurn(parts) {
					// Churn tail → scan deeper for the productive walkback (best-effort: a scan error degrades to
					// churn-only handling, never to churn-feeding).
					if fp, err := db.ListExecutionSessionPartsTail(ctx, ttx.Tx, exec.TenantID, seed.FailedExecID, recoveryChurnScanParts); err == nil {
						full = fp
					} else {
						r.log.Warn("recovery seed: churn scan", "execution", seed.FailedExecID, "error", err)
					}
				}
				tail, churnOnly = resolveRecoveryTailContent(parts, full, recoveryTailMaxBytes)
			} else {
				r.log.Warn("recovery seed: read transcript tail", "execution", seed.FailedExecID, "error", err)
			}
			_ = ttx.Rollback(ctx)
		}
	}

	// Churn-only successor: never feed churn forward and never clobber a richer prior seed.
	if churnOnly {
		priorTail := ""
		if prior, err := os.ReadFile(filepath.Join(projectDir, recoveryFileDir, recoveryFileName)); err == nil {
			priorTail = extractRecoveryTailSection(string(prior))
		}
		if priorTail != "" {
			if len(priorTail) > recoveryTailMaxBytes {
				priorTail = priorTail[:recoveryTailMaxBytes]
			}
			tail = recoveryChurnCarriedNote + priorTail + "\n"
		} else {
			tail = recoveryChurnMarkerNote
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

// writeRecoveryFileGate enforces the file half of the recovery-resume invariant:
// .orchicon/worker.recovery must exist, be non-empty.
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

// writeRecoveryFileOnce performs one write/verify cycle: idempotent reuse when the file already
// carries the same footer, a foreign-seed ownership check before overwriting a different footer.
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
		// Different footer → foreign seed.
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

// recoveryOwnerTerminal reports whether the recovery for a failed execution (identified by its
// footer's exec id) has reached a TERMINAL state.
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

// verifyRecoveryFile stat-checks the file (exists, non-empty) and read-checks the footer against
// the expected seed.
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

// removeRecoveryFileMatching deletes the seed file ONLY when its footer matches the given recovery
// execution + worker, so a success never removes a NEWER recovery's file.
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

// recoverySeedForSuccess resolves the recovery execution + worker a completed execution was
// resuming.
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

// removeRecoveryFileForSuccess is the system-side cleanup.
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

// Bootstrap-churn defense (audit finding 2026-09-10, work item 01M21PZE3QQEZ35418SXCQZZRQ): when a
// recovery-resumed session dies in bootstrap churn (repeated recovery-opener announcements.

// recoveryOpenerMarkers are the phrases the bootstrap-churn loop itself produces (the seed header,
// the composite's ## Recovery block, the fail-fast reason.
var recoveryOpenerMarkers = []string{
	"recovery-resumed session",
	"recovery resumed session",
	"resuming a recovered session",
	"resuming the recovered session",
	"stalled in a previous session",
	"stalled and was recovered",
	"recovery seed file",
	"worker.recovery",
	"this is your recovery",
}

// Churn notes rendered inside the transcript section so the worker (and a later churn-only
// successor's carry-forward) can see WHY the tail looks the way it does.
const (
	recoveryChurnWalkbackNote = "(Note: the dead session's final parts were recovery-resume bootstrap churn — repeated recovery-opener announcements with no tool activity and no file edits — so the transcript tail below starts at the last productive window.)\n\n"

	recoveryChurnCarriedNote = "(Note: the dead session's transcript contained only recovery-resume bootstrap churn — repeated recovery-opener announcements with no tool activity and no file edits — so the transcript tail below is carried forward from the prior recovery seed rather than regenerated from churn.)\n\n"

	recoveryChurnMarkerNote = "(Note: the dead session's transcript contained only recovery-resume bootstrap churn — repeated recovery-opener announcements with no tool activity and no file edits; no productive transcript window exists to seed.)\n\n"
)

// sessionPartTextContent extracts the rendered text of a user_message or
// text part, mirroring the transcript renderer's payload shapes.
func sessionPartTextContent(p db.SessionPart) string {
	switch p.Kind {
	case db.SessionPartUserMessage:
		var pl struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(p.Payload, &pl)
		return pl.Text
	case db.SessionPartText:
		var pl struct {
			Part struct {
				Text string `json:"text"`
			} `json:"part"`
		}
		_ = json.Unmarshal(p.Payload, &pl)
		return pl.Part.Text
	}
	return ""
}

// isRecoveryOpenerText reports whether a text part is one of the recovery-opener announcements the
// bootstrap-churn loop itself produces (case-insensitive substring match against.
func isRecoveryOpenerText(s string) bool {
	lower := strings.ToLower(s)
	for _, m := range recoveryOpenerMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// hasFileDiffEvidence reports whether rendered text carries file-edit evidence (a git diff header
// or unified-diff hunk markers) — the second productive signal alongside tool_use parts.
func hasFileDiffEvidence(s string) bool {
	if strings.Contains(s, "diff --git") {
		return true
	}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "+++") || strings.HasPrefix(t, "---") {
			return true
		}
	}
	return false
}

// partIsProductive reports whether a session part carries substantive work: a tool_use part,
// file-diff evidence, or non-opener user/assistant text of meaningful length.
func partIsProductive(p db.SessionPart) bool {
	if p.Kind == db.SessionPartToolUse {
		return true
	}
	txt := sessionPartTextContent(p)
	if txt == "" || isRecoveryOpenerText(txt) {
		return false
	}
	if hasFileDiffEvidence(txt) {
		return true
	}
	return len([]rune(strings.TrimSpace(txt))) >= recoverySubstantiveTextMinChars
}

// tailIsBootstrapChurn applies the heuristic floor to a raw transcript tail.
func tailIsBootstrapChurn(parts []db.SessionPart) bool {
	if len(parts) < 2 {
		return false
	}
	openers := 0
	for _, p := range parts {
		if p.Kind == db.SessionPartToolUse {
			return false
		}
		txt := sessionPartTextContent(p)
		if txt == "" {
			continue // neutral kinds (errors, steps) don't mask churn
		}
		if isRecoveryOpenerText(txt) {
			openers++
			continue
		}
		if hasFileDiffEvidence(txt) {
			return false
		}
		if len([]rune(strings.TrimSpace(txt))) >= recoverySubstantiveTextMinChars {
			return false
		}
	}
	return openers >= 2
}

// lastProductiveWindow walks a CHRONOLOGICAL part list backwards to the last productive part and
// returns the window of up to maxParts parts ending there (everything after it is churn).
func lastProductiveWindow(parts []db.SessionPart, maxParts int) ([]db.SessionPart, bool) {
	last := -1
	for i := len(parts) - 1; i >= 0; i-- {
		if partIsProductive(parts[i]) {
			last = i
			break
		}
	}
	if last < 0 {
		return nil, false
	}
	start := last + 1 - maxParts
	if start < 0 {
		start = 0
	}
	win := make([]db.SessionPart, last+1-start)
	copy(win, parts[start:last+1])
	return win, true
}

// resolveRecoveryTailContent applies the substance filter to the fetched tail.
func resolveRecoveryTailContent(tailParts, fullScan []db.SessionPart, maxBytes int) (tail string, churnOnly bool) {
	if !tailIsBootstrapChurn(tailParts) {
		return transcript.RenderTail(tailParts, maxBytes), false
	}
	if len(fullScan) > 0 {
		if win, ok := lastProductiveWindow(fullScan, recoveryTailMaxParts); ok {
			return recoveryChurnWalkbackNote + transcript.RenderTail(win, maxBytes), false
		}
	}
	return "", true
}

// extractRecoveryTailSection reads the transcript section back out of a prior seed file (everything
// after the section heading.
func extractRecoveryTailSection(content string) string {
	start := strings.Index(content, recoveryTailSectionHeading+"\n\n")
	if start < 0 {
		return ""
	}
	body := content[start+len(recoveryTailSectionHeading)+2:]
	if idx := strings.Index(body, "\n"+recoveryFooterExecLine); idx >= 0 {
		body = body[:idx]
	}
	return strings.Trim(body, "\n")
}

// recoverySeedMetrics holds the OTel counters for the recovery-seed lifecycle (ADR
// fix-recovery-seed-race §6).
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
		// NOTE: no units on these instruments — the Prometheus exporter appends "_<unit>" to metric names,
		// which would break the canonical names queried downstream (same pattern as internal/aigateway).
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

// failureReasonForExecution reads WHY an execution failed, for the recovery seed.
func failureReasonForExecution(ctx context.Context, tx pgx.Tx, tenantID, execID string) string {
	if tx == nil || execID == "" {
		return ""
	}
	exec, err := db.GetExecution(ctx, tx, tenantID, execID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(exec.ErrorMessage)
}
