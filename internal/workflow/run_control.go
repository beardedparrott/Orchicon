package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// run_control.go — the shared transactional cores behind run resume and run pause.
//
// THEY LIVE HERE, IN ONE PLACE, ON PURPOSE. Two surfaces drive these operations: the workflow
// run page (through the RPC) and the work item page (through ControlSequence). If each grew its
// own copy, "resume" would mean two slightly different things depending on which button the
// operator pressed — which is exactly the defect that made leaf RESUME a silent no-op for so
// long. Both entry points call these.

// retryFailedRunInner re-arms a terminal run in place. It is called with a live transaction and
// returns how many step runs were reset.
//
// The two rules that matter, both learned from run 01M8B2A33SB9WDKDB98GQQKV0G:
//
//	ABORTED IS RETRYABLE. Abort is how an operator stops a run, so refusing to resume an aborted
//	run made the only stop gesture a one-way door.
//
//	THE DEAD ATTEMPT'S KNOWLEDGE IS CARRIED FORWARD, not blanked. The reset step used to get
//	`{}` as its Result, discarding the recovery engine's summary — so the resumed worker started
//	blind and re-derived conclusions already paid for.
func retryFailedRunInner(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow) (int, error) {
	if run.Status != domain.WorkflowRunFailed && run.Status != domain.WorkflowRunAborted {
		return 0, fmt.Errorf("run is in status %s — only a failed or aborted run can be retried", run.Status)
	}
	stepRuns, err := db.ListWorkflowStepRuns(ctx, tx, tenantID, run.ID)
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	reset := 0
	for _, sr := range stepRuns {
		if sr.SupersededBy != "" {
			continue // archived iteration — never mutate
		}
		switch sr.Status {
		case domain.StepRunFailed, domain.StepRunSkipped, domain.StepRunBlocked:
		default:
			continue // succeeded / pending / ready / running / approval_pending / recovering stay
		}
		carried := priorAttemptContext(sr.Result)
		attempt := 0 // a resume is a genuine fresh chance, not a continuation of a spent budget
		fields := db.UpdateWorkflowStepRunFields{
			Status:            strPtr(domain.StepRunPending),
			Result:            &carried,
			WorkerExecutionID: strPtr(""),
			Attempt:           &attempt,
			StartedAt:         &now,
			ClearEndedAt:      true,
		}
		// Re-provision pruned OR failed worktrees: keep the branch for re-attach, reset status and
		// clear the stale path so the WorktreeReconciler re-creates it before dispatch.
		if sr.WorktreeStatus == domain.WorktreePruned || sr.WorktreeStatus == domain.WorktreeFailed {
			pending := domain.WorktreePending
			emptyPath := ""
			fields.WorktreeStatus = &pending
			fields.WorktreePath = &emptyPath
		}
		if _, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, fields); err != nil {
			return 0, err
		}
		reset++
	}
	if reset == 0 {
		return 0, fmt.Errorf("no failed/skipped/blocked step runs to retry")
	}

	// A COLD RESUME IS STILL A RESUME. When the worktree is gone the steps start over, but each
	// carries the previous attempt's summary, so nothing learned is lost — and the marker lets the
	// dispatch prompt say so out loud instead of presenting attempt four as a clean start.
	coldResume := run.WorktreePath == "" ||
		run.WorktreeStatus == domain.WorktreePruned ||
		run.WorktreeStatus == domain.WorktreeFailed

	runFields := db.UpdateWorkflowRunFields{
		Status:       strPtr(domain.WorkflowRunPending),
		ClearEndedAt: true,
	}
	if coldResume {
		if ctxBytes, cerr := markRunResumedCold(run.RunContext, now); cerr == nil {
			runFields.RunContext = &ctxBytes
		}
	}
	if run.WorktreeStatus == domain.WorktreePruned || run.WorktreeStatus == domain.WorktreeFailed {
		pending := domain.WorktreePending
		emptyPath := ""
		runFields.WorktreeStatus = &pending
		runFields.WorktreePath = &emptyPath
		// The recorded REFUSAL belongs to the attempt that failed, not to the retry.
		emptyErr := ""
		runFields.ErrorMessage = &emptyErr
		runFields.ErrorKind = &emptyErr
	}
	if _, err := db.UpdateWorkflowRun(ctx, tx, tenantID, run.ID, run.Version, runFields); err != nil {
		return 0, err
	}

	// The bound work item was failed at run end — put it back to running so the ticket reflects
	// the resumed run (per-step executions never mutate it).
	if run.WorkItemID != "" {
		if wi, err := db.GetWorkItem(ctx, tx, tenantID, run.WorkItemID); err == nil {
			status := domain.WorkItemRunning
			_, _ = db.UpdateWorkItem(ctx, tx, tenantID, run.WorkItemID, wi.Version, db.UpdateWorkItemFields{Status: &status})
		}
	}
	return reset, nil
}

