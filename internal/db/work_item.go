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
	// AcceptanceReview is the human-readable summary of the final work
	// done, auto-populated by the WorkflowReconciler when a bound
	// workflow run reaches a terminal state (docs/02 §2.2). Markdown;
	// mirrors acceptance_criteria (bounded, empty until a run completes).
	AcceptanceReview  string
	Status            string
	AssignedWorkerRef []byte // jsonb: {worker_id, version}
	WorkflowID        *string
	WorkflowRunID     string
	WorkflowStepID    string
	Priority          int
	Budgets           []byte // jsonb
	ContextWindow     int
	// SortOrder is the sibling order within (parent_id) — the sequence
	// engine's derived cursor (architecture-notes/
	// sequential-multi-workflow-runs.md §1). Nullable; backfilled by
	// created_at at migration time, changed only by ReorderWorkItems
	// (explicit drag), never by display sort. NULL sorts last.
	//
	// CONVENTION: 1-based — the first child is 1 (ReorderWorkItems writes
	// i+1), and 0/NULL means "unset" (sorts last; the frontend's
	// byChainOrder treats sortOrder===0 as unset). NEVER write 0 as a real
	// position: it is indistinguishable from unset on the wire (proto
	// `double sort_order` defaults to 0) and will silently sort last.
	SortOrder *float64
	Results   []byte // jsonb
	// PromptContext is the composite prompt the worker should see when
	// dispatched for this work item. Set by the WorkflowReconciler
	// before dispatch (PR B — context propagation). Read by the opencode
	// adapter via the TaskReconciler → manifest Goal. JSONB shape:
	//   {"composite": "# Task\n...\n# Project context\n...\n# Upstream context\n..."}
	PromptContext     []byte     // jsonb
	ScheduledStartAt  *time.Time // scheduled workflow start; nil = immediate
	AutoStartWorkflow bool       // true = auto-start bound workflow on save
	// RuntimeImage is the runtime container image tag for this item's
	// workflow run (empty = base image). Stamped by the backend at
	// create/update so the value always carries forward to the run.
	RuntimeImage string
	// ContextFiles is a JSON array of absolute file/directory paths
	// provided as worker context, mirroring projects.context_files
	// (internal/contextfiles). Read-only input, never mutated by the
	// reconcilers.
	ContextFiles []byte // jsonb
	// RecurringSchedule is the JSONB recurrence definition
	// {frequency, interval, days[], start_date, start_time}.
	// NULL = not recurring. Stored as raw bytes; validated at the API
	// boundary (validate.go).
	RecurringSchedule []byte // jsonb
	// NextRunAt is the computed next occurrence of a recurring item,
	// used by the scheduler due-scan cursor. NULL = not recurring or
	// no next occurrence yet.
	NextRunAt *time.Time
	// ArchivedAt is set when the work item is archived (NULL = active).
	// Every active work-item read filters archived_at IS NULL; the
	// dedicated archive view opts in via ListWorkItems include_archived
	// (archived_at IS NOT NULL).
	ArchivedAt *time.Time
	// ArchivedFromStatus is the terminal status the item had when
	// archived. RestoreWorkItem returns the item to this status (not
	// pending). NULL = never archived.
	ArchivedFromStatus *string
	// SequenceAttempts is the start-failure count for this item as a leaf child (P1 backoff+cap).
	SequenceAttempts int
	// SequenceLastAttemptAt is the last start attempt wall time (for backoff gating).
	SequenceLastAttemptAt *time.Time
	// SequenceConsecutiveScanErrors is consecutive reconcile errors as a parent (P2 heartbeat).
	SequenceConsecutiveScanErrors int
	// SequenceLastProgressAt is last time this parent made forward progress (child armed/completed/failed).
	SequenceLastProgressAt *time.Time
	Version            int
	CreatedAt          time.Time
	UpdatedAt          time.Time
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
	if w.ContextFiles == nil {
		w.ContextFiles = []byte("[]")
	}
	const q = `INSERT INTO work_items
		(id, tenant_id, project_id, parent_id, kind, title, description,
		 acceptance_criteria, status, assigned_worker_ref, workflow_id,
		 workflow_run_id, workflow_step_id,
		 priority, budgets, context_window, results, prompt_context,
		 scheduled_start_at, auto_start_workflow, runtime_image, context_files,
		 recurring_schedule, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)
		RETURNING ` + WorkItemSelectCols
	row := w
	err := tx.QueryRow(ctx, q,
		w.ID, w.TenantID, w.ProjectID, w.ParentID, w.Kind, w.Title, w.Description,
		w.AcceptanceCriteria, w.Status, w.AssignedWorkerRef, w.WorkflowID,
		w.WorkflowRunID, w.WorkflowStepID,
		w.Priority, w.Budgets, w.ContextWindow, w.Results, w.PromptContext,
		w.ScheduledStartAt, w.AutoStartWorkflow, w.RuntimeImage, w.ContextFiles,
		w.RecurringSchedule, w.NextRunAt,
	).Scan(WorkItemScanPtrs(&row)...)
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: create work item: %w", err)
	}
	return row, nil
}

