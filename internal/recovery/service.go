// Package recovery implements the RecoveryService Connect handler
// (docs/07_API_Specification.md §3.6) and the Recovery Workflow Engine
// (docs/06).
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxReasonLen   = 1000
	maxActorLen    = 200
	maxSummaryLen  = 1 << 14
)

// Service implements the RecoveryService Connect handler
// (apiv1connect.RecoveryServiceHandler).
type Service struct {
	pool       *db.Pool
	log        *slog.Logger
	engine     *Engine
	subscriber eventbus.Subscriber
	apiv1connect.UnimplementedRecoveryServiceHandler
}

var _ apiv1connect.RecoveryServiceHandler = (*Service)(nil)

// NewService constructs a RecoveryService handler.
func NewService(pool *db.Pool, log *slog.Logger, engine *Engine, sub eventbus.Subscriber) *Service {
	return &Service{pool: pool, log: log, engine: engine, subscriber: sub}
}

// TriggerRecovery manually triggers a recovery for a Task (docs/06 §2
// manual trigger).
func (s *Service) TriggerRecovery(ctx context.Context, req *connect.Request[apiv1.TriggerRecoveryRequest]) (*connect.Response[apiv1.TriggerRecoveryResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.TaskId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("task_id must not be empty"))
	}
	triggerReason, err := validateTextField(req.Msg.TriggerReason, maxReasonLen, "trigger_reason")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if triggerReason == "" {
		triggerReason = "manual"
	}
	// Resolve the latest (failed) execution for the task.
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	execs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{TenantID: tenantID, TaskID: req.Msg.TaskId, PageSize: 1})
	ttx.Rollback(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list executions: %w", err))
	}
	failedExecID := ""
	if len(execs) > 0 {
		failedExecID = execs[0].ID
	}
	if err := s.engine.TriggerOnFailure(ctx, tenantID, req.Msg.TaskId, failedExecID, triggerReason); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Fetch the created recovery.
	ttx2, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	rec, err := db.GetActiveRecoveryForTask(ctx, ttx2.Tx, tenantID, req.Msg.TaskId)
	ttx2.Rollback(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("recovery not found after trigger: %w", err))
	}
	return connect.NewResponse(&apiv1.TriggerRecoveryResponse{Recovery: recoveryRowToProto(rec)}), nil
}

// CancelRecovery transitions a running recovery to cancelled.
func (s *Service) CancelRecovery(ctx context.Context, req *connect.Request[apiv1.CancelRecoveryRequest]) (*connect.Response[apiv1.CancelRecoveryResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.RecoveryId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("recovery_id must not be empty"))
	}
	reason, err := validateTextField(req.Msg.Reason, maxReasonLen, "reason")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetRecoveryExecution(ctx, ttx.Tx, tenantID, req.Msg.RecoveryId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status == domain.RecoveryResumed || current.Status == domain.RecoveryFailed || current.Status == domain.RecoveryCancelled {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("recovery is already terminal (status=%s)", current.Status))
	}
	now := time.Now().UTC()
	updated, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, tenantID, req.Msg.RecoveryId, current.Version, db.UpdateRecoveryExecutionFields{
		Status:  strPtr(domain.RecoveryCancelled),
		EndedAt: &now,
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	// Transition the task back to ready so it can be re-dispatched.
	task, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, updated.TaskID)
	if err == nil {
		_, _ = db.UpdateWorkItem(ctx, ttx.Tx, tenantID, updated.TaskID, task.Version, db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemReady),
		})
	}
	_ = enqueueRecoveryEvent(ctx, ttx.Tx, domain.RecoveryEventCancelled, updated, "", "", current.TriggerReason, "cancelled by operator: "+reason, "")
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.CancelRecoveryResponse{Recovery: recoveryRowToProto(updated)}), nil
}