// priorAttemptContext builds the Result a RE-ARMED step run starts with.
//
// Only what a next attempt can actually USE survives:
//
//	_work_item_id   a platform key, not an attempt key — dispatch reads it to find the ticket
//	_prior_attempt  the previous result verbatim, so nothing is lost
//	_prior_output / _prior_summary / _prior_recovery_summary / _prior_decision / _prior_verdict
//
// Execution-scoped keys are NOT promoted to live fields: they name an execution that is over, and
// a step that believed it owned one would try to resume a dead session. They survive as history
// inside _prior_attempt.
func priorAttemptContext(previous []byte) []byte {
	out := map[string]any{}
	var prior map[string]any
	if len(previous) > 0 {
		_ = json.Unmarshal(previous, &prior)
	}
	if len(prior) == 0 {
		encoded, _ := json.Marshal(out)
		return encoded
	}
	if wid, ok := prior["_work_item_id"]; ok && wid != nil && wid != "" {
		out["_work_item_id"] = wid
	}
	out["_prior_attempt"] = prior
	// EXPLICIT PAIRS, not "_prior" + trimmed key: that concatenation turned `_recovery_summary`
	// into `_priorrecovery_summary` (no separator), so the carried summary was present in the JSON
	// but under a key nothing read. The test in run_control_test.go is what caught it.
	for _, pair := range [][2]string{
		{"_output", "_prior_output"},
		{"_summary", "_prior_summary"},
		{"_recovery_summary", "_prior_recovery_summary"},
		{"_decision", "_prior_decision"},
		{"_verdict", "_prior_verdict"},
	} {
		if v, ok := prior[pair[0]]; ok && v != nil {
			out[pair[1]] = v
		}
	}
	encoded, _ := json.Marshal(out)
	return encoded
}

// markRunResumedCold stamps the run context with resume provenance.
//
// The COUNT is the honest answer to "is this the first attempt at this work?" — the dispatch
// prompt uses it to tell a worker that prior conclusions exist, and the UI uses it to label a run
// instead of presenting a fourth attempt as a clean start.
func markRunResumedCold(runContext []byte, now time.Time) ([]byte, error) {
	ctx := map[string]any{}
	if len(runContext) > 0 {
		_ = json.Unmarshal(runContext, &ctx)
	}
	n := 0
	switch v := ctx["_resume_count"].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	}
	ctx["_resume_count"] = n + 1
	ctx["_resumed_cold"] = true
	ctx["_resumed_at"] = now.UTC().Format(time.RFC3339)
	return json.Marshal(ctx)
}

