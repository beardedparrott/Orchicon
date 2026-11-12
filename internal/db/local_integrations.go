package db

// local_integrations.go — THE RECORD THAT A RUN'S WORK WAS LANDED.
//
// A local-strategy project has no remote, so nothing pushes the run's branch anywhere: landing it is the
// PLATFORM's obligation. That obligation used to be inferred on every pass from an ancestry check, a
// run_context stamp and the sweep's cursor, and the inference missed — six features succeeded, two never
// reached the integration branch, and no surface said a word.
//
// An obligation that has to be inferred is an obligation that can be forgotten. This table makes it a ROW:
//
//	the queue     = SELECT ... WHERE status = 'pending' ORDER BY created_at
//	the gate      = does this project have any pending row?
//	a conflict    = a status an operator can see, instead of a branch that quietly never merges
//	did it land   = the row, not "is the branch still in refs/heads"
//
// The git-level ancestry check still runs — it is the honest answer to "is this commit an ancestor of the
// integration branch RIGHT NOW", and it is what makes the pass idempotent — but it is a VERIFICATION of the
// row rather than a substitute for it.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// Local integration statuses. Not a database enum, so a new state is addable without a migration.
const (
	// LocalIntegrationPending means the obligation exists and the work has not landed. It BLOCKS the
	// project's queue: a later run must not cut a branch over work that is still waiting to land.
	LocalIntegrationPending = "pending"
	// LocalIntegrationIntegrating means an attempt is IN FLIGHT.
	LocalIntegrationIntegrating = "integrating"
	// THIS IS THE ONLY SUCCESS.
	LocalIntegrationVerified = "verified"
	// LocalIntegrationFailed means the merge may have moved the ref but the RESULT is not usable — the
	// tree is not materialised, or the project's own check failed.
	LocalIntegrationFailed = "failed"
	// LocalIntegrationMerged is the LEGACY pre-verification success.
	LocalIntegrationMerged = "merged"
	// LocalIntegrationConflicted means a real merge conflict. Nothing was changed and a human must resolve
	// it. It KEEPS BLOCKING — branching past a conflict is what makes the next merge un-mergeable too.
	LocalIntegrationConflicted = "conflicted"
	// LocalIntegrationSkipped means there was provably nothing to land (not a local strategy, no branch,
	// branch already gone, or already an ancestor when the record was first written).
	LocalIntegrationSkipped = "skipped"
	// LocalIntegrationAdopted is work the platform RECOGNISED but did not itself record at the time: a
	// completed, branch-bearing run with no obligation row — a run from before this table existed.
	LocalIntegrationAdopted = "adopted"
)

// LocalIntegrationRow is one run's integration obligation.
type LocalIntegrationRow struct {
	ID string
	TenantID string
	ProjectID string
	WorkflowRunID string
	// ExecutionID is the worker execution that produced the commit.
	ExecutionID string
	Branch string
	BaseCommit string
	IntegrationBranch string
	Status string
	Attempts int
	LastError string
	MergeCommit string
	// VerificationDetail is what the checks found — the reason on a failure, the evidence on a pass.
	VerificationDetail string
	VerifiedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	MergedAt *time.Time
}

const localIntegrationColumns = `id, tenant_id, project_id, workflow_run_id, execution_id, branch, base_commit,
	integration_branch, status, attempts, last_error, merge_commit, verification_detail, verified_at,
	created_at, updated_at, merged_at`

func scanLocalIntegration(row pgx.Row) (LocalIntegrationRow, error) {
	var r LocalIntegrationRow
	err := row.Scan(&r.ID, &r.TenantID, &r.ProjectID, &r.WorkflowRunID, &r.ExecutionID, &r.Branch,
		&r.BaseCommit, &r.IntegrationBranch, &r.Status, &r.Attempts, &r.LastError, &r.MergeCommit,
		&r.VerificationDetail, &r.VerifiedAt, &r.CreatedAt, &r.UpdatedAt, &r.MergedAt)
	return r, err
}

// EnqueueLocalIntegration records the obligation for one run, idempotently.
func EnqueueLocalIntegration(ctx context.Context, tx pgx.Tx, tenantID, projectID, runID, executionID, branch, baseCommit string) (LocalIntegrationRow, error) {
	const q = `INSERT INTO local_integrations
		(id, tenant_id, project_id, workflow_run_id, execution_id, branch, base_commit, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, '` + LocalIntegrationPending + `')
		ON CONFLICT (tenant_id, workflow_run_id) DO NOTHING
		RETURNING ` + localIntegrationColumns
	row, err := scanLocalIntegration(tx.QueryRow(ctx, q, "li_"+ulid.Make().String(), tenantID, projectID,
		runID, executionID, branch, baseCommit))
	if err == nil {
		return row, nil
	}
	if err != pgx.ErrNoRows {
		return LocalIntegrationRow{}, fmt.Errorf("db: enqueue local integration: %w", err)
	}
	// DO NOTHING returned no row: the obligation already exists. Return it.
	existing, gerr := GetLocalIntegrationForRun(ctx, tx, tenantID, runID)
	if gerr != nil {
		return LocalIntegrationRow{}, gerr
	}
	return existing, nil
}

