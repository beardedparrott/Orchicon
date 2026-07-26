// Package approval implements the ApprovalService Connect handler for
// human-in-the-loop approval gates in workflow steps.
//
// An APPROVAL step in a workflow blocks at approval_pending until a
// human reviews and approves/rejects it. The decision is written to
// .orchicon/ files so downstream workers and loop-back steps can read
// the outcome.
//
// Future: notifications (SMS, email, browser), RBAC enforcement
// (which users/groups can approve at project/work-item scope).
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxReasonLen = 5000
)

// Service implements the ApprovalService Connect handler
// (apiv1connect.ApprovalServiceHandler).
type Service struct {
	pool *db.Pool
	log  *slog.Logger
	apiv1connect.UnimplementedApprovalServiceHandler
}

var _ apiv1connect.ApprovalServiceHandler = (*Service)(nil)

// NewService constructs an ApprovalService handler.
func NewService(pool *db.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

// ApproveStep resolves a workflow step that is awaiting human approval.
// On approval the step transitions to succeeded; on rejection it also
// succeeds but carries the rejection reason as data so a downstream
// LOOP_DECISION can branch on the decision.
func (s *Service) ApproveStep(ctx context.Context, req *connect.Request[apiv1.ApproveStepRequest]) (*connect.Response[apiv1.ApproveStepResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	msg := req.Msg
	if msg.StepRunId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("step_run_id must not be empty"))
	}
	reason := msg.Reason
	if len(reason) > maxReasonLen {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("reason too long (max %d characters)", maxReasonLen))
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("begin tx: %w", err))
	}
	defer ttx.Rollback(ctx)

	// Fetch the step run.
	sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, tenantID, msg.StepRunId)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("step run not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get step run: %w", err))
	}
	if sr.Status != domain.StepRunApprovalPending {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("step run is not pending approval (status=%s)", sr.Status))
	}

	// Determine the new status: approval always succeeds the step,
	// carrying the decision as data.
	decisionStatus := "approved"
	if !msg.Approved {
		decisionStatus = "rejected"
	}

	resultPayload, _ := json.Marshal(map[string]any{
		"_decision":    decisionStatus,
		"_approved":    msg.Approved,
		"_reason":      msg.Reason,
		"_reviewed_by": msg.ReviewedBy,
		"_reviewed_at": time.Now().UTC().Format(time.RFC3339Nano),
	})

	now := time.Now().UTC()
	updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status:    strPtr(domain.StepRunSucceeded),
		Result:    &resultPayload,
		EndedAt:   &now,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update step run: %w", err))
	}

	// Enqueue a step_succeeded event so the reconciler progresses the DAG.
	evtType := domain.WorkflowEventStepSucceeded
	evt := map[string]any{
		"event_type":      evtType,
		"tenant_id":       sr.TenantID,
		"workflow_run_id": sr.WorkflowRunID,
		"step_id":         sr.StepID,
		"step_run_id":     sr.ID,
		"step_status":     updated.Status,
		"decision":        decisionStatus,
		"occurred_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, _ := json.Marshal(evt)
	if err := db.EnqueueOutbox(ctx, ttx.Tx, db.OutboxRow{
		TenantID:      sr.TenantID,
		EventType:     evtType,
		AggregateType: "workflow",
		AggregateID:   sr.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("enqueue step event: %w", err))
	}

	// Write .orchicon/ files so downstream workers read the decision.
	if err := s.writeApprovalOrchiconFiles(ctx, ttx.Tx, tenantID, sr, msg.Approved, msg.Reason); err != nil {
		s.log.Warn("write approval .orchicon files", "step_run", sr.ID, "error", err)
	}

	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}

	s.log.Info("approval step resolved",
		"step_run", sr.ID, "approved", msg.Approved,
		"reviewed_by", msg.ReviewedBy)

	return connect.NewResponse(&apiv1.ApproveStepResponse{}), nil
}

// writeApprovalOrchiconFiles writes the approval decision to .orchicon/
// files in the project directory so downstream workers can read them.
func (s *Service) writeApprovalOrchiconFiles(ctx context.Context, tx pgx.Tx, tenantID string, sr db.WorkflowStepRunRow, approved bool, reason string) error {
	// Find the workflow run to get the project ID.
	run, err := db.GetWorkflowRun(ctx, tx, tenantID, sr.WorkflowRunID)
	if err != nil {
		return fmt.Errorf("get workflow run: %w", err)
	}
	if run.ProjectID == "" {
		return nil
	}

	// Get the project directory.
	proj, err := db.GetProject(ctx, tx, tenantID, run.ProjectID)
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	if proj.ProjectDir == "" {
		return nil
	}

	orchDir := filepath.Join(proj.ProjectDir, ".orchicon", sr.WorkflowRunID)
	if err := os.MkdirAll(orchDir, 0755); err != nil {
		return fmt.Errorf("mkdir .orchicon: %w", err)
	}

	writeFile := func(name, content string) {
		if err2 := os.WriteFile(filepath.Join(orchDir, name), []byte(content), 0644); err2 != nil {
			s.log.Warn("write .orchicon file", "file", name, "error", err2)
		}
	}

	writeFile("worker", "human_approver")
	writeFile("status", map[bool]string{true: "success", false: "failure"}[approved])
	writeFile("summary", reason)

	return nil
}

// ListPendingStepApprovals returns unresolved approval_pending step runs.
func (s *Service) ListPendingStepApprovals(ctx context.Context, req *connect.Request[apiv1.ListPendingStepApprovalsRequest]) (*connect.Response[apiv1.ListPendingStepApprovalsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	msg := req.Msg

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("begin tx: %w", err))
	}
	defer ttx.Rollback(ctx)

	items, err := listApprovalItems(ctx, ttx.Tx, tenantID, msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list approvals: %w", err))
	}

	resp := &apiv1.ListPendingStepApprovalsResponse{
		Items: items,
	}
	return connect.NewResponse(resp), nil
}

