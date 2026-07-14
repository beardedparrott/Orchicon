package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// --- identities (extended in Phase 9) -------------------------------------

// IdentityRow is the data-access shape of an identities table row.
type IdentityRow struct {
	ID           string
	TenantID     string
	Subject      string
	DisplayName  string
	IdentityType string
	Status       string
	Version      int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// GetOrCreateIdentity finds an identity by (tenant_id, subject), or
// creates one if none exists. Used by the auth middleware on first OIDC
// login to provision the identity row. The caller controls the
// transaction so the outbox row can be enqueued atomically.
func GetOrCreateIdentity(ctx context.Context, tx pgx.Tx, tenantID, subject, displayName, identityType string) (IdentityRow, bool, error) {
	const sel = `SELECT id, tenant_id, subject, display_name, identity_type, status, version,
		created_at, updated_at
		FROM identities WHERE tenant_id = $1 AND subject = $2`
	var r IdentityRow
	err := tx.QueryRow(ctx, sel, tenantID, subject).Scan(
		&r.ID, &r.TenantID, &r.Subject, &r.DisplayName, &r.IdentityType, &r.Status,
		&r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if err == nil {
		return r, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return IdentityRow{}, false, fmt.Errorf("db: get identity: %w", err)
	}
	row := IdentityRow{
		ID:           NewID(),
		TenantID:     tenantID,
		Subject:      subject,
		DisplayName:  displayName,
		IdentityType: identityType,
		Status:       "active",
	}
	const ins = `INSERT INTO identities (id, tenant_id, subject, display_name, identity_type, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, tenant_id, subject, display_name, identity_type, status, version, created_at, updated_at`
	err = tx.QueryRow(ctx, ins, row.ID, row.TenantID, row.Subject, nullableStr(row.DisplayName),
		row.IdentityType, row.Status).Scan(
		&row.ID, &row.TenantID, &row.Subject, &row.DisplayName, &row.IdentityType, &row.Status,
		&row.Version, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return IdentityRow{}, false, fmt.Errorf("db: create identity: %w", err)
	}
	return row, true, nil
}

// GetIdentity fetches a single identity by id within the tenant scope.
func GetIdentity(ctx context.Context, tx pgx.Tx, tenantID, id string) (IdentityRow, error) {
	const q = `SELECT id, tenant_id, subject, display_name, identity_type, status, version,
		created_at, updated_at
		FROM identities WHERE id = $1 AND tenant_id = $2`
	var r IdentityRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&r.ID, &r.TenantID, &r.Subject, &r.DisplayName, &r.IdentityType, &r.Status,
		&r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return IdentityRow{}, ErrNotFound
	}
	if err != nil {
		return IdentityRow{}, fmt.Errorf("db: get identity: %w", err)
	}
	return r, nil
}

// ListIdentitiesFilter scopes a list query to a tenant.
type ListIdentitiesFilter struct {
	TenantID string
	PageSize int
	AfterID  string
}

// ListIdentities returns a page of identities for the tenant.
func ListIdentities(ctx context.Context, tx pgx.Tx, f ListIdentitiesFilter) ([]IdentityRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	const q = `SELECT id, tenant_id, subject, display_name, identity_type, status, version,
		created_at, updated_at
		FROM identities
		WHERE tenant_id = $1 AND ($2 = '' OR id > $2)
		ORDER BY id ASC LIMIT $3`
	rows, err := tx.Query(ctx, q, f.TenantID, f.AfterID, f.PageSize)
	if err != nil {
		return nil, fmt.Errorf("db: list identities: %w", err)
	}
	defer rows.Close()
	var out []IdentityRow
	for rows.Next() {
		var r IdentityRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Subject, &r.DisplayName, &r.IdentityType,
			&r.Status, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan identity: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- tenants (no tenant_id; admin reads cross-tenant via a BYPASSRLS-free
// direct read on the tenants table which has no RLS by design) ---------

// TenantRow is the data-access shape of a tenants table row.
type TenantRow struct {
	ID        string
	Slug      string
	Name      string
	Status    string
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListTenants returns a page of tenants. The tenants table has no
// tenant_id (it IS the tenant — docs/09 §3.1) so it is not RLS-enabled;
// an admin listing reads all tenants. The caller must have verified the
// identity is an admin (the RBAC layer enforces this at the API).
func ListTenants(ctx context.Context, p *Pool, pageSize int, afterID string) ([]TenantRow, error) {
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 100
	}
	const q = `SELECT id, slug, name, status, version, created_at, updated_at
		FROM tenants
		WHERE ($1 = '' OR id > $1)
		ORDER BY id ASC LIMIT $2`
	rows, err := p.Query(ctx, q, afterID, pageSize)
	if err != nil {
		return nil, fmt.Errorf("db: list tenants: %w", err)
	}
	defer rows.Close()
	var out []TenantRow
	for rows.Next() {
		var r TenantRow
		if err := rows.Scan(&r.ID, &r.Slug, &r.Name, &r.Status, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan tenant: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateTenant inserts a new tenant row. The id is server-assigned (a
// ULID) and the version starts at 1. Returns the persisted row. The
// tenants table has no tenant_id column so this is an admin-only path
// (the API enforces auth:write / tenant:create entitlement).
func CreateTenant(ctx context.Context, p *Pool, slug, name, budgetEnvelopeJSON string) (TenantRow, error) {
	if budgetEnvelopeJSON == "" {
		budgetEnvelopeJSON = "{}"
	}
	const q = `INSERT INTO tenants (id, slug, name, status, budget_envelope, version)
		VALUES ($1, $2, $3, 'active', $4::jsonb, 1)
		RETURNING id, slug, name, status, version, created_at, updated_at`
	id := ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
	row := p.QueryRow(ctx, q, id, slug, name, budgetEnvelopeJSON)
	var r TenantRow
	if err := row.Scan(&r.ID, &r.Slug, &r.Name, &r.Status, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return TenantRow{}, fmt.Errorf("db: insert tenant: %w", err)
	}
	return r, nil
}

// --- roles -----------------------------------------------------------------

// RoleRow is the data-access shape of a roles table row.
type RoleRow struct {
	ID           string
	TenantID     string
	Name         string
	Scope        string
	ScopeRef     string
	Entitlements []string
	Version      int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// scanEntitlements scans a jsonb column into a []string slice. pgx
// scans jsonb into []byte by default; this helper unmarshals it.
func scanEntitlements(src []byte) []string {
	if len(src) == 0 {
		return nil
	}
	var es []string
	if err := json.Unmarshal(src, &es); err != nil {
		return nil
	}
	return es
}

// CreateRole inserts a new role.
func CreateRole(ctx context.Context, tx pgx.Tx, r RoleRow) (RoleRow, error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.Scope == "" {
		r.Scope = "tenant"
	}
	ent, err := json.Marshal(r.Entitlements)
	if err != nil {
		return RoleRow{}, fmt.Errorf("db: marshal entitlements: %w", err)
	}
	const q = `INSERT INTO roles (id, tenant_id, name, scope, scope_ref, entitlements)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, tenant_id, name, scope, scope_ref, entitlements, version, created_at, updated_at`
	var entBytes []byte
	err = tx.QueryRow(ctx, q, r.ID, r.TenantID, r.Name, r.Scope, r.ScopeRef, ent).Scan(
		&r.ID, &r.TenantID, &r.Name, &r.Scope, &r.ScopeRef, &entBytes, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return RoleRow{}, fmt.Errorf("db: create role: %w", err)
	}
	r.Entitlements = scanEntitlements(entBytes)
	return r, nil
}

// GetRole fetches a single role by id within the tenant scope.
func GetRole(ctx context.Context, tx pgx.Tx, tenantID, id string) (RoleRow, error) {
	const q = `SELECT id, tenant_id, name, scope, scope_ref, entitlements, version,
		created_at, updated_at
		FROM roles WHERE id = $1 AND tenant_id = $2`
	var r RoleRow
	var entBytes []byte
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&r.ID, &r.TenantID, &r.Name, &r.Scope, &r.ScopeRef, &entBytes, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoleRow{}, ErrNotFound
	}
	if err != nil {
		return RoleRow{}, fmt.Errorf("db: get role: %w", err)
	}
	r.Entitlements = scanEntitlements(entBytes)
	return r, nil
}

// ListRoles returns a page of roles for the tenant.
func ListRoles(ctx context.Context, tx pgx.Tx, tenantID string, pageSize int, afterID string) ([]RoleRow, error) {
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 100
	}
	const q = `SELECT id, tenant_id, name, scope, scope_ref, entitlements, version,
		created_at, updated_at
		FROM roles
		WHERE tenant_id = $1 AND ($2 = '' OR id > $2)
		ORDER BY id ASC LIMIT $3`
	rows, err := tx.Query(ctx, q, tenantID, afterID, pageSize)
	if err != nil {
		return nil, fmt.Errorf("db: list roles: %w", err)
	}
	defer rows.Close()
	var out []RoleRow
	for rows.Next() {
		var r RoleRow
		var entBytes []byte
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.Scope, &r.ScopeRef, &entBytes,
			&r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan role: %w", err)
		}
		r.Entitlements = scanEntitlements(entBytes)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- role_bindings ---------------------------------------------------------

// RoleBindingRow is the data-access shape of a role_bindings table row.
type RoleBindingRow struct {
	ID         string
	TenantID   string
	IdentityID string
	RoleID     string
	Scope      string
	ScopeRef   string
	CreatedAt  time.Time
}

// CreateRoleBinding attaches a role to an identity within a scope.
func CreateRoleBinding(ctx context.Context, tx pgx.Tx, b RoleBindingRow) (RoleBindingRow, error) {
	if b.ID == "" {
		b.ID = NewID()
	}
	if b.Scope == "" {
		b.Scope = "tenant"
	}
	const q = `INSERT INTO role_bindings (id, tenant_id, identity_id, role_id, scope, scope_ref)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, tenant_id, identity_id, role_id, scope, scope_ref, created_at`
	err := tx.QueryRow(ctx, q, b.ID, b.TenantID, b.IdentityID, b.RoleID, b.Scope, b.ScopeRef).Scan(
		&b.ID, &b.TenantID, &b.IdentityID, &b.RoleID, &b.Scope, &b.ScopeRef, &b.CreatedAt,
	)
	if err != nil {
		return RoleBindingRow{}, fmt.Errorf("db: create role binding: %w", err)
	}
	return b, nil
}

// DeleteRoleBinding removes a role binding by id within the tenant scope.
func DeleteRoleBinding(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	const q = `DELETE FROM role_bindings WHERE id = $1 AND tenant_id = $2`
	ct, err := tx.Exec(ctx, q, id, tenantID)
	if err != nil {
		return fmt.Errorf("db: delete role binding: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListRoleBindings returns a page of role bindings for the tenant,
// optionally filtered by identity.
func ListRoleBindings(ctx context.Context, tx pgx.Tx, tenantID, identityID string, pageSize int, afterID string) ([]RoleBindingRow, error) {
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 100
	}
	q := `SELECT id, tenant_id, identity_id, role_id, scope, scope_ref, created_at
		FROM role_bindings
		WHERE tenant_id = $1 AND ($2 = '' OR identity_id = $2) AND ($3 = '' OR id > $3)
		ORDER BY id ASC LIMIT $4`
	rows, err := tx.Query(ctx, q, tenantID, identityID, afterID, pageSize)
	if err != nil {
		return nil, fmt.Errorf("db: list role bindings: %w", err)
	}
	defer rows.Close()
	var out []RoleBindingRow
	for rows.Next() {
		var r RoleBindingRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.IdentityID, &r.RoleID, &r.Scope, &r.ScopeRef, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan role binding: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListIdentityEntitlements returns the set of entitlements granted to an
// identity across all its active role bindings, plus the direct scopes on
// any API keys (the latter enforced at the API-key resolution path).
// An identity with the "admin" role (name="admin") is an admin and
// bypasses per-call checks.
func ListIdentityEntitlements(ctx context.Context, tx pgx.Tx, tenantID, identityID string) (ents []string, isAdmin bool, err error) {
	const q = `SELECT r.name, r.entitlements
		FROM role_bindings b
		JOIN roles r ON r.id = b.role_id
		WHERE b.tenant_id = $1 AND b.identity_id = $2`
	rows, err := tx.Query(ctx, q, tenantID, identityID)
	if err != nil {
		return nil, false, fmt.Errorf("db: list entitlements: %w", err)
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var name string
		var entBytes []byte
		if err := rows.Scan(&name, &entBytes); err != nil {
			return nil, false, fmt.Errorf("db: scan entitlements: %w", err)
		}
		if name == "admin" {
			isAdmin = true
		}
		var es []string
		if err := json.Unmarshal(entBytes, &es); err == nil {
			for _, e := range es {
				if _, ok := seen[e]; !ok {
					seen[e] = struct{}{}
					ents = append(ents, e)
				}
			}
		}
	}
	return ents, isAdmin, rows.Err()
}

// --- api_keys --------------------------------------------------------------

// ApiKeyRow is the data-access shape of an api_keys table row. KeyHash
// is the only secret material stored (the plaintext is shown once on
// create and never persisted — AGENTS.md security standards).
type ApiKeyRow struct {
	ID         string
	TenantID   string
	IdentityID string
	Name       string
	KeyPrefix  string
	KeyHash    string
	Scopes     []string
	Status     string
	LastUsedAt *time.Time
	Version    int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// CreateApiKey inserts a new hashed API key.
func CreateApiKey(ctx context.Context, tx pgx.Tx, r ApiKeyRow) (ApiKeyRow, error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.Status == "" {
		r.Status = "active"
	}
	scopes, err := json.Marshal(r.Scopes)
	if err != nil {
		return ApiKeyRow{}, fmt.Errorf("db: marshal scopes: %w", err)
	}
	var scopesBytes []byte
	const q = `INSERT INTO api_keys (id, tenant_id, identity_id, name, key_prefix, key_hash, scopes, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, tenant_id, identity_id, name, key_prefix, key_hash, scopes, status, last_used_at, version, created_at, updated_at`
	err = tx.QueryRow(ctx, q, r.ID, r.TenantID, r.IdentityID, r.Name, r.KeyPrefix, r.KeyHash, scopes, r.Status).Scan(
		&r.ID, &r.TenantID, &r.IdentityID, &r.Name, &r.KeyPrefix, &r.KeyHash, &scopesBytes, &r.Status,
		&r.LastUsedAt, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return ApiKeyRow{}, fmt.Errorf("db: create api key: %w", err)
	}
	r.Scopes = scanEntitlements(scopesBytes)
	return r, nil
}

// GetApiKey fetches a single API key by id within the tenant scope.
func GetApiKey(ctx context.Context, tx pgx.Tx, tenantID, id string) (ApiKeyRow, error) {
	const q = `SELECT id, tenant_id, identity_id, name, key_prefix, key_hash, scopes, status,
		last_used_at, version, created_at, updated_at
		FROM api_keys WHERE id = $1 AND tenant_id = $2`
	var r ApiKeyRow
	var scopesBytes []byte
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&r.ID, &r.TenantID, &r.IdentityID, &r.Name, &r.KeyPrefix, &r.KeyHash, &scopesBytes, &r.Status,
		&r.LastUsedAt, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApiKeyRow{}, ErrNotFound
	}
	if err != nil {
		return ApiKeyRow{}, fmt.Errorf("db: get api key: %w", err)
	}
	r.Scopes = scanEntitlements(scopesBytes)
	return r, nil
}

// ListApiKeys returns a page of API keys for the tenant, optionally
// filtered by identity.
func ListApiKeys(ctx context.Context, tx pgx.Tx, tenantID, identityID string, pageSize int, afterID string) ([]ApiKeyRow, error) {
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 100
	}
	const q = `SELECT id, tenant_id, identity_id, name, key_prefix, key_hash, scopes, status,
		last_used_at, version, created_at, updated_at
		FROM api_keys
		WHERE tenant_id = $1 AND ($2 = '' OR identity_id = $2) AND ($3 = '' OR id > $3)
		ORDER BY id ASC LIMIT $4`
	rows, err := tx.Query(ctx, q, tenantID, identityID, afterID, pageSize)
	if err != nil {
		return nil, fmt.Errorf("db: list api keys: %w", err)
	}
	defer rows.Close()
	var out []ApiKeyRow
	for rows.Next() {
		var r ApiKeyRow
		var scopesBytes []byte
		if err := rows.Scan(&r.ID, &r.TenantID, &r.IdentityID, &r.Name, &r.KeyPrefix, &r.KeyHash,
			&scopesBytes, &r.Status, &r.LastUsedAt, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan api key: %w", err)
		}
		r.Scopes = scanEntitlements(scopesBytes)
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateApiKeyStatus sets the status (e.g. "revoked") with optimistic
// concurrency.
func UpdateApiKeyStatus(ctx context.Context, tx pgx.Tx, tenantID, id, status string, expectedVersion int) (ApiKeyRow, error) {
	const q = `UPDATE api_keys SET status = $1, updated_at = now(), version = version + 1
		WHERE tenant_id = $2 AND id = $3 AND version = $4
		RETURNING id, tenant_id, identity_id, name, key_prefix, key_hash, scopes, status, last_used_at, version, created_at, updated_at`
	var r ApiKeyRow
	var scopesBytes []byte
	err := tx.QueryRow(ctx, q, status, tenantID, id, expectedVersion).Scan(
		&r.ID, &r.TenantID, &r.IdentityID, &r.Name, &r.KeyPrefix, &r.KeyHash, &scopesBytes, &r.Status,
		&r.LastUsedAt, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApiKeyRow{}, ErrNotFound
	}
	if err != nil {
		return ApiKeyRow{}, fmt.Errorf("db: update api key: %w", err)
	}
	r.Scopes = scanEntitlements(scopesBytes)
	return r, nil
}

// RotateApiKeyHash replaces the hash + prefix (the plaintext is shown
// once to the caller) and bumps the version.
func RotateApiKeyHash(ctx context.Context, tx pgx.Tx, tenantID, id, prefix, hash string, expectedVersion int) (ApiKeyRow, error) {
	const q = `UPDATE api_keys SET key_prefix = $1, key_hash = $2, status = 'active', updated_at = now(), version = version + 1
		WHERE tenant_id = $3 AND id = $4 AND version = $5
		RETURNING id, tenant_id, identity_id, name, key_prefix, key_hash, scopes, status, last_used_at, version, created_at, updated_at`
	var r ApiKeyRow
	var scopesBytes []byte
	err := tx.QueryRow(ctx, q, prefix, hash, tenantID, id, expectedVersion).Scan(
		&r.ID, &r.TenantID, &r.IdentityID, &r.Name, &r.KeyPrefix, &r.KeyHash, &scopesBytes, &r.Status,
		&r.LastUsedAt, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApiKeyRow{}, ErrNotFound
	}
	if err != nil {
		return ApiKeyRow{}, fmt.Errorf("db: rotate api key: %w", err)
	}
	r.Scopes = scanEntitlements(scopesBytes)
	return r, nil
}

// LookupApiKeyByHash finds an active API key by its hash (no tenant
// scope — the key identifies the tenant). Used by the auth middleware
// for API-key bearer tokens. The read is not RLS-scoped (the middleware
// does not yet know the tenant); this is the bootstrap resolution.
func LookupApiKeyByHash(ctx context.Context, p *Pool, hash string) (ApiKeyRow, error) {
	const q = `SELECT id, tenant_id, identity_id, name, key_prefix, key_hash, scopes, status,
		last_used_at, version, created_at, updated_at
		FROM api_keys WHERE key_hash = $1 AND status = 'active'`
	var r ApiKeyRow
	var scopesBytes []byte
	err := p.QueryRow(ctx, q, hash).Scan(
		&r.ID, &r.TenantID, &r.IdentityID, &r.Name, &r.KeyPrefix, &r.KeyHash, &scopesBytes, &r.Status,
		&r.LastUsedAt, &r.Version, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApiKeyRow{}, ErrNotFound
	}
	if err != nil {
		return ApiKeyRow{}, fmt.Errorf("db: lookup api key: %w", err)
	}
	r.Scopes = scanEntitlements(scopesBytes)
	return r, nil
}

// TouchApiKeyLastUsed records the last-used timestamp. Best-effort; the
// middleware does not fail the request if this update loses.
func TouchApiKeyLastUsed(ctx context.Context, p *Pool, id string) {
	const q = `UPDATE api_keys SET last_used_at = now() WHERE id = $1`
	_, _ = p.Exec(ctx, q, id)
}
