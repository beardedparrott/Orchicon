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
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/rbac"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// AuthHeader is the request header carrying the bearer token.
const AuthHeader = "Authorization"

// publicPaths are not tenant-scoped and require no authentication.
var publicPaths = map[string]bool{
	"/healthz": true,
	"/versionz": true,
	"/auth/dev-login":   true,
	"/auth/refresh":     true,
	"/auth/oidc/login":  true,
	"/auth/oidc/callback": true,
	"/auth/session":     true,
}

// ResolveAuth wraps h with auth-resolution middleware. It resolves the
// caller's identity from the Authorization bearer token (OIDC access
// token or API key) and stores the resolved identity + tenant in the
// context. In local mode with no Authorization header, it falls back
// to the dev tenant (so the UI works before login during the
// transition) — this dev fallback is gated by the local mode flag and
// is never available in production.
func ResolveAuth(h http.Handler, issuer *auth.TokenIssuer, resolver *auth.Resolver, mode config.DeploymentMode, log *slog.Logger) http.Handler {
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
		// Dev fallback: no auth header. In local mode, allow the
		// dev tenant so the UI works before login. Production rejects.
		if mode == config.ModeLocal {
			tid := strings.TrimSpace(r.Header.Get(TenantHeader))
			if tid == "" {
				tid = DefaultTenantID
			}
			ctx = tenant.WithID(ctx, tid)
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
// entitlement required by the procedure. Admins bypass; the dev
// fallback (no identity in context) is allowed in local mode so the
// unauthenticated dev UI still works.
type RBACInterceptor struct {
	mode config.DeploymentMode
}

// NewRBACInterceptor constructs the per-RPC entitlement interceptor.
func NewRBACInterceptor(mode config.DeploymentMode) *RBACInterceptor {
	return &RBACInterceptor{mode: mode}
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

// check enforces the entitlement for a procedure. In local mode with no
// resolved identity (dev fallback) it allows the call so the
// unauthenticated dev UI works. Otherwise it requires the resolved
// identity to hold the procedure's entitlement (or be admin).
func (i *RBACInterceptor) check(ctx context.Context, procedure string) error {
	ident, ok := auth.FromContext(ctx)
	if !ok {
		if i.mode == config.ModeLocal {
			return nil // dev fallback
		}
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
