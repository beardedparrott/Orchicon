// Package middleware holds HTTP middleware shared across the API surface.
//
// Phase 9 adds the auth middleware that resolves the caller's identity
// from an OIDC access token or API key (docs/07 §6.1) and stores the
// resolved identity + tenant in the request context. It supersedes the
// dev-only tenant-header middleware; the context plumbing
// (internal/tenant) stays the same. A Connect interceptor applies the
// per-RPC RBAC entitlement check on top of the resolved identity
// (docs/07 §6.2, §6.3).
package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/rbac"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// AuthHeader is the request header carrying the bearer token.
const AuthHeader = "Authorization"

// publicPaths are not tenant-scoped and require no authentication.
var publicPaths = map[string]bool{
	"/healthz":          true,
	"/versionz":         true,
	"/auth/local-login": true,
	// Self-service account creation (embedded IdP): happens before a
	// session exists, exactly like local-login.
	"/auth/signup":    true,
	"/auth/dev-login": true,
	"/auth/refresh":   true,
	// /auth/logout is credential-free by design (cookie-based): it can only
	// end the caller's own browser session, and requiring a bearer token
	// here would 401 the sign-out request before the refresh cookie is
	// cleared — leaving a reload after sign-out able to re-authenticate.
	"/auth/logout":        true,
	"/auth/oidc/login":    true,
	"/auth/oidc/callback": true,
	"/auth/session":       true,
	// /auth/config is the pre-login capability-flags endpoint the SPA
	// login page reads to render the honest set of sign-in affordances
	// (embedded-OP / external IdP / dev-login). Public + secret-free.
	"/auth/config": true,
	// Embedded OpenID Provider endpoints (internal/auth/op). These are NOT
	// /auth/*-prefixed so they must be listed here explicitly: discovery,
	// authorize + callback, token, userinfo, and jwks are all reached by
	// unauthenticated RPs/browsers. The login bridge /auth/op/login inherits
	// the /auth/* bypass.
	"/.well-known/openid-configuration": true,
	"/authorize":                        true,
	"/authorize/callback":               true,
	"/token":                            true,
	"/userinfo":                         true,
	"/jwks":                             true,
	"/auth/op/login":                    true,
}

// ResolveAuth wraps h with auth-resolution middleware. It resolves the
// caller's identity from the Authorization bearer token (OIDC access
// token or API key) and stores the resolved identity + tenant in the
// context. A request with no (valid) credential gets a 401 in every
// mode — there is no anonymous dev bypass.
func ResolveAuth(h http.Handler, issuer *auth.TokenIssuer, resolver *auth.Resolver, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public paths skip auth entirely.
		if publicPaths[r.URL.Path] {
			h.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		_, cred, err := auth.ParseBearer(r.Header.Get(AuthHeader))
		if err == nil && cred != "" {
			ident, rerr := resolveCredential(r.Context(), issuer, resolver, cred)
			if rerr != nil {
				log.Debug("auth: resolve credential failed", "error", rerr)
				writeUnauthenticated(w, "invalid or expired token")
				return
			}
			ctx = auth.WithIdentity(ctx, ident)
			ctx = tenant.WithID(ctx, ident.TenantID)
			h.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		writeUnauthenticated(w, "missing credentials")
	})
}

// resolveCredential resolves a bearer credential into the caller's
// identity context. It tries the access-token verifier first; if that
// fails it attempts the API-key lookup.
func resolveCredential(ctx context.Context, issuer *auth.TokenIssuer, resolver *auth.Resolver, cred string) (auth.ResolvedIdentity, error) {
	if strings.HasPrefix(cred, auth.APIKeyPrefix) {
		return resolveApiKey(ctx, resolver, cred)
	}
	claims, err := issuer.VerifyAccess(cred)
	if err != nil {
		// Could still be an API key without the prefix; try once more.
		if id, kerr := resolveApiKey(ctx, resolver, cred); kerr == nil {
			return id, nil
		}
		return auth.ResolvedIdentity{}, err
	}
	return auth.ResolvedIdentity{
		IdentityID:   claims.Subject,
		TenantID:     claims.TenantID,
		Entitlements: claims.Entitlements,
		IsAdmin:      claims.IsAdmin,
		AuthMethod:   "oidc",
	}, nil
}

// resolveApiKey resolves an API key by its hash into the caller's
// identity context.
func resolveApiKey(ctx context.Context, resolver *auth.Resolver, plaintext string) (auth.ResolvedIdentity, error) {
	hash := auth.HashApiKey(plaintext)
	keyRow, ents, isAdmin, err := resolver.ResolveApiKey(ctx, hash)
	if err != nil {
		return auth.ResolvedIdentity{}, errors.New("auth: invalid api key")
	}
	return auth.ResolvedIdentity{
		IdentityID:   keyRow.IdentityID,
		TenantID:     keyRow.TenantID,
		Entitlements: ents,
		IsAdmin:      isAdmin,
		AuthMethod:   "apikey",
	}, nil
}

// writeUnauthenticated writes a 401 with a JSON body.
func writeUnauthenticated(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}

// RBACInterceptor is a Connect interceptor that enforces the per-RPC
// entitlement check (docs/07 §6.2, §6.3). It reads the resolved
// identity from the request context and checks that it holds the
// entitlement required by the procedure. Admins bypass; a missing
// identity is always Unauthenticated (no local-mode dev fallback).
type RBACInterceptor struct{}

// NewRBACInterceptor constructs the per-RPC entitlement interceptor.
func NewRBACInterceptor() *RBACInterceptor {
	return &RBACInterceptor{}
}

// WrapUnary implements connect.Interceptor.
func (i *RBACInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := i.check(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingClient implements connect.Interceptor (client side; not used).
func (i *RBACInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (i *RBACInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := i.check(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// check enforces the entitlement for a procedure. It requires the
// resolved identity to hold the procedure's entitlement (or be admin);
// a request with no resolved identity is Unauthenticated in every mode.
func (i *RBACInterceptor) check(ctx context.Context, procedure string) error {
	ident, ok := auth.FromContext(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("no authenticated identity"))
	}
	required := rbac.EntitlementFor(procedure)
	if required == "" {
		return nil // unknown procedure: fail-open at the entitlement layer
	}
	if ident.HasEntitlement(string(required)) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("insufficient entitlement: "+string(required)))
}
