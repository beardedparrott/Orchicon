package scheduler

// verify_onresult.go — WHERE THE VERDICT IS DECIDED.
//
// OnResult has always taken the worker's word: if the process exited cleanly and the summary said
// `success`, the execution succeeded. The missing half is that nothing asked the REPOSITORY whether
// the claim was true. This file supplies that half, and it is deliberately called from exactly one
// place — the terminal transition — so there is one answer to "how did this step get its status".
//
// EVERY VERDICT LANDS THE SAME WAY: the step's status is unchanged, and anything worth saying goes into the
// execution's error_message slot as a notice plus the step run's `_verification` record.
//
//	verified         → succeeded, silently. Good work is not decorated.
//	unsubstantiated  → succeeded, with a notice saying the repository does not support the claim.
//	unverified       → succeeded, with a notice saying nothing confirmed the claim.
//	not_applicable   → succeeded, silently: a plan-only or approval step is not a checked thing.
//
// WHY NOTHING FAILS. The gate was built to be able to fail a step whose claim the repository contradicted. That
// requires the platform to be RIGHT about the contradiction — which means parsing a project's output, comparing
// baselines and attributing failures, each of which can be wrong for reasons the worker does not control. The
// operator's verdict on that trade: "it is too strict, it is language-specific, and it is holding people back."
// So the verdict became information.
//
// THE COST, named rather than buried in a commit message: a worker that reports success having changed NOTHING now
// lands green with a notice, where it used to fail. The evidence still says so and the review still shows it; the
// platform simply does not act on it. See applyVerdict.
//
// NOTHING IS SILENT, which is the half worth keeping: the notice rides the same slot the worktree reconciler uses
// for its advisories, so an operator can still tell "checked and confirmed" from "nobody checked".

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
// verifyNoticePrefix is the shared marker, defined once in internal/verify so the UI can recognise a notice without
// importing the scheduler. Kept as a local name because the rest of this file reads better for it.
const verifyNoticePrefix = verify.NoticePrefix

// verifyResultKey is the step-run result key carrying the evidence record.
const verifyResultKey = "_verification"

// verdictForOutcome decides the evidence verdict for an execution that the worker reported as a
// SUCCESS.
//
// A worker that reported FAILURE is not verified: it already failed, its work is not being accepted,
// and running the project's whole gate to establish that would burn minutes to learn nothing. So this
// only runs on the success path — which is exactly the path that has been lying.
func (r *TaskReconciler) verdictForOutcome(ctx context.Context, exec db.ExecutionRow, output string) (verify.Result, bool) {
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
	res := verify.Collect(ctx, ev)

	// AC REPORT — the semantic half, explicitly NOT a pass/fail gate.
	//
	// The repository checks above answer "did work happen and does the gate hold". They cannot answer
	// "did the worker address each acceptance criterion", because that is semantic and not mechanically
	// decidable. What IS decidable is whether the worker ANSWERED them and whether the answer carries
	// anything checkable — so an unanswered or unevidenced criterion is surfaced to the operator rather
	// than being rounded into a green step.
	//
	// It ABSTAINS rather than failing when criteria were reported without evidence, deliberately: the
	// evidence heuristic can be wrong, and failing an honest step on a heuristic is worse than showing
	// the operator. An explicit FAIL verdict in the worker's own report does contradict its own
	// `success`, though, and that is a contradiction.
	res = appendACReportCheck(res, output, skipReasonForNoDeliverable(declares, skipReason))

	return res, true
}

// skipReasonForNoDeliverable converts the deliverable exemption into an AC-report skip reason, or "" when the
// step DOES owe a deliverable (so a missing report is a real omission and must abstain).
//
// The two questions are the same question: a step that owes no code also owes no acceptance-criteria account,
// because the criteria describe outcomes a LATER step produces. Deriving one from the other keeps them from
// drifting — and it is what lets the architect's deliberate "no AC REPORT, and here is why" be recognised as
// correct rather than as an omission.
func skipReasonForNoDeliverable(declares bool, skipReason string) string {
	if declares {
		return ""
	}
	if r := strings.TrimSpace(skipReason); r != "" {
		return r
	}
	return "this step does not declare a code deliverable"
}

