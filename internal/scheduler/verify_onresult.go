package scheduler

// verify_onresult.go — WHERE THE VERDICT IS DECIDED.
//
// OnResult has always taken the worker's word: if the process exited cleanly and the summary said
// `success`, the execution succeeded. The missing half is that nothing asked the REPOSITORY whether
// the claim was true. This file supplies that half, and it is deliberately called from exactly one
// place — the terminal transition — so there is one answer to "how did this step get its status".
//
// THE THREE-WAY OUTCOME AND HOW EACH LANDS:
//
//	verified         → succeeded, exactly as before. No behavioural change for good work.
//	unsubstantiated  → FAILED, with the contradicting evidence in error_message. This is a real
//	                   failure: the worker claimed something the repository disproves.
//	unverified       → succeeded, BUT the evidence and the verdict are recorded on the execution and
//	                   stamped onto the step run, so the acceptance review and the UI can say
//	                   "nothing confirmed this". It is not silent.
//
// WHY UNVERIFIED IS NOT A FAILURE. Failing it would make a step fail whenever a project declares no
// gate — which is every project today, including this one. That would fail every run immediately and
// the mechanism would be switched off within a day, which is the worst possible outcome: it would
// leave the platform exactly as unverified as before, with the added belief that verification was
// tried. Recording it honestly and NOT blocking is the version that survives contact with a real
// backlog. The pressure to declare a gate belongs in the prompts and the review, not in a hard fail.
//
// WHY IT IS NOT SILENT SUCCESS EITHER. `unverified` is written into the execution's error_message
// slot as a NOTICE (the same pattern the worktree reconciler uses for worktreeNoticeKind) and into
// the step run's result as `_verification`. The acceptance review renders it. An operator reading a
// run can therefore tell "checked and confirmed" from "nobody checked", which is the distinction the
// whole complaint — "sometimes it works, sometimes it doesn't, and I find out later" — was about.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/verify"
)

// verifyNoticePrefix marks an error_message as an advisory notice rather than a failure. Same
// convention as worktreeNoticeKind: the UI and an operator must be able to tell a step that is fine
// but unconfirmed from a step that broke.
const verifyNoticePrefix = "verification notice: "

// verifyResultKey is the step-run result key carrying the evidence record.
const verifyResultKey = "_verification"

// verdictForOutcome decides the evidence verdict for an execution that the worker reported as a
// SUCCESS.
//
// A worker that reported FAILURE is not verified: it already failed, its work is not being accepted,
// and running the project's whole gate to establish that would burn minutes to learn nothing. So this
// only runs on the success path — which is exactly the path that has been lying.
func (r *TaskReconciler) verdictForOutcome(ctx context.Context, exec db.ExecutionRow) (verify.Result, bool) {
	// A follow-up execution continues an existing conversation; its "work" is a delta on a step that
	// was already verified, and re-running the gate would double-count. Treat as not-applicable.
	if exec.IsFollowUp {
		return verify.Result{}, false
	}
	projectDir := r.lookupProjectDir(ctx, exec.ProjectID)
	if projectDir == "" {
		return verify.Result{}, false
	}
	// No worktree means no isolated state to read. In-place runs (git_strategy=none, or a non-repo
	// project) still get the declared gate measured against the project dir, because that is the only
	// evidence available for them.
	worktreePath := ""
	if exec.WorktreePath != nil {
		worktreePath = *exec.WorktreePath
	}
	if worktreePath == "" {
		worktreePath = "" // gatherEvidence abstains on the git checks when this is empty
	}

	// The step's contract decides whether a deliverable was expected. A plan-only step must not be
	// failed for producing no commits — that is a correct outcome, and the decision function SKIPS
	// those checks rather than abstaining.
	declares, skipReason := r.stepDeclaresDeliverable(ctx, exec)

	in := evidenceInput{
		projectDir:              projectDir,
		worktreePath:            worktreePath,
		baseline:                r.baselineForEvidence(ctx, projectDir, worktreePath),
		claimedFiles:            r.claimedFilesForExecution(ctx, exec),
		stepDeclaresDeliverable: declares,
		skipReason:              skipReason,
	}
	// The base ref the step's work was cut from. The worktree carries the run branch, which was cut
	// from the integration branch, so the merge-base is the honest answer when nothing is recorded.
	if exec.WorktreeBranch != nil && *exec.WorktreeBranch != "" {
		in.integrationBranch = r.integrationBranchForProject(ctx, exec.ProjectID)
	}

	ev := r.gatherEvidence(ctx, in)
	return verify.Collect(ctx, ev), true
}

