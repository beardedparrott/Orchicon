package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PolicyRow is the data-access shape of a policies table row — the
// immutable header (docs/02 §2.5, docs/09 §3.5). The mutable snapshot
// (rego_module, decision_point, scope, effect) lives in PolicyVersionRow.
type PolicyRow struct {
	ID             string
	TenantID       string
	Name           string
	CurrentVersion int
	Status         string
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PolicyVersionRow is the data-access shape of a policy_versions table
// row — the immutable snapshot of a Policy's Rego module at a specific
// version (docs/02 §2.5). Once published, a version is immutable.
type PolicyVersionRow struct {
	ID            string
	TenantID      string
	PolicyID      string
	Version       int
	VersionNote   string
	Status        string
	DecisionPoint string
	Scope         string
	ScopeRef      string
	Effect        string
	RegoModule     string
	Query         string
	PublishedAt   *time.Time
	CreatedAt     time.Time
}

// PolicyDecisionRow is the data-access shape of a policy_decisions table
// row (docs/02 §2.5, docs/07 §3.5). Persisted so ExplainDecision can
// return the Rego trace for a past policy.evaluated event.
type PolicyDecisionRow struct {
	ID            string
	TenantID      string
	PolicyID      string
	PolicyVersion int
	DecisionPoint string
	Effect        string
	Scope         string
	ScopeRef      string
	TargetType    string
	TargetID      string
	ActorType     string
	ActorID       string
	Input         []byte // jsonb
	Result        []byte // jsonb
	Trace         []byte // jsonb
	TraceID       string
	Error         string
	OccurredAt    time.Time
}

// CreatePolicy inserts a new policy header row within the given tenant
// transaction. current_version starts at 0 (no published versions yet).
func CreatePolicy(ctx context.Context, tx pgx.Tx, p PolicyRow) (PolicyRow, error) {
	const q = `INSERT INTO policies (id, tenant_id, name, current_version, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, tenant_id, name, current_version, status, version, created_at, updated_at`
	row := p
	err := tx.QueryRow(ctx, q, p.ID, p.TenantID, p.Name, p.CurrentVersion, p.Status).Scan(
		&row.ID, &row.TenantID, &row.Name, &row.CurrentVersion, &row.Status,
		&row.Version, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return PolicyRow{}, fmt.Errorf("db: create policy: %w", err)
	}
	return row, nil
}

// GetPolicy fetches a single policy by id within the tenant scope.
func GetPolicy(ctx context.Context, tx pgx.Tx, tenantID, id string) (PolicyRow, error) {
	const q = `SELECT id, tenant_id, name, current_version, status, version, created_at, updated_at
		FROM policies WHERE id = $1 AND tenant_id = $2`
	var p PolicyRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.CurrentVersion, &p.Status,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyRow{}, fmt.Errorf("db: get policy: %w", err)
	}
	return p, nil
}

// ListPoliciesFilter scopes a list query to a tenant, optionally
// filtered by decision point and status.
type ListPoliciesFilter struct {
	TenantID       string
	DecisionPoint string // empty = all
	Status        string // empty = all
	PageSize      int
	AfterID       string
}