// WorkItemSelectCols is the canonical 31-column SELECT / RETURNING list
// shared by every query that reads a full WorkItemRow. Keeping it in one
// place prevents the column-list drift that caused UnassignWorker to
// silently zero-out fields (the original bug that motivated this constant).
const WorkItemSelectCols = `id, tenant_id, project_id, parent_id, kind, title, description,
	acceptance_criteria, acceptance_review, status, assigned_worker_ref, workflow_id,
	workflow_run_id, workflow_step_id,
	priority, budgets, context_window, sort_order, results, prompt_context,
	scheduled_start_at, auto_start_workflow, runtime_image, context_files,
	recurring_schedule, next_run_at,
	archived_at, archived_from_status,
	sequence_attempts, sequence_last_attempt_at, sequence_consecutive_scan_errors, sequence_last_progress_at,
	version, created_at, updated_at`

// WorkItemScanPtrs returns a slice of Scan pointers matching
// WorkItemSelectCols for the given WorkItemRow. The positional order must
// exactly match the column list.
func WorkItemScanPtrs(w *WorkItemRow) []any {
	return []any{
		&w.ID, &w.TenantID, &w.ProjectID, &w.ParentID, &w.Kind, &w.Title,
		&w.Description, &w.AcceptanceCriteria, &w.AcceptanceReview, &w.Status, &w.AssignedWorkerRef,
		&w.WorkflowID, &w.WorkflowRunID, &w.WorkflowStepID,
		&w.Priority, &w.Budgets, &w.ContextWindow, &w.SortOrder, &w.Results,
		&w.PromptContext,
		&w.ScheduledStartAt, &w.AutoStartWorkflow, &w.RuntimeImage, &w.ContextFiles,
		&w.RecurringSchedule, &w.NextRunAt,
		&w.ArchivedAt, &w.ArchivedFromStatus,
		&w.SequenceAttempts, &w.SequenceLastAttemptAt, &w.SequenceConsecutiveScanErrors, &w.SequenceLastProgressAt,
		&w.Version, &w.CreatedAt, &w.UpdatedAt,
	}
}

// GetWorkItem fetches a single work item by id within the tenant scope.
func GetWorkItem(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkItemRow, error) {
	const q = `SELECT ` + WorkItemSelectCols + `
		FROM work_items WHERE id = $1 AND tenant_id = $2`
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(WorkItemScanPtrs(&w)...)
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
	// IncludeArchived selects which archive partition to return:
	//   false (default) → ONLY active items (archived_at IS NULL) — every
	//     normal view (board/tree/list/sequence/workflows/counts).
	//   true → ONLY archived items (archived_at IS NOT NULL) — the dedicated
	//     archive view.
	IncludeArchived bool
}