// approvalItemRow is a joined row from the listApprovalItems query.
type approvalItemRow struct {
	StepRunID        string
	WorkflowRunID    string
	ProjectName      string
	WorkItemTitle    string
	WorkflowName     string
	UpstreamWorker   string
	UpstreamSummary  string
	TouchedFilesJSON string
	AcceptanceCrit   string
	Status           string
	CreatedAt        time.Time
}

// listApprovalItems queries step runs in approval_pending status and joins
// with workflow_runs, workflows, projects, and work_items to resolve
// display names.
func listApprovalItems(ctx context.Context, tx pgx.Tx, tenantID string, req *apiv1.ListPendingStepApprovalsRequest) ([]*apiv1.ApprovalItem, error) {
	// Build the query dynamically based on filters.
	q := `SELECT
		wsr.id,
		wr.id,
		COALESCE(p.name, ''),
		COALESCE(wi.title, ''),
		COALESCE(w.name, ''),
		COALESCE(wsr.result::jsonb->>'_upstream_worker', ''),
		COALESCE(wsr.result::jsonb->>'_upstream_summary', ''),
		COALESCE(wsr.result::jsonb->>'_upstream_files', '[]'),
		COALESCE(wi.acceptance_criteria, ''),
		wsr.status,
		wsr.created_at
		FROM workflow_step_runs wsr
		JOIN workflow_runs wr ON wr.id = wsr.workflow_run_id AND wr.tenant_id = wsr.tenant_id
		JOIN workflows w ON w.id = wr.workflow_id AND w.tenant_id = wr.tenant_id
		LEFT JOIN work_items wi ON wi.workflow_run_id = wr.id AND wi.id = wr.work_item_id
		LEFT JOIN projects p ON p.id = wr.project_id AND p.tenant_id = wr.tenant_id
		WHERE wsr.tenant_id = $1 AND wsr.step_kind = 'approval'`

	args := []any{tenantID}
	argIdx := 2

	if req.WorkflowRunId != nil && *req.WorkflowRunId != "" {
		q += fmt.Sprintf(` AND wsr.workflow_run_id = $%d`, argIdx)
		args = append(args, *req.WorkflowRunId)
		argIdx++
	}
	if req.Status != nil && *req.Status != "" {
		statusFilter := *req.Status
		if statusFilter == "pending" {
			q += fmt.Sprintf(` AND wsr.status = $%d`, argIdx)
			args = append(args, domain.StepRunApprovalPending)
		} else {
			q += fmt.Sprintf(` AND wsr.status = $%d`, argIdx)
			args = append(args, domain.StepRunSucceeded)
		}
		argIdx++
	}
	if req.Search != "" {
		q += fmt.Sprintf(` AND (p.name ILIKE $%d OR wi.title ILIKE $%d OR w.name ILIKE $%d)`, argIdx, argIdx+1, argIdx+2)
		searchPattern := "%" + req.Search + "%"
		args = append(args, searchPattern, searchPattern, searchPattern)
		argIdx += 3
	}

	// Sort.
	sortBy := "wsr.created_at"
	switch req.SortBy {
	case "project_name":
		sortBy = "p.name"
	case "workflow_name":
		sortBy = "w.name"
	}
	sortOrder := "DESC"
	if req.SortOrder == "asc" {
		sortOrder = "ASC"
	}
	q += fmt.Sprintf(` ORDER BY %s %s`, sortBy, sortOrder)

	// Pagination.
	pageSize := int(req.PageSize)
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 50
	}
	q += fmt.Sprintf(` LIMIT $%d`, argIdx)
	args = append(args, pageSize)
	argIdx++

	if req.PageToken != "" {
		q += fmt.Sprintf(` AND wsr.id > $%d`, argIdx)
		args = append(args, req.PageToken)
	}

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query approval items: %w", err)
	}
	defer rows.Close()

	var out []*apiv1.ApprovalItem
	for rows.Next() {
		var r approvalItemRow
		if err := rows.Scan(
			&r.StepRunID, &r.WorkflowRunID,
			&r.ProjectName, &r.WorkItemTitle,
			&r.WorkflowName, &r.UpstreamWorker,
			&r.UpstreamSummary, &r.TouchedFilesJSON,
			&r.AcceptanceCrit, &r.Status, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan approval item: %w", err)
		}

		var files []string
		json.Unmarshal([]byte(r.TouchedFilesJSON), &files)
		if files == nil {
			files = []string{}
		}

		// Map domain statuses to proto contract values ("pending" / "approved" / "rejected").
		mappedStatus := r.Status
		switch r.Status {
		case domain.StepRunApprovalPending:
			mappedStatus = "pending"
		case domain.StepRunSucceeded:
			mappedStatus = "approved"
		}

		item := &apiv1.ApprovalItem{
			StepRunId:         r.StepRunID,
			WorkflowRunId:     r.WorkflowRunID,
			ProjectName:       r.ProjectName,
			WorkItemName:      r.WorkItemTitle,
			WorkflowName:      r.WorkflowName,
			UpstreamWorker:    r.UpstreamWorker,
			UpstreamSummary:   r.UpstreamSummary,
			TouchedFiles:      files,
			AcceptanceCriteria: r.AcceptanceCrit,
			Status:            mappedStatus,
			CreatedAt:         timestamppb.New(r.CreatedAt),
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// --- helpers ---------------------------------------------------------------

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

func strPtr(s string) *string { return &s }