// GetLocalIntegrationForRun returns a run's integration record, or ErrNotFound when the run has none (a
// `pr`/`none` run, a run with no branch, or a run that predates the table).
func GetLocalIntegrationForRun(ctx context.Context, tx pgx.Tx, tenantID, runID string) (LocalIntegrationRow, error) {
	const q = `SELECT ` + localIntegrationColumns + `
		FROM local_integrations WHERE tenant_id = $1 AND workflow_run_id = $2`
	r, err := scanLocalIntegration(tx.QueryRow(ctx, q, tenantID, runID))
	if err == pgx.ErrNoRows {
		return LocalIntegrationRow{}, ErrNotFound
	}
	if err != nil {
		return LocalIntegrationRow{}, fmt.Errorf("db: get local integration: %w", err)
	}
	return r, nil
}

// ListLocalIntegrationsByStatus returns one project's records in a given status, OLDEST FIRST.
func ListLocalIntegrationsByStatus(ctx context.Context, tx pgx.Tx, tenantID, projectID string, statuses []string, limit int) ([]LocalIntegrationRow, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 16
	}
	const q = `SELECT ` + localIntegrationColumns + `
		FROM local_integrations
		WHERE tenant_id = $1 AND project_id = $2 AND status = ANY($3)
		ORDER BY created_at ASC, id ASC
		LIMIT $4`
	rows, err := tx.Query(ctx, q, tenantID, projectID, statuses, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list local integrations: %w", err)
	}
	defer rows.Close()
	var out []LocalIntegrationRow
	for rows.Next() {
		r, serr := scanLocalIntegration(rows)
		if serr != nil {
			return nil, fmt.Errorf("db: scan local integration: %w", serr)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountBlockingLocalIntegrations reports how many of a project's records are still holding the
// queue: anything not PROVEN GOOD.
func CountBlockingLocalIntegrations(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error) {
	const q = `SELECT count(*) FROM local_integrations
		WHERE tenant_id = $1 AND project_id = $2 AND status = ANY($3)`
	var n int
	if err := tx.QueryRow(ctx, q, tenantID, projectID,
		[]string{
			LocalIntegrationPending,
			LocalIntegrationIntegrating,
			LocalIntegrationConflicted,
			LocalIntegrationFailed,
			// LEGACY: landed but never verified.
			LocalIntegrationMerged,
		}).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: count blocking local integrations: %w", err)
	}
	return n, nil
}

// SetLocalIntegrationStatus writes the outcome of one attempt.
func SetLocalIntegrationStatus(ctx context.Context, tx pgx.Tx, tenantID, id, status, mergeCommit, lastError, integrationBranch string) error {
	return SetLocalIntegrationStatusWithDetail(ctx, tx, tenantID, id, status, mergeCommit, lastError, "", integrationBranch)
}

// SetLocalIntegrationStatusWithDetail writes the outcome together with what the verification found.
func SetLocalIntegrationStatusWithDetail(ctx context.Context, tx pgx.Tx, tenantID, id, status, mergeCommit, lastError, verificationDetail, integrationBranch string) error {
	var mergedAt, verifiedAt *time.Time
	now := time.Now().UTC()
	switch status {
	case LocalIntegrationVerified, LocalIntegrationMerged, LocalIntegrationFailed:
		mergedAt = &now
	}
	if status == LocalIntegrationVerified {
		verifiedAt = &now
	}
	const q = `UPDATE local_integrations SET
		status = $1,
		merge_commit = CASE WHEN $2 = '' THEN merge_commit ELSE $2 END,
		last_error = $3,
		verification_detail = CASE WHEN $4 = '' THEN verification_detail ELSE $4 END,
		integration_branch = CASE WHEN $5 = '' THEN integration_branch ELSE $5 END,
		merged_at = COALESCE($6, merged_at),
		verified_at = COALESCE($7, verified_at),
		attempts = attempts + 1,
		updated_at = now()
		WHERE tenant_id = $8 AND id = $9`
	if _, err := tx.Exec(ctx, q, status, mergeCommit, lastError, verificationDetail, integrationBranch,
		mergedAt, verifiedAt, tenantID, id); err != nil {
		return fmt.Errorf("db: set local integration status: %w", err)
	}
	return nil
}

// ListRunsNeedingLocalIntegration returns completed branch-bearing runs of one project that have NO
// record yet, oldest first.
func ListRunsNeedingLocalIntegration(ctx context.Context, tx pgx.Tx, tenantID, projectID string, limit int) ([]WorkflowRunRow, error) {
	if limit <= 0 {
		limit = 16
	}
	// work_item_id is a NULLABLE text column, and a plain scan of it fails the whole read on a run that
	// has none (found by running this against a real database).
	const q = `SELECT r.id, r.tenant_id, r.workflow_id, r.workflow_version, r.project_id, r.status,
		current_step, r.run_context, COALESCE(r.work_item_id, ''), r.bound_worker_ref, r.runtime_image,
		r.runtime_ready, r.worktree_status, r.worktree_path, r.worktree_branch, r.error_message, r.error_kind,
			r.version, r.started_at, r.ended_at, r.created_at, r.updated_at
		FROM workflow_runs r
		WHERE r.tenant_id = $1 AND r.project_id = $2
		  AND r.status = 'completed'
		  AND r.worktree_branch <> ''
		  -- DELIBERATELY NO worktree_status FILTER. A completed run whose branch is NOT provably merged has its
		  -- worktree pruned and its branch KEPT, so filtering on 'pruned' would skip exactly the runs this
		  -- exists for — which is how a green, committed run was never integrated. (Carried over verbatim from
		  -- the candidate query this replaces; the reasoning did not stop being true when the query changed.)
		  AND NOT EXISTS (
		    SELECT 1 FROM local_integrations li
		     WHERE li.tenant_id = r.tenant_id AND li.workflow_run_id = r.id
		  )
		ORDER BY COALESCE(r.ended_at, r.created_at) ASC
		LIMIT $3`
	rows, err := tx.Query(ctx, q, tenantID, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list runs needing local integration: %w", err)
	}
	defer rows.Close()
	var out []WorkflowRunRow
	for rows.Next() {
		var r WorkflowRunRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.WorkflowID, &r.WorkflowVersion, &r.ProjectID, &r.Status,
			&r.CurrentStep, &r.RunContext, &r.WorkItemID, &r.BoundWorkerRef, &r.RuntimeImage,
			&r.RuntimeReady, &r.WorktreeStatus, &r.WorktreePath, &r.WorktreeBranch, &r.ErrorMessage, &r.ErrorKind,
			&r.Version, &r.StartedAt, &r.EndedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan run needing local integration: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExecutionIDForRun returns the worker execution that produced this run's work ("" when none is
// recorded).
func ExecutionIDForRun(ctx context.Context, tx pgx.Tx, tenantID, runID string) string {
	const q = `SELECT worker_execution_id FROM workflow_step_runs
		WHERE tenant_id = $1 AND workflow_run_id = $2 AND worker_execution_id <> ''
		ORDER BY (worktree_branch <> '') DESC, created_at ASC
		LIMIT 1`
	var id string
	if err := tx.QueryRow(ctx, q, tenantID, runID).Scan(&id); err != nil {
		return ""
	}
	return id
}

// MarshalLocalIntegration renders a record for the API/GUI. Kept beside the row so the JSON shape and the
// columns cannot drift.
func MarshalLocalIntegration(r LocalIntegrationRow) map[string]any {
	b, err := json.Marshal(map[string]any{
		"id":                 r.ID,
		"project_id":         r.ProjectID,
		"workflow_run_id":    r.WorkflowRunID,
		"execution_id":       r.ExecutionID,
		"branch":             r.Branch,
		"base_commit":        r.BaseCommit,
		"integration_branch": r.IntegrationBranch,
		"status":             r.Status,
		"attempts":           r.Attempts,
		"last_error":         r.LastError,
		"merge_commit":       r.MergeCommit,
		"verification_detail": r.VerificationDetail,
		"verified_at":         r.VerifiedAt,
		"created_at":         r.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":         r.UpdatedAt.UTC().Format(time.RFC3339),
		"merged_at":          r.MergedAt,
	})
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// ListProjectsWithPendingLocalIntegration returns the projects that have work waiting to land,
// oldest-obligation first, so the periodic backstop can drain exactly those and nothing else.
func ListProjectsWithPendingLocalIntegration(ctx context.Context, tx pgx.Tx, tenantID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 32
	}
	const q = `SELECT project_id FROM local_integrations
		WHERE tenant_id = $1 AND status = ANY($2)
		GROUP BY project_id
		ORDER BY min(created_at) ASC
		LIMIT $3`
	rows, err := tx.Query(ctx, q, tenantID,
		[]string{LocalIntegrationPending, LocalIntegrationAdopted, LocalIntegrationConflicted}, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list projects with pending local integration: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan pending project: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
