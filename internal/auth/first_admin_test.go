package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// testDualAuthEnv wires a real auth.Handler with BOTH the embedded OP (local
// sign-up) and an external OIDC IdP configured, over a migrated test DB. This
// is the surface for the unified first-admin guard: exactly one tenant admin
// can exist regardless of which path runs first.
func testDualAuthEnv(t *testing.T) (*httptest.Server, *db.Pool, *Handler) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed dual-auth test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if err := db.SeedDevTenant(ctx, pool, "tnt_dev"); err != nil {
		t.Fatalf("seed dev tenant: %v", err)
	}

	idp := fakeOIDCIdP(t)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Auth.SigningKey = "test-signing-key-dual-auth"
	cfg.Auth.RedirectURL = "http://localhost:8080/auth/callback"
	cfg.Auth.Issuer = idp.URL
	cfg.Auth.ClientID = "orchicon"
	cfg.Auth.EmbeddedOP = true

	log := slog.New(slog.DiscardHandler)
	h := NewHandler(cfg, pool, log)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); h.CloseEmbeddedOP() })
	return srv, pool, h
}

// oidcFirstLogin drives the external-OIDC callback once for the fake IdP's
// subject and returns the issued access token's is_admin claim.
func oidcFirstLogin(t *testing.T, srv *httptest.Server, h *Handler) bool {
	t.Helper()
	resp, err := noRedirectClient().Get(srv.URL + "/auth/oidc/callback?code=fake-code&state=fake-state")
	if err != nil {
		t.Fatalf("GET oidc callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("callback status = %d, want 302: %s", resp.StatusCode, body)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	frag, err := url.ParseQuery(strings.TrimPrefix(loc.Fragment, "/auth/callback?"))
	if err != nil {
		t.Fatalf("parse redirect fragment: %v", err)
	}
	access := frag.Get("access_token")
	if access == "" {
		t.Fatal("no access_token in the SPA redirect fragment")
	}
	claims, err := h.issuer.VerifyAccess(access)
	if err != nil {
		t.Fatalf("VerifyAccess: %v", err)
	}
	return claims.IsAdmin
}

// TestFirstAdminGuardLocalThenOIDC pins the unified first-admin guard: a
// local first sign-up becomes the admin, and a subsequent external-OIDC
// first login (a NEW identity, created=true) must NOT become admin — the
// grant keys on "no admin binding exists", not on whether the identity row
// was new. Exactly one admin exists.
func TestFirstAdminGuardLocalThenOIDC(t *testing.T) {
	srv, pool, h := testDualAuthEnv(t)
	const tenantID = "tnt_dev"
	clearAdminBindings(t, pool, tenantID)
	t.Cleanup(func() { tenantMappingCleanup(t, pool, tenantID, "oidc-subject-tenant-map") })

	// Local first sign-up → admin.
	username := "dual_local_" + randSuffix()
	resp, body := postSignup(t, srv.URL, username, "dual-password")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signup status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	var out tokenResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode signup response: %v", err)
	}
	t.Cleanup(func() { cleanupSignupIdentity(t, pool, tenantID, out.IdentityID) })
	if !out.IsAdmin {
		t.Fatal("the first local sign-up must be the tenant admin")
	}

	// External-OIDC first login (new identity) must NOT become admin.
	if oidcFirstLogin(t, srv, h) {
		t.Fatal("external-OIDC first login became admin after a local admin already existed; want exactly one admin")
	}
	if !adminRoleBound(t, pool, tenantID) {
		t.Fatal("the local admin was lost after the OIDC login")
	}
}

// TestFirstAdminGuardOIDCThenLocal pins the unified first-admin guard in the
// other order: an external-OIDC first login becomes the admin, and a
// subsequent local first sign-up must NOT become admin. Exactly one admin
// exists regardless of which path ran first.
func TestFirstAdminGuardOIDCThenLocal(t *testing.T) {
	srv, pool, h := testDualAuthEnv(t)
	const tenantID = "tnt_dev"
	clearAdminBindings(t, pool, tenantID)
	t.Cleanup(func() { tenantMappingCleanup(t, pool, tenantID, "oidc-subject-tenant-map") })

	// External-OIDC first login → admin.
	if !oidcFirstLogin(t, srv, h) {
		t.Fatal("the external-OIDC first login must be the tenant admin")
	}

	// Local first sign-up must NOT become admin.
	username := "dual_oidc_" + randSuffix()
	resp, body := postSignup(t, srv.URL, username, "dual-password")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signup status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	var out tokenResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode signup response: %v", err)
	}
	t.Cleanup(func() { cleanupSignupIdentity(t, pool, tenantID, out.IdentityID) })
	if out.IsAdmin {
		t.Fatal("a local sign-up after an OIDC admin must not become admin; want exactly one admin")
	}
	if !adminRoleBound(t, pool, tenantID) {
		t.Fatal("the OIDC admin was lost after the local sign-up")
	}
}

func randSuffix() string {
	return strings.ReplaceAll(url.QueryEscape(time.Now().Format("150405.000000000")), ".", "")
}
