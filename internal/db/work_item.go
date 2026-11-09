package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkItemRow is the data-access shape of a work_items table row (docs/02 §2.2, docs/09 §3.2).
type WorkItemRow struct {
	ID                 string
	TenantID           string
	ProjectID          string
	ParentID           *string
	Kind               string
	Title              string
	Description        string
	AcceptanceCriteria string
	// AcceptanceReview is the human-readable summary of the final work done, auto-populated by the
	// WorkflowReconciler when a bound workflow run reaches a terminal state (docs/02 §2.2).
	AcceptanceReview  string
	Status            string
	AssignedWorkerRef []byte // jsonb: {worker_id, version}
	WorkflowID        *string
	WorkflowRunID     string
	WorkflowStepID    string
	Priority          int
	Budgets           []byte // jsonb
	ContextWindow     int
	// NEVER write 0 as a real position: it is indistinguishable from unset on the wire (proto `double
	// sort_order` defaults to 0) and will silently sort last.
	SortOrder *float64
	Results   []byte // jsonb
	// PromptContext is the composite prompt the worker should see when dispatched for this work item.
	PromptContext     []byte     // jsonb
	ScheduledStartAt  *time.Time // scheduled workflow start; nil = immediate
	AutoStartWorkflow bool       // true = auto-start bound workflow on save
	// RuntimeImage is the runtime container image tag for this item's workflow run (empty = base
	// image).
	RuntimeImage string
	// ExecutionMode overrides the project's execution_mode for THIS item's executions ("runtime" |
	// "local").
	ExecutionMode *string
	// ContextFiles is a JSON array of absolute file/directory paths provided as worker context,
	// mirroring projects.context_files (internal/contextfiles).
	ContextFiles []byte // jsonb
	// RecurringSchedule is the JSONB recurrence definition {frequency, interval, days[], start_date,
	// start_time}.
	RecurringSchedule []byte // jsonb
	// NextRunAt is the computed next occurrence of a recurring item, used by the scheduler due-scan
	// cursor.
	NextRunAt *time.Time
	// RecurringEnabled is the recurrence on/off flag.
	RecurringEnabled bool
	// ArchivedAt is set when the work item is archived (NULL = active).
	ArchivedAt *time.Time
	// ArchivedFromStatus is the terminal status the item had when archived.
	ArchivedFromStatus *string
	// Ephemeral marks a machine-managed transient item (Ask Orchicon Quick Work): created to carry one
	// job, hidden from every human work-item view, and HARD-DELETED when the job ends.
	Ephemeral bool
	// SpawnedByWorkItemID is the recurring item id that produced this work item (empty = not an
	// automation spawn).
	SpawnedByWorkItemID *string
	// SpawnedByRunID is the workflow run id of the recurring fire's run
	// that produced this work item (empty = not an automation spawn).
	SpawnedByRunID *string
	SecretIDs      []byte // jsonb array of secret IDs to inject at dispatch (max 10)
	// SequenceAttempts is the start-failure count for this item as a leaf child (P1 backoff+cap).
	SequenceAttempts int
	// SequenceLastAttemptAt is the last start attempt wall time (for backoff gating).
	SequenceLastAttemptAt *time.Time
	// SequenceConsecutiveScanErrors is consecutive reconcile errors as a parent (P2 heartbeat).
	SequenceConsecutiveScanErrors int
	// SequenceLastProgressAt is last time this parent made forward progress (child armed/completed/failed).
	SequenceLastProgressAt *time.Time
	Version                int
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// CreateWorkItem inserts a new work item within the given tenant transaction.
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
	if w.SecretIDs == nil {
		w.SecretIDs = []byte("[]")
	}
	// New items always start recurrence-enabled: nothing in the create path
	// represents "create already-paused" — pause is a later UpdateWorkItem.
	w.RecurringEnabled = true
	// Ephemeral (Ask Orchicon Quick Work) items get a CONCRETE mode at create instead of inheriting:
	// see db.EphemeralExecutionMode for why the decision is local and why it is stamped rather than
	// left.
	if w.Ephemeral && w.ExecutionMode == nil {
		m := EphemeralExecutionMode()
		w.ExecutionMode = &m
	}
	// Always-container runtime: an empty runtime_image inherits the project's default_runtime_image
	// (future creates inherit; explicit per-item values win.
	if strings.TrimSpace(w.RuntimeImage) == "" && strings.TrimSpace(w.ProjectID) != "" {
		if p, perr := GetProject(ctx, tx, w.TenantID, w.ProjectID); perr == nil && p.DefaultRuntimeImage != nil && strings.TrimSpace(*p.DefaultRuntimeImage) != "" {
			w.RuntimeImage = *p.DefaultRuntimeImage
		}
	}
	const q = `INSERT INTO work_items
		(id, tenant_id, project_id, parent_id, kind, title, description,
		 acceptance_criteria, status, assigned_worker_ref, workflow_id,
		 workflow_run_id, workflow_step_id,
		 priority, budgets, context_window, results, prompt_context,
		 scheduled_start_at, auto_start_workflow, runtime_image, execution_mode, context_files,
		 recurring_schedule, next_run_at, recurring_enabled, spawned_by_work_item_id, spawned_by_run_id, secret_ids,
		 ephemeral)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30)
		RETURNING ` + WorkItemSelectCols
	row := w
	err := tx.QueryRow(ctx, q,
		w.ID, w.TenantID, w.ProjectID, w.ParentID, w.Kind, w.Title, w.Description,
		w.AcceptanceCriteria, w.Status, w.AssignedWorkerRef, w.WorkflowID,
		w.WorkflowRunID, w.WorkflowStepID,
		w.Priority, w.Budgets, w.ContextWindow, w.Results, w.PromptContext,
		w.ScheduledStartAt, w.AutoStartWorkflow, w.RuntimeImage, w.ExecutionMode, w.ContextFiles,
		w.RecurringSchedule, w.NextRunAt, w.RecurringEnabled,
		w.SpawnedByWorkItemID, w.SpawnedByRunID, w.SecretIDs,
		w.Ephemeral,
	).Scan(WorkItemScanPtrs(&row)...)
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: create work item: %w", err)
	}
	return row, nil
}