// appendACReportCheck folds the worker's own AC REPORT into the verdict.
//
// PRECEDENCE, and why:
//   - no report block at all → ABSTAIN. Many work items have no acceptance criteria, and a step is not
//     suspect merely for skipping a section it was told to skip. The abstention lands the step in
//     "unverified", which is accurate.
//   - the worker reported a FAIL or NOT VERIFIED on any criterion while claiming `success` overall →
//     FAIL. That is the worker contradicting itself, which is a positive fact, not an absence. (NOT
//     VERIFIED counts: a worker that says "I could not establish criterion 3" and then reports success
//     has told the operator the step is not done.)
//   - every criterion PASS but some without evidence → ABSTAIN. Surfaced, not punished: the heuristic
//     is deliberately generous but not omniscient.
//   - every criterion PASS with evidence → PASS.
func appendACReportCheck(res verify.Result, output, skipReason string) verify.Result {
	rep := verify.ParseACReport(output)
	switch {
	case !rep.Present && skipReason != "":
		// THE STEP OWES NO ACCEPTANCE-CRITERIA REPORT, and the caller said why. A plan-only or approval step
		// cannot assert outcomes for work a LATER step performs, so skipping the block is correct behaviour
		// rather than an omission — SKIP, not abstain. (An abstention would hold the step at `unverified` for
		// declining a question that was never its job to answer.)
		res.Add("acceptance_criteria", verify.CheckSkip, skipReason)
		return res.Conclude()
	case !rep.Present:
		res.Add("acceptance_criteria", verify.CheckAbstain,
			"the worker reported no acceptance-criteria outcomes")
		return res.Conclude()
	}
	var failed, unevidenced int
	for _, a := range rep.Answers {
		if a.Verdict == verify.ACFail || a.Verdict == verify.ACNotVerified {
			failed++
			continue
		}
		if a.Verdict == verify.ACMalformed || (a.Verdict == verify.ACPass && !a.Evidenced) {
			unevidenced++
		}
	}
	switch {
	case failed > 0:
		res.Add("acceptance_criteria", verify.CheckFail, rep.Summary())
	case unevidenced > 0:
		res.Add("acceptance_criteria", verify.CheckAbstain, rep.Summary())
	default:
		res.Add("acceptance_criteria", verify.CheckPass, rep.Summary())
	}
	return res.Conclude()
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

// applyVerdict folds the evidence verdict into the outcome — as a RECORD, never as a status change.
//
// THE OPERATOR'S DECISION, and it is a real simplification of what this function used to do.
//
// The evidence gate was built to be able to FAIL a step whose claim the repository contradicted. That is a
// powerful idea and it was the wrong shape for this product: to act on a contradiction the platform has to be
// right about it, and being right means parsing output, comparing baselines and attributing failures — every one
// of which can be wrong in a way the worker cannot control. The operator's verdict: "it is too strict, it is
// language-specific, and it is holding people back instead of helping them get work done."
//
// So the verdict is now INFORMATION. It is computed, recorded on the execution and rendered in the acceptance
// review, and it never changes whether a step succeeded. Success/failure is what it has always been: the worker's
// own verdict plus the process exit code.
//
// WHAT IS LOST, stated plainly rather than buried: the smoking-gun case — a worker that reports success having
// changed nothing — now lands GREEN with a notice attached, instead of failing. The evidence still says so; the
// notice still says so; nothing blocks anyone. That is the trade, chosen deliberately.
//
// NOTHING IS SILENT, though, which is the half worth keeping. An unverified or contradicted step carries a
// sentence saying what was and was not established, in the same slot the worktree reconciler uses for its
// advisories.
func applyVerdict(succeeded bool, res verify.Result) (bool, string) {
	if !succeeded {
		// A FAILED worker keeps its failure and gets no evidence notice: there is nothing to substantiate, and
		// adding "unverified" to an already-failing step is noise at the moment the operator wants the cause.
		return false, ""
	}
	switch res.Verdict {
	case verify.VerdictUnsubstantiated:
		// The strongest thing the platform can say without acting on it, so it says it plainly and does not
		// soften it — the operator should be able to see that this run's account of itself does not match the
		// repository.
		reason := "the worker reported success but the repository does not support it"
		if res.Reason != "" {
			reason += " — " + res.Reason
		}
		return true, verifyNoticePrefix + reason
	case verify.VerdictUnverified:
		// THE OPENER MUST NOT OVERCLAIM, and the old one did: "nothing confirmed it" was printed for a step whose
		// acceptance-criteria check had PASSED with evidence, because the sentence summarised the verdict rather
		// than what the checks actually established. Naming the counts keeps it true whichever way it lands.
		confirmed, total := countConfirmed(res)
		reason := fmt.Sprintf("the platform could not confirm this step mechanically (%d of %d check(s) "+
			"confirmed)", confirmed, total)
		if res.Reason != "" {
			reason += " — " + res.Reason
		}
		return true, verifyNoticePrefix + reason
	case verify.VerdictNotApplicable:
		// No notice. A plan-only step is CORRECT when it produces a plan, and flagging every architect and
		// approval step is how an operator learns to ignore the notice on the steps where it matters.
		return true, ""
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
		verify.VerdictNotApplicable.String():   3,
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
	var verified, unverified, unsubstantiated, notApplicable int
	var b strings.Builder
	b.WriteString("### Verification (advisory)\n\n")
	for _, l := range lines {
		sym := "✓"
		switch v := verify.Verdict(l.verdict); {
		case v.IsSuccess():
			// CONFIRMED, and IsSuccess is the single place that decides what that word covers — so the count
			// and the vocabulary cannot drift apart.
			verified++
		case v == verify.VerdictUnverified:
			sym = "?"
			unverified++
		case v == verify.VerdictUnsubstantiated:
			sym = "!" // advisory: nothing was failed on this
			unsubstantiated++
		case v == verify.VerdictNotApplicable:
			// A step that produces no repository evidence (a plan, an approval) is neither confirmed nor
			// at fault. It gets its own mark so it cannot be read as either — counting it as "verified"
			// would inflate the confirmed total, and counting it as "unverified" would train the operator
			// to ignore that word on the steps where it means something.
			sym = "·"
			notApplicable++
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
	// NOTHING IN THIS SECTION CHANGED THE RUN'S OUTCOME, and it says so before it says anything else. Without
	// that sentence an operator reads a `!` mark as a failure the platform chose to tolerate, when in fact the
	// platform is not acting on it at all.
	if unverified > 0 || unsubstantiated > 0 {
		b.WriteString("This section is a RECORD. No step's status was changed by it.\n\n")
	}
	switch {
	case unverified > 0 || unsubstantiated > 0:
		b.WriteString(fmt.Sprintf("%d of %d step(s) confirmed; %d unverified, %d not supported by the "+
			"repository. An UNVERIFIED step is not a failure and is not a confirmation — the platform could "+
			"not determine it. Declaring a result format in `.orchicon/verify` is what makes a step's gate "+
			"measurable.\n\n", verified, total, unverified, unsubstantiated))
	case notApplicable > 0 && verified == 0:
		b.WriteString(fmt.Sprintf("None of the %d step(s) produce repository evidence to examine (%d "+
			"not-applicable), so nothing here was mechanically confirmed. That is expected for a run made "+
			"of planning and approval steps — it is not a bad sign, and it is not a confirmation.\n\n",
			total, notApplicable))
	case notApplicable > 0:
		b.WriteString(fmt.Sprintf("%d of %d step(s) verified; %d produce no repository evidence and were "+
			"not mechanically checked.\n\n", verified, total, notApplicable))
	default:
		b.WriteString(fmt.Sprintf("All %d step(s) confirmed against the repository and the project's gate.\n\n",
			total))
	}
	return b.String()
}

// countConfirmed reports how many of a result's checks reached a conclusion and how many there were.
//
// A check that passed is CONFIRMED; one that abstained was not measured; one that was skipped did not apply. The
// count exists so a notice can say what WAS established instead of claiming nothing was.
func countConfirmed(res verify.Result) (confirmed, total int) {
	for _, c := range res.Checks {
		if c.Status == verify.CheckSkip {
			continue
		}
		total++
		if c.Status == verify.CheckPass {
			confirmed++
		}
	}
	return confirmed, total
}