// stepDeclaresDeliverable reports whether this execution's step is expected to change files.
//
// THE DEFAULT IS TRUE. An unknown step that might have changed code should be HELD TO the commit
// check, because the failure mode of wrongly expecting a deliverable is a loud, obvious, immediately
// fixable "no commits" — whereas wrongly NOT expecting one silently exempts a code step from the only
// universal check there is. The two steps that legitimately produce nothing are named here, and the
// list is deliberately short so adding an exemption requires a reason.
//
// The signal is the step's worker/name, because that is what decides the step's job:
//   - the Principal Software Architect writes a gitignored plan note and commits nothing
//   - a human approval step is a gate, not work
//   - a loop_decision / end / parallel / project step is graph plumbing
func (r *TaskReconciler) stepDeclaresDeliverable(ctx context.Context, exec db.ExecutionRow) (bool, string) {
	if exec.WorkflowStepID == "" {
		// Standalone dispatch: the worker was assigned a work item, so it owes a deliverable.
		return true, ""
	}
	stepID := exec.WorkflowStepID
	// Graph-plumbing step kinds are identified by their step id prefix in practice, but the reliable
	// signal is the worker: these step kinds carry no worker at all, so an execution with one is a
	// task/approval step by construction.
	if exec.WorkerName == "" {
		return true, ""
	}
	lower := strings.ToLower(stepID + " " + exec.WorkerName)
	switch {
	case strings.Contains(lower, "architect"):
		// The architect's deliverable is a plan. In this repository the plan directory is gitignored
		// (`architecture-notes/`), so it commits nothing BY DESIGN — established repeatedly in the
		// FACTS LEARNED ledger. Holding it to a commit check would fail every architect step.
		return false, "architect steps deliver a plan note, which this project gitignores"
	case strings.Contains(lower, "approver"):
		return false, "approval steps are gates, not work"
	case strings.Contains(lower, "loop decision"), strings.HasSuffix(lower, "end"):
		return false, "graph plumbing carries no deliverable"
	}
	return true, ""
}