// ListWorkItems returns a page of work items for a project, ordered by
// ULID id for stable cursor pagination (docs/07 §5.2).
func ListWorkItems(ctx context.Context, tx pgx.Tx, f ListWorkItemsFilter) ([]WorkItemRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	q := `SELECT ` + WorkItemSelectCols + `
		FROM work_items
		WHERE tenant_id = $1 AND ($2 = '' OR project_id = $2) AND ($3 = '' OR id > $3)`
	args := []any{f.TenantID, f.ProjectID, f.AfterID}
	// Archive gate (the single additive, regression-free default): active
	// reads filter archived_at IS NULL; the archive view opts in with
	// archived_at IS NOT NULL.
	if f.IncludeArchived {
		q += ` AND archived_at IS NOT NULL`
	} else {
		q += ` AND archived_at IS NULL`
	}
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
	// Default ordering (no explicit sort_by) follows the sequence chain:
	// sort_order NULLS LAST, created_at — the tree/board show sibling order
	// by default. sort_order is never a display-sort option (the filter-bar
	// dropdown only offers title/priority/created_at), so no UI control
	// claims to write it. Cursor pagination (AfterID set) keeps the stable
	// id order — the chain-order default only applies to full-page reads.
	if f.SortBy == "" && f.AfterID == "" {
		if orderDir == "ASC" {
			q += ` ORDER BY sort_order NULLS LAST, created_at ASC, id ASC`
		} else {
			q += ` ORDER BY sort_order DESC NULLS LAST, created_at DESC, id DESC`
		}
	} else {
		q += ` ORDER BY ` + orderCol + ` ` + orderDir
	}
	q += ` LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list work items: %w", err)
	}
	defer rows.Close()
	var out []WorkItemRow
	for rows.Next() {
		var w WorkItemRow
		if err := rows.Scan(WorkItemScanPtrs(&w)...); err != nil {
			return nil, fmt.Errorf("db: scan work item: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListDirectChildren returns the immediate children of a work item within
// the tenant scope. Used by kind-switch resolution (ADR-WIT-2) to reparent
// direct children that can no longer sit under a switched item.
func ListDirectChildren(ctx context.Context, tx pgx.Tx, tenantID, parentID string) ([]WorkItemRow, error) {
	const q = `SELECT ` + WorkItemSelectCols + `
		FROM work_items WHERE tenant_id = $1 AND parent_id = $2 AND archived_at IS NULL
		ORDER BY sort_order NULLS LAST, created_at, id`
	rows, err := tx.Query(ctx, q, tenantID, parentID)
	if err != nil {
		return nil, fmt.Errorf("db: list direct children: %w", err)
	}
	defer rows.Close()
	var out []WorkItemRow
	for rows.Next() {
		var w WorkItemRow
		if err := rows.Scan(WorkItemScanPtrs(&w)...); err != nil {
			return nil, fmt.Errorf("db: scan direct child: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListSequenceActiveParents returns the work items that are sequence
// parents in flight OR halted: status IN ('running','failed'), NO bound
// workflow run (workflow_run_id empty), and at least one direct child.
// This is the sequence engine's scan predicate (architecture-notes/
// sequential-multi-workflow-runs.md §2). It is disjoint from bound-run
// tickets (which carry a workflow_run_id) and from standalone one-shot
// running items (which have no children). FAILED parents are included so
// a retried child (the derived cursor's next sibling) can revive the
// chain automatically.
func ListSequenceActiveParents(ctx context.Context, tx pgx.Tx, tenantID string) ([]WorkItemRow, error) {
	const q = `SELECT ` + WorkItemSelectCols + `
		FROM work_items w
		WHERE w.tenant_id = $1
		  AND w.status IN ('running', 'failed')
		  AND w.workflow_run_id = ''
		  AND EXISTS (SELECT 1 FROM work_items c WHERE c.tenant_id = $1 AND c.parent_id = w.id)
		ORDER BY w.updated_at
		LIMIT 100`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list sequence active parents: %w", err)
	}
	defer rows.Close()
	var out []WorkItemRow
	for rows.Next() {
		var w WorkItemRow
		if err := rows.Scan(WorkItemScanPtrs(&w)...); err != nil {
			return nil, fmt.Errorf("db: scan sequence active parent: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListSiblingsForReorder returns the direct children of parentID
// (empty = top-level) within a project, ordered by sort_order NULLS LAST,
// created_at — the ReorderWorkItems read (architecture-notes/
// sequential-multi-workflow-runs.md §1).
func ListSiblingsForReorder(ctx context.Context, tx pgx.Tx, tenantID, projectID, parentID string) ([]WorkItemRow, error) {
	var q string
	var args []any
	if parentID == "" {
		q = `SELECT ` + WorkItemSelectCols + `
		FROM work_items
		WHERE tenant_id = $1 AND project_id = $2 AND parent_id IS NULL AND archived_at IS NULL
		ORDER BY sort_order NULLS LAST, created_at, id`
		args = []any{tenantID, projectID}
	} else {
		q = `SELECT ` + WorkItemSelectCols + `
		FROM work_items
		WHERE tenant_id = $1 AND project_id = $2 AND parent_id = $3 AND archived_at IS NULL
		ORDER BY sort_order NULLS LAST, created_at, id`
		args = []any{tenantID, projectID, parentID}
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list siblings for reorder: %w", err)
	}
	defer rows.Close()
	var out []WorkItemRow
	for rows.Next() {
		var w WorkItemRow
		if err := rows.Scan(WorkItemScanPtrs(&w)...); err != nil {
			return nil, fmt.Errorf("db: scan sibling for reorder: %w", err)
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
	// AcceptanceReview sets the human-readable acceptance review. Empty
	// string clears it; nil = unchanged (field-mask semantics).
	AcceptanceReview  *string
	Status            *string
	Priority          *int
	Budgets           *[]byte
	ContextWindow     *int
	AssignedWorkerRef *[]byte
	ProjectID         *string
	// Kind switches the item's hierarchy kind (epic/feature/task/subtask).
	// The CHECK constraint on kind is satisfied because the service
	// normalizes via domain.NormalizeWorkItemKind before calling.
	Kind *string
	// PromptContext is set by the WorkflowReconciler before dispatch
	// (PR B — context propagation). The opencode adapter reads it via
	// the TaskReconciler → manifest Goal. JSONB payload (see
	// migration 20260713210000).
	PromptContext *[]byte
	// WorkflowID links a work item to the workflow it's part of (set
	// when a TASK step dispatches it). Nullable; empty string clears.
	WorkflowID *string
	// ParentID reparents the item. Empty string clears (sets NULL);
	// non-empty sets the new parent. nil = unchanged.
	ParentID *string
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
	ScheduledStartAt  *time.Time
	AutoStartWorkflow *bool
	// ClearScheduledStartAt, when true, sets scheduled_start_at = NULL.
	// Used when auto_start_workflow is enabled to clear a prior schedule.
	ClearScheduledStartAt bool
	// RuntimeImage is the runtime container image tag; empty string resets
	// to the base image. Stamped at create/update so the value carries
	// forward to the workflow run.
	RuntimeImage *string
	// ClearAssignedWorkerRef, when true, sets assigned_worker_ref = NULL.
	// Used when switching a work item to a non-schedulable kind
	// (epic/feature), where a worker binding is meaningless.
	ClearAssignedWorkerRef bool
	// ContextFiles updates the item's context_files JSONB. A non-nil
	// pointer to an empty JSON array ("[]") clears the selection.
	ContextFiles *[]byte
	// SortOrder renumbers the sibling order within (parent_id). Set
	// exclusively by ReorderWorkItems (explicit drag); display sort never
	// mutates it. The sequence cursor is derived from sort_order at
	// reconcile time, so a mid-run drag only shifts future arming.
	SortOrder *float64
	// RecurringSchedule is the JSONB recurrence definition
	// {frequency, interval, days[], start_date, start_time}. nil =
	// unchanged (field-mask semantics).
	RecurringSchedule *[]byte
	// NextRunAt is the computed next occurrence timestamp for a
	// recurring item. nil = unchanged (field-mask semantics).
	NextRunAt *time.Time
	// ClearRecurringSchedule, when true, sets recurring_schedule = NULL
	// and next_run_at = NULL. Used when status changes to non-recurring
	// or when the kind switches to a non-schedulable kind.
	ClearRecurringSchedule bool
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
	if f.AcceptanceReview != nil {
		q += fmt.Sprintf(`, acceptance_review = $%d`, setIdx)
		args = append(args, *f.AcceptanceReview)
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
	if f.ClearAssignedWorkerRef {
		// A worker binding is meaningless for non-schedulable kinds; the
		// bytea column cannot encode NULL through the *[]byte pointer, so
		// clearing is a dedicated flag (mirrors ClearScheduledStartAt).
		q += `, assigned_worker_ref = NULL`
	}
	if f.Kind != nil {
		q += fmt.Sprintf(`, kind = $%d`, setIdx)
		args = append(args, *f.Kind)
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
		if *f.WorkflowID == "" {
			q += fmt.Sprintf(`, workflow_id = NULL`)
		} else {
			q += fmt.Sprintf(`, workflow_id = $%d`, setIdx)
			args = append(args, *f.WorkflowID)
			setIdx++
		}
	}
	if f.ParentID != nil {
		if *f.ParentID == "" {
			q += fmt.Sprintf(`, parent_id = NULL`)
		} else {
			q += fmt.Sprintf(`, parent_id = $%d`, setIdx)
			args = append(args, *f.ParentID)
			setIdx++
		}
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
	if f.ClearScheduledStartAt {
		q += `, scheduled_start_at = NULL`
	} else if f.ScheduledStartAt != nil {
		q += fmt.Sprintf(`, scheduled_start_at = $%d`, setIdx)
		args = append(args, *f.ScheduledStartAt)
		setIdx++
	}
	if f.AutoStartWorkflow != nil {
		q += fmt.Sprintf(`, auto_start_workflow = $%d`, setIdx)
		args = append(args, *f.AutoStartWorkflow)
		setIdx++
	}
	if f.RuntimeImage != nil {
		q += fmt.Sprintf(`, runtime_image = $%d`, setIdx)
		args = append(args, *f.RuntimeImage)
		setIdx++
	}
	if f.ContextFiles != nil {
		q += fmt.Sprintf(`, context_files = $%d`, setIdx)
		args = append(args, *f.ContextFiles)
		setIdx++
	}
	if f.SortOrder != nil {
		q += fmt.Sprintf(`, sort_order = $%d`, setIdx)
		args = append(args, *f.SortOrder)
		setIdx++
	}
	if f.ClearRecurringSchedule {
		q += `, recurring_schedule = NULL, next_run_at = NULL`
	} else {
		if f.RecurringSchedule != nil {
			q += fmt.Sprintf(`, recurring_schedule = $%d`, setIdx)
			args = append(args, *f.RecurringSchedule)
			setIdx++
		}
		if f.NextRunAt != nil {
			q += fmt.Sprintf(`, next_run_at = $%d`, setIdx)
			args = append(args, *f.NextRunAt)
			setIdx++
		}
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING ` + WorkItemSelectCols
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, args...).Scan(WorkItemScanPtrs(&w)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkItemRow{}, ErrNotFound
	}
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: update work item: %w", err)
	}
	return w, nil
}

