package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// Run-blockage kinds. Stable, machine-readable, and reported verbatim on the
// wire so a client can branch on them without parsing prose.
const (
	// blockageNone — the run is not waiting on anything.
	blockageNone = ""
	// blockageProjectAtCapacity — the project's dispatch cap is fully
	// occupied, so no step of ANY run in it can dispatch. The ROOT cause; the
	// per-step wait below is its symptom, so this wins when both hold.
	blockageProjectAtCapacity = "project_at_capacity"
	// blockageDispatchDeferred — a step is waiting for a dispatch slot
	// (worker or project capacity) and says so in its `_dispatch_wait` note.
	blockageDispatchDeferred = "dispatch_deferred"
	// blockageRecoveringUnresumable — a step sits in `recovering` with no
	// recovery that can resolve it, so it will never re-dispatch on its own.
	blockageRecoveringUnresumable = "recovering_unresumable"
	// blockageAwaitingApproval — a human approval step is pending.
	blockageAwaitingApproval = "awaiting_approval"
	// blockageStalledUnexplained — running, nothing active, and none of the
	// causes above identified. Reported rather than hidden: "I don't know" is
	// a finding, and it is the case that most needs a human.
	blockageStalledUnexplained = "stalled_unexplained"
)

// diagnosisInput is the fully-assembled recorded state a diagnosis is drawn
// from. Keeping it a plain struct (rather than doing the queries inside the
// classifier) is what makes the classification unit-testable without a
// database: every rule below is a function of these fields alone.
type diagnosisInput struct {
	runStatus    string
	runtimeReady bool
	projectID    string
	// active and limit are the count the dispatch gate compares and the
	// EFFECTIVE limit it enforces (project floored by tenant) — the same two
	// numbers the gate uses, so the card can never disagree with it.
	active int
	limit  int
	// stepRuns are the run's step runs; stuck are the project's slot-holding
	// executions already marked not-well by the liveness layer.
	stepRuns []db.WorkflowStepRunRow
	stuck    []db.ExecutionRow
	now      time.Time
}

// isWaitingStepRun reports whether a step run is in a state that means "this
// run cannot advance without something happening". Deliberately narrow:
//
//   - pending is NOT waiting — it is simply not armed yet (an earlier step
//     still has to succeed), and calling that a blockage would make every
//     healthy run report itself blocked.
//   - succeeded/failed/skipped are terminal.
//   - running WITH an execution is work in progress.
//   - running WITHOUT an execution is the case this whole feature exists for:
//     it was dispatched and produced nothing.
func isWaitingStepRun(sr db.WorkflowStepRunRow) bool {
	switch sr.Status {
	case domain.StepRunReady, domain.StepRunRecovering, domain.StepRunApprovalPending:
		return true
	case domain.StepRunRunning:
		return sr.WorkerExecutionID == ""
	}
	return false
}

// dispatchWaitNote returns the TaskReconciler's deferral note from a step
// run's result, if it recorded one. Its presence means the dispatch was
// DEFERRED for capacity — a healthy dispatch with no slot — which is a very
// different situation from a dispatch that was attempted and lost.
func dispatchWaitNote(sr db.WorkflowStepRunRow) string {
	if len(sr.Result) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(sr.Result, &m) != nil {
		return ""
	}
	note, _ := m["_dispatch_wait"].(string)
	return note
}

// buildRunDiagnosis classifies why a run is or is not progressing.
//
// This is a REPORT. It reads recorded state and names the cause; it never
// decides that anything is dead and never clears anything. The strongest claim
// it makes is "these executions are holding slots while carrying a status the
// liveness layer already marked not-well", which is exactly what the operator
// needs to see — and the only thing entitled to act on it is the reaper, via
// ReleaseStuckExecutions, where its own liveness probe makes the call.
func buildRunDiagnosis(in diagnosisInput) *apiv1.RunDiagnosis {
	d := &apiv1.RunDiagnosis{
		ProjectId:        in.projectID,
		ActiveExecutions: int32(in.active),
		DispatchLimit:    int32(in.limit),
	}

	// Only a RUNNING run can be blocked. A terminal run is finished, and a
	// pending one has simply not started.
	if in.runStatus != domain.WorkflowRunRunning {
		return d
	}

	var waiting []db.WorkflowStepRunRow
	var approvalWaiting, recoveringWaiting, dispatchWaiting bool
	var waitNote string
	for _, sr := range in.stepRuns {
		if !isWaitingStepRun(sr) {
			continue
		}
		waiting = append(waiting, sr)
		d.BlockedStepIds = append(d.BlockedStepIds, sr.ID)
		switch sr.Status {
		case domain.StepRunApprovalPending:
			approvalWaiting = true
		case domain.StepRunRecovering:
			recoveringWaiting = true
		}
		if note := dispatchWaitNote(sr); note != "" {
			dispatchWaiting = true
			waitNote = note
		}
	}

	// The project cap is the ROOT cause when it is full: it stops every run in
	// the project, and it explains a step's deferral rather than competing with
	// it. Reported first for that reason.
	atCapacity := in.limit > 0 && in.active >= in.limit
	d.AtCapacity = atCapacity
	d.StuckExecutions = stuckExecutions(in.stuck, in.now)
	d.Clearable = len(d.StuckExecutions) > 0

	switch {
	case !in.runtimeReady:
		// Above the cap check: with no usable runtime serve, nothing can
		// dispatch at all, so freeing a slot would not help — and saying
		// otherwise would send the operator to the wrong remedy.
		d.Blocked = true
		d.BlockageKind = blockageDispatchDeferred
		d.Detail = "The run's runtime is not ready, so no step can dispatch yet. " +
			"The reconciler is bringing the runtime up; this resolves on its own."

	case atCapacity:
		d.Blocked = true
		d.BlockageKind = blockageProjectAtCapacity
		d.Detail = fmt.Sprintf(
			"The project is at its dispatch limit (%d/%d), so this run cannot dispatch any step. %s",
			in.active, in.limit, clearableSentence(len(d.StuckExecutions)))

	case dispatchWaiting:
		d.Blocked = true
		d.BlockageKind = blockageDispatchDeferred
		d.Detail = "A step's dispatch was deferred for capacity: " + waitNote + "."

	case recoveringWaiting:
		d.Blocked = true
		d.BlockageKind = blockageRecoveringUnresumable
		d.Detail = "A step is held in recovery and cannot re-dispatch. If its recovery never " +
			"resolved, the step is failed once the recovering-stall window expires and the run " +
			"can be retried."

	case approvalWaiting:
		d.Blocked = true
		d.BlockageKind = blockageAwaitingApproval
		d.Detail = "Waiting on a human approval decision."

	case len(waiting) > 0:
		d.Blocked = true
		d.BlockageKind = blockageStalledUnexplained
		d.Detail = fmt.Sprintf("%d step(s) are waiting to dispatch and no capacity or recovery "+
			"cause was identified. The TaskReconciler re-evaluates them on its next pass.", len(waiting))
	}
	return d
}