// claimedFilesForExecution returns the paths the worker's own report said it touched.
//
// It reads the persisted `_touched_files` on the work item's results — the same set the prompt hands
// DOWNSTREAM steps — rather than re-parsing the report here, so the platform and the next worker
// cannot disagree about what was claimed.
func (r *TaskReconciler) claimedFilesForExecution(ctx context.Context, exec db.ExecutionRow) map[string]struct{} {
	if exec.TenantID == "" || exec.TaskID == "" {
		return nil
	}
	ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID)
	if err != nil {
		return nil
	}
	defer ttx.Rollback(ctx)
	var raw []byte
	if err = ttx.Tx.QueryRow(ctx,
		`SELECT results FROM work_items WHERE tenant_id = $1 AND id = $2`,
		exec.TenantID, exec.TaskID,
	).Scan(&raw); err != nil || len(raw) == 0 {
		return nil
	}
	var results struct {
		Touched []any `json:"_touched_files"`
	}
	if jerr := json.Unmarshal(raw, &results); jerr != nil {
		return nil
	}
	set := map[string]struct{}{}
	for _, v := range results.Touched {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			set[s] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// integrationBranchForProject resolves the branch a run's work is measured against.
//
// It reuses integrationTarget's convention (`develop` when it exists, else the repo's own default) but
// without the git probing: this is a name for a merge-base computation, and a wrong name simply makes
// the differential abstain rather than lie.
func (r *TaskReconciler) integrationBranchForProject(_ context.Context, projectID string) string {
	_ = projectID
	return defaultIntegrationBranch
}

// applyVerdict folds the evidence verdict into the outcome.
//
// THIS IS THE ONLY PLACE A VERDICT AFFECTS A STATUS. Kept as a pure function of (verdict, current
// outcome) so the policy is readable in one screen and testable without a database.
//
//	worker failed           → unchanged. A failed worker is never upgraded by evidence, and there is
//	                          nothing to substantiate.
//	verified                → unchanged (succeeded).
//	unsubstantiated         → succeeded becomes FAILED. The claim is contradicted.
//	unverified              → unchanged (succeeded) + a notice. Deliberate, documented at the top.
func applyVerdict(succeeded bool, res verify.Result) (bool, string) {
	if !succeeded {
		return false, ""
	}
	switch res.Verdict {
	case verify.VerdictUnsubstantiated:
		reason := "the worker reported success but the evidence contradicts it"
		if res.Reason != "" {
			reason += " — " + res.Reason
		}
		return false, reason
	case verify.VerdictUnverified:
		reason := "the worker reported success and nothing confirmed it"
		if res.Reason != "" {
			reason += " — " + res.Reason
		}
		return true, verifyNoticePrefix + reason
	}
	return true, ""
}

// recordVerification stamps the evidence onto the step run so the acceptance review and the UI can
// read it. Best-effort: a failure here must never change an outcome that has already been decided.
func (r *TaskReconciler) recordVerification(ctx context.Context, exec db.ExecutionRow, res verify.Result) {
	if exec.TenantID == "" || exec.ID == "" {
		return
	}
	ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID)
	if err != nil {
		r.log.Warn("verify: record evidence: begin tx", "error", err)
		return
	}
	defer ttx.Rollback(ctx)

	// Merge into the step run's existing result JSON rather than replacing it — the result also
	// carries _work_item_id, _summary and the rest, and clobbering it would break dispatch.
	var raw []byte
	err = ttx.Tx.QueryRow(ctx,
		`SELECT result FROM workflow_step_runs
		  WHERE tenant_id = $1 AND worker_execution_id = $2 LIMIT 1`,
		exec.TenantID, exec.ID,
	).Scan(&raw)
	if err != nil {
		// No step run (a standalone dispatch). Nothing to stamp.
		return
	}
	merged := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &merged)
	}
	merged[verifyResultKey] = map[string]any{
		"verdict": res.Verdict.String(),
		"reason":  res.Reason,
		"checks":  res.Checks,
	}
	payload, merr := json.Marshal(merged)
	if merr != nil {
		return
	}
	if _, uerr := ttx.Tx.Exec(ctx,
		`UPDATE workflow_step_runs SET result = $1, updated_at = now()
		  WHERE tenant_id = $2 AND worker_execution_id = $3`,
		payload, exec.TenantID, exec.ID,
	); uerr != nil {
		r.log.Warn("verify: record evidence: update", "execution", exec.ID, "error", uerr)
		return
	}
	if cerr := ttx.Commit(ctx); cerr != nil {
		r.log.Warn("verify: record evidence: commit", "execution", exec.ID, "error", cerr)
	}
}

// verificationNotice renders the one-line notice an operator sees for an unverified step. Exported
// shape kept simple (a string) because it rides the existing error_message field rather than needing
// a schema change.
func verificationNotice(res verify.Result) string {
	if res.Verdict == verify.VerdictVerified {
		return ""
	}
	detail := strings.TrimSpace(res.Reason)
	if detail == "" {
		detail = string(res.Verdict)
	}
	return fmt.Sprintf("%s%s", verifyNoticePrefix, detail)
}