// ArchiveWorkItem marks a work item archived with optimistic concurrency
// (docs/09 §5): sets status='archived', archived_at=now() and preserves the
// pre-archive terminal status in archived_from_status. The tenant_id +
// version are injected into the WHERE clause. Returns ErrNotFound if no row
// matches id+tenant+version. The caller (ArchiveWorkItem RPC) enforces the
// terminal-status and no-children preconditions before calling.
func ArchiveWorkItem(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, fromStatus string) (WorkItemRow, error) {
	const q = `UPDATE work_items
		SET status = 'archived', archived_at = now(), archived_from_status = $4,
			updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING ` + WorkItemSelectCols
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, fromStatus).Scan(WorkItemScanPtrs(&w)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkItemRow{}, ErrNotFound
	}
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: archive work item: %w", err)
	}
	return w, nil
}

// RestoreWorkItem returns an archived work item to the active views with
// optimistic concurrency: clears archived_at and archived_from_status and
// returns the item to the terminal status it was archived from. The caller
// (RestoreWorkItem RPC) enforces the currently-archived invariant before
// calling. Returns ErrNotFound on a version mismatch.
func RestoreWorkItem(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, fromStatus string) (WorkItemRow, error) {
	const q = `UPDATE work_items
		SET status = $4, archived_at = NULL, archived_from_status = NULL,
			updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING ` + WorkItemSelectCols
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, fromStatus).Scan(WorkItemScanPtrs(&w)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkItemRow{}, ErrNotFound
	}
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: restore work item: %w", err)
	}
	return w, nil
}