// pauseRunInner stops a run at a resumable point, keeping its worktree, branch and step progress.
func pauseRunInner(ctx context.Context, tx pgx.Tx, tenantID, runID, reason string) error {
	current, err := db.GetWorkflowRun(ctx, tx, tenantID, runID)
	if err != nil {
		return err
	}
	if current.Status != domain.WorkflowRunRunning && current.Status != domain.WorkflowRunPending {
		return fmt.Errorf("run is in status %s — only a running or pending run can be paused", current.Status)
	}
	if _, err := db.UpdateWorkflowRun(ctx, tx, tenantID, current.ID, current.Version, db.UpdateWorkflowRunFields{
		// EndedAt deliberately NOT set: the run has not ended.
		Status: strPtr(domain.WorkflowRunPaused),
	}); err != nil {
		return err
	}

	// Ask in-flight executions to checkpoint. This is the SAME transition PauseExecution makes
	// (internal/execution/service.go): the execution status set has no pause state, so a pause is
	// expressed as terminating-with-checkpoint. Inventing an ExecutionCheckpointing value here
	// would put something in the column no other reconciler knows how to read.
	stepRuns, err := db.ListWorkflowStepRuns(ctx, tx, tenantID, current.ID)
	if err != nil {
		return err
	}
	paused := 0
	for _, sr := range stepRuns {
		if sr.Status != domain.StepRunRunning || sr.WorkerExecutionID == "" {
			continue
		}
		exec, err := db.GetExecution(ctx, tx, tenantID, sr.WorkerExecutionID)
		if err != nil || (exec.Status != domain.ExecutionRunning && exec.Status != domain.ExecutionDispatching) {
			continue
		}
		terminating := domain.ExecutionTerminating
		terminatingHealth := domain.HealthTerminating
		if _, err := db.UpdateExecution(ctx, tx, tenantID, exec.ID, exec.Version, db.UpdateExecutionFields{
			Status:      &terminating,
			HealthState: &terminatingHealth,
		}); err != nil {
			return err
		}
		paused++
	}

	// THE WORK ITEM'S STATUS MUST MATCH WHAT ACTUALLY HAPPENED.
	//
	// This used to stamp `checkpointing` unconditionally, and that deadlocked the item:
	//
	//	checkpointing is in IsActiveRunStatus
	//	  -> ControlSequence RESUME refuses ("cannot RESUME a work item that is running")
	//	  -> PAUSE refuses (no run in flight) and RESTART refuses (still "in flight")
	//	  -> the reconciler sees the run is `paused` and returns before touching the item
	//
	// Measured on work item 01M84WD1G42BHD1HTCB2YMQ82Z: it sat in `checkpointing` indefinitely with
	// NOTHING in flight, because QA had already failed 110 minutes earlier. A pause with nothing to
	// checkpoint is simply a pause, and the item must stay resumable.
	//
	// So: `checkpointing` only when we actually asked something to checkpoint. Otherwise `pending`,
	// which is the honest description of "parked, ready to resume" and is what every gesture expects.
	// THE ITEM IS PARKED, NOT "CHECKPOINTING".
	//
	// `checkpointing` describes a checkpoint IN PROGRESS, and pause does not produce one: it ends the
	// session and the resume re-establishes it. Stamping `checkpointing` therefore described a state
	// that never existed — and because `checkpointing` is in IsActiveRunStatus, it locked every
	// gesture out. Measured twice on work item 01M84WD1G42BHD1HTCB2YMQ82Z, which sat in it for hours
	// with nothing in flight the first time and an orphaned `terminating` row the second.
	//
	// `pending` + a `paused` run is the truthful pair: the work is parked and resumable. The RUN's
	// status carries the meaning the operator needs; the item's status only needs to not lie.
	// PARK IT AS `paused`, NOT `pending`.
	//
	// `pending` means "on deck" to the sequence reconciler, which ARMS it: measured on
	// 01M84WD1G42BHD1HTCB2YMQ82Z, where each of two pauses was followed by a new run in the SAME
	// SECOND. `paused` is a status the arming paths skip, and it is not an active run status, so it
	// locks nothing out either.
	if current.WorkItemID != "" {
		if wi, err := db.GetWorkItem(ctx, tx, tenantID, current.WorkItemID); err == nil {
			status := domain.WorkItemPaused
			_, _ = db.UpdateWorkItem(ctx, tx, tenantID, current.WorkItemID, wi.Version, db.UpdateWorkItemFields{Status: &status})
		}
	}
	_ = paused // counted for the log line; deliberately NOT a status input
	return nil
}

// RetryRunInTx is the transactional entry point the WORK ITEM service uses for a leaf RESUME,
// so the work item page and the run page cannot disagree about what "resume" means.
func (s *Service) RetryRunInTx(ctx context.Context, tenantID, runID string) (int, error) {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		return 0, mapDBError(err)
	}
	reset, err := retryFailedRunInner(ctx, ttx.Tx, tenantID, run)
	if err != nil {
		return 0, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return 0, err
	}
	return reset, nil
}

// PauseRunInTx is the transactional entry point the WORK ITEM service uses for PAUSE.
func (s *Service) PauseRunInTx(ctx context.Context, tenantID, runID, reason string) error {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)
	if err := pauseRunInner(ctx, ttx.Tx, tenantID, runID, reason); err != nil {
		return err
	}
	return ttx.Commit(ctx)
}

// ResumeRunInTx continues a PAUSED run — the transactional entry point the WORK ITEM service uses
// for a leaf RESUME, so the work item page and the run page cannot disagree about what resume means.
func (s *Service) ResumeRunInTx(ctx context.Context, tenantID, runID string) error {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)
	if err := resumeRunInner(ctx, ttx.Tx, tenantID, runID); err != nil {
		return err
	}
	return ttx.Commit(ctx)
}