// GetRecovery returns a single RecoveryExecution by id.
func (s *Service) GetRecovery(ctx context.Context, req *connect.Request[apiv1.GetRecoveryRequest]) (*connect.Response[apiv1.GetRecoveryResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	rec, err := db.GetRecoveryExecution(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetRecoveryResponse{Recovery: recoveryRowToProto(rec)}), nil
}

// ListRecoveries returns a page of RecoveryExecutions.
func (s *Service) ListRecoveries(ctx context.Context, req *connect.Request[apiv1.ListRecoveriesRequest]) (*connect.Response[apiv1.ListRecoveriesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	f := db.ListRecoveriesFilter{
		TenantID: tenantID, ProjectID: req.Msg.ProjectId, TaskID: req.Msg.TaskId,
		PageSize: int(req.Msg.PageSize), AfterID: req.Msg.PageToken,
	}
	if req.Msg.Status != nil {
		f.Status = recoveryStatusFromProto(*req.Msg.Status)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	recs, err := db.ListRecoveries(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListRecoveriesResponse{}
	for _, r := range recs {
		resp.Recoveries = append(resp.Recoveries, recoveryRowToProto(r))
	}
	if len(recs) > 0 {
		resp.NextPageToken = recs[len(recs)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// GetRecoveryStepRuns returns all step runs for a recovery (the timeline).
func (s *Service) GetRecoveryStepRuns(ctx context.Context, req *connect.Request[apiv1.GetRecoveryStepRunsRequest]) (*connect.Response[apiv1.GetRecoveryStepRunsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.RecoveryId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("recovery_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	steps, err := db.ListRecoveryStepRuns(ctx, ttx.Tx, tenantID, req.Msg.RecoveryId)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.GetRecoveryStepRunsResponse{}
	for _, sr := range steps {
		resp.StepRuns = append(resp.StepRuns, stepRunRowToProto(sr))
	}
	return connect.NewResponse(resp), nil
}

// StreamRecoveryEvents fans out recovery events from NATS
// (docs/07 §4, docs/06 §11).
func (s *Service) StreamRecoveryEvents(ctx context.Context, req *connect.Request[apiv1.StreamRecoveryEventsRequest], stream *connect.ServerStream[apiv1.StreamRecoveryEventsResponse]) error {
	if s.subscriber == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("event streaming is unavailable (NATS subscriber not connected)"))
	}
	filter := "orchicon.events.recovery.>"
	var fromSeq uint64
	if req.Msg.FromSequence != nil && *req.Msg.FromSequence > 0 {
		fromSeq = uint64(*req.Msg.FromSequence)
	}
	ch, err := s.subscriber.Subscribe(ctx, filter, fromSeq)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe to recovery events: %w", err))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			evt, err := parseRecoveryEvent(msg.Data)
			if err != nil {
				s.log.Warn("failed to parse recovery event", "subject", msg.Subject, "error", err)
				continue
			}
			if req.Msg.RecoveryId != "" && evt.RecoveryId != req.Msg.RecoveryId {
				continue
			}
			if req.Msg.ProjectId != "" && evt.ProjectId != "" && evt.ProjectId != req.Msg.ProjectId {
				continue
			}
			if err := stream.Send(&apiv1.StreamRecoveryEventsResponse{
				Event:    evt,
				Sequence: int64(msg.Seq),
			}); err != nil {
				return err
			}
		}
	}
}

// ApproveContinuationPlan approves a pending plan (L3 — docs/06 §7, §8).
func (s *Service) ApproveContinuationPlan(ctx context.Context, req *connect.Request[apiv1.ApproveContinuationPlanRequest]) (*connect.Response[apiv1.ApproveContinuationPlanResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.RecoveryId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("recovery_id must not be empty"))
	}
	actor, err := validateTextField(req.Msg.Actor, maxActorLen, "actor")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if actor == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("actor must not be empty"))
	}
	plan, rec, err := s.engine.ApproveContinuationPlan(ctx, tenantID, req.Msg.RecoveryId, actor)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.ApproveContinuationPlanResponse{
		Plan: planRowToProto(plan), Recovery: recoveryRowToProto(rec),
	}), nil
}

// RejectContinuationPlan rejects a pending plan (docs/06 §8).
func (s *Service) RejectContinuationPlan(ctx context.Context, req *connect.Request[apiv1.RejectContinuationPlanRequest]) (*connect.Response[apiv1.RejectContinuationPlanResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.RecoveryId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("recovery_id must not be empty"))
	}
	actor, err := validateTextField(req.Msg.Actor, maxActorLen, "actor")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if actor == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("actor must not be empty"))
	}
	reason, err := validateTextField(req.Msg.Reason, maxReasonLen, "reason")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	plan, rec, err := s.engine.RejectContinuationPlan(ctx, tenantID, req.Msg.RecoveryId, actor, reason)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.RejectContinuationPlanResponse{
		Plan: planRowToProto(plan), Recovery: recoveryRowToProto(rec),
	}), nil
}

