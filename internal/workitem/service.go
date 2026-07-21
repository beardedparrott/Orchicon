package workitem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// StartWorkflowStarter starts a workflow run for a bound work item.
// Injected by the server wired to the workflow service.
type StartWorkflowStarter func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error

// Service implements the WorkItemService Connect handler
// (apiv1connect.WorkItemServiceHandler). Each mutation writes an outbox
// row in the same transaction as the state change (AGENTS.md invariant
// #3). Optimistic concurrency is enforced via the version column
// (docs/09 §5). Dependency cycles are rejected at admission using a
// recursive CTE (docs/09 §11).
type Service struct {
	pool            *db.Pool
	log             *slog.Logger
	startWorkflowFn StartWorkflowStarter
	apiv1connect.UnimplementedWorkItemServiceHandler
}

// Compile-time assertion.
var _ apiv1connect.WorkItemServiceHandler = (*Service)(nil)

// New constructs a WorkItemService handler.
func New(pool *db.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

// SetStartWorkflowStarter injects the function to start a bound workflow run.
// Called by the server before the reconciler starts (docs/11 §5.2).
func (s *Service) SetStartWorkflowStarter(fn StartWorkflowStarter) { s.startWorkflowFn = fn }

// CreateWorkItem creates a new work item within a project. Depth is
// constrained to 4 levels (docs/02 §2.2).
func (s *Service) CreateWorkItem(ctx context.Context, req *connect.Request[apiv1.CreateWorkItemRequest]) (*connect.Response[apiv1.CreateWorkItemResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	kind, err := validateKind(msg.Kind)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	title, err := validateTitle(msg.Title)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	description, err := validateDescription(msg.Description)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	acceptanceCriteria, err := validateAcceptanceCriteria(msg.AcceptanceCriteria)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	budgets, err := validateBudgets(msg.Budgets)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	var parentID *string
	if msg.ParentId != "" {
		parentID = &msg.ParentId
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Only active projects may host work items (docs/02 §2.1).
	if err := db.RequireProjectActive(ctx, ttx.Tx, tenantID, msg.ProjectId); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("project not active: %w", err))
	}

	// Enforce hierarchy depth: a subtask's parent must be a task, etc.
	if parentID != nil {
		parent, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, *parentID)
		if err != nil {
			return nil, mapDBError(err)
		}
		if parent.ProjectID != msg.ProjectId {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("parent must be in the same project"))
		}
		parentDepth := kindOrder[parent.Kind]
		childDepth := kindOrder[kind]
		if childDepth <= parentDepth {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("a %s must be deeper than its parent (parent is %s)", kind, parent.Kind))
		}
	} else if kind != domain.WorkItemKindEpic {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("a top-level work item must be an epic, not a %s", kind))
	}

	// Parse scheduled start and workflow binding fields (docs/11 §5.1).
	var scheduledStartAt *time.Time
	if msg.ScheduledStartAt != nil {
		t := msg.ScheduledStartAt.AsTime()
		scheduledStartAt = &t
	}
	autoStart := true
	if msg.ScheduledStartAt == nil && !msg.AutoStartWorkflow {
		autoStart = false
	}
	workflowID := msg.WorkflowId
	if workflowID == "" {
		workflowID = "" // keep empty for unbound items
	}

	row := db.WorkItemRow{
		ID:                 db.NewID(),
		TenantID:           tenantID,
		ProjectID:          msg.ProjectId,
		ParentID:           parentID,
		Kind:               kind,
		Title:              title,
		Description:        description,
		AcceptanceCriteria: acceptanceCriteria,
		Status:             domain.WorkItemPending,
		Priority:           int(msg.Priority),
		Budgets:            budgets,
		ContextWindow:      int(msg.ContextWindow),
		WorkflowID:         &workflowID,
		ScheduledStartAt:   scheduledStartAt,
		AutoStartWorkflow:  autoStart,
	}
	if workflowID == "" {
		row.WorkflowID = nil
	}
	created, err := db.CreateWorkItem(ctx, ttx.Tx, row)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.created", created); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}

	// If the work item has a workflow binding and should auto-start
	// immediately, call StartWorkflow (docs/11 §5.2).
	if workflowID != "" && scheduledStartAt == nil && autoStart && s.startWorkflowFn != nil {
		if err := s.startWorkflowFn(ctx, tenantID, workflowID, msg.ProjectId, created.ID); err != nil {
			s.log.Warn("auto-start workflow failed", "work_item", created.ID, "workflow", workflowID, "error", err)
		}
	}

	s.log.Info("work item created", "id", created.ID, "kind", kind, "project", msg.ProjectId)
	return connect.NewResponse(&apiv1.CreateWorkItemResponse{WorkItem: rowToProto(created)}), nil
}

