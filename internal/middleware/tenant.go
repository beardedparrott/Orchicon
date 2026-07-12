// Package middleware holds HTTP middleware shared across the API surface.
//
// v0.1 ships the tenant-resolution middleware, a dev-only stand-in for
// the auth middleware that arrives in Phase 9 (docs/07 §6). It reads
// the tenant id from the x-orchicon-tenant-id header and stores it in
// the request context. When OIDC auth lands, this will be replaced by
// tenant derivation from the authenticated identity's OIDC subject —
// the context plumbing (internal/tenant) stays the same.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TenantHeader is the request header carrying the tenant id. Dev-only:
// in production the tenant is derived from the OIDC token, not a header.
const TenantHeader = "x-orchicon-tenant-id"

// DefaultTenantID is the dev tenant seeded by the control plane on boot
// (internal/db/seed.go). Used when the caller omits the header so the
// UI works out of the box before auth is implemented.
const DefaultTenantID = "tnt_dev"

// ResolveTenant wraps h with tenant-resolution middleware. It reads the
// tenant id from the TenantHeader (falling back to DefaultTenantID in
// dev) and stores it in the context via tenant.WithID. Health and
// version endpoints are exempt (they are not tenant-scoped).
//
// The header value is trimmed and validated to be non-empty; a malformed
// value is rejected with 400 rather than silently defaulting, so a
// misconfigured client is noisy rather than cross-scoped.
func ResolveTenant(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health/version endpoints are not tenant-scoped.
		if r.URL.Path == "/healthz" || r.URL.Path == "/versionz" {
			h.ServeHTTP(w, r)
			return
		}
		tid := strings.TrimSpace(r.Header.Get(TenantHeader))
		if tid == "" {
			tid = DefaultTenantID
		}
		ctx := tenant.WithID(r.Context(), tid)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TenantFromContext is a convenience for handlers that need the tenant
// id directly (most use internal/tenant.FromContext instead).
func TenantFromContext(ctx context.Context) string {
	return tenant.FromContext(ctx)
}