// GetContinuationPlan returns the continuation plan for a recovery.
func (s *Service) GetContinuationPlan(ctx context.Context, req *connect.Request[apiv1.GetContinuationPlanRequest]) (*connect.Response[apiv1.GetContinuationPlanResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.RecoveryId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("recovery_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	plan, err := db.GetContinuationPlanByRecovery(ctx, ttx.Tx, tenantID, req.Msg.RecoveryId)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return connect.NewResponse(&apiv1.GetContinuationPlanResponse{}), nil
		}
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetContinuationPlanResponse{Plan: planRowToProto(plan)}), nil
}

// MarkTaskSucceeded marks a Task succeeded by the Reviewer Worker or a
// human (docs/06 §11, docs/02 §4 #2).
func (s *Service) MarkTaskSucceeded(ctx context.Context, req *connect.Request[apiv1.MarkTaskSucceededRequest]) (*connect.Response[apiv1.MarkTaskSucceededResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.TaskId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("task_id must not be empty"))
	}
	actorType, err := validateTextField(req.Msg.ActorType, maxActorLen, "actor_type")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if actorType == "" {
		actorType = "human"
	}
	actorID, err := validateTextField(req.Msg.ActorId, maxActorLen, "actor_id")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	reason, err := validateTextField(req.Msg.Reason, maxReasonLen, "reason")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	auditID, err := s.engine.MarkTaskSucceeded(ctx, tenantID, req.Msg.TaskId, actorType, actorID, reason)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.MarkTaskSucceededResponse{
		TaskId: req.Msg.TaskId, Status: domain.WorkItemSucceeded, AuditEventId: auditID,
	}), nil
}

// --- helpers + mappers -----------------------------------------------------