// GetWorkItem returns a single work item by id.
func (s *Service) GetWorkItem(ctx context.Context, req *connect.Request[apiv1.GetWorkItemRequest]) (*connect.Response[apiv1.GetWorkItemResponse], error) {
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
	w, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetWorkItemResponse{WorkItem: rowToProto(w)}), nil
}

// ListWorkItems returns a page of work items for a project, optionally
// filtered by parent (tree) or status (Kanban).
func (s *Service) ListWorkItems(ctx context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	f := db.ListWorkItemsFilter{
		TenantID:  tenantID,
		ProjectID: req.Msg.ProjectId,
		PageSize:  int(req.Msg.PageSize),
		AfterID:   req.Msg.PageToken,
		Search:    req.Msg.Search,
		SortBy:    req.Msg.SortBy,
		SortOrder: req.Msg.SortOrder,
	}
	if req.Msg.ParentId != nil {
		pid := *req.Msg.ParentId
		f.ParentID = &pid
	}
	if req.Msg.Status != nil {
		f.Status = validateStatus(*req.Msg.Status)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	items, err := db.ListWorkItems(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListWorkItemsResponse{}
	for _, w := range items {
		resp.WorkItems = append(resp.WorkItems, rowToProto(w))
	}
	if len(items) > 0 {
		resp.NextPageToken = items[len(items)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// UpdateWorkItem applies a partial update with optimistic concurrency
// (docs/09 §5). Only non-nil fields are written (field-mask semantics).
func (s *Service) UpdateWorkItem(ctx context.Context, req *connect.Request[apiv1.UpdateWorkItemRequest]) (*connect.Response[apiv1.UpdateWorkItemResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	var fields db.UpdateWorkItemFields
	if msg.Title != nil {
		title, err := validateTitle(*msg.Title)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Title = &title
	}
	if msg.Description != nil {
		desc, err := validateDescription(*msg.Description)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Description = &desc
	}
	if msg.AcceptanceCriteria != nil {
		ac, err := validateAcceptanceCriteria(*msg.AcceptanceCriteria)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.AcceptanceCriteria = &ac
	}
	if msg.Status != nil {
		fields.Status = strPtr(validateStatus(*msg.Status))
	}
	if msg.Priority != nil {
		fields.Priority = intPtr(int(*msg.Priority))
	}
	if msg.Budgets != nil {
		budgets, err := validateBudgets(*msg.Budgets)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Budgets = &budgets
	}
	if msg.ContextWindow != nil {
		fields.ContextWindow = intPtr(int(*msg.ContextWindow))
	}
	if msg.ProjectId != nil {
		fields.ProjectID = msg.ProjectId
	}
	if msg.WorkflowId != nil {
		wfid := *msg.WorkflowId
		fields.WorkflowID = &wfid
	}
	if msg.ScheduledStartAt != nil {
		t := msg.ScheduledStartAt.AsTime()
		fields.ScheduledStartAt = &t
	}
	if msg.AutoStartWorkflow != nil {
		v := *msg.AutoStartWorkflow
		fields.AutoStartWorkflow = &v
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	// If reassigning to a different project, the target must be active.
	if fields.ProjectID != nil && *fields.ProjectID != current.ProjectID {
		if err := db.RequireProjectActive(ctx, ttx.Tx, tenantID, *fields.ProjectID); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target project not active: %w", err))
		}
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item updated", "id", updated.ID, "version", updated.Version)
	return connect.NewResponse(&apiv1.UpdateWorkItemResponse{WorkItem: rowToProto(updated)}), nil
}

// DeleteWorkItem soft-deletes a work item by setting status to cancelled.
func (s *Service) DeleteWorkItem(ctx context.Context, req *connect.Request[apiv1.DeleteWorkItemRequest]) (*connect.Response[apiv1.DeleteWorkItemResponse], error) {
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
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	fields := db.UpdateWorkItemFields{Status: strPtr(domain.WorkItemCancelled)}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.deleted", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item deleted (cancelled)", "id", updated.ID)
	return connect.NewResponse(&apiv1.DeleteWorkItemResponse{WorkItem: rowToProto(updated)}), nil
}

// HardDeleteWorkItem permanently removes a work item. Cascades to its
// dependencies. The outbox emits a work_item.purged event.
func (s *Service) HardDeleteWorkItem(ctx context.Context, req *connect.Request[apiv1.HardDeleteWorkItemRequest]) (*connect.Response[apiv1.HardDeleteWorkItemResponse], error) {
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
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := db.HardDeleteWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.purged", current); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item hard-deleted", "id", req.Msg.Id)
	return connect.NewResponse(&apiv1.HardDeleteWorkItemResponse{}), nil
}

// AddDependency adds an edge to the work DAG. Cycles are rejected at
// admission using a recursive CTE (docs/02 §2.2, docs/09 §11).
func (s *Service) AddDependency(ctx context.Context, req *connect.Request[apiv1.AddDependencyRequest]) (*connect.Response[apiv1.AddDependencyResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	if msg.FromId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("from_id must not be empty"))
	}
	if msg.ToId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("to_id must not be empty"))
	}
	if msg.FromId == msg.ToId {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cannot create a self-dependency"))
	}
	depType, err := validateDependencyType(msg.Type)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Verify both work items exist and are in the same project.
	fromItem, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, msg.FromId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if fromItem.ProjectID != msg.ProjectId {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("from_id work item is not in the specified project"))
	}
	toItem, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, msg.ToId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if toItem.ProjectID != msg.ProjectId {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("to_id work item is not in the specified project"))
	}

	// Cycle check: would adding from→to create a cycle? Traverse
	// forward from `to` — if `from` is reachable, the edge closes a
	// cycle (docs/09 §11: recursive CTE).
	createsCycle, err := db.CheckCycleWithRecursiveCTE(ctx, ttx.Tx, tenantID, msg.ProjectId, msg.FromId, msg.ToId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if createsCycle {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("adding this dependency would create a cycle in the work DAG"))
	}

	dep := db.DependencyRow{
		ID:       db.NewID(),
		TenantID: tenantID,
		ProjectID: msg.ProjectId,
		FromID:   msg.FromId,
		ToID:     msg.ToId,
		Type:     depType,
	}
	created, err := db.CreateDependency(ctx, ttx.Tx, dep)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueDependencyEvent(ctx, ttx.Tx, "work_item.dependency_added", created); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("dependency added", "from", created.FromID, "to", created.ToID, "type", depType)
	return connect.NewResponse(&apiv1.AddDependencyResponse{Dependency: depRowToProto(created)}), nil
}

