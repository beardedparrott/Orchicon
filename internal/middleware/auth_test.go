package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/beardedparrott/orchicon/internal/auth"
)

var testLog = slog.New(slog.DiscardHandler)

// TestResolveAuthRequiresCredentialInLocalMode pins the removal of the
// anonymous dev bypass: a request with no Authorization header is 401 in
// local mode (previously it fell back to the dev tenant).
func TestResolveAuthRequiresCredentialInLocalMode(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler reached without a credential")
	})
	wrapped := ResolveAuth(inner, nil, nil, testLog)
	req := httptest.NewRequest(http.MethodGet, "/orchicon.api.v1.ProjectService/ListProjects", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (missing credentials)", rec.Code)
	}
}

// TestResolveAuthPublicPathsSkipAuth confirms /auth/config is public and
// reachable without a credential.
func TestResolveAuthPublicPathsSkipAuth(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := ResolveAuth(inner, nil, nil, testLog)
	for _, p := range []string{"/auth/config", "/healthz", "/auth/local-login"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("path %s: status = %d, want 200 (public)", p, rec.Code)
		}
	}
}

// TestResolveAuthAdmitsCredentialFreeLogout pins the sign-out contract at
// the middleware boundary: POST /auth/logout must reach its handler with NO
// bearer token (it is cookie-based by design), otherwise ResolveAuth 401s
// before the refresh-cookie clear runs and a reload after sign-out
// re-authenticates via the still-valid HttpOnly cookie. The mux-level
// handler test in internal/auth bypasses this wrapper; this test closes
// that gap.
func TestResolveAuthAdmitsCredentialFreeLogout(t *testing.T) {
	var reached bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})
	wrapped := ResolveAuth(inner, nil, nil, testLog)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if !reached {
		t.Fatal("POST /auth/logout did not reach the handler (blocked by auth middleware)")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

// TestRBACInterceptorRejectsMissingIdentity pins the removal of the
// RBAC dev fallback: no resolved identity is Unauthenticated.
func TestRBACInterceptorRejectsMissingIdentity(t *testing.T) {
	i := NewRBACInterceptor()
	req := connect.NewRequest(&struct{}{})
	err := i.check(context.Background(), "/orchicon.api.v1.ProjectService/ListProjects")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated (no identity)", err)
	}
	_ = req
}

// TestRBACInterceptorAdmitsAdmin confirms the interceptor still admits an
// admin identity after the mode field removal.
func TestRBACInterceptorAdmitsAdmin(t *testing.T) {
	i := NewRBACInterceptor()
	ctx := auth.WithIdentity(context.Background(), auth.ResolvedIdentity{
		IdentityID: "id-admin",
		TenantID:   "tnt_dev",
		IsAdmin:    true,
		AuthMethod: "oidc",
	})
	if err := i.check(ctx, "/orchicon.api.v1.ProjectService/ListProjects"); err != nil {
		t.Fatalf("admin check failed: %v", err)
	}
}

// TestRBACInterceptorDeniesLackingEntitlement confirms a non-admin without
// the required entitlement is PermissionDenied (not silently allowed).
func TestRBACInterceptorDeniesLackingEntitlement(t *testing.T) {
	i := NewRBACInterceptor()
	ctx := auth.WithIdentity(context.Background(), auth.ResolvedIdentity{
		IdentityID:   "id-user",
		TenantID:     "tnt_dev",
		Entitlements: []string{},
		AuthMethod:   "oidc",
	})
	err := i.check(ctx, "/orchicon.api.v1.ProjectService/CreateProject")
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", err)
	}
}
