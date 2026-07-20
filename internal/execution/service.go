// Package execution implements the ExecutionService Connect handler
// (docs/07_API_Specification.md §3.8). It manages WorkerExecutions:
// live streaming telemetry, manual control (pause/resume/cancel/
// checkpoint), and Tier 2 per-tool-call approval (docs/05 §7.1).
//
// The scheduler (TaskReconciler) is the only component permitted to
// create WorkerExecutions (docs/03 §8 invariant #1); this service
// reads and controls existing executions.
package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Input size bounds (AGENTS.md security standards).
const (
	maxReasonLen    = 1000
	maxToolCatLen   = 63
	maxActorLen     = 200
	approvalTimeout = 5 * time.Minute
)

// TaskDispatcher dispatches a ready work item synchronously. Injected
// from the scheduler so CreateFollowUpExecution can dispatch immediately
// instead of waiting for the next reconciler scan pass.
type TaskDispatcher interface {
	DispatchTask(ctx context.Context, taskID string) error
}

// Service implements the ExecutionService Connect handler.
type Service struct {
	pool       *db.Pool
	log        *slog.Logger
	subscriber eventbus.Subscriber
	dispatcher TaskDispatcher // optional, for follow-up dispatch
	apiv1connect.UnimplementedExecutionServiceHandler

	// In-memory approval registry: pending Tier 2 per-tool-call approval
	// requests (docs/05 §7.1). Keyed by request_id. When the adapter
	// emits an ApprovalRequest, the TaskReconciler registers it here;
	// when a human resolves it via ApproveToolCall, the result is
	// signaled back to the adapter's Execute stream.
	mu        sync.Mutex
	approvals map[string]*pendingApproval
}

// SetDispatcher injects the task dispatcher for follow-up execution
// dispatch. Called by the server after constructing both the execution
// service and the TaskReconciler.
func (s *Service) SetDispatcher(d TaskDispatcher) { s.dispatcher = d }

// pendingApproval tracks a Tier 2 approval request awaiting a human
// decision (docs/05 §7.1, docs/07 §3.8).
type pendingApproval struct {
	request   *apiv1.ApprovalRequest
	createdAt time.Time
	// resolvedCh is closed when the approval is resolved; the value
	// sent is the human's decision (approved + reason).
	resolvedCh chan approvalDecision
}

type approvalDecision struct {
	approved bool
	reason   string
}

var _ apiv1connect.ExecutionServiceHandler = (*Service)(nil)

// New constructs an ExecutionService handler.
func New(pool *db.Pool, log *slog.Logger, sub eventbus.Subscriber) *Service {
	return &Service{
		pool:       pool,
		log:        log,
		subscriber: sub,
		approvals:  make(map[string]*pendingApproval),
	}
}

// GetExecution returns a single execution by id.
func (s *Service) GetExecution(ctx context.Context, req *connect.Request[apiv1.GetExecutionRequest]) (*connect.Response[apiv1.GetExecutionResponse], error) {
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
	e, err := db.GetExecution(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetExecutionResponse{Execution: rowToProto(e)}), nil
}