// RemoveDependency removes an edge from the work DAG.
func (s *Service) RemoveDependency(ctx context.Context, req *connect.Request[apiv1.RemoveDependencyRequest]) (*connect.Response[apiv1.RemoveDependencyResponse], error) {
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
	if err := db.DeleteDependency(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("dependency removed", "id", req.Msg.Id)
	return connect.NewResponse(&apiv1.RemoveDependencyResponse{}), nil
}

// GetDependencyGraph returns the full DAG (nodes + edges) for a
// project. Used by the frontend's read-only React Flow graph (docs/10).
func (s *Service) GetDependencyGraph(ctx context.Context, req *connect.Request[apiv1.GetDependencyGraphRequest]) (*connect.Response[apiv1.GetDependencyGraphResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	items, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
		TenantID:  tenantID,
		ProjectID: req.Msg.ProjectId,
		PageSize:  1000,
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	deps, err := db.ListDependencies(ctx, ttx.Tx, tenantID, req.Msg.ProjectId)
	if err != nil {
		return nil, mapDBError(err)
	}
	graph := &apiv1.DependencyGraph{}
	for _, w := range items {
		graph.Nodes = append(graph.Nodes, rowToProto(w))
	}
	for _, d := range deps {
		graph.Edges = append(graph.Edges, depRowToProto(d))
	}
	return connect.NewResponse(&apiv1.GetDependencyGraphResponse{Graph: graph}), nil
}

// AssignWorker binds a Worker (id + version) to a Task/Subtask
// (docs/02 §2.2).
func (s *Service) AssignWorker(ctx context.Context, req *connect.Request[apiv1.AssignWorkerRequest]) (*connect.Response[apiv1.AssignWorkerResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	workerRef, err := validateWorkerRef(req.Msg.WorkerRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if workerRef == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_ref must not be empty for assignment"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	fields := db.UpdateWorkItemFields{AssignedWorkerRef: &workerRef}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.worker_assigned", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker assigned to work item", "id", updated.ID)
	return connect.NewResponse(&apiv1.AssignWorkerResponse{WorkItem: rowToProto(updated)}), nil
}

// UnassignWorker removes the worker binding from a Task/Subtask.
func (s *Service) UnassignWorker(ctx context.Context, req *connect.Request[apiv1.UnassignWorkerRequest]) (*connect.Response[apiv1.UnassignWorkerResponse], error) {
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
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	// Clear assigned_worker_ref with a status CAS, bumping the version
	// (docs/09 §5). The data-access layer's UpdateWorkItemFields treats
	// AssignedWorkerRef as a non-nil pointer to a bytea value, which
	// cannot encode NULL. So we use a direct CAS query for unassign.
	const q = `UPDATE work_items
		SET assigned_worker_ref = NULL, updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, project_id, parent_id, kind, title, description,
			acceptance_criteria, status, assigned_worker_ref, workflow_id,
			priority, budgets, context_window, results, prompt_context, version, created_at, updated_at`
	var updated db.WorkItemRow
	err = ttx.Tx.QueryRow(ctx, q, tenantID, req.Msg.Id, current.Version).Scan(
		&updated.ID, &updated.TenantID, &updated.ProjectID, &updated.ParentID, &updated.Kind, &updated.Title,
		&updated.Description, &updated.AcceptanceCriteria, &updated.Status, &updated.AssignedWorkerRef,
		&updated.WorkflowID, &updated.Priority, &updated.Budgets, &updated.ContextWindow, &updated.Results,
		&updated.PromptContext, &updated.Version, &updated.CreatedAt, &updated.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("db: unassign worker: %w", err))
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.worker_unassigned", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker unassigned from work item", "id", updated.ID)
	return connect.NewResponse(&apiv1.UnassignWorkerResponse{WorkItem: rowToProto(updated)}), nil
}

// --- helpers ---------------------------------------------------------------

func enqueueWorkItemEvent(ctx context.Context, tx pgx.Tx, eventType string, w db.WorkItemRow) error {
	payload, err := buildWorkItemEventPayload(eventType, w)
	if err != nil {
		return err
	}
	row := db.OutboxRow{
		TenantID:      w.TenantID,
		EventType:     eventType,
		AggregateType: "work_item",
		AggregateID:   w.ID,
		AggregateVer:  w.Version,
		Payload:       payload,
		OccurredAt:     time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

func enqueueDependencyEvent(ctx context.Context, tx pgx.Tx, eventType string, d db.DependencyRow) error {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":        d.TenantID,
		"project_id":       d.ProjectID,
		"aggregate_type":   "work_item_dependency",
		"aggregate_id":     d.ID,
		"dependency_id":    d.ID,
		"from_id":          d.FromID,
		"to_id":            d.ToID,
		"type":             d.Type,
		"occurred_at":      time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal dependency event payload: %w", err)
	}
	row := db.OutboxRow{
		TenantID:      d.TenantID,
		EventType:     eventType,
		AggregateType: "work_item_dependency",
		AggregateID:   d.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

func buildWorkItemEventPayload(eventType string, w db.WorkItemRow) ([]byte, error) {
	evt := map[string]any{
		"event_type":        eventType,
		"tenant_id":         w.TenantID,
		"project_id":        w.ProjectID,
		"work_item_id":      w.ID,
		"aggregate_type":    "work_item",
		"aggregate_id":      w.ID,
		"aggregate_version": w.Version,
		"kind":              w.Kind,
		"title":             w.Title,
		"status":            w.Status,
		"occurred_at":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal work item event payload: %w", err)
	}
	return b, nil
}

func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

func kindToProto(kind string) apiv1.WorkItemKind {
	switch kind {
	case domain.WorkItemKindEpic:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC
	case domain.WorkItemKindFeature:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE
	case domain.WorkItemKindTask:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_TASK
	case domain.WorkItemKindSubtask:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK
	default:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_UNSPECIFIED
	}
}

func statusToProto(status string) apiv1.WorkItemStatus {
	switch status {
	case domain.WorkItemPending:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING
	case domain.WorkItemReady:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_READY
	case domain.WorkItemAssigned:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_ASSIGNED
	case domain.WorkItemRunning:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING
	case domain.WorkItemCheckpointing:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_CHECKPOINTING
	case domain.WorkItemSucceeded:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED
	case domain.WorkItemFailed:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_FAILED
	case domain.WorkItemCancelled:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED
	case domain.WorkItemRecovering:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECOVERING
	default:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_UNSPECIFIED
	}
}

func depTypeToProto(t string) apiv1.DependencyType {
	switch t {
	case domain.DependencyBlocks:
		return apiv1.DependencyType_DEPENDENCY_TYPE_BLOCKS
	case domain.DependencyDependsOn:
		return apiv1.DependencyType_DEPENDENCY_TYPE_DEPENDS_ON
	case domain.DependencyRelatesTo:
		return apiv1.DependencyType_DEPENDENCY_TYPE_RELATES_TO
	default:
		return apiv1.DependencyType_DEPENDENCY_TYPE_UNSPECIFIED
	}
}

func kindForDepth(depth int) string {
	switch depth {
	case 1:
		return domain.WorkItemKindEpic
	case 2:
		return domain.WorkItemKindFeature
	case 3:
		return domain.WorkItemKindTask
	case 4:
		return domain.WorkItemKindSubtask
	default:
		return "unknown"
	}
}

func rowToProto(w db.WorkItemRow) *apiv1.WorkItem {
	p := &apiv1.WorkItem{
		Id:                  w.ID,
		TenantId:            w.TenantID,
		ProjectId:           w.ProjectID,
		Kind:                kindToProto(w.Kind),
		Title:               w.Title,
		Description:         w.Description,
		AcceptanceCriteria:  w.AcceptanceCriteria,
		Status:              statusToProto(w.Status),
		Priority:            int32(w.Priority),
		Budgets:             string(w.Budgets),
		ContextWindow:       int32(w.ContextWindow),
		Results:             string(w.Results),
		// PR B (context propagation): carries the composite prompt.
		// Stored as JSONB {"composite": "# Task\n..."} — extract the
		// inner text so the frontend gets plain markdown.
		PromptContext:       extractCompositePrompt(w.PromptContext),
		Version:             int32(w.Version),
		CreatedAt:           timestamppb.New(w.CreatedAt),
		UpdatedAt:           timestamppb.New(w.UpdatedAt),
	}
	if w.ParentID != nil {
		p.ParentId = *w.ParentID
	}
	if w.AssignedWorkerRef != nil {
		p.AssignedWorkerRef = string(w.AssignedWorkerRef)
	}
	if w.WorkflowID != nil {
		p.WorkflowId = *w.WorkflowID
	}
	if w.ScheduledStartAt != nil {
		p.ScheduledStartAt = timestamppb.New(*w.ScheduledStartAt)
	}
	p.AutoStartWorkflow = w.AutoStartWorkflow
	return p
}

func depRowToProto(d db.DependencyRow) *apiv1.WorkItemDependency {
	return &apiv1.WorkItemDependency{
		Id:        d.ID,
		TenantId:  d.TenantID,
		ProjectId: d.ProjectID,
		FromId:    d.FromID,
		ToId:      d.ToID,
		Type:      depTypeToProto(d.Type),
		CreatedAt: timestamppb.New(d.CreatedAt),
	}
}

func extractCompositePrompt(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var pc struct {
		Composite string `json:"composite"`
	}
	if err := json.Unmarshal(raw, &pc); err == nil && pc.Composite != "" {
		return pc.Composite
	}
	return string(raw)
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }
