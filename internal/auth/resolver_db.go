package auth

import (
	"context"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
)

// Resolver resolves an authenticated subject into the full identity
// context (tenant + entitlements). It is shared by the auth HTTP
// handlers (login/refresh issue tokens with the resolved context) and
// the middleware (per-request resolution from an access token or API
// key). All DB access is tenant-scoped via BeginTenantTx so RLS is the
// backstop (docs/09 §8.5).
type Resolver struct {
	pool *db.Pool
}

// NewResolver constructs a Resolver.
func NewResolver(pool *db.Pool) *Resolver { return &Resolver{pool: pool} }

// ResolveIdentity looks up the entitlements + admin flag for an identity
// within a tenant. Returns ErrNotFound if the identity does not exist.
func (r *Resolver) ResolveIdentity(ctx context.Context, tenantID, identityID string) (ents []string, isAdmin bool, err error) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, false, fmt.Errorf("auth: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := db.GetIdentity(ctx, ttx.Tx, tenantID, identityID); err != nil {
		return nil, false, err
	}
	return db.ListIdentityEntitlements(ctx, ttx.Tx, tenantID, identityID)
}

// EnsureIdentityForSubject upserts an identity row for the given OIDC
// subject within a tenant, returning the identity id. When create is
// true the caller should also seed a default role binding (the dev flow
// binds the admin role; production binds a configured default role).
func (r *Resolver) EnsureIdentityForSubject(ctx context.Context, tenantID, subject, displayName, identityType string) (db.IdentityRow, bool, error) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.IdentityRow{}, false, fmt.Errorf("auth: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	row, created, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, subject, displayName, identityType)
	if err != nil {
		return db.IdentityRow{}, false, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return db.IdentityRow{}, false, fmt.Errorf("auth: commit: %w", err)
	}
	return row, created, nil
}

// ResolveApiKey looks up an active API key by its hash (no tenant scope
// — the key identifies the tenant). Returns the row + the key's own
// scopes as the effective entitlement set. An API key is a
// least-privilege machine credential: its scopes ARE the entitlements,
// and it is never an admin (admins bypass per-call checks — a machine
// credential must declare exactly what it may do, docs/07 §6.1).
func (r *Resolver) ResolveApiKey(ctx context.Context, hash string) (db.ApiKeyRow, []string, bool, error) {
	keyRow, err := db.LookupApiKeyByHash(ctx, r.pool, hash)
	if err != nil {
		return db.ApiKeyRow{}, nil, false, err
	}
	// The key's scopes are the effective entitlement set. We do NOT
	// union the identity's role entitlements — that would widen the
	// key beyond its declared scope and defeat least-privilege. The
	// identity is only used to resolve the tenant + record the caller.
	return keyRow, keyRow.Scopes, false, nil
}