// DependencyRow is the data-access shape of a work_item_dependencies
// table row — an edge in the work DAG (docs/02 §2.2, docs/09 §3.2).
type DependencyRow struct {
	ID        string
	TenantID  string
	ProjectID string
	FromID    string
	ToID      string
	Type      string
	CreatedAt time.Time
}

// CreateDependency inserts a new DAG edge within the given tenant
// transaction. The caller is responsible for cycle detection before
// calling this (docs/02 §2.2: cycles are rejected at admission); the
// DB trigger enforce_work_dag_acyclic is the authoritative backstop and
// aborts the transaction if the edge would close a cycle.
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

// GetDependency fetches a single DAG edge by id within the tenant scope
// (used to snapshot before/after for the audit trail).
func GetDependency(ctx context.Context, tx pgx.Tx, tenantID, id string) (DependencyRow, error) {
	const q = `SELECT id, tenant_id, project_id, from_id, to_id, type, created_at
		FROM work_item_dependencies WHERE tenant_id = $1 AND id = $2`
	var d DependencyRow
	err := tx.QueryRow(ctx, q, tenantID, id).Scan(
		&d.ID, &d.TenantID, &d.ProjectID, &d.FromID, &d.ToID, &d.Type, &d.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DependencyRow{}, ErrNotFound
	}
	if err != nil {
		return DependencyRow{}, fmt.Errorf("db: get dependency: %w", err)
	}
	return d, nil
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

// ListDependenciesForItems returns all dependency edges of the given type
// whose from_id is in the provided set — the single-query payload
// population for WorkItem.depends_on (avoids an N+1 per item in List).
func ListDependenciesForItems(ctx context.Context, tx pgx.Tx, tenantID string, fromIDs []string, depType string) ([]DependencyRow, error) {
	const q = `SELECT id, tenant_id, project_id, from_id, to_id, type, created_at
		FROM work_item_dependencies
		WHERE tenant_id = $1 AND from_id = ANY($2) AND type = $3
		ORDER BY created_at`
	rows, err := tx.Query(ctx, q, tenantID, fromIDs, depType)
	if err != nil {
		return nil, fmt.Errorf("db: list dependencies for items: %w", err)
	}
	defer rows.Close()
	var out []DependencyRow
	for rows.Next() {
		var d DependencyRow
		if err := rows.Scan(&d.ID, &d.TenantID, &d.ProjectID, &d.FromID, &d.ToID,
			&d.Type, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan dependency for item: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDependenciesTargetingItems returns all dependency edges of the given
// types whose to_id is in the provided set — the to_id-side mirror of
// ListDependenciesForItems. Used by the sequence engine to classify each
// child as dependency-governed (any incoming blocking edge) vs a strict
// sequence child (no incoming edges) in a single query.
func ListDependenciesTargetingItems(ctx context.Context, tx pgx.Tx, tenantID string, toIDs []string, depTypes []string) ([]DependencyRow, error) {
	const q = `SELECT id, tenant_id, project_id, from_id, to_id, type, created_at
		FROM work_item_dependencies
		WHERE tenant_id = $1 AND to_id = ANY($2) AND type = ANY($3)
		ORDER BY created_at`
	rows, err := tx.Query(ctx, q, tenantID, toIDs, depTypes)
	if err != nil {
		return nil, fmt.Errorf("db: list dependencies targeting items: %w", err)
	}
	defer rows.Close()
	var out []DependencyRow
	for rows.Next() {
		var d DependencyRow
		if err := rows.Scan(&d.ID, &d.TenantID, &d.ProjectID, &d.FromID, &d.ToID,
			&d.Type, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan dependency targeting item: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UnsatisfiedDependencyRow is one blocking edge source that keeps the
// dependent work item from dispatching: the source is not
// terminal-success. ToID is the dependent (the edge's to_id), ID/Title/
// Status belong to the blocker (the edge's from_id) — the batch attach
// keys rows by ToID so a page of dependents maps to their blockers.
// Populated at read time for WorkItem.blocked_by so the server names the
// blocking edges (AGENTS invariant #1: the UI reflects server state).
type UnsatisfiedDependencyRow struct {
	ToID   string
	ID     string
	Title  string
	Status string
}

// ListUnsatisfiedDependencies returns the sources of every incoming
// blocks/depends_on edge whose status is NOT terminal-success (NOT IN
// terminalSuccessStatuses) for the given work items. The predicate is
// IDENTICAL to CheckDependenciesSatisfied (relates_to never blocks; only
// succeeded/skipped unblock), so the read-time blocked_by list and the
// reconciler's dispatch gate can never disagree.
// Bounded by the number of edges targeting the items — one indexed join
// over the dependency edges + work_items.
func ListUnsatisfiedDependencies(ctx context.Context, tx pgx.Tx, tenantID string, toIDs []string) ([]UnsatisfiedDependencyRow, error) {
	const q = `SELECT d.to_id, wi.id, wi.title, wi.status
		FROM work_item_dependencies d
		JOIN work_items wi ON wi.id = d.from_id AND wi.tenant_id = d.tenant_id
		WHERE d.tenant_id = $1 AND d.to_id = ANY($2)
		  AND d.type IN ('blocks', 'depends_on')
		  AND wi.status NOT IN ` + terminalSuccessStatuses + `
		ORDER BY wi.created_at`
	rows, err := tx.Query(ctx, q, tenantID, toIDs)
	if err != nil {
		return nil, fmt.Errorf("db: list unsatisfied dependencies: %w", err)
	}
	defer rows.Close()
	var out []UnsatisfiedDependencyRow
	for rows.Next() {
		var d UnsatisfiedDependencyRow
		if err := rows.Scan(&d.ToID, &d.ID, &d.Title, &d.Status); err != nil {
			return nil, fmt.Errorf("db: scan unsatisfied dependency: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteOutgoingDependencies removes every dependency edge of the given
// type where from_id = fromID within the tenant scope (set-replace
// support for UpdateWorkItem's depends_on list).
func DeleteOutgoingDependencies(ctx context.Context, tx pgx.Tx, tenantID, fromID, depType string) error {
	const q = `DELETE FROM work_item_dependencies WHERE tenant_id = $1 AND from_id = $2 AND type = $3`
	if _, err := tx.Exec(ctx, q, tenantID, fromID, depType); err != nil {
		return fmt.Errorf("db: delete outgoing dependencies: %w", err)
	}
	return nil
}

// ItemParticipatesInDependency reports whether the item is on either side
// of any dependency edge (outgoing or incoming) within the tenant scope —
// the project-reassignment guard (edges are project-scoped, so an item
// with edges cannot silently move across projects).
func ItemParticipatesInDependency(ctx context.Context, tx pgx.Tx, tenantID, itemID string) (bool, error) {
	const q = `SELECT EXISTS(
		SELECT 1 FROM work_item_dependencies WHERE tenant_id = $1 AND (from_id = $2 OR to_id = $2))`
	var exists bool
	if err := tx.QueryRow(ctx, q, tenantID, itemID).Scan(&exists); err != nil {
		return false, fmt.Errorf("db: item participates in dependency: %w", err)
	}
	return exists, nil
}

// CheckCycleWithRecursiveCTE checks whether adding an edge from→to
// would create a cycle in the dependency DAG. It uses WITH RECURSIVE to
// traverse the existing edges starting from `to` — if `from` is
// reachable from `to`, adding the edge would close a cycle
// (docs/09 §11: recursive CTE for dependency traversal).
//
// Only DAG edges (`depends_on`, `blocks`) participate in the walk;
// `relates_to` is a symmetric, non-ordering relationship and is exempt
// from the DAG invariant (consistent with the DB trigger
// enforce_work_dag_acyclic). Callers that add a `relates_to` edge must
// skip this check entirely — the traversal filter alone is not enough
// (an existing `A depends_on B` would make `B relates_to A` falsely
// reach B) — the AddDependency service gates the call on edge type.
//
// Returns true if adding from→to would create a cycle.
func CheckCycleWithRecursiveCTE(ctx context.Context, tx pgx.Tx, tenantID, projectID, fromID, toID string) (bool, error) {
	// Traverse forward from `to`: follow from_id → to_id edges. If we
	// reach `from`, then from→to would close a cycle.
	const q = `WITH RECURSIVE reach AS (
		SELECT to_id AS node FROM work_item_dependencies
		WHERE tenant_id = $1 AND project_id = $2 AND from_id = $3
		  AND type IN ('depends_on', 'blocks')
		UNION
		SELECT d.to_id FROM work_item_dependencies d
		JOIN reach r ON d.from_id = r.node
		WHERE d.tenant_id = $1 AND d.project_id = $2
		  AND d.type IN ('depends_on', 'blocks')
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