// loadExecutionRow reads the execution row OnResult was told about.
//
// OnResult's signature carries only an id, and every fact the evidence gate needs (project, worktree,
// step, task) lives on the row. Reading it here keeps that lookup in one place instead of widening an
// interface used by several callers.
func (r *TaskReconciler) loadExecutionRow(ctx context.Context, execID string) (db.ExecutionRow, error) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return db.ExecutionRow{}, err
	}
	defer ttx.Rollback(ctx)
	row, gerr := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if gerr != nil {
		return db.ExecutionRow{}, gerr
	}
	_ = ttx.Commit(ctx)
	return row, nil
}

// verificationSummaryLine is the per-step rendering inside an acceptance review.
type verificationSummaryLine struct {
	step    string
	verdict string
	reason  string
}

// collectVerificationSummaries extracts the `_verification` record from each step run.
//
// Sorted by the VERDICT ORDER that matters to a reader: unsubstantiated first (something is wrong),
// then unverified (nobody checked), then verified. A review that lists the confirmed steps first
// buries the two that need attention.
func collectVerificationSummaries(stepRuns []db.WorkflowStepRunRow) []verificationSummaryLine {
	var out []verificationSummaryLine
	for _, sr := range stepRuns {
		if sr.SupersededBy != "" || len(sr.Result) == 0 {
			continue
		}
		var meta struct {
			Verification *struct {
				Verdict string `json:"verdict"`
				Reason  string `json:"reason"`
			} `json:"_verification"`
		}
		if err := json.Unmarshal(sr.Result, &meta); err != nil || meta.Verification == nil {
			continue
		}
		out = append(out, verificationSummaryLine{
			step:    sr.StepName,
			verdict: meta.Verification.Verdict,
			reason:  meta.Verification.Reason,
		})
	}
	rank := map[string]int{
		verify.VerdictUnsubstantiated.String(): 0,
		verify.VerdictUnverified.String():      1,
		verify.VerdictVerified.String():        2,
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[out[i].verdict], rank[out[j].verdict]
		if ri != rj {
			return ri < rj
		}
		return out[i].step < out[j].step
	})
	return out
}

// formatVerificationSummary renders the evidence block for the acceptance review.
//
// IT ALWAYS RENDERS. A run whose steps carry no verification record at all is itself the finding —
// "nothing was checked" — and silently omitting the section is exactly the silent-success the whole
// feature exists to remove. An empty block would be worse still, so the "no evidence recorded" case is
// stated explicitly.
func formatVerificationSummary(stepRuns []db.WorkflowStepRunRow) string {
	lines := collectVerificationSummaries(stepRuns)
	if len(lines) == 0 {
		return "### Verification\n\nNo verification evidence was recorded for any step in this run. " +
			"The worker reports were accepted on their own word — treat the outcome above as UNCONFIRMED.\n\n"
	}
	var verified, unverified, unsubstantiated int
	var b strings.Builder
	b.WriteString("### Verification\n\n")
	for _, l := range lines {
		sym := "✓"
		switch l.verdict {
		case verify.VerdictUnverified.String():
			sym = "?"
			unverified++
		case verify.VerdictUnsubstantiated.String():
			sym = "✗"
			unsubstantiated++
		default:
			verified++
		}
		if l.reason != "" {
			b.WriteString(fmt.Sprintf("- %s **%s** — %s: %s\n", sym, l.step, l.verdict, l.reason))
			continue
		}
		b.WriteString(fmt.Sprintf("- %s **%s** — %s\n", sym, l.step, l.verdict))
	}
	b.WriteString("\n")
	total := len(lines)
	switch {
	case unverified > 0 || unsubstantiated > 0:
		b.WriteString(fmt.Sprintf("%d of %d step(s) verified; %d unverified, %d contradicted. "+
			"An UNVERIFIED step is not a failure and is not a confirmation — the platform could not "+
			"determine it. Declaring the project's gate in `.orchicon/verify` is what makes a step "+
			"verifiable.\n\n", verified, total, unverified, unsubstantiated))
	default:
		b.WriteString(fmt.Sprintf("All %d step(s) verified against the repository and the project's gate.\n\n", total))
	}
	return b.String()
}
