package workitem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/contextfiles"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// StartWorkflowStarter starts a workflow run for a bound work item.
// Injected by the server wired to the workflow service.
type StartWorkflowStarter func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error

// StartSequenceStarter starts a sequence run for a parent work item with children
// (architecture-notes/sequential-multi-workflow-runs.md §2.3): flips the parent to running.
type StartSequenceStarter func(ctx context.Context, tenantID, parentID string) error

// ResumeSequenceStarter resumes a sequence parent from its current state: parent → running, first
// non-succeeded child armed, history kept.
type ResumeSequenceStarter func(ctx context.Context, tenantID, parentID string) error

// StopSequenceStarter parks a sequence parent: parent → pending and the scheduled start cleared,
// nothing else.
type StopSequenceStarter func(ctx context.Context, tenantID, parentID string) error

// RuntimeImageResolver resolves the base runtime image tag (or "" when no daemon is configured).
type RuntimeImageResolver func(ctx context.Context) string

// Service implements the WorkItemService Connect handler (apiv1connect.WorkItemServiceHandler).
type Service struct {
	pool             *db.Pool
	log              *slog.Logger
	startWorkflowFn  StartWorkflowStarter
	sequenceStartFn  StartSequenceStarter
	sequenceResumeFn ResumeSequenceStarter
	sequenceStopFn   StopSequenceStarter
	runtimeImageFn   RuntimeImageResolver
	// archiveConflictHook is a test-only seam: invoked with the attempt number (1 = initial, 2 = the
	// one-shot retry) right after ArchiveWorkItem's read+validate for that attempt and before.
	archiveConflictHook func(attempt int)
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

// SetStartSequenceStarter injects the function that starts a sequence run for a parent work item
// with children (auto-start / run-instant path).
func (s *Service) SetStartSequenceStarter(fn StartSequenceStarter) { s.sequenceStartFn = fn }

// SetResumeSequenceStarter injects the function that resumes a sequence parent (parent → running,
// first non-succeeded child armed, history kept).
func (s *Service) SetResumeSequenceStarter(fn ResumeSequenceStarter) { s.sequenceResumeFn = fn }

// SetStopSequenceStarter injects the function that parks a sequence parent (parent → pending,
// scheduled start cleared).
func (s *Service) SetStopSequenceStarter(fn StopSequenceStarter) { s.sequenceStopFn = fn }

// SetRuntimeImageResolver injects the function that resolves the base
// runtime image tag, used to stamp the work item's runtime_image default.
func (s *Service) SetRuntimeImageResolver(fn RuntimeImageResolver) { s.runtimeImageFn = fn }

// validateContextFilesInput validates a request's context_files list and marshals it to the JSONB
// column value.
func validateContextFilesInput(paths []string) ([]byte, error) {
	if err := contextfiles.Validate(paths); err != nil {
		return nil, err
	}
	return contextfiles.ToJSON(paths)
}

// CreateWorkItem creates a new work item within a project.
type AutomationRunContextKey struct{}

// WithAutomationRunContext returns a context carrying the given run_context so a create path can
// stamp automation provenance.
func WithAutomationRunContext(ctx context.Context, runContext []byte) context.Context {
	if len(runContext) == 0 {
		return ctx
	}
	return context.WithValue(ctx, AutomationRunContextKey{}, runContext)
}

// RunContextFrom returns the run_context carried on the request context, if any.
func RunContextFrom(ctx context.Context) []byte {
	rc, _ := ctx.Value(AutomationRunContextKey{}).([]byte)
	return rc
}

// ApplyAutomationProvenance stamps the automation provenance block onto a work item being created
// from a recurring fire's run: spawned_by (the recurring item id).
func ApplyAutomationProvenance(row *db.WorkItemRow, runContext []byte) {
	if len(runContext) == 0 {
		return
	}
	spawnedBy, runID, mode := db.ProvenanceFromRunContext(runContext)
	if spawnedBy == "" {
		return
	}
	row.SpawnedByWorkItemID = &spawnedBy
	row.SpawnedByRunID = &runID
	if mode == domain.RecurringOutputsIdea {
		row.Status = domain.WorkItemIdea
	}
}

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
	title, err := ValidateTitle(msg.Title)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	description, err := ValidateDescription(msg.Description)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	acceptanceCriteria, err := ValidateAcceptanceCriteria(msg.AcceptanceCriteria)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	budgets, err := ValidateBudgets(msg.Budgets)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	contextFiles, err := validateContextFilesInput(msg.ContextFiles)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	var secretIDsJSON []byte
	if len(msg.SecretIds) > 0 {
		if err := ValidateSecretIDs(msg.SecretIds); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	// validate existence via DB after tx begins — store JSON for row

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

	// Context files must live inside the project's directory — the only path guaranteed to be mounted
	// where workers run.
	if len(msg.ContextFiles) > 0 {
		if proj, err := db.GetProject(ctx, ttx.Tx, tenantID, msg.ProjectId); err == nil {
			if err := contextfiles.ValidateWithin(msg.ContextFiles, proj.ProjectDir); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
	}

	// Parse and validate the recurring schedule BEFORE hierarchy validation so a flat-recurring item
	// (kind=task, no parent) is exempt from the "only epics are top-level" rule and its flat shape.
	if msg.RecurringSchedule != nil && IsRecurringScheduleEmpty(msg.RecurringSchedule) {
		msg.RecurringSchedule = nil
	}
	recurringSchedule, err := ValidateRecurringSchedule(msg.RecurringSchedule)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	isRecurring := recurringSchedule != nil

	// Enforce hierarchy depth: a subtask's parent must be a task, etc.
	// Shared with the Update path so the two cannot drift.
	if err := ValidateParent(ctx, ttx.Tx, tenantID, msg.ParentId, kind, msg.ProjectId, isRecurring); err != nil {
		return nil, mapParentError(err)
	}
	// Flat-recurring invariants (D1/D7): recurring items are flat top-level tasks (no hierarchy
	// participation), and a schedulable recurring LEAF must be bound to a workflow (the recurring fire
	// has.
	if isRecurring {
		if err := ValidateRecurringFlatness(true, kind, msg.ParentId); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if err := ValidateRecurringWorkflowBinding(true, false, msg.WorkflowId); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
	}

	// Parse scheduled start and workflow binding fields (docs/11 §5.1).
	var scheduledStartAt *time.Time
	if msg.ScheduledStartAt != nil {
		t := msg.ScheduledStartAt.AsTime()
		scheduledStartAt = &t
	}
	// auto_start_workflow defaults to false when unset (proto optional) — "Start immediately on save"
	// is opt-in, never the default (bug fix: the old true default kicked off runs on every
	// create/save).
	autoStart := false
	if msg.AutoStartWorkflow != nil && *msg.AutoStartWorkflow {
		autoStart = true
	}
	workflowID := msg.WorkflowId
	if workflowID == "" {
		workflowID = "" // keep empty for unbound items
	}

	if len(msg.SecretIds) > 0 {
		if len(msg.SecretIds) > 0 {
			m, err := db.BatchGetSecrets(ctx, ttx.Tx, tenantID, msg.SecretIds)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			if len(m) != len(msg.SecretIds) {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("one or more secrets not found"))
			}
		}
		b, _ := json.Marshal(msg.SecretIds)
		secretIDsJSON = b
	} else {
		secretIDsJSON = []byte("[]")
	}

	status := domain.WorkItemPending
	if scheduledStartAt != nil {
		status = domain.WorkItemScheduled
	}
	if recurringSchedule != nil {
		status = domain.WorkItemRecurring
	}
	now := time.Now().UTC()
	var nextRunAt *time.Time
	if recurringSchedule != nil {
		nextRunAt = ComputeNextRunAt(msg.RecurringSchedule, now)
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
		Status:             status,
		Priority:           int(msg.Priority),
		Budgets:            budgets,
		ContextWindow:      int(msg.ContextWindow),
		WorkflowID:         &workflowID,
		ScheduledStartAt:   scheduledStartAt,
		AutoStartWorkflow:  autoStart,
		ContextFiles:       contextFiles,
		RecurringSchedule:  recurringSchedule,
		SecretIDs:          secretIDsJSON,
		NextRunAt:          nextRunAt,
	}
	// Stamp automation provenance (feature 4.1, AC2): when this create runs inside a recurring fire's
	// run, the run_context carried on the request (or on the request context.
	rc := []byte(msg.RunContext)
	if len(rc) == 0 {
		rc = RunContextFrom(ctx)
	}
	ApplyAutomationProvenance(&row, rc)
	// Stamp the runtime image: the caller's choice wins; empty = the base image (resolved from the
	// daemon).
	runtimeImage := strings.TrimSpace(msg.RuntimeImage)
	if runtimeImage == "" && s.runtimeImageFn != nil {
		runtimeImage = s.runtimeImageFn(ctx)
	}
	row.RuntimeImage = runtimeImage
	// Work-item execution_mode override: an unspecified request value means INHERIT and stays nil
	// (never stamped from the project) so the resolution chain at dispatch.
	row.ExecutionMode = executionModeFromProto(msg.ExecutionMode)
	if workflowID == "" {
		row.WorkflowID = nil
	}
	created, err := db.CreateWorkItem(ctx, ttx.Tx, row)
	if err != nil {
		return nil, mapDBError(err)
	}
	// Workflow-first enforcement at CREATE: a SCHEDULED item with no workflow binding is a zombie — its
	// start time comes due and nothing can run it (standalone dispatch is retired).
	if scheduledStartAt != nil {
		if err := ValidateSequenceSchedule(ctx, ttx.Tx, tenantID, created); err != nil {
			return nil, err
		}
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.created", created); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.created", "work_item", created.ID, nil, audit.Snapshot(workItemAuditSnapshot(created))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.created: %w", err))
	}
	// depends_on edges: create a DEPENDS_ON-type edge for each listed ID in the same transaction as the
	// item.
	if len(msg.DependsOn) > 0 {
		validated, err := validateDependsOn(ctx, ttx.Tx, tenantID, msg.ProjectId, created.ID, msg.DependsOn)
		if err != nil {
			return nil, err
		}
		for _, depID := range validated {
			d := db.DependencyRow{
				ID:        db.NewID(),
				TenantID:  tenantID,
				ProjectID: msg.ProjectId,
				FromID:    created.ID,
				ToID:      depID,
				Type:      domain.DependencyDependsOn,
			}
			createdDep, err := db.CreateDependency(ctx, ttx.Tx, d)
			if err != nil {
				return nil, mapDBError(err)
			}
			if err := enqueueDependencyEvent(ctx, ttx.Tx, "work_item.dependency_added", createdDep); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.dependency_added", "work_item", createdDep.FromID,
				nil, audit.Snapshot(map[string]any{"from_id": createdDep.FromID, "to_id": createdDep.ToID, "type": createdDep.Type})); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.dependency_added: %w", err))
			}
		}
	}
	proto := rowToProto(created)
	fillEffectiveMode(ctx, ttx.Tx, tenantID, proto, created)
	if err := attachDependsOn(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}

	// If the work item should auto-start immediately.
	if scheduledStartAt == nil && autoStart {
		if s.itemHasChildren(ctx, tenantID, created.ID) {
			s.maybeStartSequence(ctx, tenantID, created)
		} else if workflowID != "" && s.startWorkflowFn != nil {
			if err := s.startWorkflowFn(ctx, tenantID, workflowID, msg.ProjectId, created.ID); err != nil {
				s.log.Warn("auto-start workflow failed", "work_item", created.ID, "workflow", workflowID, "error", err)
			}
		}
	}

	s.log.Info("work item created", "id", created.ID, "kind", kind, "project", msg.ProjectId)
	return connect.NewResponse(&apiv1.CreateWorkItemResponse{WorkItem: proto}), nil
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
	proto := rowToProto(w)
	fillEffectiveMode(ctx, ttx.Tx, tenantID, proto, w)
	if err := attachDependsOn(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedBy(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Idea surface: render the 'from automation X' badge for items that carry
	// automation provenance (feature 5.1). Read-time only; nothing is stored.
	if err := attachSpawnedByTitles(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.GetWorkItemResponse{WorkItem: proto}), nil
}

// GetWorkItemRunHistory returns the per-fire run-history ledger for a recurring work item, newest
// first.
func (s *Service) GetWorkItemRunHistory(ctx context.Context, req *connect.Request[apiv1.GetWorkItemRunHistoryRequest]) (*connect.Response[apiv1.GetWorkItemRunHistoryResponse], error) {
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
	entries, err := db.ListRecurringRunHistory(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := &apiv1.GetWorkItemRunHistoryResponse{
		Entries: make([]*apiv1.RecurringRunHistoryEntry, 0, len(entries)),
	}
	for _, e := range entries {
		pe := &apiv1.RecurringRunHistoryEntry{
			Id:            e.ID,
			Status:        e.Status,
			WorkflowRunId: e.WorkflowRunID,
			RunStatus:     e.RunStatus,
			Error:         e.Error,
		}
		if !e.FireAt.IsZero() {
			pe.FireAt = timestamppb.New(e.FireAt)
		}
		if e.RunStartedAt != nil {
			pe.RunStartedAt = timestamppb.New(*e.RunStartedAt)
		}
		if e.RunEndedAt != nil {
			pe.RunEndedAt = timestamppb.New(*e.RunEndedAt)
		}
		for _, x := range e.Executions {
			px := &apiv1.RecurringRunExecution{Id: x.ID, Status: x.Status, StepId: x.StepID, Output: x.Output}
			if x.StartedAt != nil {
				px.StartedAt = timestamppb.New(*x.StartedAt)
			}
			if x.EndedAt != nil {
				px.EndedAt = timestamppb.New(*x.EndedAt)
			}
			pe.Executions = append(pe.Executions, px)
		}
		out.Entries = append(out.Entries, pe)
	}
	return connect.NewResponse(out), nil
}

// ListWorkItems returns a page of work items for a project, optionally
// filtered by parent (tree) or status (Kanban).
func (s *Service) ListWorkItems(ctx context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Empty project_id = list across all projects (for "All" filter).
	f := db.ListWorkItemsFilter{
		TenantID:        tenantID,
		ProjectID:       req.Msg.ProjectId,
		PageSize:        int(req.Msg.PageSize),
		AfterID:         req.Msg.PageToken,
		Search:          req.Msg.Search,
		SortBy:          req.Msg.SortBy,
		SortOrder:       req.Msg.SortOrder,
		IncludeArchived: req.Msg.IncludeArchived,
	}
	switch req.Msg.RecurringFilter {
	case apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING:
		f.RecurringFilter = "exclude"
	case apiv1.RecurringFilter_RECURRING_FILTER_ONLY_RECURRING:
		f.RecurringFilter = "only"
	case apiv1.RecurringFilter_RECURRING_FILTER_ALL:
		f.RecurringFilter = "all"
	default:
		// UNSPECIFIED legacy: exclude recurring so normal Work Items callers that pre-date the field do not
		// leak recurring rows.
		f.RecurringFilter = "exclude"
	}
	switch req.Msg.IdeaScope {
	case apiv1.IdeaScope_IDEA_SCOPE_ONLY_IDEA:
		f.IdeaScope = "only"
	default:
		// UNSPECIFIED / EXCLUDE_IDEA legacy default: hide idea-state items so
		// normal Work Items callers never leak automation-produced ideas.
		f.IdeaScope = "exclude"
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
	if err := attachDependsOnBatch(ctx, ttx.Tx, tenantID, resp.WorkItems); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedByBatch(ctx, ttx.Tx, tenantID, resp.WorkItems); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(items) > 0 {
		resp.NextPageToken = items[len(items)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// ListIdeas returns a page of idea-state work items (feature 5.1) with their automation provenance
// and a read-time spawned_by_title badge.
func (s *Service) ListIdeas(ctx context.Context, req *connect.Request[apiv1.ListIdeasRequest]) (*connect.Response[apiv1.ListIdeasResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Map the proto scope enum onto the store key; UNSPECIFIED keeps the
	// feature-5.1 behavior byte-for-byte (ACTIVE).
	var stateScope string
	switch req.Msg.IdeaStateScope {
	case apiv1.IdeaStateScope_IDEA_STATE_SCOPE_REJECTED:
		stateScope = domain.IdeaStateRejected
	default:
		stateScope = domain.IdeaStateActive
	}
	f := db.ListWorkItemsFilter{
		TenantID:  tenantID,
		ProjectID: req.Msg.ProjectId,
		PageSize:  int(req.Msg.PageSize),
		AfterID:   req.Msg.PageToken,
		Search:    req.Msg.Search,
		SortBy:    req.Msg.SortBy,
		SortOrder: req.Msg.SortOrder,
		// The idea surface always asks for idea-state items.
		IdeaScope:      domain.IdeaScopeOnly,
		IdeaStateScope: stateScope,
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
	resp := &apiv1.ListIdeasResponse{}
	for _, w := range items {
		resp.Ideas = append(resp.Ideas, rowToProto(w))
	}
	if err := attachDependsOnBatch(ctx, ttx.Tx, tenantID, resp.Ideas); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedByBatch(ctx, ttx.Tx, tenantID, resp.Ideas); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachSpawnedByTitlesBatch(ctx, ttx.Tx, tenantID, resp.Ideas); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(items) > 0 {
		resp.NextPageToken = items[len(items)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// PromoteIdea is the ONLY sanctioned path out of idea state — the generic update/delete paths are
// gated for idea items.
func (s *Service) PromoteIdea(ctx context.Context, req *connect.Request[apiv1.PromoteIdeaRequest]) (*connect.Response[apiv1.PromoteIdeaResponse], error) {
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
	if current.Status != domain.WorkItemIdea {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("only idea-state work items can be promoted; approve it via PromoteIdea, or discard it via DismissIdea"))
	}
	fields := db.UpdateWorkItemFields{Status: strPtr(domain.WorkItemPending)}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.promoted", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.promoted", "work_item", updated.ID,
		audit.Snapshot(workItemAuditSnapshot(current)), audit.Snapshot(workItemAuditSnapshot(updated))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.promoted: %w", err))
	}
	proto := rowToProto(updated)
	fillEffectiveMode(ctx, ttx.Tx, tenantID, proto, updated)
	if err := attachDependsOn(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedBy(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachSpawnedByTitles(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item promoted", "id", updated.ID, "version", updated.Version)
	return connect.NewResponse(&apiv1.PromoteIdeaResponse{WorkItem: proto}), nil
}

// DismissIdea discards an idea (feature 5.1): it transitions an idea-state work item to cancelled —
// the soft-delete/cancel terminal, consistent with DeleteWorkItem.
func (s *Service) DismissIdea(ctx context.Context, req *connect.Request[apiv1.DismissIdeaRequest]) (*connect.Response[apiv1.DismissIdeaResponse], error) {
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
	if current.Status != domain.WorkItemIdea {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("only idea-state work items can be dismissed; discard it via DismissIdea, or approve it via PromoteIdea"))
	}
	fields := db.UpdateWorkItemFields{Status: strPtr(domain.WorkItemCancelled)}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.dismissed", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.dismissed", "work_item", updated.ID,
		audit.Snapshot(workItemAuditSnapshot(current)), audit.Snapshot(workItemAuditSnapshot(updated))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.dismissed: %w", err))
	}
	proto := rowToProto(updated)
	fillEffectiveMode(ctx, ttx.Tx, tenantID, proto, updated)
	if err := attachDependsOn(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedBy(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Provenance is retained; render the badge if the spawning item still
	// exists (harmless if not).
	if err := attachSpawnedByTitles(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item dismissed", "id", updated.ID, "version", updated.Version)
	return connect.NewResponse(&apiv1.DismissIdeaResponse{WorkItem: proto}), nil
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
		title, err := ValidateTitle(*msg.Title)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Title = &title
	}
	if msg.Description != nil {
		desc, err := ValidateDescription(*msg.Description)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Description = &desc
	}
	if msg.AcceptanceCriteria != nil {
		ac, err := ValidateAcceptanceCriteria(*msg.AcceptanceCriteria)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.AcceptanceCriteria = &ac
	}
	if msg.AcceptanceReview != nil {
		ar, err := ValidateAcceptanceReview(*msg.AcceptanceReview)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.AcceptanceReview = &ar
	}
	if msg.Status != nil {
		st := validateStatus(*msg.Status)
		// Archiving is a deliberate, gated action with its own preconditions (terminal status only, no
		// children) and its own archived_at / archived_from_status bookkeeping.
		if st == domain.WorkItemArchived {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				errors.New("archiving must go through ArchiveWorkItem; it is not settable via the generic update path"))
		}
		// "idea" is system-managed (set only by the automation spawn path and Idea Cloud promotion) —
		// routing it through the generic update path would let a caller request it directly.
		if st == domain.WorkItemIdea {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				errors.New("idea is a system-managed status set only by the automation spawn path; it is not settable via the generic update path"))
		}
		fields.Status = strPtr(st)
	}
	if msg.Priority != nil {
		fields.Priority = intPtr(int(*msg.Priority))
	}
	if msg.Budgets != nil {
		budgets, err := ValidateBudgets(*msg.Budgets)
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
	if msg.ParentId != nil {
		pid := *msg.ParentId
		fields.ParentID = &pid
	}
	if msg.ClearScheduledStartAt {
		// An EXPLICIT removal of the schedule, independent of starting.
		fields.ClearScheduledStartAt = true
	} else if msg.ScheduledStartAt != nil {
		t := msg.ScheduledStartAt.AsTime()
		fields.ScheduledStartAt = &t
	}
	if msg.SecretIds != nil {
		if err := ValidateSecretIDs(msg.SecretIds.Ids); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		b, _ := json.Marshal(msg.SecretIds.Ids)
		fields.SecretIDs = &b
	}

	if msg.AutoStartWorkflow != nil {
		v := *msg.AutoStartWorkflow
		fields.AutoStartWorkflow = &v
		if v && msg.ScheduledStartAt == nil {
			fields.ClearScheduledStartAt = true
		}
	}
	// Parse and validate recurring schedule (if provided).
	if msg.RecurringSchedule != nil {
		if IsRecurringScheduleEmpty(msg.RecurringSchedule) {
			fields.ClearRecurringSchedule = true
		} else {
			recurringSchedule, err := ValidateRecurringSchedule(msg.RecurringSchedule)
			if err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
			fields.RecurringSchedule = &recurringSchedule
			nextRunAt := ComputeNextRunAt(msg.RecurringSchedule, time.Now().UTC())
			fields.NextRunAt = nextRunAt
		}
	}
	if msg.RecurringEnabled != nil {
		v := *msg.RecurringEnabled
		fields.RecurringEnabled = &v
	}
	if msg.WorkflowRunId != nil {
		v := *msg.WorkflowRunId
		fields.WorkflowRunID = &v
	}
	if msg.ClearExecutionMode {
		// CLEAR WINS over a simultaneously supplied execution_mode: the explicit removal of the override is
		// the more specific intent.
		fields.ClearExecutionMode = true
	} else if msg.ExecutionMode != nil {
		v := executionModeFromProto(*msg.ExecutionMode)
		if v == nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("execution_mode must be runtime or local"))
		}
		fields.ExecutionMode = v
	}
	// A LOCAL OVERRIDE CLEARS THE RUNTIME IMAGE, because a local run has no container to put it in.
	if msg.ExecutionMode != nil && fields.ExecutionMode != nil && *fields.ExecutionMode == db.ExecutionModeLocal {
		fields.ClearRuntimeImage = true
	}
	if msg.RuntimeImage != nil {
		v := strings.TrimSpace(*msg.RuntimeImage)
		if v == "" && s.runtimeImageFn != nil {
			// Empty = reset to the base image.
			v = s.runtimeImageFn(ctx)
		}
		fields.RuntimeImage = &v
		if fields.ClearRuntimeImage {
			// LOCAL WINS: drop the image field so the clear is not defeated by a value
			// the client merely round-tripped. See the block above.
			fields.RuntimeImage = nil
		}
	}
	if msg.ContextFiles != nil {
		paths := msg.ContextFiles.Files
		if err := contextfiles.Validate(paths); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		filesJSON, err := contextfiles.ToJSON(paths)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.ContextFiles = &filesJSON
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
	// Ideas are system-managed (feature 4.1): the generic update path must never move an idea out of
	// idea state — only PromoteIdea / DismissIdea are sanctioned.
	if IsIdeaStatus(current.Status) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, ErrWorkItemIsIdea())
	}
	// Per-item secret selection: validate dangling refs inside the tx so a
	// concurrent delete cannot race the pre-tx check (fail-closed at write).
	if fields.SecretIDs != nil {
		var ids []string
		_ = json.Unmarshal(*fields.SecretIDs, &ids)
		if len(ids) > 0 {
			m, err := db.BatchGetSecrets(ctx, ttx.Tx, tenantID, ids)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			if len(m) != len(ids) {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("one or more secrets not found"))
			}
		}
	}
	// A currently-recurring item is exempt from the "only epics are top-level" parent rule
	// (flat-recurring items are top-level tasks).
	itemIsRecurring := current.RecurringSchedule != nil
	// An EPHEMERAL item carries the same exemption for the same shape of reason: it is a
	// machine-managed transient that lives in no tree (see ValidateParent).
	itemIsEphemeral := current.Ephemeral
	// Binding a workflow switches the item from one-shot (assigned worker, standalone dispatch) to
	// template-bound: a stale worker assignment from the standalone path would flag the item.
	if msg.WorkflowId != nil && *msg.WorkflowId != "" && len(current.AssignedWorkerRef) > 0 {
		fields.ClearAssignedWorkerRef = true
	}
	// Context files must live inside the effective project's directory — the only path guaranteed to be
	// mounted where workers run.
	if msg.ContextFiles != nil {
		effProject := current.ProjectID
		if fields.ProjectID != nil && *fields.ProjectID != "" {
			effProject = *fields.ProjectID
		}
		if proj, err := db.GetProject(ctx, ttx.Tx, tenantID, effProject); err == nil {
			if err := contextfiles.ValidateWithin(msg.ContextFiles.Files, proj.ProjectDir); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
	}
	// Kind switch (ADR-WIT-1/2): resolve parent/child + schedulability automatically inside the same
	// transaction.
	var kindSwitchPlan *KindSwitchPlan
	if msg.Kind != nil {
		newKind, err := validateKind(*msg.Kind)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if newKind != current.Kind {
			effectiveProject := current.ProjectID
			if fields.ProjectID != nil && *fields.ProjectID != "" {
				effectiveProject = *fields.ProjectID
			}
			if effectiveProject != current.ProjectID {
				// Switching kind AND moving to another project: the auto-resolution walk-up follows the current
				// ancestor chain, which must not cross projects.
				keepCurrent := msg.ParentId != nil && current.ParentID != nil && *msg.ParentId == *current.ParentID
				explicitForTarget := msg.ParentId != nil && !keepCurrent
				if !explicitForTarget {
					carryOK := false
					if keepCurrent {
						ok, err := workItemInProject(ctx, ttx.Tx, tenantID, *current.ParentID, effectiveProject)
						if err != nil {
							return nil, connect.NewError(connect.CodeInternal, err)
						}
						carryOK = ok
					}
					if !carryOK {
						return nil, connect.NewError(connect.CodeInvalidArgument,
							errors.New("when switching kind and project together, choose the new parent explicitly"))
					}
				}
			}
			plan, err := ResolveKindSwitch(ctx, ttx.Tx, tenantID, current, newKind, msg.ParentId, effectiveProject)
			if err != nil {
				return nil, mapKindSwitchError(err)
			}
			kindSwitchPlan = plan
			fields.Kind = &newKind
		}
	}
	// If reassigning to a different project, the target must be active.
	if fields.ProjectID != nil && *fields.ProjectID != current.ProjectID {
		if err := db.RequireProjectActive(ctx, ttx.Tx, tenantID, *fields.ProjectID); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target project not active: %w", err))
		}
		// Dependency edges are project-scoped and the DAG is per-project, so an item that participates in
		// any edge (outgoing or incoming) may not silently move across projects.
		participates, err := db.ItemParticipatesInDependency(ctx, ttx.Tx, tenantID, msg.Id)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if participates {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				errors.New("cannot move a work item to another project while it has dependency edges; remove the edges first"))
		}
	}
	// Reparenting is validated against the *effective* project — the request's project_id if also being
	// changed, otherwise the current one — using the same rules as Create (shared helper).
	if msg.ParentId != nil && kindSwitchPlan == nil {
		effectiveProject := current.ProjectID
		if fields.ProjectID != nil && *fields.ProjectID != "" {
			effectiveProject = *fields.ProjectID
		}
		if err := ValidateParent(ctx, ttx.Tx, tenantID, *msg.ParentId, current.Kind, effectiveProject, itemIsRecurring, itemIsEphemeral); err != nil {
			return nil, mapParentError(err)
		}
	} else if msg.ParentId == nil && fields.ProjectID != nil && *fields.ProjectID != "" && *fields.ProjectID != current.ProjectID && current.ParentID != nil {
		// The item is moving to another project WITHOUT an explicit reparent, so its current parent is
		// carried over.
		if err := ValidateParent(ctx, ttx.Tx, tenantID, *current.ParentID, current.Kind, *fields.ProjectID, itemIsRecurring, itemIsEphemeral); err != nil {
			return nil, mapParentError(err)
		}
	}
	// Apply the kind-switch plan to the update fields (ADR-WIT-2): the
	// resolved parent, the status demotion, and the schedulability clears.
	if kindSwitchPlan != nil {
		if kindSwitchPlan.NewParentID != nil && (current.ParentID == nil || *current.ParentID != *kindSwitchPlan.NewParentID) {
			fields.ParentID = kindSwitchPlan.NewParentID
		} else if kindSwitchPlan.NewParentID == nil && current.ParentID != nil {
			// epic switch: a top-level item has no parent.
			empty := ""
			fields.ParentID = &empty
		}
		if kindSwitchPlan.NewStatus != nil {
			fields.Status = kindSwitchPlan.NewStatus
		}
		if kindSwitchPlan.ClearWorkerRef {
			fields.ClearAssignedWorkerRef = true
		}
		if kindSwitchPlan.ClearScheduledStartAt {
			fields.ClearScheduledStartAt = true
		}
		if kindSwitchPlan.ClearRecurringSchedule {
			fields.ClearRecurringSchedule = true
		}
	}
	// Saving a scheduled start flips the item to "scheduled" (ADR-001 in
	// architecture-notes/running-workflows-not-showing-in-schedules.md).
	if msg.ScheduledStartAt != nil &&
		!IsActiveRunStatus(current.Status) &&
		!(kindSwitchPlan != nil && kindSwitchPlan.ClearScheduledStartAt) {
		fields.Status = strPtr(domain.WorkItemScheduled)
	}
	// Setting a recurring schedule flips the item to "recurring" status.
	if msg.RecurringSchedule != nil &&
		!IsRecurringScheduleEmpty(msg.RecurringSchedule) &&
		!IsActiveRunStatus(current.Status) &&
		!(kindSwitchPlan != nil && kindSwitchPlan.ClearRecurringSchedule) {
		fields.Status = strPtr(domain.WorkItemRecurring)
	}
	// Switching status to anything OTHER than "recurring" clears the recurring schedule and
	// next_run_at.
	if fields.Status != nil && *fields.Status != domain.WorkItemRecurring && current.RecurringSchedule != nil {
		fields.ClearRecurringSchedule = true
	}
	// Final-state invariant: "recurring" is derived state — it is only valid while a recurring_schedule
	// is present.
	schedulePresent := !fields.ClearRecurringSchedule &&
		(fields.RecurringSchedule != nil || current.RecurringSchedule != nil)
	wouldBeRecurring := current.Status == domain.WorkItemRecurring ||
		(fields.Status != nil && *fields.Status == domain.WorkItemRecurring)
	explicitOtherStatus := fields.Status != nil && *fields.Status != domain.WorkItemRecurring
	if wouldBeRecurring && !schedulePresent && !explicitOtherStatus {
		s := domain.WorkItemPending
		fields.Status = &s
	}
	// Resume after a pause: pausing froze next_run_at, so a paused item can hold a stale (past) cursor.
	if msg.RecurringEnabled != nil && *msg.RecurringEnabled &&
		current.RecurringSchedule != nil && fields.RecurringSchedule == nil &&
		!fields.ClearRecurringSchedule {
		fields.NextRunAt = ComputeNextRunAtFromScheduleJSON(current.RecurringSchedule, time.Now().UTC())
	}
	// Schedule-time validation (architecture-notes §3): scheduling or auto-starting runs the subtree
	// validation — a parent WITH children is a sequence (the subtree must be executable.
	effItem := current
	if msg.WorkflowId != nil {
		effItem.WorkflowID = msg.WorkflowId
	}
	// Flat-recurring + workflow-binding result-state checks (D1/D7).
	resultRecurring := !fields.ClearRecurringSchedule &&
		(fields.RecurringSchedule != nil || current.RecurringSchedule != nil)
	if resultRecurring {
		effKind := current.Kind
		if fields.Kind != nil {
			effKind = *fields.Kind
		}
		effParent := ""
		if fields.ParentID != nil {
			effParent = *fields.ParentID
		} else if current.ParentID != nil {
			effParent = *current.ParentID
		}
		if err := ValidateRecurringFlatness(true, effKind, effParent); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		effWorkflow := ""
		if effItem.WorkflowID != nil && *effItem.WorkflowID != "" {
			effWorkflow = *effItem.WorkflowID
		}
		hasChildren := s.itemHasChildren(ctx, tenantID, current.ID)
		if err := ValidateRecurringWorkflowBinding(true, hasChildren, effWorkflow); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
	}
	// Workflow-first enforcement (standalone dispatch is retired): a STATUS TRANSITION into a runnable
	// status (ready / assigned / scheduled / running) is rejected when the resulting item has no.
	if fields.Status != nil && *fields.Status != current.Status {
		effWorkflow := effItem.WorkflowID
		if err := ValidateWorkflowFirstTransition(current.Title, *fields.Status, effWorkflow, s.itemHasChildren(ctx, tenantID, current.ID)); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
	}
	if msg.ScheduledStartAt != nil || (msg.AutoStartWorkflow != nil && *msg.AutoStartWorkflow) {
		if err := s.validateSequenceSchedule(ctx, ttx.Tx, tenantID, effItem); err != nil {
			return nil, err
		}
	}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	// A kind switch must never trigger the post-commit auto-start on its own — the user re-typed the
	// item, they did not ask to start it now (ADR-WIT-2).
	kindSwitchInFlight := kindSwitchPlan != nil
	userExplicitlyAutoStarts := msg.AutoStartWorkflow != nil && *msg.AutoStartWorkflow
	// Kind switch events: work_item.kind_changed for the switched item (the authoritative record of the
	// switch, carrying old_kind + new_kind).
	if kindSwitchPlan != nil {
		if err := enqueueKindChangedEvent(ctx, ttx.Tx, current, updated); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, cr := range kindSwitchPlan.ReparentedChildren {
			childFields := db.UpdateWorkItemFields{ParentID: cr.NewParentID}
			child, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, cr.ChildID, cr.ChildVersion, childFields)
			if err != nil {
				return nil, mapDBError(err)
			}
			if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", child); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		}
	} else if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// depends_on set-replace semantics: the request's list fully replaces the item's outgoing
	// DEPENDS_ON edges (absent = unchanged, empty = clear), mirroring context_files.
	if msg.DependsOn != nil {
		effectiveProject := current.ProjectID
		if fields.ProjectID != nil && *fields.ProjectID != "" {
			effectiveProject = *fields.ProjectID
		}
		if err := s.replaceDependsOn(ctx, ttx.Tx, tenantID, effectiveProject, updated.ID, msg.DependsOn.Ids); err != nil {
			return nil, err
		}
	}
	proto := rowToProto(updated)
	fillEffectiveMode(ctx, ttx.Tx, tenantID, proto, updated)
	if err := attachDependsOn(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedBy(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.updated", "work_item", updated.ID,
		audit.Snapshot(workItemAuditSnapshot(current)), audit.Snapshot(workItemAuditSnapshot(updated))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.updated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	// Any other status (cancelled, failed, succeeded, skipped, recovering, checkpointing, running,
	// recurring, …) is NEVER re-armed by an edit.
	requestNamedStart := userExplicitlyAutoStarts || msg.ScheduledStartAt != nil
	wouldAutoStart := requestNamedStart && updated.ScheduledStartAt == nil && updated.AutoStartWorkflow &&
		!(kindSwitchInFlight && !userExplicitlyAutoStarts)
	autoStartWarning := ""
	if wouldAutoStart {
		effectiveStatus := current.Status
		if fields.Status != nil {
			effectiveStatus = *fields.Status
		}
		// WHO MAY FIRE. A STALE STORED FLAG STILL NEVER FIRES.
		effectiveStartable := IsStartableForAutoStart(effectiveStatus)
		if userExplicitlyAutoStarts || effectiveStartable {
			if s.itemHasChildren(ctx, tenantID, updated.ID) {
				s.maybeStartSequence(ctx, tenantID, updated)
			} else if updated.WorkflowID != nil && *updated.WorkflowID != "" && s.startWorkflowFn != nil {
				// A previous run must be terminal (completed/failed/aborted) —
				// active runs are not duplicated.
				shouldStart := true
				if updated.WorkflowRunID != "" {
					var runStatus string
					if err := s.pool.Pool.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1 AND tenant_id = $2`, updated.WorkflowRunID, tenantID).Scan(&runStatus); err == nil {
						if runStatus != domain.WorkflowRunCompleted && runStatus != domain.WorkflowRunFailed && runStatus != domain.WorkflowRunAborted {
							shouldStart = false
						}
					}
				}
				if shouldStart {
					if err := s.startWorkflowFn(ctx, tenantID, *updated.WorkflowID, updated.ProjectID, updated.ID); err != nil {
						s.log.Warn("auto-start workflow after update failed", "work_item", updated.ID, "error", err)
					}
				}
			}
		} else if userExplicitlyAutoStarts {
			autoStartWarning = AutoStartDeclinedWarning(effectiveStatus)
		} else {
			s.log.Info("auto-start declined: item not in a startable status",
				"work_item", updated.ID, "status", effectiveStatus)
		}
	}
	s.log.Info("work item updated", "id", updated.ID, "version", updated.Version)
	// Context files changed → refresh the container mount manifest now (the work_item.context_files
	// paths are mounted into the single-container instance.
	if msg.ContextFiles != nil {
		project.NotifyProjectChanged()
	}
	return connect.NewResponse(&apiv1.UpdateWorkItemResponse{WorkItem: proto, Warning: autoStartWarning}), nil
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
	// An idea must be dismissed via DismissIdea (cancelled + audited as
	// work_item.dismissed), not soft-deleted through the generic path.
	if IsIdeaStatus(current.Status) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, ErrWorkItemIsIdea())
	}
	fields := db.UpdateWorkItemFields{Status: strPtr(domain.WorkItemCancelled)}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.deleted", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.deleted", "work_item", updated.ID,
		audit.Snapshot(workItemAuditSnapshot(current)), audit.SnapshotStatus(updated.Status)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.deleted: %w", err))
	}
	proto := rowToProto(updated)
	fillEffectiveMode(ctx, ttx.Tx, tenantID, proto, updated)
	if err := attachDependsOn(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedBy(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item deleted (cancelled)", "id", updated.ID)
	return connect.NewResponse(&apiv1.DeleteWorkItemResponse{WorkItem: proto}), nil
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
	// Hard-deleting an idea would destroy its provenance before triage; an
	// idea must be dismissed via DismissIdea (which retains provenance).
	if IsIdeaStatus(current.Status) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, ErrWorkItemIsIdea())
	}
	if err := db.HardDeleteWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.purged", current); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.hard_deleted", "work_item", current.ID,
		audit.Snapshot(workItemAuditSnapshot(current)), nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.hard_deleted: %w", err))
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

	// Cycle check: would adding from→to create a cycle?
	if depType != domain.DependencyRelatesTo {
		createsCycle, err := db.CheckCycleWithRecursiveCTE(ctx, ttx.Tx, tenantID, msg.ProjectId, msg.FromId, msg.ToId)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if createsCycle {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("cannot add dependency %s -> %s: would create a cycle in the work DAG", msg.FromId, msg.ToId))
		}
	}

	dep := db.DependencyRow{
		ID:        db.NewID(),
		TenantID:  tenantID,
		ProjectID: msg.ProjectId,
		FromID:    msg.FromId,
		ToID:      msg.ToId,
		Type:      depType,
	}
	created, err := db.CreateDependency(ctx, ttx.Tx, dep)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueDependencyEvent(ctx, ttx.Tx, "work_item.dependency_added", created); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.dependency_added", "work_item", created.FromID,
		nil, audit.Snapshot(map[string]any{"from_id": created.FromID, "to_id": created.ToID, "type": created.Type})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.dependency_added: %w", err))
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
	current, err := db.GetDependency(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := db.DeleteDependency(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.dependency_removed", "work_item", current.FromID,
		audit.Snapshot(map[string]any{"from_id": current.FromID, "to_id": current.ToID, "type": current.Type}), nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.dependency_removed: %w", err))
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
	if err := attachDependsOnBatch(ctx, ttx.Tx, tenantID, graph.Nodes); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedByBatch(ctx, ttx.Tx, tenantID, graph.Nodes); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
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
	workerRef, err := ValidateWorkerRef(req.Msg.WorkerRef)
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.worker_assigned", "work_item", updated.ID,
		audit.Snapshot(workItemAuditSnapshot(current)), audit.Snapshot(workItemAuditSnapshot(updated))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.worker_assigned: %w", err))
	}
	proto := rowToProto(updated)
	fillEffectiveMode(ctx, ttx.Tx, tenantID, proto, updated)
	if err := attachDependsOn(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedBy(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker assigned to work item", "id", updated.ID)
	return connect.NewResponse(&apiv1.AssignWorkerResponse{WorkItem: proto}), nil
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
	// Clear assigned_worker_ref with a status CAS, bumping the version (docs/09 §5).
	const q = `UPDATE work_items
		SET assigned_worker_ref = NULL, updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING ` + db.WorkItemSelectCols
	var updated db.WorkItemRow
	err = ttx.Tx.QueryRow(ctx, q, tenantID, req.Msg.Id, current.Version).Scan(
		db.WorkItemScanPtrs(&updated)...,
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.worker_unassigned", "work_item", updated.ID,
		audit.Snapshot(workItemAuditSnapshot(current)), audit.Snapshot(workItemAuditSnapshot(updated))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.worker_unassigned: %w", err))
	}
	proto := rowToProto(updated)
	fillEffectiveMode(ctx, ttx.Tx, tenantID, proto, updated)
	if err := attachDependsOn(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedBy(ctx, ttx.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker unassigned from work item", "id", updated.ID)
	return connect.NewResponse(&apiv1.UnassignWorkerResponse{WorkItem: proto}), nil
}

// ReorderWorkItems renumbers sort_order for the siblings under parent_id (empty = top level) to the
// given order, in one transaction.
func (s *Service) ReorderWorkItems(ctx context.Context, req *connect.Request[apiv1.ReorderWorkItemsRequest]) (*connect.Response[apiv1.ReorderWorkItemsResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	if len(msg.ChildIds) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("child_ids must not be empty"))
	}
	seen := make(map[string]bool, len(msg.ChildIds))
	for _, id := range msg.ChildIds {
		if id == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("child_ids must not contain empty ids"))
		}
		if seen[id] {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("duplicate child id %q", id))
		}
		seen[id] = true
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// The parent must exist (or be top level) and live in the project.
	if msg.ParentId != "" {
		parent, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, msg.ParentId)
		if err != nil {
			return nil, mapDBError(err)
		}
		if parent.ProjectID != msg.ProjectId {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("parent_id work item is not in the specified project"))
		}
	}
	// Load the current sibling set (sort_order NULLS LAST, created_at).
	siblings, err := db.ListSiblingsForReorder(ctx, ttx.Tx, tenantID, msg.ProjectId, msg.ParentId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if len(siblings) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("no siblings under the given parent"))
	}
	byID := make(map[string]db.WorkItemRow, len(siblings))
	for _, sib := range siblings {
		byID[sib.ID] = sib
	}
	// Every requested child must be a direct child in the same project.
	for _, id := range msg.ChildIds {
		if _, ok := byID[id]; !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("child %q is not a direct child of the given parent in this project", id))
		}
	}
	// New order = requested order first, then any unlisted siblings keep
	// their relative position (a partial drag appends the rest).
	ordered := make([]db.WorkItemRow, 0, len(siblings))
	placed := make(map[string]bool, len(siblings))
	for _, id := range msg.ChildIds {
		ordered = append(ordered, byID[id])
		placed[id] = true
	}
	for _, sib := range siblings {
		if !placed[sib.ID] {
			ordered = append(ordered, sib)
		}
	}
	// Renumber 1..N in one transaction, emitting an outbox event per child.
	for i, sib := range ordered {
		order := float64(i + 1)
		updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, sib.ID, sib.Version, db.UpdateWorkItemFields{
			SortOrder: &order,
		})
		if err != nil {
			return nil, mapDBError(err)
		}
		if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		ordered[i] = updated
	}
	ids := make([]string, len(ordered))
	for i, sib := range ordered {
		ids[i] = sib.ID
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.reordered", "work_item", msg.ParentId,
		nil, audit.Snapshot(map[string]any{"parent_id": msg.ParentId, "child_ids": ids})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.reordered: %w", err))
	}
	resp := &apiv1.ReorderWorkItemsResponse{}
	for _, sib := range ordered {
		resp.WorkItems = append(resp.WorkItems, rowToProto(sib))
	}
	if err := attachDependsOnBatch(ctx, ttx.Tx, tenantID, resp.WorkItems); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedByBatch(ctx, ttx.Tx, tenantID, resp.WorkItems); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(resp), nil
}

// ArchiveWorkItem hides a terminal work item from every normal view.
func (s *Service) ArchiveWorkItem(ctx context.Context, req *connect.Request[apiv1.ArchiveWorkItemRequest]) (*connect.Response[apiv1.ArchiveWorkItemResponse], error) {
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
	if err := validateArchivePreconditions(ctx, ttx.Tx, tenantID, current); err != nil {
		return nil, err
	}
	if s.archiveConflictHook != nil {
		s.archiveConflictHook(1)
	}

	archived, err := db.ArchiveWorkItem(ctx, ttx.Tx, tenantID, current.ID, current.Version, current.Status)
	if errors.Is(err, db.ErrVersionConflict) {
		// The sequence engine (or any concurrent writer) can bump a parent's version between our read and
		// this write — status transitions and SequenceLastProgressAt updates happen behind the UI's back.
		current, err = db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
		if err != nil {
			return nil, mapDBError(err)
		}
		if err := validateArchivePreconditions(ctx, ttx.Tx, tenantID, current); err != nil {
			return nil, err
		}
		if s.archiveConflictHook != nil {
			s.archiveConflictHook(2)
		}
		archived, err = db.ArchiveWorkItem(ctx, ttx.Tx, tenantID, current.ID, current.Version, current.Status)
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.archived", archived); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.archived", "work_item", archived.ID,
		audit.Snapshot(workItemAuditSnapshot(current)), audit.Snapshot(workItemAuditSnapshot(archived))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.archived: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item archived", "id", archived.ID)
	return connect.NewResponse(&apiv1.ArchiveWorkItemResponse{WorkItem: rowToProto(archived)}), nil
}

// validateArchivePreconditions enforces the full archive precondition set (not an idea,
// terminal-archivable status, no non-archived children) against the given row.
func validateArchivePreconditions(ctx context.Context, tx pgx.Tx, tenantID string, current db.WorkItemRow) error {
	// An idea is never terminal-archivable; route it through DismissIdea so
	// the dismissal is audited as work_item.dismissed rather than archived.
	if IsIdeaStatus(current.Status) {
		return connect.NewError(connect.CodeFailedPrecondition, ErrWorkItemIsIdea())
	}
	if !domain.WorkItemIsTerminalArchivable(current.Status) {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("work item must be in a terminal state (succeeded, failed, cancelled, or skipped) to be archived; finish or cancel it first"))
	}
	children, err := db.ListDirectChildren(ctx, tx, tenantID, current.ID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if len(children) > 0 {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("cannot archive a work item that has %d child work item(s); archive the children first", len(children)))
	}
	return nil
}

// RestoreWorkItem returns an archived work item to the active views, back to the terminal status it
// was archived from (archived_from_status, not pending).
func (s *Service) RestoreWorkItem(ctx context.Context, req *connect.Request[apiv1.RestoreWorkItemRequest]) (*connect.Response[apiv1.RestoreWorkItemResponse], error) {
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
	if current.Status != domain.WorkItemArchived {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("work item is not archived; only archived work items can be restored"))
	}
	fromStatus := current.ArchivedFromStatus
	if fromStatus == nil || *fromStatus == "" {
		// Defensive fallback: an archived item should always carry its prior status; default to cancelled
		// (a terminal, non-dispatchable state) rather than pending so a restored item never becomes.
		fromStatus = strPtr(domain.WorkItemCancelled)
	}
	restored, err := db.RestoreWorkItem(ctx, ttx.Tx, tenantID, current.ID, current.Version, *fromStatus)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.restored", restored); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.restored", "work_item", restored.ID,
		audit.Snapshot(workItemAuditSnapshot(current)), audit.Snapshot(workItemAuditSnapshot(restored))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.restored: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item restored", "id", restored.ID)
	return connect.NewResponse(&apiv1.RestoreWorkItemResponse{WorkItem: rowToProto(restored)}), nil
}

// ControlSequence drives a sequence parent manually (START / RESUME /
// STOP). A parent with children IS a sequence run; these explicit gestures
// are what the engine's derived cursor cannot infer on its own:
//   - START re-fires the chain from child #1 (destructive — every
//     descendant resets to pending); validations + in-flight guards run
//     server-side.
//   - RESUME continues from the first non-succeeded child (keeps state).
//   - STOP parks the chain (parent → pending, schedule cleared) so
//     children can be run standalone.
//
// All three actions require a work item that IS a sequence parent: it must
// have at least one direct child and carry no bound workflow run (a parent
// with children and a bound run is a run ticket, not a sequence).
func (s *Service) ControlSequence(ctx context.Context, req *connect.Request[apiv1.ControlSequenceRequest]) (*connect.Response[apiv1.ControlSequenceResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	switch msg.Action {
	case apiv1.SequenceAction_SEQUENCE_ACTION_START,
		apiv1.SequenceAction_SEQUENCE_ACTION_RESUME,
		apiv1.SequenceAction_SEQUENCE_ACTION_STOP:
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("action must be one of START, RESUME, STOP"))
	}

	// Load the item and determine whether it is a sequence parent (has
	// children) or a leaf (bound to a workflow run / standalone). The
	// engine's reconcileParent guard uses the same predicate, so this can
	// never drift. Controls work on BOTH kinds:
	//   - parent (has children): START/RESUME re-fire/continue the chain;
	//     STOP cascades — parks the whole subtree and aborts in-flight runs.
	//   - leaf (no children): START/RESUME fire the item's own bound
	//     workflow; STOP parks just the leaf and aborts its run.
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	isParent := len(children) > 0

	switch msg.Action {
	case apiv1.SequenceAction_SEQUENCE_ACTION_START:
		if isParent {
			// START is destructive (wipes prior child successes).
			if IsActiveRunStatus(current.Status) {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					errors.New("cannot START a sequence that is already running — STOP it first"))
			}
			// Subtree validation (workflows bound, no one-shots) before the
			// destructive fire, mirroring the schedule-time path.
			if err := ValidateSequenceSchedule(ctx, ttx.Tx, tenantID, current); err != nil {
				return nil, err
			}
			if s.sequenceStartFn == nil {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("sequence starter not wired"))
			}
			if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.sequence_started", "work_item", current.ID,
				audit.SnapshotStatus(current.Status), nil); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.sequence_started: %w", err))
			}
			if err := ttx.Commit(ctx); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
			}
			if err := s.sequenceStartFn(ctx, tenantID, msg.Id); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		} else {
			// Leaf START: fire the item's own bound workflow immediately.
			if current.WorkflowID == nil || *current.WorkflowID == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					errors.New("cannot START a leaf work item with no workflow bound"))
			}
			if current.WorkflowRunID != "" || IsActiveRunStatus(current.Status) ||
				current.Status == domain.WorkItemReady || current.Status == domain.WorkItemAssigned {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					errors.New("cannot START a work item that is already running or queued for dispatch — STOP it first"))
			}
			if s.startWorkflowFn == nil {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("workflow starter not wired"))
			}
			if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.started", "work_item", current.ID,
				audit.SnapshotStatus(current.Status), nil); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.started: %w", err))
			}
			if err := ttx.Commit(ctx); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
			}
			if err := s.startWorkflowFn(ctx, tenantID, *current.WorkflowID, current.ProjectID, msg.Id); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		}
	case apiv1.SequenceAction_SEQUENCE_ACTION_RESUME:
		if isParent {
			// RESUME is enabled when the chain is halted (parent failed) or parked (parent pending with
			// children).
			if current.Status != domain.WorkItemFailed && current.Status != domain.WorkItemPending {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					errors.New("cannot RESUME a sequence that is not halted (failed) or parked (pending)"))
			}
			// The subtree must still be schedulable (a failed child's workflow
			// may have been unbound); validate before re-arming.
			if err := ValidateSequenceSchedule(ctx, ttx.Tx, tenantID, current); err != nil {
				return nil, err
			}
			if s.sequenceResumeFn == nil {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("sequence resume not wired"))
			}
			if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.sequence_resumed", "work_item", current.ID,
				audit.SnapshotStatus(current.Status), nil); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.sequence_resumed: %w", err))
			}
			if err := ttx.Commit(ctx); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
			}
			if err := s.sequenceResumeFn(ctx, tenantID, msg.Id); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		} else {
			// Leaf RESUME: re-arm a halted/parked leaf — reset to pending and
			// fire its bound workflow.
			if current.WorkflowID == nil || *current.WorkflowID == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					errors.New("cannot RESUME a leaf work item with no workflow bound"))
			}
			if current.WorkflowRunID != "" || IsActiveRunStatus(current.Status) ||
				current.Status == domain.WorkItemReady || current.Status == domain.WorkItemAssigned {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					errors.New("cannot RESUME a work item that is already running or queued for dispatch"))
			}
			if s.startWorkflowFn == nil {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("workflow starter not wired"))
			}
			if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.resumed", "work_item", current.ID,
				audit.SnapshotStatus(current.Status), nil); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.resumed: %w", err))
			}
			if err := ttx.Commit(ctx); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
			}
			if err := s.startWorkflowFn(ctx, tenantID, *current.WorkflowID, current.ProjectID, msg.Id); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		}
	case apiv1.SequenceAction_SEQUENCE_ACTION_STOP:
		// STOP halts the item: a parent parks its whole subtree (every descendant → pending, in-flight runs
		// aborted), a leaf parks just itself and aborts its run.
		if current.Status == domain.WorkItemSucceeded {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				errors.New("cannot STOP a work item that has already succeeded"))
		}
		if s.sequenceStopFn == nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("sequence stop not wired"))
		}
		if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.stopped", "work_item", current.ID,
			audit.SnapshotStatus(current.Status), nil); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.stopped: %w", err))
		}
		if err := ttx.Commit(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
		}
		if err := s.sequenceStopFn(ctx, tenantID, msg.Id); err != nil {
			if errors.Is(err, db.ErrVersionConflict) {
				return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("version conflict: %w", err))
			}
			if errors.Is(err, db.ErrNotFound) {
				return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	// Re-read the parent after the action and return server-confirmed state.
	ttx2, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx2.Rollback(ctx)
	updated, err := db.GetWorkItem(ctx, ttx2.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	proto := rowToProto(updated)
	if err := attachDependsOn(ctx, ttx2.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := attachBlockedBy(ctx, ttx2.Tx, tenantID, proto); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.ControlSequenceResponse{WorkItem: proto}), nil
}

// --- helpers ---------------------------------------------------------------

// recordAudit writes an actor-based audit row in the caller's tx, resolving the actor from the
// request context.
func recordAudit(ctx context.Context, tx pgx.Tx, tenantID, action, targetType, targetID string, before, after json.RawMessage) error {
	e := auth.ActorFromContext(ctx)
	if e.TenantID == "" {
		e.TenantID = tenantID
	}
	e.Action = action
	e.TargetType = targetType
	e.TargetID = targetID
	e.Before = before
	e.After = after
	return audit.Record(ctx, tx, e)
}

// workItemAuditSnapshot is the non-secret projection of a work item row
// for the audit trail (no credentials or run artifacts).
func workItemAuditSnapshot(w db.WorkItemRow) map[string]any {
	return map[string]any{
		"id":         w.ID,
		"project_id": w.ProjectID,
		"kind":       w.Kind,
		"title":      w.Title,
		"status":     w.Status,
		"priority":   w.Priority,
		"version":    w.Version,
	}
}

// EnqueueWorkItemEvent emits a work item outbox row (work_item.created / work_item.updated /
// work_item.worker_assigned / ...) in the caller's transaction.
func EnqueueWorkItemEvent(ctx context.Context, tx pgx.Tx, eventType string, w db.WorkItemRow) error {
	return enqueueWorkItemEvent(ctx, tx, eventType, w)
}

// EnqueueKindChangedEvent emits work_item.kind_changed — the authoritative record of a kind switch.
func EnqueueKindChangedEvent(ctx context.Context, tx pgx.Tx, before, after db.WorkItemRow) error {
	return enqueueKindChangedEvent(ctx, tx, before, after)
}

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
		OccurredAt:    time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

// enqueueKindChangedEvent emits work_item.kind_changed — the authoritative record of a kind switch
// (ADR-WIT-2).
func enqueueKindChangedEvent(ctx context.Context, tx pgx.Tx, before, after db.WorkItemRow) error {
	payload, err := buildKindChangedEventPayload(before, after)
	if err != nil {
		return err
	}
	row := db.OutboxRow{
		TenantID:      after.TenantID,
		EventType:     "work_item.kind_changed",
		AggregateType: "work_item",
		AggregateID:   after.ID,
		AggregateVer:  after.Version,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

func enqueueDependencyEvent(ctx context.Context, tx pgx.Tx, eventType string, d db.DependencyRow) error {
	evt := map[string]any{
		"event_type":     eventType,
		"tenant_id":      d.TenantID,
		"project_id":     d.ProjectID,
		"aggregate_type": "work_item_dependency",
		"aggregate_id":   d.ID,
		"dependency_id":  d.ID,
		"from_id":        d.FromID,
		"to_id":          d.ToID,
		"type":           d.Type,
		"occurred_at":    time.Now().UTC().Format(time.RFC3339Nano),
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

// validateDependsOn dedupes and validates a depends_on ID list against the effective project,
// mirroring the AddDependency admission rules: targets must exist (NotFound).
func validateDependsOn(ctx context.Context, tx pgx.Tx, tenantID, projectID, itemID string, depIDs []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, id := range depIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("depends_on ids must not be blank"))
		}
		if id == itemID {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cannot create a self-dependency"))
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		item, err := db.GetWorkItem(ctx, tx, tenantID, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("depends_on target %q not found", id))
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if item.ProjectID != projectID {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("depends_on target %q is not in the same project", id))
		}
		out = append(out, id)
	}
	return out, nil
}

// replaceDependsOn applies set-replace semantics to an item's outgoing DEPENDS_ON edges inside the
// caller's transaction: it deletes the current outgoing edge set.
func (s *Service) replaceDependsOn(ctx context.Context, tx pgx.Tx, tenantID, projectID, itemID string, depIDs []string) error {
	current, err := db.ListDependenciesForItems(ctx, tx, tenantID, []string{itemID}, domain.DependencyDependsOn)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	desired, err := validateDependsOn(ctx, tx, tenantID, projectID, itemID, depIDs)
	if err != nil {
		return err
	}
	// Delete the whole outgoing depends_on set first, then re-add the desired set — the recursive-CTE
	// cycle check then reads the live intermediate state (design decision.
	if err := db.DeleteOutgoingDependencies(ctx, tx, tenantID, itemID, domain.DependencyDependsOn); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	had := map[string]bool{}
	for _, d := range current {
		had[d.ToID] = true
	}
	var removed []db.DependencyRow
	for _, d := range current {
		if !stringInSet(d.ToID, desired) {
			removed = append(removed, d)
		}
	}
	for _, depID := range desired {
		createsCycle, err := db.CheckCycleWithRecursiveCTE(ctx, tx, tenantID, projectID, itemID, depID)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		if createsCycle {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("cannot add dependency %s -> %s: would create a cycle in the work DAG", itemID, depID))
		}
		created, err := db.CreateDependency(ctx, tx, db.DependencyRow{
			ID:        db.NewID(),
			TenantID:  tenantID,
			ProjectID: projectID,
			FromID:    itemID,
			ToID:      depID,
			Type:      domain.DependencyDependsOn,
		})
		if err != nil {
			return mapDBError(err)
		}
		// Emit events + audit only for the diff: an edge that already
		// existed before this request is recreated silently (same set).
		if !had[depID] {
			if err := enqueueDependencyEvent(ctx, tx, "work_item.dependency_added", created); err != nil {
				return connect.NewError(connect.CodeInternal, err)
			}
			if err := recordAudit(ctx, tx, tenantID, "work_item.dependency_added", "work_item", created.FromID,
				nil, audit.Snapshot(map[string]any{"from_id": created.FromID, "to_id": created.ToID, "type": created.Type})); err != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.dependency_added: %w", err))
			}
		}
	}
	for _, d := range removed {
		if err := recordAudit(ctx, tx, tenantID, "work_item.dependency_removed", "work_item", d.FromID,
			audit.Snapshot(map[string]any{"from_id": d.FromID, "to_id": d.ToID, "type": d.Type}), nil); err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("audit work_item.dependency_removed: %w", err))
		}
		if err := enqueueDependencyEvent(ctx, tx, "work_item.dependency_removed", d); err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
	}
	return nil
}

// stringInSet reports whether s is present in ids.
func stringInSet(s string, ids []string) bool {
	for _, id := range ids {
		if id == s {
			return true
		}
	}
	return false
}

// attachDependsOn populates proto.DependsOn (the item's outgoing DEPENDS_ON target IDs) with one
// extra query, inside the caller's transaction.
func attachDependsOn(ctx context.Context, tx pgx.Tx, tenantID string, p *apiv1.WorkItem) error {
	if p == nil {
		return nil
	}
	deps, err := db.ListDependenciesForItems(ctx, tx, tenantID, []string{p.Id}, domain.DependencyDependsOn)
	if err != nil {
		return err
	}
	for _, d := range deps {
		p.DependsOn = append(p.DependsOn, d.ToID)
	}
	return nil
}

// attachDependsOnBatch populates DependsOn for a whole page of work items
// with ONE query (from_id = ANY(...)) to avoid an N+1 per row.
func attachDependsOnBatch(ctx context.Context, tx pgx.Tx, tenantID string, items []*apiv1.WorkItem) error {
	ids := make([]string, 0, len(items))
	byID := map[string]*apiv1.WorkItem{}
	for _, it := range items {
		if it == nil {
			continue
		}
		ids = append(ids, it.Id)
		byID[it.Id] = it
	}
	if len(ids) == 0 {
		return nil
	}
	deps, err := db.ListDependenciesForItems(ctx, tx, tenantID, ids, domain.DependencyDependsOn)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if it, ok := byID[d.FromID]; ok {
			it.DependsOn = append(it.DependsOn, d.ToID)
		}
	}
	return nil
}

// attachBlockedBy populates proto.BlockedBy (the sources of the item's unsatisfied incoming
// blocks/depends_on edges) with one extra query, inside the caller's transaction.
func attachBlockedBy(ctx context.Context, tx pgx.Tx, tenantID string, p *apiv1.WorkItem) error {
	if p == nil {
		return nil
	}
	blockers, err := db.ListUnsatisfiedDependencies(ctx, tx, tenantID, []string{p.Id})
	if err != nil {
		return err
	}
	for _, b := range blockers {
		p.BlockedBy = append(p.BlockedBy, &apiv1.WorkItemBlocker{Id: b.ID, Title: b.Title, Status: b.Status})
	}
	return nil
}

// attachBlockedByBatch populates BlockedBy for a whole page of work items
// with ONE query (to_id = ANY(...)) to avoid an N+1 per row.
func attachBlockedByBatch(ctx context.Context, tx pgx.Tx, tenantID string, items []*apiv1.WorkItem) error {
	ids := make([]string, 0, len(items))
	byID := map[string]*apiv1.WorkItem{}
	for _, it := range items {
		if it == nil {
			continue
		}
		ids = append(ids, it.Id)
		byID[it.Id] = it
	}
	if len(ids) == 0 {
		return nil
	}
	blockers, err := db.ListUnsatisfiedDependencies(ctx, tx, tenantID, ids)
	if err != nil {
		return err
	}
	for _, b := range blockers {
		// ListUnsatisfiedDependencies returns rows keyed by the DEPENDENT (ToID) — the blocker is the
		// edge's from_id.
		if it, ok := byID[b.ToID]; ok {
			it.BlockedBy = append(it.BlockedBy, &apiv1.WorkItemBlocker{Id: b.ID, Title: b.Title, Status: b.Status})
		}
	}
	return nil
}

// attachSpawnedByTitles populates proto.SpawnedByTitle — the title of the spawning recurring item
// (the "from automation X" badge) — with ONE batched query for the rows that carry provenance.
func attachSpawnedByTitlesBatch(ctx context.Context, tx pgx.Tx, tenantID string, items []*apiv1.WorkItem) error {
	ids := make([]string, 0, len(items))
	// One spawner commonly produces SEVERAL items (an automation that spawned two ideas, or a rejected
	// idea plus the same-run promoted one), so the index must hold a SLICE per provenance id.
	byID := map[string][]*apiv1.WorkItem{}
	for _, it := range items {
		if it == nil || it.SpawnedBy == "" {
			continue
		}
		ids = append(ids, it.SpawnedBy)
		byID[it.SpawnedBy] = append(byID[it.SpawnedBy], it)
	}
	if len(ids) == 0 {
		return nil
	}
	titles, err := db.SpawnedByTitles(ctx, tx, tenantID, ids)
	if err != nil {
		return err
	}
	for id, title := range titles {
		for _, it := range byID[id] {
			it.SpawnedByTitle = title
		}
	}
	return nil
}

// attachSpawnedByTitles populates proto.SpawnedByTitle for a single work item (the "from automation
// X" badge).
func attachSpawnedByTitles(ctx context.Context, tx pgx.Tx, tenantID string, p *apiv1.WorkItem) error {
	if p == nil || p.SpawnedBy == "" {
		return nil
	}
	titles, err := db.SpawnedByTitles(ctx, tx, tenantID, []string{p.SpawnedBy})
	if err != nil {
		return err
	}
	if t, ok := titles[p.SpawnedBy]; ok {
		p.SpawnedByTitle = t
	}
	return nil
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
		"parent_id":         parentIDString(w.ParentID),
		"occurred_at":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal work item event payload: %w", err)
	}
	return b, nil
}

// buildKindChangedEventPayload builds the payload for work_item.kind_changed,
// carrying old_kind + new_kind alongside the standard aggregate fields.
func buildKindChangedEventPayload(before, after db.WorkItemRow) ([]byte, error) {
	base := map[string]any{
		"event_type":        "work_item.kind_changed",
		"tenant_id":         after.TenantID,
		"project_id":        after.ProjectID,
		"work_item_id":      after.ID,
		"aggregate_type":    "work_item",
		"aggregate_id":      after.ID,
		"aggregate_version": after.Version,
		"kind":              after.Kind,
		"old_kind":          before.Kind,
		"new_kind":          after.Kind,
		"title":             after.Title,
		"status":            after.Status,
		"parent_id":         parentIDString(after.ParentID),
		"occurred_at":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("marshal kind-changed event payload: %w", err)
	}
	return b, nil
}

// parentIDString returns the parent id or "" when the item is top-level.
func parentIDString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// WorkItemInProject reports whether the work item lives in the given project.
func WorkItemInProject(ctx context.Context, tx pgx.Tx, tenantID, id, projectID string) (bool, error) {
	return workItemInProject(ctx, tx, tenantID, id, projectID)
}

func workItemInProject(ctx context.Context, tx pgx.Tx, tenantID, id, projectID string) (bool, error) {
	var p string
	err := tx.QueryRow(ctx, `SELECT project_id FROM work_items WHERE id = $1 AND tenant_id = $2`, id, tenantID).Scan(&p)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: read work item project: %w", err)
	}
	return p == projectID, nil
}

// mapDBError maps a DB error to a Connect error: a missing row is NotFound; a cycle rejection from
// the enforce_work_dag_acyclic trigger (raised by the concurrent-write serialization path.
func mapDBError(err error) error {
	if errors.Is(err, db.ErrVersionConflict) {
		return connect.NewError(connect.CodeAborted, fmt.Errorf("version conflict: %w", err))
	}
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	if isDependencyCycleError(err) {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// isDependencyCycleError reports whether err is the enforce_work_dag_acyclic trigger's cycle
// rejection.
func isDependencyCycleError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return strings.Contains(pgErr.Message, "would create a cycle in the work DAG")
}

// mapParentError maps a ValidateParent error to a Connect error: a missing parent is NotFound;
// every hierarchy violation is InvalidArgument.
func mapParentError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("parent work item not found"))
	}
	return connect.NewError(connect.CodeInvalidArgument, err)
}

// mapKindSwitchError maps a ResolveKindSwitch error to a Connect error: the system-managed
// preconditions (running item / active run) are FailedPrecondition; a missing parent is NotFound.
func mapKindSwitchError(err error) error {
	switch {
	case errors.Is(err, ErrKindSwitchRunning), errors.Is(err, ErrKindSwitchActiveRun):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, db.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("parent work item not found"))
	default:
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
}

// validateSequenceSchedule runs the schedule-time subtree validation when the request schedules or
// auto-starts a parent work item that has children and no bound workflow (a sequence).
func (s *Service) validateSequenceSchedule(ctx context.Context, tx pgx.Tx, tenantID string, item db.WorkItemRow) error {
	return ValidateSequenceSchedule(ctx, tx, tenantID, item)
}

// ValidateSequenceSchedule is the shared schedule-time validation used by the Connect Update path
// and the Ask Orchicon schedule/update tools so the two surfaces cannot drift (AGENTS.md.
func ValidateSequenceSchedule(ctx context.Context, tx pgx.Tx, tenantID string, item db.WorkItemRow) error {
	// "Has children" is the sequence determinant: a parent with children IS a sequence run (the ADR's
	// parent-is-the-container model).
	children, err := db.ListDirectChildren(ctx, tx, tenantID, item.ID)
	if err != nil {
		return mapDBError(err)
	}
	if len(children) == 0 {
		// Leaf — not a sequence.
		if item.WorkflowID == nil || *item.WorkflowID == "" {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("Cannot schedule %q: no workflow is set, so there is nothing to run.", item.Title))
		}
		return nil // a workflow-bound leaf is a normal bound run
	}
	noWorkflow, badWorkflow, err := ValidateSequenceSubtree(ctx, tx, tenantID, item)
	if err != nil {
		return mapDBError(err)
	}
	if len(noWorkflow) == 0 && len(badWorkflow) == 0 {
		return nil
	}
	return connect.NewError(connect.CodeInvalidArgument,
		BuildSequenceValidationError(item.Title, noWorkflow, badWorkflow))
}

// maybeStartSequence starts a sequence run for a parent with children after the mutation commits
// (auto-start / run-instant path).
func (s *Service) maybeStartSequence(ctx context.Context, tenantID string, item db.WorkItemRow) {
	if s.sequenceStartFn == nil {
		return
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, item.ID)
	ttx.Rollback(ctx)
	if err != nil || len(children) == 0 {
		return
	}
	if err := s.sequenceStartFn(ctx, tenantID, item.ID); err != nil {
		s.log.Warn("auto-start sequence failed", "parent", item.ID, "error", err)
	}
}

// itemHasChildren reports whether the work item has any direct children — the "has children =
// sequence parent" determinant used to route the auto-start fire path.
func (s *Service) itemHasChildren(ctx context.Context, tenantID, itemID string) bool {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return false
	}
	defer ttx.Rollback(ctx)
	children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, itemID)
	return err == nil && len(children) > 0
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
	case domain.WorkItemScheduled:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED
	case domain.WorkItemRecurring:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECURRING
	case domain.WorkItemBlocked:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_BLOCKED
	case domain.WorkItemSkipped:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_SKIPPED
	case domain.WorkItemArchived:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED
	case domain.WorkItemIdea:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA
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

// fillEffectiveMode populates the read-only effective_execution_mode — the answer to "where will
// this item actually run?" — by calling the ONE resolver (db.EffectiveExecutionMode.
func fillEffectiveMode(ctx context.Context, tx pgx.Tx, tenantID string, p *apiv1.WorkItem, w db.WorkItemRow) {
	if p == nil {
		return
	}
	p.EffectiveExecutionMode = executionModeEnumFor(db.EffectiveExecutionMode(ctx, tx, tenantID, w.ExecutionMode, w.ProjectID))
}

func rowToProto(w db.WorkItemRow) *apiv1.WorkItem {
	p := &apiv1.WorkItem{
		Id:                 w.ID,
		TenantId:           w.TenantID,
		ProjectId:          w.ProjectID,
		Kind:               kindToProto(w.Kind),
		Title:              w.Title,
		Description:        w.Description,
		AcceptanceCriteria: w.AcceptanceCriteria,
		AcceptanceReview:   w.AcceptanceReview,
		Status:             statusToProto(w.Status),
		Priority:           int32(w.Priority),
		Budgets:            string(w.Budgets),
		ContextWindow:      int32(w.ContextWindow),
		Results:            string(w.Results),
		ContextFiles:       contextFilesFromJSONOrEmpty(w.ContextFiles),
		// PR B (context propagation): carries the composite prompt.
		PromptContext: extractCompositePrompt(w.PromptContext),
		Version:       int32(w.Version),
		CreatedAt:     timestamppb.New(w.CreatedAt),
		UpdatedAt:     timestamppb.New(w.UpdatedAt),
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
	p.WorkflowRunId = w.WorkflowRunID
	p.WorkflowStepId = w.WorkflowStepID
	if w.ScheduledStartAt != nil {
		p.ScheduledStartAt = timestamppb.New(*w.ScheduledStartAt)
	}
	av := w.AutoStartWorkflow
	p.AutoStartWorkflow = &av
	// The OVERRIDE only — nil (inherit) leaves the optional field unset so the
	// client can tell "inherit" from "explicitly runtime".
	if m, ok := executionModeToProto(w.ExecutionMode); ok {
		p.ExecutionMode = &m
	}
	p.RuntimeImage = w.RuntimeImage
	if w.SortOrder != nil {
		p.SortOrder = *w.SortOrder
	}
	if len(w.RecurringSchedule) > 0 {
		var rs apiv1.RecurringSchedule
		if err := json.Unmarshal(w.RecurringSchedule, &rs); err == nil {
			p.RecurringSchedule = &rs
		}
	}
	if w.NextRunAt != nil {
		p.NextRunAt = timestamppb.New(*w.NextRunAt)
	}
	p.RecurringEnabled = w.RecurringEnabled
	if w.ArchivedAt != nil {
		p.ArchivedAt = timestamppb.New(*w.ArchivedAt)
	}
	if w.ArchivedFromStatus != nil {
		p.ArchivedFromStatus = *w.ArchivedFromStatus
	}
	// Automation provenance (feature 4.1): stamped by the spawn path from the
	// recurring fire's run_context; empty = not an automation spawn.
	if w.SpawnedByWorkItemID != nil {
		p.SpawnedBy = *w.SpawnedByWorkItemID
	}
	if w.SpawnedByRunID != nil {
		p.SpawnedByRunId = *w.SpawnedByRunID
	}
	if len(w.SecretIDs) > 0 {
		var ids []string
		if err := json.Unmarshal(w.SecretIDs, &ids); err == nil {
			p.SecretIds = ids
		}
	}
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

// contextFilesFromJSONOrEmpty is a best-effort parser for the context_files JSONB column.
func contextFilesFromJSONOrEmpty(data []byte) []string {
	paths, err := contextfiles.FromJSON(data)
	if err != nil {
		return nil
	}
	return paths
}