func validateTextField(s string, max int, field string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		return "", fmt.Errorf("%s must be at most %d characters", field, max)
	}
	return s, nil
}

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("recovery not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

// parseRecoveryEvent decodes the JSON event payload into a RecoveryEvent.
func parseRecoveryEvent(data []byte) (*apiv1.RecoveryEvent, error) {
	var env struct {
		EventType      string `json:"event_type"`
		TenantID       string `json:"tenant_id"`
		ProjectID      string `json:"project_id"`
		RecoveryID     string `json:"recovery_id"`
		TaskID         string `json:"task_id"`
		FailedExecID   string `json:"failed_execution_id"`
		StepID         string `json:"step_id"`
		StepRunID      string `json:"step_run_id"`
		RecoveryStatus string `json:"recovery_status"`
		StepStatus    string `json:"step_status"`
		TriggerReason  string `json:"trigger_reason"`
		Action         string `json:"action"`
		AdapterRef    string `json:"adapter_ref"`
		OccurredAt     string `json:"occurred_at"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse recovery event: %w", err)
	}
	evt := &apiv1.RecoveryEvent{
		EventType: env.EventType, TenantId: env.TenantID, ProjectId: env.ProjectID,
		RecoveryId: env.RecoveryID, TaskId: env.TaskID, FailedExecutionId: env.FailedExecID,
		StepId: env.StepID, StepRunId: env.StepRunID,
		RecoveryStatus: recoveryStatusToProto(env.RecoveryStatus),
		TriggerReason:  env.TriggerReason, Action: env.Action, AdapterRef: env.AdapterRef,
		Payload: data,
	}
	if t, err := time.Parse(time.RFC3339Nano, env.OccurredAt); err == nil {
		evt.OccurredAt = timestamppb.New(t)
	}
	return evt, nil
}

func recoveryStatusToProto(s string) apiv1.RecoveryStatus {
	switch s {
	case domain.RecoveryPending:
		return apiv1.RecoveryStatus_RECOVERY_STATUS_PENDING
	case domain.RecoveryRunning:
		return apiv1.RecoveryStatus_RECOVERY_STATUS_RUNNING
	case domain.RecoveryResumed:
		return apiv1.RecoveryStatus_RECOVERY_STATUS_RESUMED
	case domain.RecoveryEscalated:
		return apiv1.RecoveryStatus_RECOVERY_STATUS_ESCALATED
	case domain.RecoveryFailed:
		return apiv1.RecoveryStatus_RECOVERY_STATUS_FAILED
	case domain.RecoveryCancelled:
		return apiv1.RecoveryStatus_RECOVERY_STATUS_CANCELLED
	case domain.RecoveryBlocked:
		return apiv1.RecoveryStatus_RECOVERY_STATUS_BLOCKED
	}
	return apiv1.RecoveryStatus_RECOVERY_STATUS_UNSPECIFIED
}

func recoveryStatusFromProto(s apiv1.RecoveryStatus) string {
	switch s {
	case apiv1.RecoveryStatus_RECOVERY_STATUS_PENDING:
		return domain.RecoveryPending
	case apiv1.RecoveryStatus_RECOVERY_STATUS_RUNNING:
		return domain.RecoveryRunning
	case apiv1.RecoveryStatus_RECOVERY_STATUS_RESUMED:
		return domain.RecoveryResumed
	case apiv1.RecoveryStatus_RECOVERY_STATUS_ESCALATED:
		return domain.RecoveryEscalated
	case apiv1.RecoveryStatus_RECOVERY_STATUS_FAILED:
		return domain.RecoveryFailed
	case apiv1.RecoveryStatus_RECOVERY_STATUS_CANCELLED:
		return domain.RecoveryCancelled
	case apiv1.RecoveryStatus_RECOVERY_STATUS_BLOCKED:
		return domain.RecoveryBlocked
	}
	return ""
}

func recoveryStepStatusToProto(s string) apiv1.RecoveryStepStatus {
	switch s {
	case domain.RecoveryStepPending:
		return apiv1.RecoveryStepStatus_RECOVERY_STEP_STATUS_PENDING
	case domain.RecoveryStepReady:
		return apiv1.RecoveryStepStatus_RECOVERY_STEP_STATUS_READY
	case domain.RecoveryStepRunning:
		return apiv1.RecoveryStepStatus_RECOVERY_STEP_STATUS_RUNNING
	case domain.RecoveryStepSucceeded:
		return apiv1.RecoveryStepStatus_RECOVERY_STEP_STATUS_SUCCEEDED
	case domain.RecoveryStepFailed:
		return apiv1.RecoveryStepStatus_RECOVERY_STEP_STATUS_FAILED
	case domain.RecoveryStepSkipped:
		return apiv1.RecoveryStepStatus_RECOVERY_STEP_STATUS_SKIPPED
	case domain.RecoveryStepBlocked:
		return apiv1.RecoveryStepStatus_RECOVERY_STEP_STATUS_BLOCKED
	}
	return apiv1.RecoveryStepStatus_RECOVERY_STEP_STATUS_UNSPECIFIED
}

func recoveryLevelToProto(l int32) apiv1.RecoveryLevel {
	switch l {
	case domain.RecoveryLevelL1:
		return apiv1.RecoveryLevel_RECOVERY_LEVEL_L1
	case domain.RecoveryLevelL2:
		return apiv1.RecoveryLevel_RECOVERY_LEVEL_L2
	case domain.RecoveryLevelL3:
		return apiv1.RecoveryLevel_RECOVERY_LEVEL_L3
	}
	return apiv1.RecoveryLevel_RECOVERY_LEVEL_UNSPECIFIED
}

func planStatusToProto(s string) apiv1.PlanStatus {
	switch s {
	case domain.PlanPending:
		return apiv1.PlanStatus_PLAN_STATUS_PENDING
	case domain.PlanApproved:
		return apiv1.PlanStatus_PLAN_STATUS_APPROVED
	case domain.PlanRejected:
		return apiv1.PlanStatus_PLAN_STATUS_REJECTED
	}
	return apiv1.PlanStatus_PLAN_STATUS_UNSPECIFIED
}

func recoveryRowToProto(r db.RecoveryExecutionRow) *apiv1.RecoveryExecution {
	p := &apiv1.RecoveryExecution{
		Id: r.ID, TenantId: r.TenantID, ProjectId: r.ProjectID, TaskId: r.TaskID,
		FailedExecutionId: r.FailedExecutionID, RecoveryWorkflowId: r.RecoveryWorkflowID,
		TriggerReason: r.TriggerReason, Level: recoveryLevelToProto(r.Level),
		Status: recoveryStatusToProto(r.Status), CurrentStep: r.CurrentStep,
		ResumptionPath: r.ResumptionPath,
		BudgetTokensLimit: r.BudgetTokensLimit, BudgetTokensUsed: r.BudgetTokensUsed,
		BudgetCostLimitUsd: r.BudgetCostLimitUSD, BudgetCostUsedUsd: r.BudgetCostUsedUSD,
		BudgetRelaxFraction: r.BudgetRelaxFraction, NeedsHumanApproval: r.NeedsHumanApproval,
		ContinuationPlanId: r.ContinuationPlanID, ReviewerWorkerId: r.ReviewerWorkerID,
		Summary: r.Summary, Version: int32(r.Version),
		TriggeredAt: timestamppb.New(r.TriggeredAt),
		CreatedAt:   timestamppb.New(r.CreatedAt), UpdatedAt: timestamppb.New(r.UpdatedAt),
	}
	if r.EndedAt != nil {
		p.EndedAt = timestamppb.New(*r.EndedAt)
	}
	return p
}

func stepRunRowToProto(s db.RecoveryStepRunRow) *apiv1.RecoveryStepRun {
	p := &apiv1.RecoveryStepRun{
		Id: s.ID, TenantId: s.TenantID, RecoveryId: s.RecoveryID, StepId: s.StepID,
		StepName: s.StepName, Status: recoveryStepStatusToProto(s.Status),
		Attempt: int32(s.Attempt), Result: string(s.Result),
		WorkerExecutionId: s.WorkerExecutionID, TriggerReason: s.TriggerReason,
		AffectedRef: s.AffectedRef, AdapterRef: s.AdapterRef, Action: s.Action,
		Version: int32(s.Version), CreatedAt: timestamppb.New(s.CreatedAt),
		UpdatedAt: timestamppb.New(s.UpdatedAt),
	}
	if s.StartedAt != nil {
		p.StartedAt = timestamppb.New(*s.StartedAt)
	}
	if s.EndedAt != nil {
		p.EndedAt = timestamppb.New(*s.EndedAt)
	}
	return p
}

func planRowToProto(p db.ContinuationPlanRow) *apiv1.ContinuationPlan {
	pp := &apiv1.ContinuationPlan{
		Id: p.ID, TenantId: p.TenantID, RecoveryId: p.RecoveryID, Version: int32(p.Version),
		Completed: string(p.Completed), InProgress: string(p.InProgress),
		Remaining: string(p.Remaining), Corrections: string(p.Corrections),
		ContextSummary: p.ContextSummary, CheckpointRef: p.CheckpointRef,
		Assumptions: string(p.Assumptions), Status: planStatusToProto(p.Status),
		ApprovedBy: p.ApprovedBy, CreatedAt: timestamppb.New(p.CreatedAt),
	}
	if p.DecidedAt != nil {
		pp.DecidedAt = timestamppb.New(*p.DecidedAt)
	}
	return pp
}