// ListPolicies returns a page of policies for the tenant, ordered by
// ULID id for stable cursor pagination (docs/07 §5.2).
func ListPolicies(ctx context.Context, tx pgx.Tx, f ListPoliciesFilter) ([]PolicyRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	q := `SELECT id, tenant_id, name, current_version, status, version, created_at, updated_at
		FROM policies WHERE tenant_id = $1 AND ($2 = '' OR id > $2)`
	args := []any{f.TenantID, f.AfterID}
	if f.Status != "" {
		q += fmt.Sprintf(` AND status = $%d`, len(args)+1)
		args = append(args, f.Status)
	}
	q += ` ORDER BY id ASC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list policies: %w", err)
	}
	defer rows.Close()
	var out []PolicyRow
	for rows.Next() {
		var p PolicyRow
		if err := rows.Scan(
			&p.ID, &p.TenantID, &p.Name, &p.CurrentVersion, &p.Status,
			&p.Version, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan policy: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePolicyStatus transitions a policy's status with optimistic
// concurrency (docs/09 §5). tenant_id injected into WHERE.
func UpdatePolicyStatus(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, status string) (PolicyRow, error) {
	const q = `UPDATE policies SET status = $4, updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, name, current_version, status, version, created_at, updated_at`
	var p PolicyRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, status).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.CurrentVersion, &p.Status,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyRow{}, fmt.Errorf("db: update policy status: %w", err)
	}
	return p, nil
}

// UpdatePolicyCurrentVersion bumps the current_version pointer to the
// newly published version (status → published).
func UpdatePolicyCurrentVersion(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion, newVersion int) (PolicyRow, error) {
	const q = `UPDATE policies SET current_version = $4, status = 'published', updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, name, current_version, status, version, created_at, updated_at`
	var p PolicyRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, newVersion).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.CurrentVersion, &p.Status,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyRow{}, fmt.Errorf("db: update policy current_version: %w", err)
	}
	return p, nil
}

// --- PolicyVersion ---------------------------------------------------------

// CreatePolicyVersion inserts a new policy version snapshot row.
func CreatePolicyVersion(ctx context.Context, tx pgx.Tx, v PolicyVersionRow) (PolicyVersionRow, error) {
	const q = `INSERT INTO policy_versions
		(id, tenant_id, policy_id, version, version_note, status,
		 decision_point, scope, scope_ref, effect, rego_module, query)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, tenant_id, policy_id, version, version_note, status,
			decision_point, scope, scope_ref, effect, rego_module, query,
			published_at, created_at`
	row := v
	err := tx.QueryRow(ctx, q,
		v.ID, v.TenantID, v.PolicyID, v.Version, v.VersionNote, v.Status,
		v.DecisionPoint, v.Scope, v.ScopeRef, v.Effect, v.RegoModule, v.Query,
	).Scan(
		&row.ID, &row.TenantID, &row.PolicyID, &row.Version, &row.VersionNote,
		&row.Status, &row.DecisionPoint, &row.Scope, &row.ScopeRef, &row.Effect,
		&row.RegoModule, &row.Query, &row.PublishedAt, &row.CreatedAt,
	)
	if err != nil {
		return PolicyVersionRow{}, fmt.Errorf("db: create policy version: %w", err)
	}
	return row, nil
}

// PublishPolicyVersion transitions a draft version to published. Returns
// ErrNotFound if the version is not in draft state.
func PublishPolicyVersion(ctx context.Context, tx pgx.Tx, tenantID, policyID string, version int) (PolicyVersionRow, error) {
	const q = `UPDATE policy_versions SET status = 'published', published_at = now()
		WHERE tenant_id = $1 AND policy_id = $2 AND version = $3 AND status = 'draft'
		RETURNING id, tenant_id, policy_id, version, version_note, status,
			decision_point, scope, scope_ref, effect, rego_module, query,
			published_at, created_at`
	var v PolicyVersionRow
	err := tx.QueryRow(ctx, q, tenantID, policyID, version).Scan(
		&v.ID, &v.TenantID, &v.PolicyID, &v.Version, &v.VersionNote,
		&v.Status, &v.DecisionPoint, &v.Scope, &v.ScopeRef, &v.Effect,
		&v.RegoModule, &v.Query, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyVersionRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyVersionRow{}, fmt.Errorf("db: publish policy version: %w", err)
	}
	return v, nil
}

// SupersedePolicyVersion transitions a published version to superseded.
func SupersedePolicyVersion(ctx context.Context, tx pgx.Tx, tenantID, policyID string, version int) (PolicyVersionRow, error) {
	const q = `UPDATE policy_versions SET status = 'superseded'
		WHERE tenant_id = $1 AND policy_id = $2 AND version = $3 AND status = 'published'
		RETURNING id, tenant_id, policy_id, version, version_note, status,
			decision_point, scope, scope_ref, effect, rego_module, query,
			published_at, created_at`
	var v PolicyVersionRow
	err := tx.QueryRow(ctx, q, tenantID, policyID, version).Scan(
		&v.ID, &v.TenantID, &v.PolicyID, &v.Version, &v.VersionNote,
		&v.Status, &v.DecisionPoint, &v.Scope, &v.ScopeRef, &v.Effect,
		&v.RegoModule, &v.Query, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyVersionRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyVersionRow{}, fmt.Errorf("db: supersede policy version: %w", err)
	}
	return v, nil
}

// GetLatestPolicyVersion returns the latest version (by version number)
// for a policy. If publishedOnly is true, returns the latest published.
func GetLatestPolicyVersion(ctx context.Context, tx pgx.Tx, tenantID, policyID string, publishedOnly bool) (PolicyVersionRow, error) {
	q := `SELECT id, tenant_id, policy_id, version, version_note, status,
		decision_point, scope, scope_ref, effect, rego_module, query,
		published_at, created_at
		FROM policy_versions WHERE tenant_id = $1 AND policy_id = $2`
	args := []any{tenantID, policyID}
	if publishedOnly {
		q += ` AND status = 'published'`
	}
	q += ` ORDER BY version DESC LIMIT 1`
	var v PolicyVersionRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&v.ID, &v.TenantID, &v.PolicyID, &v.Version, &v.VersionNote,
		&v.Status, &v.DecisionPoint, &v.Scope, &v.ScopeRef, &v.Effect,
		&v.RegoModule, &v.Query, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyVersionRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyVersionRow{}, fmt.Errorf("db: get latest policy version: %w", err)
	}
	return v, nil
}

// GetPolicyVersion returns a specific policy version.
func GetPolicyVersion(ctx context.Context, tx pgx.Tx, tenantID, policyID string, version int) (PolicyVersionRow, error) {
	const q = `SELECT id, tenant_id, policy_id, version, version_note, status,
		decision_point, scope, scope_ref, effect, rego_module, query,
		published_at, created_at
		FROM policy_versions WHERE tenant_id = $1 AND policy_id = $2 AND version = $3`
	var v PolicyVersionRow
	err := tx.QueryRow(ctx, q, tenantID, policyID, version).Scan(
		&v.ID, &v.TenantID, &v.PolicyID, &v.Version, &v.VersionNote,
		&v.Status, &v.DecisionPoint, &v.Scope, &v.ScopeRef, &v.Effect,
		&v.RegoModule, &v.Query, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyVersionRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyVersionRow{}, fmt.Errorf("db: get policy version: %w", err)
	}
	return v, nil
}

// ListPolicyVersions returns all versions of a policy, newest first.
func ListPolicyVersions(ctx context.Context, tx pgx.Tx, tenantID, policyID string) ([]PolicyVersionRow, error) {
	const q = `SELECT id, tenant_id, policy_id, version, version_note, status,
		decision_point, scope, scope_ref, effect, rego_module, query,
		published_at, created_at
		FROM policy_versions WHERE tenant_id = $1 AND policy_id = $2
		ORDER BY version DESC`
	rows, err := tx.Query(ctx, q, tenantID, policyID)
	if err != nil {
		return nil, fmt.Errorf("db: list policy versions: %w", err)
	}
	defer rows.Close()
	var out []PolicyVersionRow
	for rows.Next() {
		var v PolicyVersionRow
		if err := rows.Scan(
			&v.ID, &v.TenantID, &v.PolicyID, &v.Version, &v.VersionNote,
			&v.Status, &v.DecisionPoint, &v.Scope, &v.ScopeRef, &v.Effect,
			&v.RegoModule, &v.Query, &v.PublishedAt, &v.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan policy version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// NextPolicyVersionNumber returns the next version number for a policy.
func NextPolicyVersionNumber(ctx context.Context, tx pgx.Tx, tenantID, policyID string) (int, error) {
	var maxVersion int
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM policy_versions WHERE tenant_id = $1 AND policy_id = $2`,
		tenantID, policyID,
	).Scan(&maxVersion)
	if err != nil {
		return 0, fmt.Errorf("db: next policy version number: %w", err)
	}
	return maxVersion + 1, nil
}