// clearableSentence phrases the stuck-execution count so an empty list is not
// read as a failure — zero stuck executions alongside a full cap means the
// slots are held by LIVE work, which is not something to clear.
func clearableSentence(n int) string {
	if n == 0 {
		return "No executions are marked unhealthy or stalled, so the slots are held by live work " +
			"and the run will proceed as those finish."
	}
	return fmt.Sprintf("%d execution(s) are holding slots while marked unhealthy/stalled — "+
		"release them to free capacity. (Slots may also be held by live work, which release "+
		"never touches.)", n)
}

// stuckExecutions converts the project's not-well slot holders for the wire.
func stuckExecutions(rows []db.ExecutionRow, now time.Time) []*apiv1.StuckExecution {
	out := make([]*apiv1.StuckExecution, 0, len(rows))
	for _, e := range rows {
		se := &apiv1.StuckExecution{
			Id:          e.ID,
			WorkerName:  e.WorkerName,
			Status:      e.Status,
			HealthState: e.HealthState,
			RunId:       e.WorkflowRunID,
		}
		if e.StartedAt != nil {
			se.StartedAtUnix = e.StartedAt.Unix()
			age := now.Sub(*e.StartedAt)
			if age > 0 {
				se.AgeSeconds = int64(age.Seconds())
			}
		}
		out = append(out, se)
	}
	return out
}

// GetWorkflowRunDiagnosis answers "why is this run not moving?".
//
// Read-only, and cheap enough to poll alongside the run: two counts, one small
// per-project list, and the run's own step runs. Every number it reports comes
// from the same helpers the dispatch gate and the reaper use — the effective
// dispatch limit and the active-execution count are literally the gate's own
// queries — so the card cannot drift from the behaviour it describes.
func (s *Service) GetWorkflowRunDiagnosis(ctx context.Context, req *connect.Request[apiv1.GetWorkflowRunDiagnosisRequest]) (*connect.Response[apiv1.GetWorkflowRunDiagnosisResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.RunId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, req.Msg.RunId)
	if err != nil {
		return nil, mapDBError(err)
	}
	stepRuns, err := db.ListWorkflowStepRuns(ctx, ttx.Tx, tenantID, run.ID)
	if err != nil {
		return nil, mapDBError(err)
	}

	in := diagnosisInput{
		runStatus:    run.Status,
		runtimeReady: run.RuntimeReady,
		projectID:    run.ProjectID,
		stepRuns:     stepRuns,
		now:          time.Now().UTC(),
	}
	// The project-level facts. A run whose project row is gone is a real state
	// (the project was deleted under a live run): report the run's own blockage
	// and simply make no capacity claim, rather than failing the whole
	// diagnosis — an absent cap is not a full one.
	if run.ProjectID != "" {
		active, aerr := db.CountActiveExecutionsForProject(ctx, ttx.Tx, tenantID, run.ProjectID)
		if aerr != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("count active executions: %w", aerr))
		}
		in.active = active

		limit, lerr := db.GetEffectiveDispatchLimit(ctx, ttx.Tx, tenantID, run.ProjectID)
		switch {
		case lerr == nil:
			in.limit = limit
		case errors.Is(lerr, db.ErrNotFound):
			s.log.Warn("run diagnosis: project row missing; reporting no dispatch limit",
				"run", run.ID, "project", run.ProjectID)
		default:
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolve dispatch limit: %w", lerr))
		}

		stuck, serr := db.ListStuckExecutionsForProject(ctx, ttx.Tx, tenantID, run.ProjectID)
		if serr != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list stuck executions: %w", serr))
		}
		in.stuck = stuck
	}

	return connect.NewResponse(&apiv1.GetWorkflowRunDiagnosisResponse{
		Diagnosis: buildRunDiagnosis(in),
	}), nil
}