// WorkItemSelectCols is the canonical 31-column SELECT / RETURNING list shared by every query that
// reads a full WorkItemRow.
const WorkItemSelectCols = `id, tenant_id, project_id, parent_id, kind, title, description,
	acceptance_criteria, acceptance_review, status, assigned_worker_ref, workflow_id,
	workflow_run_id, workflow_step_id,
	priority, budgets, context_window, sort_order, results, prompt_context,
	scheduled_start_at, auto_start_workflow, runtime_image, execution_mode, context_files,
	recurring_schedule, next_run_at, recurring_enabled,
	archived_at, archived_from_status, ephemeral,
	sequence_attempts, sequence_last_attempt_at, sequence_consecutive_scan_errors, sequence_last_progress_at,
	spawned_by_work_item_id, spawned_by_run_id, secret_ids,
	version, created_at, updated_at`

// WorkItemScanPtrs returns a slice of Scan pointers matching WorkItemSelectCols for the given
// WorkItemRow.
func WorkItemScanPtrs(w *WorkItemRow) []any {
	return []any{
		&w.ID, &w.TenantID, &w.ProjectID, &w.ParentID, &w.Kind, &w.Title,
		&w.Description, &w.AcceptanceCriteria, &w.AcceptanceReview, &w.Status, &w.AssignedWorkerRef,
		&w.WorkflowID, &w.WorkflowRunID, &w.WorkflowStepID,
		&w.Priority, &w.Budgets, &w.ContextWindow, &w.SortOrder, &w.Results,
		&w.PromptContext,
		&w.ScheduledStartAt, &w.AutoStartWorkflow, &w.RuntimeImage, &w.ExecutionMode, &w.ContextFiles,
		&w.RecurringSchedule, &w.NextRunAt, &w.RecurringEnabled,
		&w.ArchivedAt, &w.ArchivedFromStatus, &w.Ephemeral,
		&w.SequenceAttempts, &w.SequenceLastAttemptAt, &w.SequenceConsecutiveScanErrors, &w.SequenceLastProgressAt,
		&w.SpawnedByWorkItemID, &w.SpawnedByRunID, &w.SecretIDs,
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

// SpawnedByTitles resolves the titles of the given work item ids within the tenant — a single
// batched lookup used to render the "from automation X" badge (feature 5.1).
func SpawnedByTitles(ctx context.Context, tx pgx.Tx, tenantID string, ids []string) (map[string]string, error) {
	out := make(map[string]string)
	if len(ids) == 0 {
		return out, nil
	}
	const q = `SELECT id, title FROM work_items WHERE tenant_id = $1 AND id = ANY($2)`
	rows, err := tx.Query(ctx, q, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("db: spawned-by titles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, fmt.Errorf("db: scan spawned-by title: %w", err)
		}
		out[id] = title
	}
	return out, rows.Err()
}

// ListWorkItemsFilter scopes a list query to a tenant + project,
// optionally filtered by parent (tree view) or status (Kanban).
type ListWorkItemsFilter struct {
	TenantID  string
	ProjectID string
	ParentID  *string // nil = all; empty string = top-level only
	Status    string  // empty = all statuses
	Kind      string  // empty = all kinds (epic/feature/task/subtask)
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
	// IdeaScope scopes the idea-state split (feature 4.1): ""/"exclude" = exclude idea-state items from
	// the normal list; "only" = Idea Cloud view (status='idea', provenance carried).
	IdeaScope string
	// IdeaStateScope selects the idea population for the Idea Cloud surfaces (ListIdeas): "" (default)
	// = active idea-state items.
	IdeaStateScope string
	// RecurringFilter scopes the recurring split: ""/"all" = no filter, "exclude" = only non-recurring
	// (recurring_schedule IS NULL), "only" = only recurring (recurring_schedule IS NOT NULL).
	RecurringFilter string
	// EphemeralScope scopes the ephemeral split (Ask Orchicon Quick Work): ""/"exclude" (the default) =
	// ONLY non-ephemeral items.
	EphemeralScope string
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
	// Archive gate (the single additive, regression-free default): active reads filter archived_at IS
	// NULL; the archive view opts in with archived_at IS NOT NULL.
	if f.IncludeArchived {
		q += ` AND archived_at IS NOT NULL`
	} else {
		q += ` AND archived_at IS NULL`
	}
	q += ephemeralPredicate(f.EphemeralScope)
	switch f.RecurringFilter {
	case "only":
		q += ` AND recurring_schedule IS NOT NULL`
	case "exclude":
		q += ` AND recurring_schedule IS NULL`
	}
	// Idea-state gate: the normal list excludes idea items; the Idea Cloud view opts in with "only".
	switch {
	case f.IdeaScope == "only" && f.IdeaStateScope == "rejected":
		q += ` AND status = 'cancelled' AND spawned_by_work_item_id IS NOT NULL`
	case f.IdeaScope == "only":
		q += ` AND status = 'idea'`
	default:
		q += ` AND status <> 'idea'`
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
	if f.Kind != "" {
		q += fmt.Sprintf(` AND kind = $%d`, len(args)+1)
		args = append(args, f.Kind)
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
	// ORDERING, and the cursor must agree with it.
	switch {
	case f.SortBy == "":
		// Chain order.
		if f.AfterID != "" {
			cmp := ">"
			if orderDir == "DESC" {
				cmp = "<"
			}
			q += fmt.Sprintf(` AND (COALESCE(sort_order, 9223372036854775807), created_at, id) %s (
				SELECT COALESCE(w2.sort_order, 9223372036854775807), w2.created_at, w2.id
				FROM work_items w2 WHERE w2.tenant_id = $1 AND w2.id = $%d)`, cmp, len(args)+1)
			args = append(args, f.AfterID)
		}
		if orderDir == "DESC" {
			q += ` ORDER BY sort_order DESC NULLS LAST, created_at DESC, id DESC`
		} else {
			q += ` ORDER BY sort_order ASC NULLS LAST, created_at ASC, id ASC`
		}
	default:
		// An explicit display sort.
		if f.AfterID != "" {
			cmp := ">"
			if orderDir == "DESC" {
				cmp = "<"
			}
			q += fmt.Sprintf(` AND (%s, id) %s (
				SELECT w2.%s, w2.id FROM work_items w2
				WHERE w2.tenant_id = $1 AND w2.id = $%d)`, orderCol, cmp, orderCol, len(args)+1)
			args = append(args, f.AfterID)
		}
		q += ` ORDER BY ` + orderCol + ` ` + orderDir + `, id ` + orderDir
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

// ListDirectChildren returns the immediate children of a work item within the tenant scope.
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

// ListSequenceActiveParents returns the work items that are sequence parents in flight OR halted:
// status IN ('running','failed'), NO bound workflow run (workflow_run_id empty).
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

// ListSiblingsForReorder returns the direct children of parentID (empty = top-level) within a
// project, ordered by sort_order NULLS LAST, created_at.
func ListSiblingsForReorder(ctx context.Context, tx pgx.Tx, tenantID, projectID, parentID string) ([]WorkItemRow, error) {
	// Ephemeral items are excluded from BOTH branches, and the top-level branch is why:
	// ReorderWorkItems requires child_ids to be an exact permutation of this result.
	var q string
	var args []any
	if parentID == "" {
		q = `SELECT ` + WorkItemSelectCols + `
		FROM work_items
		WHERE tenant_id = $1 AND project_id = $2 AND parent_id IS NULL AND archived_at IS NULL
		  AND NOT ephemeral
		ORDER BY sort_order NULLS LAST, created_at, id`
		args = []any{tenantID, projectID}
	} else {
		q = `SELECT ` + WorkItemSelectCols + `
		FROM work_items
		WHERE tenant_id = $1 AND project_id = $2 AND parent_id = $3 AND archived_at IS NULL
		  AND NOT ephemeral
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

// UpdateWorkItemFields is a partial update applied with optimistic concurrency (docs/09 §5).
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
	Kind *string
	// PromptContext is set by the WorkflowReconciler before dispatch (PR B — context propagation).
	PromptContext *[]byte
	// WorkflowID links a work item to the workflow it's part of (set
	// when a TASK step dispatches it). Nullable; empty string clears.
	WorkflowID *string
	// ParentID reparents the item. Empty string clears (sets NULL);
	// non-empty sets the new parent. nil = unchanged.
	ParentID *string
	// Results is the work item's output JSON.
	Results *[]byte
	// WorkflowRunID and WorkflowStepID track which workflow run + step dispatched this work item.
	WorkflowRunID  *string
	WorkflowStepID *string
	// ScheduledStartAt and AutoStartWorkflow control template-bound
	// runs (docs/11 §5.1). Set on create/update.
	ScheduledStartAt  *time.Time
	AutoStartWorkflow *bool
	// ClearScheduledStartAt, when true, sets scheduled_start_at = NULL.
	// Used when auto_start_workflow is enabled to clear a prior schedule.
	ClearScheduledStartAt bool
	// RuntimeImage is the runtime container image tag; empty string resets to the base image.
	RuntimeImage *string
	// ExecutionMode sets the item-level override ("runtime" | "local").
	ExecutionMode *string
	// ClearExecutionMode, when true, sets execution_mode = NULL, i.e. the item inherits its project's
	// mode again.
	ClearExecutionMode bool
	// ClearAssignedWorkerRef, when true, sets assigned_worker_ref = NULL.
	ClearAssignedWorkerRef bool
	// ContextFiles updates the item's context_files JSONB. A non-nil
	// pointer to an empty JSON array ("[]") clears the selection.
	ContextFiles *[]byte
	// SortOrder renumbers the sibling order within (parent_id).
	SortOrder *float64
	// RecurringSchedule is the JSONB recurrence definition {frequency, interval, days[], start_date,
	// start_time}.
	RecurringSchedule *[]byte
	// NextRunAt is the computed next occurrence timestamp for a
	// recurring item. nil = unchanged (field-mask semantics).
	NextRunAt *time.Time
	// ClearRecurringSchedule, when true, sets recurring_schedule = NULL and next_run_at = NULL.
	ClearRecurringSchedule bool
	// ClearRuntimeImage, when true, sets runtime_image = ''.
	ClearRuntimeImage bool
	// RecurringEnabled toggles the recurrence on/off flag.
	RecurringEnabled *bool
	// SpawnedByWorkItemID and SpawnedByRunID stamp automation provenance (feature 4.1).
	SpawnedByWorkItemID *string
	SpawnedByRunID      *string
	SecretIDs           *[]byte
}

// UpdateWorkItem applies a partial update with optimistic concurrency.
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
		// A worker binding is meaningless for non-schedulable kinds; the bytea column cannot encode NULL
		// through the *[]byte pointer, so clearing is a dedicated flag (mirrors ClearScheduledStartAt).
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
	if f.ClearRuntimeImage {
		q += `, runtime_image = ''`
	}
	if f.RuntimeImage != nil {
		q += fmt.Sprintf(`, runtime_image = $%d`, setIdx)
		args = append(args, *f.RuntimeImage)
		setIdx++
	}
	if f.ClearExecutionMode {
		q += `, execution_mode = NULL`
	} else if f.ExecutionMode != nil {
		q += fmt.Sprintf(`, execution_mode = $%d`, setIdx)
		args = append(args, *f.ExecutionMode)
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
	if f.RecurringEnabled != nil {
		q += fmt.Sprintf(`, recurring_enabled = $%d`, setIdx)
		args = append(args, *f.RecurringEnabled)
		setIdx++
	}
	if f.SpawnedByWorkItemID != nil {
		q += fmt.Sprintf(`, spawned_by_work_item_id = $%d`, setIdx)
		args = append(args, *f.SpawnedByWorkItemID)
		setIdx++
	}
	if f.SpawnedByRunID != nil {
		q += fmt.Sprintf(`, spawned_by_run_id = $%d`, setIdx)
		args = append(args, *f.SpawnedByRunID)
		setIdx++
	}
	if f.SecretIDs != nil {
		q += fmt.Sprintf(`, secret_ids = $%d`, setIdx)
		args = append(args, *f.SecretIDs)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING ` + WorkItemSelectCols
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, args...).Scan(WorkItemScanPtrs(&w)...)
	if errors.Is(err, pgx.ErrNoRows) {
		if isWorkItemVersionConflict(ctx, tx, tenantID, id) {
			return WorkItemRow{}, ErrVersionConflict
		}
		return WorkItemRow{}, ErrNotFound
	}
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: update work item: %w", err)
	}
	return w, nil
}

// isWorkItemVersionConflict probes whether a work item row exists but the version did not match.
func isWorkItemVersionConflict(ctx context.Context, tx pgx.Tx, tenantID, id string) bool {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_items WHERE tenant_id=$1 AND id=$2 AND archived_at IS NULL)`, tenantID, id).Scan(&exists)
	return err == nil && exists
}

// ParkWorkItem parks a work item unconditionally (no version check).
func ParkWorkItem(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkItemRow, error) {
	const q = `UPDATE work_items
		SET status = CASE WHEN recurring_schedule IS NOT NULL THEN 'recurring' ELSE 'pending' END,
			scheduled_start_at = NULL,
			workflow_run_id = '',
			updated_at = now(),
			version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND archived_at IS NULL
		RETURNING ` + WorkItemSelectCols
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, tenantID, id).Scan(WorkItemScanPtrs(&w)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkItemRow{}, ErrNotFound
	}
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: park work item: %w", err)
	}
	return w, nil
}

// ArchiveWorkItem marks a work item archived with optimistic concurrency (docs/09 §5): sets
// status='archived', archived_at=now() and preserves the pre-archive terminal status.
func ArchiveWorkItem(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, fromStatus string) (WorkItemRow, error) {
	const q = `UPDATE work_items
		SET status = 'archived', archived_at = now(), archived_from_status = $4,
			updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING ` + WorkItemSelectCols
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, fromStatus).Scan(WorkItemScanPtrs(&w)...)
	if errors.Is(err, pgx.ErrNoRows) {
		if isWorkItemVersionConflict(ctx, tx, tenantID, id) {
			return WorkItemRow{}, ErrVersionConflict
		}
		return WorkItemRow{}, ErrNotFound
	}
	if err != nil {
		return WorkItemRow{}, fmt.Errorf("db: archive work item: %w", err)
	}
	return w, nil
}

// RestoreWorkItem returns an archived work item to the active views with optimistic concurrency:
// clears archived_at and archived_from_status and returns the item to the terminal status it.
func RestoreWorkItem(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, fromStatus string) (WorkItemRow, error) {
	const q = `UPDATE work_items
		SET status = $4, archived_at = NULL, archived_from_status = NULL,
			updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING ` + WorkItemSelectCols
	var w WorkItemRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, fromStatus).Scan(WorkItemScanPtrs(&w)...)
	if errors.Is(err, pgx.ErrNoRows) {
		// Restoring an archived row: probe the archived partition distinctly
		var exists bool
		_ = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_items WHERE tenant_id=$1 AND id=$2)`, tenantID, id).Scan(&exists)
		if exists {
			return WorkItemRow{}, ErrVersionConflict
		}
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

// CreateDependency inserts a new DAG edge within the given tenant transaction.
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

// ListDependenciesForItems returns all dependency edges of the given type whose from_id is in the
// provided set — the single-query payload population for WorkItem.depends_on (avoids an N+1 per.
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

// ListDependenciesTargetingItems returns all dependency edges of the given types whose to_id is in
// the provided set — the to_id-side mirror of ListDependenciesForItems.
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

// UnsatisfiedDependencyRow is one blocking edge source that keeps the dependent work item from
// dispatching: the source is not terminal-success.
type UnsatisfiedDependencyRow struct {
	ToID   string
	ID     string
	Title  string
	Status string
}

// ListUnsatisfiedDependencies returns the sources of every incoming blocks/depends_on edge whose
// status is NOT terminal-success (NOT IN terminalSuccessStatuses) for the given work items.
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

// DeleteOutgoingDependencies removes every dependency edge of the given type where from_id = fromID
// within the tenant scope (set-replace support for UpdateWorkItem's depends_on list).
func DeleteOutgoingDependencies(ctx context.Context, tx pgx.Tx, tenantID, fromID, depType string) error {
	const q = `DELETE FROM work_item_dependencies WHERE tenant_id = $1 AND from_id = $2 AND type = $3`
	if _, err := tx.Exec(ctx, q, tenantID, fromID, depType); err != nil {
		return fmt.Errorf("db: delete outgoing dependencies: %w", err)
	}
	return nil
}

// ItemParticipatesInDependency reports whether the item is on either side of any dependency edge
// (outgoing or incoming) within the tenant scope.
func ItemParticipatesInDependency(ctx context.Context, tx pgx.Tx, tenantID, itemID string) (bool, error) {
	const q = `SELECT EXISTS(
		SELECT 1 FROM work_item_dependencies WHERE tenant_id = $1 AND (from_id = $2 OR to_id = $2))`
	var exists bool
	if err := tx.QueryRow(ctx, q, tenantID, itemID).Scan(&exists); err != nil {
		return false, fmt.Errorf("db: item participates in dependency: %w", err)
	}
	return exists, nil
}

// CheckCycleWithRecursiveCTE checks whether adding an edge from→to would create a cycle in the
// dependency DAG.
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

// HardDeleteWorkItem permanently removes a work item and cascades to its dependencies (rows where
// this item is either the from or to side).
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