// UpdatePolicyVersionFields is a partial update on a draft version.
type UpdatePolicyVersionFields struct {
	DecisionPoint *string
	Scope         *string
	ScopeRef      *string
	Effect        *string
	RegoModule     *string
	Query         *string
	VersionNote   *string
}

// UpdatePolicyVersion applies a partial update to a draft version's
// mutable fields (docs/02 §2.5). Only draft versions are mutable.
func UpdatePolicyVersion(ctx context.Context, tx pgx.Tx, tenantID, policyID string, version int, f UpdatePolicyVersionFields) (PolicyVersionRow, error) {
	q := `UPDATE policy_versions SET updated_at = now()`
	args := []any{tenantID, policyID, version}
	setIdx := len(args) + 1
	if f.DecisionPoint != nil {
		q += fmt.Sprintf(`, decision_point = $%d`, setIdx)
		args = append(args, *f.DecisionPoint)
		setIdx++
	}
	if f.Scope != nil {
		q += fmt.Sprintf(`, scope = $%d`, setIdx)
		args = append(args, *f.Scope)
		setIdx++
	}
	if f.ScopeRef != nil {
		q += fmt.Sprintf(`, scope_ref = $%d`, setIdx)
		args = append(args, *f.ScopeRef)
		setIdx++
	}
	if f.Effect != nil {
		q += fmt.Sprintf(`, effect = $%d`, setIdx)
		args = append(args, *f.Effect)
		setIdx++
	}
	if f.RegoModule != nil {
		q += fmt.Sprintf(`, rego_module = $%d`, setIdx)
		args = append(args, *f.RegoModule)
		setIdx++
	}
	if f.Query != nil {
		q += fmt.Sprintf(`, query = $%d`, setIdx)
		args = append(args, *f.Query)
		setIdx++
	}
	if f.VersionNote != nil {
		q += fmt.Sprintf(`, version_note = $%d`, setIdx)
		args = append(args, *f.VersionNote)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND policy_id = $2 AND version = $3 AND status = 'draft'`
	q += ` RETURNING id, tenant_id, policy_id, version, version_note, status,
		decision_point, scope, scope_ref, effect, rego_module, query,
		published_at, created_at`
	var v PolicyVersionRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&v.ID, &v.TenantID, &v.PolicyID, &v.Version, &v.VersionNote,
		&v.Status, &v.DecisionPoint, &v.Scope, &v.ScopeRef, &v.Effect,
		&v.RegoModule, &v.Query, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyVersionRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyVersionRow{}, fmt.Errorf("db: update policy version: %w", err)
	}
	return v, nil
}

// --- PolicyDecision --------------------------------------------------------

// CreatePolicyDecision persists a recorded evaluation (docs/02 §2.5,
// docs/07 §3.5). Used by the Policy Engine so ExplainDecision can return
// the Rego trace for a past policy.evaluated event.
func CreatePolicyDecision(ctx context.Context, tx pgx.Tx, d PolicyDecisionRow) (PolicyDecisionRow, error) {
	row := d
	if row.Input == nil {
		row.Input = []byte("{}")
	}
	if row.Result == nil {
		row.Result = []byte("{}")
	}
	if row.Trace == nil {
		row.Trace = []byte("[]")
	}
	const q = `INSERT INTO policy_decisions
		(id, tenant_id, policy_id, policy_version, decision_point, effect,
		 scope, scope_ref, target_type, target_id, actor_type, actor_id,
		 input, result, trace, trace_id, error, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		RETURNING id, tenant_id, policy_id, policy_version, decision_point, effect,
			scope, scope_ref, target_type, target_id, actor_type, actor_id,
			input, result, trace, trace_id, error, occurred_at`
	err := tx.QueryRow(ctx, q,
		row.ID, row.TenantID, row.PolicyID, row.PolicyVersion, row.DecisionPoint,
		row.Effect, row.Scope, row.ScopeRef, row.TargetType, row.TargetID,
		row.ActorType, row.ActorID, row.Input, row.Result, row.Trace,
		row.TraceID, row.Error, row.OccurredAt,
	).Scan(
		&row.ID, &row.TenantID, &row.PolicyID, &row.PolicyVersion,
		&row.DecisionPoint, &row.Effect, &row.Scope, &row.ScopeRef,
		&row.TargetType, &row.TargetID, &row.ActorType, &row.ActorID,
		&row.Input, &row.Result, &row.Trace, &row.TraceID, &row.Error,
		&row.OccurredAt,
	)
	if err != nil {
		return PolicyDecisionRow{}, fmt.Errorf("db: create policy decision: %w", err)
	}
	return row, nil
}

// GetPolicyDecision fetches a single decision by id.
func GetPolicyDecision(ctx context.Context, tx pgx.Tx, tenantID, id string) (PolicyDecisionRow, error) {
	const q = `SELECT id, tenant_id, policy_id, policy_version, decision_point, effect,
		scope, scope_ref, target_type, target_id, actor_type, actor_id,
		input, result, trace, trace_id, error, occurred_at
		FROM policy_decisions WHERE id = $1 AND tenant_id = $2`
	var d PolicyDecisionRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&d.ID, &d.TenantID, &d.PolicyID, &d.PolicyVersion,
		&d.DecisionPoint, &d.Effect, &d.Scope, &d.ScopeRef,
		&d.TargetType, &d.TargetID, &d.ActorType, &d.ActorID,
		&d.Input, &d.Result, &d.Trace, &d.TraceID, &d.Error,
		&d.OccurredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyDecisionRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyDecisionRow{}, fmt.Errorf("db: get policy decision: %w", err)
	}
	return d, nil
}

// GetPolicyDecisionByTrace fetches a decision by OTel trace_id
// (docs/07 §3.5 ExplainDecision by trace_id).
func GetPolicyDecisionByTrace(ctx context.Context, tx pgx.Tx, tenantID, traceID string) (PolicyDecisionRow, error) {
	const q = `SELECT id, tenant_id, policy_id, policy_version, decision_point, effect,
		scope, scope_ref, target_type, target_id, actor_type, actor_id,
		input, result, trace, trace_id, error, occurred_at
		FROM policy_decisions WHERE tenant_id = $1 AND trace_id = $2
		ORDER BY occurred_at DESC LIMIT 1`
	var d PolicyDecisionRow
	err := tx.QueryRow(ctx, q, tenantID, traceID).Scan(
		&d.ID, &d.TenantID, &d.PolicyID, &d.PolicyVersion,
		&d.DecisionPoint, &d.Effect, &d.Scope, &d.ScopeRef,
		&d.TargetType, &d.TargetID, &d.ActorType, &d.ActorID,
		&d.Input, &d.Result, &d.Trace, &d.TraceID, &d.Error,
		&d.OccurredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyDecisionRow{}, ErrNotFound
	}
	if err != nil {
		return PolicyDecisionRow{}, fmt.Errorf("db: get policy decision by trace: %w", err)
	}
	return d, nil
}

// ListPolicyDecisionsFilter scopes the decision log query.
type ListPolicyDecisionsFilter struct {
	TenantID       string
	DecisionPoint string
	TargetType    string
	TargetID      string
	PolicyID      string
	PageSize      int
	AfterID       string
}

// ListPolicyDecisions returns a page of decisions, newest first.
func ListPolicyDecisions(ctx context.Context, tx pgx.Tx, f ListPolicyDecisionsFilter) ([]PolicyDecisionRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	q := `SELECT id, tenant_id, policy_id, policy_version, decision_point, effect,
		scope, scope_ref, target_type, target_id, actor_type, actor_id,
		input, result, trace, trace_id, error, occurred_at
		FROM policy_decisions WHERE tenant_id = $1 AND ($2 = '' OR id < $2)`
	args := []any{f.TenantID, f.AfterID}
	if f.DecisionPoint != "" {
		q += fmt.Sprintf(` AND decision_point = $%d`, len(args)+1)
		args = append(args, f.DecisionPoint)
	}
	if f.TargetType != "" {
		q += fmt.Sprintf(` AND target_type = $%d`, len(args)+1)
		args = append(args, f.TargetType)
	}
	if f.TargetID != "" {
		q += fmt.Sprintf(` AND target_id = $%d`, len(args)+1)
		args = append(args, f.TargetID)
	}
	if f.PolicyID != "" {
		q += fmt.Sprintf(` AND policy_id = $%d`, len(args)+1)
		args = append(args, f.PolicyID)
	}
	q += ` ORDER BY occurred_at DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list policy decisions: %w", err)
	}
	defer rows.Close()
	var out []PolicyDecisionRow
	for rows.Next() {
		var d PolicyDecisionRow
		if err := rows.Scan(
			&d.ID, &d.TenantID, &d.PolicyID, &d.PolicyVersion,
			&d.DecisionPoint, &d.Effect, &d.Scope, &d.ScopeRef,
			&d.TargetType, &d.TargetID, &d.ActorType, &d.ActorID,
			&d.Input, &d.Result, &d.Trace, &d.TraceID, &d.Error,
			&d.OccurredAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan policy decision: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListPublishedPoliciesByDecisionPoint returns all published policy
// versions for a tenant at a given decision point, ordered by scope
// narrowness (task > worker > project > tenant) so the engine can apply
// first-definitive-decision-wins (docs/02 §2.5).
func ListPublishedPoliciesByDecisionPoint(ctx context.Context, tx pgx.Tx, tenantID, decisionPoint string) ([]PolicyVersionRow, error) {
	const q = `SELECT id, tenant_id, policy_id, version, version_note, status,
		decision_point, scope, scope_ref, effect, rego_module, query,
		published_at, created_at
		FROM policy_versions
		WHERE tenant_id = $1 AND decision_point = $2 AND status = 'published'
		ORDER BY CASE scope
			WHEN 'task' THEN 0
			WHEN 'worker' THEN 1
			WHEN 'project' THEN 2
			WHEN 'tenant' THEN 3
			ELSE 4
		END, version DESC`
	rows, err := tx.Query(ctx, q, tenantID, decisionPoint)
	if err != nil {
		return nil, fmt.Errorf("db: list published policies by decision point: %w", err)
	}
	defer rows.Close()
	var out []PolicyVersionRow
	for rows.Next() {
		var v PolicyVersionRow
		if err := rows.Scan(
			&v.ID, &v.TenantID, &v.PolicyID, &v.Version, &v.VersionNote,
			&v.Status, &v.DecisionPoint, &v.Scope, &v.ScopeRef, &v.Effect,
			&v.RegoModule, &v.Query, &v.PublishedAt, &v.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan policy version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
