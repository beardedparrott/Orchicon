package auth

import "context"

// ResolvedIdentity is the authenticated identity context the middleware
// resolves per request and stores in the request context. Handlers and
// the RBAC layer read it to enforce per-call authorization (docs/07 §6.3).
type ResolvedIdentity struct {
	IdentityID   string
	TenantID     string
	Subject      string
	Entitlements []string
	IsAdmin      bool
	// AuthMethod is how the request authenticated: "oidc" (access token),
	// "apikey", or "dev" (local mode synthetic login).
	AuthMethod string
}

type ctxKey struct{}

// WithIdentity stores the resolved identity in the context.
func WithIdentity(ctx context.Context, id ResolvedIdentity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the resolved identity from the context, or the
// zero value + false if none is set.
func FromContext(ctx context.Context) (ResolvedIdentity, bool) {
	v, ok := ctx.Value(ctxKey{}).(ResolvedIdentity)
	return v, ok
}

// HasEntitlement reports whether the identity holds the given
// resource:action entitlement. Admins bypass the check (return true).
// "*" matches any resource or action.
func (r ResolvedIdentity) HasEntitlement(ent string) bool {
	if r.IsAdmin {
		return true
	}
	for _, e := range r.Entitlements {
		if e == ent || e == "*" || e == "*:*" {
			return true
		}
		// Wildcard action: "resource:*" matches "resource:action".
		if len(e) > 2 && e[len(e)-2:] == ":*" {
			res := e[:len(e)-2]
			if len(ent) > len(res) && ent[:len(res)] == res && ent[len(res)] == ':' {
				return true
			}
		}
	}
	return false
}