// ListExecutions returns a page of executions, optionally filtered by
// project/task/status.
func (s *Service) ListExecutions(ctx context.Context, req *connect.Request[apiv1.ListExecutionsRequest]) (*connect.Response[apiv1.ListExecutionsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	f := db.ListExecutionsFilter{
		TenantID:  tenantID,
		PageSize:  int(req.Msg.PageSize),
		AfterID:   req.Msg.PageToken,
	}
	if req.Msg.ProjectId != nil {
		f.ProjectID = *req.Msg.ProjectId
	}
	if req.Msg.TaskId != nil {
		f.TaskID = *req.Msg.TaskId
	}
	if req.Msg.Status != nil {
		f.Status = execStatusFromProto(*req.Msg.Status)
	}
	if req.Msg.WorkflowRunId != nil {
		f.WorkflowRunID = *req.Msg.WorkflowRunId
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	execs, err := db.ListExecutions(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListExecutionsResponse{}
	for _, e := range execs {
		resp.Executions = append(resp.Executions, rowToProto(e))
	}
	if len(execs) > 0 {
		resp.NextPageToken = execs[len(execs)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// DeleteExecution hard-deletes an execution. If running, it is cancelled first.
func (s *Service) DeleteExecution(ctx context.Context, req *connect.Request[apiv1.DeleteExecutionRequest]) (*connect.Response[apiv1.DeleteExecutionResponse], error) {
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
	current, err := db.GetExecution(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	// If still running (not yet terminated), cancel it first.
	if current.Status != domain.ExecutionTerminated && current.Status != domain.ExecutionFailedToStart {
		now := time.Now().UTC()
		_, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, db.UpdateExecutionFields{
			Status:      strPtr(domain.ExecutionTerminated),
			HealthState: strPtr(domain.HealthTerminating),
			EndedAt:     &now,
		})
		if err != nil {
			return nil, mapDBError(err)
		}
	}
	if err := db.DeleteExecution(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("execution deleted", "id", req.Msg.Id)
	return connect.NewResponse(&apiv1.DeleteExecutionResponse{}), nil
}

// StreamExecutionEvents is the server-stream RPC that fans out execution
// events from NATS to connected clients (docs/07 §4, docs/10 §4). It
// subscribes to the orchicon.events.execution.* subject filter and
// streams each event as a StreamExecutionEventsResponse.
func (s *Service) StreamExecutionEvents(ctx context.Context, req *connect.Request[apiv1.StreamExecutionEventsRequest], stream *connect.ServerStream[apiv1.StreamExecutionEventsResponse]) error {
	if s.subscriber == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("event streaming is unavailable (NATS subscriber not connected)"))
	}
	filter := "orchicon.events.execution.>"
	var fromSeq uint64
	if req.Msg.FromSequence != nil && *req.Msg.FromSequence > 0 {
		fromSeq = uint64(*req.Msg.FromSequence)
	}
	ch, err := s.subscriber.Subscribe(ctx, filter, fromSeq)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe to execution events: %w", err))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			evt, err := parseExecutionEvent(msg.Data)
			if err != nil {
				s.log.Warn("failed to parse execution event", "subject", msg.Subject, "error", err)
				continue
			}
			// Filter by execution_id if specified.
			if req.Msg.ExecutionId != "" && evt.ExecutionId != req.Msg.ExecutionId {
				continue
			}
			resp := &apiv1.StreamExecutionEventsResponse{
				Event:    evt,
				Sequence: int64(msg.Seq),
			}
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

// PauseExecution transitions a running execution to checkpointing
// (docs/03 §6). Best-effort in v0.1 CLI mode (docs/04 §6.1).
func (s *Service) PauseExecution(ctx context.Context, req *connect.Request[apiv1.PauseExecutionRequest]) (*connect.Response[apiv1.PauseExecutionResponse], error) {
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
	current, err := db.GetExecution(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status != domain.ExecutionRunning && current.Status != domain.ExecutionHealthy {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("execution must be running to pause (status=%s)", current.Status))
	}
	updated, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, db.UpdateExecutionFields{
		Status:      strPtr(domain.ExecutionTerminating),
		HealthState: strPtr(domain.HealthTerminating),
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueExecEvent(ctx, ttx.Tx, "execution.control", updated, map[string]any{"action": "pause"}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("execution paused", "id", updated.ID)
	return connect.NewResponse(&apiv1.PauseExecutionResponse{Execution: rowToProto(updated)}), nil
}

// ResumeExecution transitions a checkpointed execution back to running
// (docs/03 §6).
func (s *Service) ResumeExecution(ctx context.Context, req *connect.Request[apiv1.ResumeExecutionRequest]) (*connect.Response[apiv1.ResumeExecutionResponse], error) {
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
	current, err := db.GetExecution(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status != domain.ExecutionTerminating {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("execution must be paused to resume (status=%s)", current.Status))
	}
	updated, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, db.UpdateExecutionFields{
		Status:      strPtr(domain.ExecutionRunning),
		HealthState: strPtr(domain.HealthHealthy),
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueExecEvent(ctx, ttx.Tx, "execution.control", updated, map[string]any{"action": "resume"}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("execution resumed", "id", updated.ID)
	return connect.NewResponse(&apiv1.ResumeExecutionResponse{Execution: rowToProto(updated)}), nil
}

// CancelExecution transitions an execution to terminated (docs/03 §6).
func (s *Service) CancelExecution(ctx context.Context, req *connect.Request[apiv1.CancelExecutionRequest]) (*connect.Response[apiv1.CancelExecutionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
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
	current, err := db.GetExecution(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status == domain.ExecutionTerminated {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("execution is already terminated"))
	}
	now := time.Now().UTC()
	updated, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, db.UpdateExecutionFields{
		Status:      strPtr(domain.ExecutionTerminated),
		HealthState: strPtr(domain.HealthTerminating),
		EndedAt:     &now,
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueExecEvent(ctx, ttx.Tx, "execution.control", updated, map[string]any{"action": "cancel", "reason": reason}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("execution cancelled", "id", updated.ID, "reason", reason)
	return connect.NewResponse(&apiv1.CancelExecutionResponse{Execution: rowToProto(updated)}), nil
}

// CheckpointNow requests an immediate checkpoint from the adapter
// (docs/04 §5). In v0.1 CLI mode, this is a coarse transcript summary
// + working-tree ref (docs/04 §6.1).
func (s *Service) CheckpointNow(ctx context.Context, req *connect.Request[apiv1.CheckpointNowRequest]) (*connect.Response[apiv1.CheckpointNowResponse], error) {
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
	current, err := db.GetExecution(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	// In v0.1, the checkpoint is a coarse reference. The actual blob
	// is produced by the adapter and stored via the blob store. Here
	// we record the checkpoint event; the adapter writes the blob.
	checkpointRef := fmt.Sprintf("checkpoint-%s-%d", current.ID, time.Now().Unix())
	updated, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, db.UpdateExecutionFields{
		CheckpointRef: &checkpointRef,
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueExecEvent(ctx, ttx.Tx, "execution.checkpoint", updated, map[string]any{"checkpoint_ref": checkpointRef}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("execution checkpoint requested", "id", updated.ID, "checkpoint_ref", checkpointRef)
	return connect.NewResponse(&apiv1.CheckpointNowResponse{
		Execution:    rowToProto(updated),
		CheckpointRef: checkpointRef,
	}), nil
}

// ApproveToolCall resolves a Tier 2 per-tool-call approval request
// (docs/05 §7.1, docs/07 §3.8). The decision is signaled to the
// adapter's Execute stream via the pending approval registry.
func (s *Service) ApproveToolCall(ctx context.Context, req *connect.Request[apiv1.ApproveToolCallRequest]) (*connect.Response[apiv1.ApproveToolCallResponse], error) {
	if req.Msg.RequestId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request_id must not be empty"))
	}
	reason, err := validateTextField(req.Msg.Reason, maxReasonLen, "reason")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	s.mu.Lock()
	pending, ok := s.approvals[req.Msg.RequestId]
	s.mu.Unlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("approval request not found (it may have expired or already been resolved)"))
	}
	// Signal the decision to the adapter's Execute stream.
	select {
	case pending.resolvedCh <- approvalDecision{approved: req.Msg.Approved, reason: reason}:
	default:
		// Channel already has a decision (double-resolve); treat as
		// already resolved.
	}
	pending.request.Resolved = true
	pending.request.Approved = req.Msg.Approved
	pending.request.Reason = reason
	s.mu.Lock()
	delete(s.approvals, req.Msg.RequestId)
	s.mu.Unlock()
	s.log.Info("tool call approved", "request_id", req.Msg.RequestId, "approved", req.Msg.Approved)
	return connect.NewResponse(&apiv1.ApproveToolCallResponse{Approval: pending.request}), nil
}

// ListPendingApprovals returns unresolved Tier 2 approval requests for
// the tenant (optionally scoped to an execution).
func (s *Service) ListPendingApprovals(ctx context.Context, req *connect.Request[apiv1.ListPendingApprovalsRequest]) (*connect.Response[apiv1.ListPendingApprovalsResponse], error) {
	resp := &apiv1.ListPendingApprovalsResponse{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.approvals {
		if req.Msg.ExecutionId != nil && p.request.ExecutionId != *req.Msg.ExecutionId {
			continue
		}
		// Expire stale approvals.
		if time.Since(p.createdAt) > approvalTimeout {
			continue
		}
		resp.Approvals = append(resp.Approvals, p.request)
	}
	return connect.NewResponse(resp), nil
}

// CreateFollowUpExecution creates a new work item that continues from a
// completed execution. The new work item includes the previous context
// (composite prompt + output) plus the user's follow-up message, and is
// marked as ready for the TaskReconciler to dispatch.
func (s *Service) CreateFollowUpExecution(ctx context.Context, req *connect.Request[apiv1.CreateFollowUpExecutionRequest]) (*connect.Response[apiv1.CreateFollowUpExecutionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	msg := req.Msg
	if msg.ExecutionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("execution_id must not be empty"))
	}
	q := strings.TrimSpace(msg.Message)
	if q == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("message must not be empty"))
	}
	if utf8.RuneCountInString(q) > 10000 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("message too long (max 10000 characters)"))
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// 1. Get the previous execution.
	prevExec, err := db.GetExecution(ctx, ttx.Tx, tenantID, msg.ExecutionId)
	if err != nil {
		return nil, mapDBError(err)
	}

	// 2. Get the task (work item) for that execution.
	task, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, prevExec.TaskID)
	if err != nil {
		return nil, mapDBError(err)
	}

	// 3. Build the follow-up composite prompt.
	var prevComposite string
	if len(task.PromptContext) > 0 {
		var pc struct {
			Composite string `json:"composite"`
		}
		if err := json.Unmarshal(task.PromptContext, &pc); err == nil {
			prevComposite = strings.TrimSpace(pc.Composite)
		}
	}
	var followUpPrompt strings.Builder
	followUpPrompt.WriteString("# Follow-up task\n\n")
	followUpPrompt.WriteString("This is a follow-up to a previous execution. Continue from where the previous work left off.\n\n")
	if prevComposite != "" {
		followUpPrompt.WriteString("## Previous context\n\n")
		followUpPrompt.WriteString(prevComposite)
		followUpPrompt.WriteString("\n\n")
	}
	output := strings.TrimSpace(prevExec.Output)
	if output != "" {
		followUpPrompt.WriteString("## Previous execution output\n\n")
		if len(output) > 32000 {
			output = output[:32000] + "\n\n*(truncated — previous output was longer)*"
		}
		followUpPrompt.WriteString(output)
		followUpPrompt.WriteString("\n\n")
	}
	followUpPrompt.WriteString("## Follow-up question\n\n")
	followUpPrompt.WriteString(q)
	followUpPrompt.WriteString("\n\n")
	followUpPrompt.WriteString("# Instructions\n\n")
	followUpPrompt.WriteString("Complete the follow-up task above. Continue from where the previous execution left off. When you have finished, end your response with the literal line `ORCHICON WORKER SUMMARY:` followed by one short paragraph summarizing what you did.\n\n")

	promptCtx, err := json.Marshal(map[string]string{"composite": followUpPrompt.String()})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("marshal prompt context: %w", err))
	}

	// 4. Create a new work item as a child of the original task.
	newWIID := db.NewID()
	newWI := db.WorkItemRow{
		ID:        newWIID,
		TenantID:  tenantID,
		ProjectID: task.ProjectID,
		ParentID:  &task.ID,
		Kind:      domain.WorkItemKindTask,
		Title:     fmt.Sprintf("Follow-up: %s", strings.TrimSpace(task.Title)),
		Status:    domain.WorkItemReady,
		AssignedWorkerRef: task.AssignedWorkerRef,
		Priority:  task.Priority,
		PromptContext: promptCtx,
	}
	created, err := db.CreateWorkItem(ctx, ttx.Tx, newWI)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create follow-up work item: %w", err))
	}

	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}

	// 5. If we have a dispatcher, dispatch immediately so the execution
	// appears right away instead of waiting for the next reconciler scan.
	if s.dispatcher != nil {
		if err := s.dispatcher.DispatchTask(ctx, created.ID); err != nil {
			s.log.Warn("follow-up dispatch failed (will be picked up by scan)", "work_item", created.ID, "error", err)
		}
	}

	s.log.Info("follow-up execution created",
		"original_execution", msg.ExecutionId,
		"new_work_item", created.ID,
	)

	return connect.NewResponse(&apiv1.CreateFollowUpExecutionResponse{
		WorkItemId: created.ID,
	}), nil
}

// RegisterApproval creates a pending Tier 2 approval request in the
// in-memory registry. Called by the TaskReconciler/adapter bridge when
// the adapter emits an ApprovalRequest on its Execute stream. Returns a
// channel that receives the human's decision.
func (s *Service) RegisterApproval(req *apiv1.ApprovalRequest) <-chan approvalDecision {
	pending := &pendingApproval{
		request:    req,
		createdAt:  time.Now(),
		resolvedCh: make(chan approvalDecision, 1),
	}
	s.mu.Lock()
	s.approvals[req.RequestId] = pending
	s.mu.Unlock()
	return pending.resolvedCh
}

// --- helpers ---------------------------------------------------------------

func enqueueExecEvent(ctx context.Context, tx pgx.Tx, eventType string, e db.ExecutionRow, extra map[string]any) error {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":        e.TenantID,
		"execution_id":     e.ID,
		"task_id":          e.TaskID,
		"project_id":       e.ProjectID,
		"worker_id":        e.WorkerID,
		"worker_version":   e.WorkerVersion,
		"status":           e.Status,
		"health_state":     e.HealthState,
		"aggregate_type":   "execution",
		"aggregate_id":     e.ID,
		"aggregate_version": e.Version,
		"occurred_at":      time.Now().UTC().Format(time.RFC3339Nano),
	}
	for k, v := range extra {
		evt[k] = v
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal execution event payload: %w", err)
	}
	row := db.OutboxRow{
		TenantID:      e.TenantID,
		EventType:     eventType,
		AggregateType: "execution",
		AggregateID:   e.ID,
		AggregateVer:  e.Version,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

// parseExecutionEvent decodes the JSON event payload from the outbox/NATS
// into an ExecutionEvent proto message.
func parseExecutionEvent(data []byte) (*apiv1.ExecutionEvent, error) {
	var env struct {
		EventType    string `json:"event_type"`
		TenantID     string `json:"tenant_id"`
		ExecutionID  string `json:"execution_id"`
		TaskID       string `json:"task_id"`
		Status       string `json:"status"`
		HealthState  string `json:"health_state"`
		OccurredAt   string `json:"occurred_at"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse execution event: %w", err)
	}
	evt := &apiv1.ExecutionEvent{
		EventId:     "",
		EventType:   eventTypeToProto(env.EventType),
		TenantId:    env.TenantID,
		ExecutionId: env.ExecutionID,
		Payload:     data,
	}
	if t, err := time.Parse(time.RFC3339Nano, env.OccurredAt); err == nil {
		evt.OccurredAt = timestamppb.New(t)
	}
	return evt, nil
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
		return connect.NewError(connect.CodeNotFound, errors.New("execution not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

func validateTextField(s string, max int, field string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		return "", fmt.Errorf("%s must be at most %d characters", field, max)
	}
	return s, nil
}

func execStatusToProto(status string) apiv1.ExecutionStatus {
	switch status {
	case domain.ExecutionDispatching:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_DISPATCHING
	case domain.ExecutionRunning:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	case domain.ExecutionHealthy:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_HEALTHY
	case domain.ExecutionStalled:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_STALLED
	case domain.ExecutionUnhealthy:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_UNHEALTHY
	case domain.ExecutionTerminating:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_TERMINATING
	case domain.ExecutionTerminated:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_TERMINATED
	case domain.ExecutionFailedToStart:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_FAILED_TO_START
	case domain.ExecutionSucceeded:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED
	case domain.ExecutionFailed:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_FAILED
	default:
		return apiv1.ExecutionStatus_EXECUTION_STATUS_UNSPECIFIED
	}
}

func execStatusFromProto(status apiv1.ExecutionStatus) string {
	switch status {
	case apiv1.ExecutionStatus_EXECUTION_STATUS_DISPATCHING:
		return domain.ExecutionDispatching
	case apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING:
		return domain.ExecutionRunning
	case apiv1.ExecutionStatus_EXECUTION_STATUS_HEALTHY:
		return domain.ExecutionHealthy
	case apiv1.ExecutionStatus_EXECUTION_STATUS_STALLED:
		return domain.ExecutionStalled
	case apiv1.ExecutionStatus_EXECUTION_STATUS_UNHEALTHY:
		return domain.ExecutionUnhealthy
	case apiv1.ExecutionStatus_EXECUTION_STATUS_TERMINATING:
		return domain.ExecutionTerminating
	case apiv1.ExecutionStatus_EXECUTION_STATUS_TERMINATED:
		return domain.ExecutionTerminated
	case apiv1.ExecutionStatus_EXECUTION_STATUS_FAILED_TO_START:
		return domain.ExecutionFailedToStart
	case apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED:
		return domain.ExecutionSucceeded
	case apiv1.ExecutionStatus_EXECUTION_STATUS_FAILED:
		return domain.ExecutionFailed
	default:
		return ""
	}
}

func healthStateToProto(state string) apiv1.HealthState {
	switch state {
	case domain.HealthHealthy:
		return apiv1.HealthState_HEALTH_STATE_HEALTHY
	case domain.HealthStalled:
		return apiv1.HealthState_HEALTH_STATE_STALLED
	case domain.HealthUnhealthy:
		return apiv1.HealthState_HEALTH_STATE_UNHEALTHY
	case domain.HealthTerminating:
		return apiv1.HealthState_HEALTH_STATE_TERMINATING
	default:
		return apiv1.HealthState_HEALTH_STATE_UNSPECIFIED
	}
}

func eventTypeToProto(eventType string) apiv1.ExecutionEventType {
	// Map the `execution.<status>` lifecycle events from updateExecStatus.
	switch eventType {
	case "execution.ready", "execution.dispatching", "execution.created":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_STARTED
	case "execution.running":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY
	case "execution.terminated":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_RESULT
	case "execution.unhealthy", "execution.failed_to_start":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_ERROR
	case "execution.healthy":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_HEALTH
	case "execution.stalled":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_HEALTH
	case "execution.tool_call":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TOOL_CALL
	case "execution.text":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY
	case "execution.checkpoint":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_CHECKPOINT
	case "execution.approval_request":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_APPROVAL_REQUEST
	case "execution.control":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_CONTROL
	case "execution.artifact":
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_ARTIFACT
	default:
		return apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_UNSPECIFIED
	}
}

func rowToProto(e db.ExecutionRow) *apiv1.WorkerExecution {
	p := &apiv1.WorkerExecution{
		Id:            e.ID,
		TenantId:      e.TenantID,
		ProjectId:     e.ProjectID,
		TaskId:        e.TaskID,
		WorkerId:      e.WorkerID,
		WorkerVersion: int32(e.WorkerVersion),
		Status:        execStatusToProto(e.Status),
		HealthState:   healthStateToProto(e.HealthState),
		TokenUsage:    e.TokenUsage,
		CostUsd:       e.CostUSD,
		Version:       int32(e.Version),
		WorkflowRunId: e.WorkflowRunID,
		WorkflowStepId: e.WorkflowStepID,
		WorkflowName:  e.WorkflowName,
	}
	if e.AdapterID != nil {
		p.AdapterId = *e.AdapterID
	}
	if e.StartedAt != nil {
		p.StartedAt = timestamppb.New(*e.StartedAt)
	}
	if e.EndedAt != nil {
		p.EndedAt = timestamppb.New(*e.EndedAt)
	}
	if e.CheckpointRef != nil {
		p.CheckpointRef = *e.CheckpointRef
	}
	if e.RecoveryID != nil {
		p.RecoveryId = *e.RecoveryID
	}
	if e.ErrorMessage != "" {
		p.ErrorMessage = e.ErrorMessage
	}
	if e.Output != "" {
		p.Output = e.Output
	}
	return p
}

func strPtr(s string) *string { return &s }
