package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkItemRow is the data-access shape of a work_items table row
// (docs/02 §2.2, docs/09 §3.2). All four kinds (epic/feature/task/
// subtask) share this shape. JSON-typed columns (budgets, results,
// assigned_worker_ref) are stored as raw []byte and validated at the
// API boundary. The version column powers optimistic concurrency
// (docs/09 §5).
type WorkItemRow struct {
	ID                 string
	TenantID           string
	ProjectID          string
	ParentID           *string
	Kind               string
	Title              string
	Description        string
	AcceptanceCriteria string
	Status             string
	AssignedWorkerRef  []byte // jsonb: {worker_id, version}
	WorkflowID         *string
	WorkflowRunID      string
	WorkflowStepID     string
	Priority           int
	Budgets            []byte // jsonb
	ContextWindow      int
	Results            []byte // jsonb
	// PromptContext is the composite prompt the worker should see when
	// dispatched for this work item. Set by the WorkflowReconciler
	// before dispatch (PR B — context propagation). Read by the opencode
	// adapter via the TaskReconciler → manifest Goal. JSONB shape:
	//   {"composite": "# Task\n...\n# Project context\n...\n# Upstream context\n..."}
	PromptContext   []byte // jsonb
	ScheduledStartAt *time.Time // scheduled workflow start; nil = immediate
	AutoStartWorkflow bool      // true = auto-start bound workflow on save
	Version          int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// CreateWorkItem inserts a new work item within the given tenant
// transaction. The caller controls the transaction so the outbox row
// can be enqueued in the same atomic unit (docs/09 §6). Version starts
// at 1. JSON-typed columns (budgets, results) default to "{}" if the
// caller doesn't provide them.
func CreateWorkItem(ctx context.Context, tx pgx.Tx, w WorkItemRow) (WorkItemRow, error) {
	if w.Budgets == nil {
		w.Budgets = []byte("{}")
	}
	if w.Results == nil {
		w.Results = []byte("{}")
	}
	const q = `INSERT INTO work_items
		(id, tenant_id, project_id, parent_id, kind, title, description,
		 acceptance_criteria, status, assigned_worker_ref, workflow_id,
		 workflow_run_id, workflow_step_id,
		 priority, budgets, context_window, results, prompt_context,
		 scheduled_start_at, auto_start_workflow)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		RETURNING id, tenant_id, project_id, parent_id, kind, title, description,
			acceptance_criteria, status, assigned_worker_ref, workflow_id,
			workflow_run_id, workflow_step_id,
			priority, budgets, context_window, results, prompt_context,
			scheduled_start_at, auto_start_workflow, version, created_at, updated_at`
	row := w
	err := tx.QueryRow(ctx, q,
		w.ID, w.TenantID, w.ProjectID, w.ParentID, w.Kind, w.Title, w.Description,
		w.AcceptanceCriteria, w.Status, w.AssignedWorkerRef, w.WorkflowID,
		w.WorkflowRunID, w.WorkflowStepID,
		w.Priority, w.Budgets, w.ContextWindow, w.Results, w.PromptContext,
		w.ScheduledStartAt, w.AutoStartWorkflow,
	).Scan(
		&row.ID, &row.TenantID, &row.ProjectID, &row.ParentID, &row.Kind, &row.Title,
		&row.Description, &row.AcceptanceCriteria, &row.Status, &row.AssignedWorkerRef,
		&row.WorkflowID, &row.WorkflowRunID, &row.WorkflowStepID,
		&row.Priority, &row.Budgets, &row.ContextWindow, &row.Results,
		&row.PromptContext,
		&row.ScheduledStartAt, &row.AutoStartWorkflow,
		&row.Version, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: create work item: %w", err)
	}
	return row, nil
}

// GetWorkItem fetches a single work item by id within the tenant scope.
func GetWorkItem(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkItemRow, error) {
	const q = `SELECT id, tenant_id, project_id, parent_id, kind, title, description,
		acceptance_criteria, status, assigned_worker_ref, workflow_id,
		workflow_run_id, workflow_step_id,
		priority, budgets, context_window, results, prompt_context,
		scheduled_start_at, auto_start_workflow, version, created_at, updated_at
		FROM work_items WHERE id = $1 AND tenant_id = $2`
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&w.ID, &w.TenantID, &w.ProjectID, &w.ParentID, &w.Kind, &w.Title,
		&w.Description, &w.AcceptanceCriteria, &w.Status, &w.AssignedWorkerRef,
		&w.WorkflowID, &w.WorkflowRunID, &w.WorkflowStepID,
		&w.Priority, &w.Budgets, &w.ContextWindow, &w.Results,
		&w.PromptContext,
		&w.ScheduledStartAt, &w.AutoStartWorkflow,
		&w.Version, &w.CreatedAt, &w.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkItemRow{}, ErrNotFound
	}
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: get work item: %w", err)
	}
	return w, nil
}

