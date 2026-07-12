// Package tenant carries the resolved tenant identity through the request
// context. The data-access layer reads it to set app.tenant_id per
// transaction (docs/09 §8.5).
//
// v0.1 resolves the tenant from the x-orchicon-tenant-id header in a
// dev-only middleware (internal/middleware/tenant.go). When auth lands
// in Phase 9, the tenant will be derived from the authenticated
// identity's OIDC subject instead; the context plumbing here stays the
// same.
package tenant

import "context"

type ctxKey struct{}

// WithID stores the tenant id in the context. The id must be a valid
// tenant ULID; callers are responsible for validating it exists before
// storing it (the RLS backstop rejects rows for unknown tenants, but a
// known-tenant check up front produces a friendlier error).
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the tenant id from the context, or empty if none
// is set. Handlers should treat an empty result as an internal error
// (the middleware failed to resolve a tenant) rather than serving the
// request unscoped.
func FromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}