// resumeRunInner continues a PAUSED run in place. Shared by the run RPC and the work item page so
// the two cannot disagree about what "resume" means — the divergence that made leaf RESUME a no-op
// in the first place.
func resumeRunInner(ctx context.Context, tx pgx.Tx, tenantID, runID string) error {
	current, err := db.GetWorkflowRun(ctx, tx, tenantID, runID)
	if err != nil {
		return err
	}
	_, err = resumeRunInnerReturning(ctx, tx, tenantID, current)
	return err
}

// resumeRunInnerReturning is resumeRunInner with the pre-read row supplied (the RPC already has it).
//
// RESUMING DOES NOT RESURRECT THE PAUSED EXECUTION. Pause ENDED that session, so flipping its row
// back to `running` points at a process that no longer exists. The interrupted execution is ended
// honestly and its STEP returns to pending so the reconciler dispatches it again — the same attempt
// continuing, with the bridge resuming the same conversation from the persisted session id.
func resumeRunInnerReturning(ctx context.Context, tx pgx.Tx, tenantID string, current db.WorkflowRunRow) (db.WorkflowRunRow, error) {
	if current.Status != domain.WorkflowRunPaused {
		return current, fmt.Errorf("run is in status %s — only a paused run can be resumed this way", current.Status)
	}
	updated, err := db.UpdateWorkflowRun(ctx, tx, tenantID, current.ID, current.Version, db.UpdateWorkflowRunFields{
		Status: strPtr(domain.WorkflowRunRunning),
	})
	if err != nil {
		return current, err
	}

	stepRuns, err := db.ListWorkflowStepRuns(ctx, tx, tenantID, current.ID)
	if err != nil {
		return current, err
	}
	for _, sr := range stepRuns {
		if sr.WorkerExecutionID == "" {
			continue
		}
		exec, err := db.GetExecution(ctx, tx, tenantID, sr.WorkerExecutionID)
		if err != nil || exec.Status != domain.ExecutionTerminating {
			continue
		}
		ended := time.Now().UTC()
		terminated := domain.ExecutionTerminated
		if _, err := db.UpdateExecution(ctx, tx, tenantID, exec.ID, exec.Version, db.UpdateExecutionFields{
			Status:       &terminated,
			EndedAt:      &ended,
			ErrorMessage: strPtr("paused; re-dispatched on resume"),
		}); err != nil {
			return current, err
		}
		// WHICH STEPS RE-ARM ON RESUME, AND WHY IT MATTERS.
		//
		//   running / recovering   were interrupted mid-work -> re-dispatch so they continue
		//   failed                 the run was PAUSED WHILE A STEP HAD FAILED (the loop had not yet
		//                          routed it). The operator's rule: "If you pause during a failure,
		//                          hitting resume starts from the failed step and retries from that
		//                          point." Leaving it failed stranded the run — running, with a
		//                          failed step and nothing to dispatch.
		//   succeeded / pending    untouched. Succeeded work is never replayed, and pending steps
		//                          are already queued for the reconciler.
		//
		// `Attempt` is deliberately NOT reset on a re-armed failed step: this is the same attempt
		// continuing from where it stopped, not a fresh try. A genuine retry is RetryRunInTx.
		switch sr.Status {
		case domain.StepRunRunning, domain.StepRunRecovering, domain.StepRunFailed:
			pending := domain.StepRunPending
			if _, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:            &pending,
				WorkerExecutionID: strPtr(""),
				ClearEndedAt:      true,
			}); err != nil {
				return current, err
			}
		}
	}
	// THE WORK ITEM MOVES WITH THE RUN. This lived only in the RPC wrapper, so the WORK ITEM page's
	// resume left the item `paused` while the run was already `running` — the operator saw "kicked it
	// off" and then an item that had not moved, and a second click failed because the RUN was now
	// running while the ITEM still said paused. Two surfaces, two answers, again.
	//
	// It belongs in the shared core precisely because it is not optional: a resumed run whose item
	// still reads `paused` is a state no gesture can reconcile.
	if current.WorkItemID != "" {
		if wi, err := db.GetWorkItem(ctx, tx, tenantID, current.WorkItemID); err == nil {
			runningItem := domain.WorkItemRunning
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, current.WorkItemID, wi.Version, db.UpdateWorkItemFields{
				Status: &runningItem,
			}); err != nil {
				return current, fmt.Errorf("set work item running: %w", err)
			}
		}
	}
	return updated, nil
}