// ListWorkItemsFilter scopes a list query to a tenant + project,
// optionally filtered by parent (tree view) or status (Kanban).
type ListWorkItemsFilter struct {
	TenantID  string
	ProjectID string
	ParentID  *string // nil = all; empty string = top-level only
	Status    string  // empty = all statuses
	Search    string  // ILIKE across title and description
	SortBy    string  // "title", "priority", "created_at" (default)
	SortOrder string  // "asc" or "desc" (default "asc")
	PageSize  int
	AfterID   string
}

// ListWorkItems returns a page of work items for a project, ordered by
// ULID id for stable cursor pagination (docs/07 §5.2).
func ListWorkItems(ctx context.Context, tx pgx.Tx, f ListWorkItemsFilter) ([]WorkItemRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	q := `SELECT id, tenant_id, project_id, parent_id, kind, title, description,
		acceptance_criteria, status, assigned_worker_ref, workflow_id,
		workflow_run_id, workflow_step_id,
		priority, budgets, context_window, results, prompt_context,
		scheduled_start_at, auto_start_workflow, version, created_at, updated_at
		FROM work_items
		WHERE tenant_id = $1 AND project_id = $2 AND ($3 = '' OR id > $3)`
	args := []any{f.TenantID, f.ProjectID, f.AfterID}
	if f.ParentID != nil {
		if *f.ParentID == "" {
			q += fmt.Sprintf(` AND parent_id IS NULL`)
		} else {
			q += fmt.Sprintf(` AND parent_id = $%d`, len(args)+1)
			args = append(args, *f.ParentID)
		}
	}
	if f.Status != "" {
		q += fmt.Sprintf(` AND status = $%d`, len(args)+1)
		args = append(args, f.Status)
	}
	if f.Search != "" {
		q += fmt.Sprintf(` AND (title ILIKE $%d OR description ILIKE $%d)`, len(args)+1, len(args)+1)
		args = append(args, "%"+f.Search+"%")
	}
	orderCol := "id"
	switch f.SortBy {
	case "title":
		orderCol = "title"
	case "priority":
		orderCol = "priority"
	case "created_at":
		orderCol = "created_at"
	}
	orderDir := "ASC"
	if strings.ToLower(f.SortOrder) == "desc" {
		orderDir = "DESC"
	}
	q += ` ORDER BY ` + orderCol + ` ` + orderDir + ` LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list work items: %w", err)
	}
	defer rows.Close()
	var out []WorkItemRow
	for rows.Next() {
		var w WorkItemRow
		if err := rows.Scan(
			&w.ID, &w.TenantID, &w.ProjectID, &w.ParentID, &w.Kind, &w.Title,
			&w.Description, &w.AcceptanceCriteria, &w.Status, &w.AssignedWorkerRef,
			&w.WorkflowID, &w.WorkflowRunID, &w.WorkflowStepID,
			&w.Priority, &w.Budgets, &w.ContextWindow, &w.Results,
			&w.PromptContext,
			&w.ScheduledStartAt, &w.AutoStartWorkflow,
			&w.Version, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan work item: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateWorkItemFields is a partial update applied with optimistic
// concurrency (docs/09 §5). Only non-nil fields are written (field-mask
// semantics — docs/07 §5.4).
type UpdateWorkItemFields struct {
	Title              *string
	Description        *string
	AcceptanceCriteria *string
	Status             *string
	Priority           *int
	Budgets            *[]byte
	ContextWindow      *int
	AssignedWorkerRef  *[]byte
	ProjectID          *string
	// PromptContext is set by the WorkflowReconciler before dispatch
	// (PR B — context propagation). The opencode adapter reads it via
	// the TaskReconciler → manifest Goal. JSONB payload (see
	// migration 20260713210000).
	PromptContext *[]byte
	// WorkflowID links a work item to the workflow it's part of (set
	// when a TASK step dispatches it). Nullable; empty string clears.
	WorkflowID *string
	// Results is the work item's output JSON. The TaskReconciler
	// writes _output (raw worker text) and _summary (extracted
	// summary) here on terminal state (PR B).
	Results *[]byte
	// WorkflowRunID and WorkflowStepID track which workflow run + step
	// dispatched this work item. Set by the WorkflowReconciler and
	// propagated to the WorkerExecution by the TaskReconciler.
	WorkflowRunID  *string
	WorkflowStepID *string
	// ScheduledStartAt and AutoStartWorkflow control template-bound
	// runs (docs/11 §5.1). Set on create/update.
	ScheduledStartAt   *time.Time
	AutoStartWorkflow  *bool
}

// UpdateWorkItem applies a partial update with optimistic concurrency.
// The tenant_id is injected into the WHERE clause. Returns ErrNotFound
// if no row matches the id+tenant+version.
func UpdateWorkItem(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, f UpdateWorkItemFields) (WorkItemRow, error) {
	q := `UPDATE work_items SET updated_at = now(), version = version + 1`
	args := []any{tenantID, id, expectedVersion}
	setIdx := len(args) + 1
	if f.Title != nil {
		q += fmt.Sprintf(`, title = $%d`, setIdx)
		args = append(args, *f.Title)
		setIdx++
	}
	if f.Description != nil {
		q += fmt.Sprintf(`, description = $%d`, setIdx)
		args = append(args, *f.Description)
		setIdx++
	}
	if f.AcceptanceCriteria != nil {
		q += fmt.Sprintf(`, acceptance_criteria = $%d`, setIdx)
		args = append(args, *f.AcceptanceCriteria)
		setIdx++
	}
	if f.Status != nil {
		q += fmt.Sprintf(`, status = $%d`, setIdx)
		args = append(args, *f.Status)
		setIdx++
	}
	if f.Priority != nil {
		q += fmt.Sprintf(`, priority = $%d`, setIdx)
		args = append(args, *f.Priority)
		setIdx++
	}
	if f.Budgets != nil {
		q += fmt.Sprintf(`, budgets = $%d`, setIdx)
		args = append(args, *f.Budgets)
		setIdx++
	}
	if f.ContextWindow != nil {
		q += fmt.Sprintf(`, context_window = $%d`, setIdx)
		args = append(args, *f.ContextWindow)
		setIdx++
	}
	if f.AssignedWorkerRef != nil {
		q += fmt.Sprintf(`, assigned_worker_ref = $%d`, setIdx)
		args = append(args, *f.AssignedWorkerRef)
		setIdx++
	}
	if f.ProjectID != nil {
		q += fmt.Sprintf(`, project_id = $%d`, setIdx)
		args = append(args, *f.ProjectID)
		setIdx++
	}
	if f.PromptContext != nil {
		q += fmt.Sprintf(`, prompt_context = $%d`, setIdx)
		args = append(args, *f.PromptContext)
		setIdx++
	}
	if f.WorkflowID != nil {
		q += fmt.Sprintf(`, workflow_id = $%d`, setIdx)
		args = append(args, *f.WorkflowID)
		setIdx++
	}
	if f.Results != nil {
		q += fmt.Sprintf(`, results = $%d`, setIdx)
		args = append(args, *f.Results)
		setIdx++
	}
	if f.WorkflowRunID != nil {
		q += fmt.Sprintf(`, workflow_run_id = $%d`, setIdx)
		args = append(args, *f.WorkflowRunID)
		setIdx++
	}
	if f.WorkflowStepID != nil {
		q += fmt.Sprintf(`, workflow_step_id = $%d`, setIdx)
		args = append(args, *f.WorkflowStepID)
		setIdx++
	}
	if f.ScheduledStartAt != nil {
		q += fmt.Sprintf(`, scheduled_start_at = $%d`, setIdx)
		args = append(args, *f.ScheduledStartAt)
		setIdx++
	}
	if f.AutoStartWorkflow != nil {
		q += fmt.Sprintf(`, auto_start_workflow = $%d`, setIdx)
		args = append(args, *f.AutoStartWorkflow)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, project_id, parent_id, kind, title, description,
		acceptance_criteria, status, assigned_worker_ref, workflow_id,
		workflow_run_id, workflow_step_id,
		priority, budgets, context_window, results, prompt_context,
		scheduled_start_at, auto_start_workflow, version, created_at, updated_at`
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&w.ID, &w.TenantID, &w.ProjectID, &w.ParentID, &w.Kind, &w.Title,
		&w.Description, &w.AcceptanceCriteria, &w.Status, &w.AssignedWorkerRef,
		&w.WorkflowID, &w.WorkflowRunID, &w.WorkflowStepID,
		&w.Priority, &w.Budgets, &w.ContextWindow, &w.Results,
		&w.PromptContext,
		&w.ScheduledStartAt, &w.AutoStartWorkflow,
		&w.Version, &w.CreatedAt, &w.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkItemRow{}, ErrNotFound
	}
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: update work item: %w", err)
	}
	return w, nil
}

// DependencyRow is the data-access shape of a work_item_dependencies
// table row — an edge in the work DAG (docs/02 §2.2, docs/09 §3.2).
type DependencyRow struct {
	ID        string
	TenantID  string
	ProjectID  string
	FromID    string
	ToID      string
	Type      string
	CreatedAt time.Time
}

// CreateDependency inserts a new DAG edge within the given tenant
// transaction. The caller is responsible for cycle detection before
// calling this (docs/02 §2.2: cycles are rejected at admission).
func CreateDependency(ctx context.Context, tx pgx.Tx, d DependencyRow) (DependencyRow, error) {
	const q = `INSERT INTO work_item_dependencies
		(id, tenant_id, project_id, from_id, to_id, type)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, tenant_id, project_id, from_id, to_id, type, created_at`
	row := d
	err := tx.QueryRow(ctx, q,
		d.ID, d.TenantID, d.ProjectID, d.FromID, d.ToID, d.Type,
	).Scan(
		&row.ID, &row.TenantID, &row.ProjectID, &row.FromID, &row.ToID,
		&row.Type, &row.CreatedAt,
	)
	if err != nil {
		return DependencyRow{}, fmt.Errorf("db: create dependency: %w", err)
	}
	return row, nil
}

// DeleteDependency removes a DAG edge by id within the tenant scope.
func DeleteDependency(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	const q = `DELETE FROM work_item_dependencies WHERE tenant_id = $1 AND id = $2`
	tag, err := tx.Exec(ctx, q, tenantID, id)
	if err != nil {
		return fmt.Errorf("db: delete dependency: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListDependencies returns all dependency edges for a project.
func ListDependencies(ctx context.Context, tx pgx.Tx, tenantID, projectID string) ([]DependencyRow, error) {
	const q = `SELECT id, tenant_id, project_id, from_id, to_id, type, created_at
		FROM work_item_dependencies
		WHERE tenant_id = $1 AND project_id = $2
		ORDER BY created_at`
	rows, err := tx.Query(ctx, q, tenantID, projectID)
	if err != nil {
		return nil, fmt.Errorf("db: list dependencies: %w", err)
	}
	defer rows.Close()
	var out []DependencyRow
	for rows.Next() {
		var d DependencyRow
		if err := rows.Scan(&d.ID, &d.TenantID, &d.ProjectID, &d.FromID, &d.ToID,
			&d.Type, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan dependency: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CheckCycleWithRecursiveCTE checks whether adding an edge from→to
// would create a cycle in the dependency DAG. It uses WITH RECURSIVE to
// traverse the existing edges starting from `to` — if `from` is
// reachable from `to`, adding the edge would close a cycle
// (docs/09 §11: recursive CTE for dependency traversal).
//
// Returns true if adding from→to would create a cycle.
func CheckCycleWithRecursiveCTE(ctx context.Context, tx pgx.Tx, tenantID, projectID, fromID, toID string) (bool, error) {
	// Traverse forward from `to`: follow from_id → to_id edges. If we
	// reach `from`, then from→to would close a cycle.
	const q = `WITH RECURSIVE reach AS (
		SELECT to_id AS node FROM work_item_dependencies
		WHERE tenant_id = $1 AND project_id = $2 AND from_id = $3
		UNION
		SELECT d.to_id FROM work_item_dependencies d
		JOIN reach r ON d.from_id = r.node
		WHERE d.tenant_id = $1 AND d.project_id = $2
	)
	SELECT EXISTS(SELECT 1 FROM reach WHERE node = $4)`
	var creates bool
	err := tx.QueryRow(ctx, q, tenantID, projectID, toID, fromID).Scan(&creates)
	if err != nil {
		return false, fmt.Errorf("db: check cycle (recursive CTE): %w", err)
	}
	return creates, nil
}

// HardDeleteWorkItem permanently removes a work item and cascades to
// its dependencies (rows where this item is either the from or to side).
// Returns ErrNotFound if no row matches the id+tenant.
func HardDeleteWorkItem(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM work_item_dependencies
		 WHERE tenant_id = $1 AND (from_id = $2 OR to_id = $2)`,
		tenantID, id); err != nil {
		return fmt.Errorf("db: hard delete work item dependencies: %w", err)
	}
	ct, err := tx.Exec(ctx,
		`DELETE FROM work_items WHERE id = $1 AND tenant_id = $2`,
		id, tenantID)
	if err != nil {
		return fmt.Errorf("db: hard delete work item: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
