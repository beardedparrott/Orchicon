package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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

// testTenantMappingEnv wires a real auth.Handler over a migrated test DB
// with the deployment tenant set to a non-default value (ORCHICON_
// DEPLOYMENT_TENANT_ID). Guarded by ORCHICON_TEST_DSN like the other
// DB-backed auth tests.
func testTenantMappingEnv(t *testing.T, deploymentTenant string) (*httptest.Server, *db.Pool, *Handler) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed tenant-mapping test")
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
	// Mirror boot: the deployment tenant is provisioned at boot.
	if err := db.SeedDevTenant(ctx, pool, deploymentTenant); err != nil {
		t.Fatalf("seed deployment tenant: %v", err)
	}

	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Auth.SigningKey = "test-signing-key-tenant-mapping"
	cfg.Auth.RedirectURL = "http://localhost:8080/auth/callback"
	cfg.Auth.EmbeddedOP = true
	cfg.DeploymentTenantID = deploymentTenant

	log := slog.New(slog.DiscardHandler)
	h := NewHandler(cfg, pool, log)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); h.CloseEmbeddedOP() })
	return srv, pool, h
}

// tenantMappingCleanup removes a test-provisioned identity from both the
// deployment tenant and the dev tenant so the tests stay isolated.
func tenantMappingCleanup(t *testing.T, pool *db.Pool, tenantID, subject string) {
	t.Helper()
	ctx := context.Background()
	for _, tid := range []string{tenantID, "tnt_dev"} {
		ttx, err := pool.BeginTenantTx(ctx, tid)
		if err != nil {
			continue
		}
		_, _ = ttx.Exec(ctx, `DELETE FROM role_bindings WHERE identity_id IN (SELECT id FROM identities WHERE subject = $1)`, subject)
		_, _ = ttx.Exec(ctx, `DELETE FROM identities WHERE subject = $1`, subject)
		_, _ = ttx.Exec(ctx, `DELETE FROM roles r WHERE r.name = 'admin'
			AND NOT EXISTS (SELECT 1 FROM role_bindings b WHERE b.role_id = r.id)`)
		_ = ttx.Commit(ctx)
	}
}

// TestLocalLoginLandsInDeploymentTenant pins the embedded-OP local-login
// path resolves within the deployment tenant: an account created in acme
// logs in there (never the hardcoded tnt_dev).
func TestLocalLoginLandsInDeploymentTenant(t *testing.T) {
	srv, pool, h := testTenantMappingEnv(t, "acme")
	createLocalAccount(t, pool, "acme", "local-acme@orchicon.local", "acmeuser", "acme-password", false)
	t.Cleanup(func() { tenantMappingCleanup(t, pool, "acme", "local-acme@orchicon.local") })

	resp, err := http.Post(srv.URL+"/auth/local-login", "application/json",
		strings.NewReader(`{"username":"acmeuser","password":"acme-password"}`))
	if err != nil {
		t.Fatalf("POST local-login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var body tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.TenantID != "acme" {
		t.Fatalf("tenant = %q, want %q (deployment tenant, not tnt_dev)", body.TenantID, "acme")
	}
	claims, err := h.issuer.VerifyAccess(body.AccessToken)
	if err != nil {
		t.Fatalf("VerifyAccess: %v", err)
	}
	if claims.TenantID != "acme" {
		t.Fatalf("token tenant claim = %q, want %q", claims.TenantID, "acme")
	}
}

// identityExists reports whether an identity row for the subject exists in
// the given tenant. The tenant_id filter is explicit (the test connection
// is a superuser that bypasses RLS, so relying on the session variable
// would make every tenant look identical).
func identityExists(t *testing.T, pool *db.Pool, tenantID, subject string) bool {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx (%s): %v", tenantID, err)
	}
	defer ttx.Rollback(ctx)
	var exists bool
	if err := ttx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identities WHERE tenant_id = $1 AND subject = $2)`, tenantID, subject).Scan(&exists); err != nil {
		t.Fatalf("query identity in %s: %v", tenantID, err)
	}
	return exists
}

// --- fake external OIDC IdP -----------------------------------------------

// fakeOIDCIdP stands up a minimal external OIDC provider (discovery +
// token + jwks) that exchanges any code for a real RS256-signed ID token.
// No PKCE is advertised, so the client-side state is a no-op — the same
// byte-for-byte flow a non-PKCE IdP gets in production.
func fakeOIDCIdP(t *testing.T) *httptest.Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	kid := "fake-idp-key-1"
	var jwksSrv *httptest.Server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer := "http://" + r.Host
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                issuer,
				"authorization_endpoint":                issuer + "/authorize",
				"token_endpoint":                        issuer + "/token",
				"jwks_uri":                              jwksSrv.URL,
				"userinfo_endpoint":                     issuer + "/userinfo",
				"response_types_supported":              []string{"code"},
				"subject_types_supported":               []string{"public"},
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/token":
			payload := map[string]any{
				"iss":   issuer,
				"sub":   "oidc-subject-tenant-map",
				"aud":   "orchicon",
				"exp":   time.Now().Add(time.Hour).Unix(),
				"iat":   time.Now().Unix(),
				"email": "oidc-user@example.com",
				"name":  "OIDC Tenant Map User",
			}
			tok := signRS256JWT(t, key, kid, payload)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "fake-access-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
				"id_token":     tok,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	// The jwks server must be reachable before the provider resolves it.
	jwksSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e := make([]byte, 8)
		binary.BigEndian.PutUint64(e, uint64(key.PublicKey.E))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": kid,
				"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(trimLeadingZeros(e)),
			}},
		})
	}))
	t.Cleanup(func() { srv.Close(); jwksSrv.Close() })
	return srv
}

func trimLeadingZeros(b []byte) []byte {
	i := 0
	for i < len(b)-1 && b[i] == 0 {
		i++
	}
	return b[i:]
}

func signRS256JWT(t *testing.T, key *rsa.PrivateKey, kid string, payload map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	body, _ := json.Marshal(payload)
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	signingInput := enc(header) + "." + enc(body)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign id token: %v", err)
	}
	return signingInput + "." + enc(sig)
}

// TestOIDCCallbackLandsInDeploymentTenant is the headline acceptance
// criterion: an external-OIDC login lands in the tenant resolved from the
// mapping (the configured deployment tenant), never the hardcoded tnt_dev.
func TestOIDCCallbackLandsInDeploymentTenant(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed OIDC tenant-mapping test")
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
	subject := "oidc-subject-tenant-map"
	t.Cleanup(func() { tenantMappingCleanup(t, pool, "acme", subject) })

	idp := fakeOIDCIdP(t)
	if err := db.SeedDevTenant(ctx, pool, "acme"); err != nil {
		t.Fatalf("seed deployment tenant: %v", err)
	}

	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Auth.Issuer = idp.URL
	cfg.Auth.ClientID = "orchicon"
	cfg.Auth.RedirectURL = "http://localhost:8080/auth/callback"
	cfg.Auth.SigningKey = "test-signing-key-oidc-tenant-map"
	cfg.Auth.EmbeddedOP = false
	cfg.DeploymentTenantID = "acme"

	log := slog.New(slog.DiscardHandler)
	h := NewHandler(cfg, pool, log)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

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
	// The token is delivered in the URL fragment (never sent to servers),
	// so it lives under loc.Fragment: "#/auth/callback?access_token=…".
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
	if claims.TenantID != "acme" {
		t.Fatalf("token tenant claim = %q, want %q (deployment tenant, not tnt_dev)", claims.TenantID, "acme")
	}
	// The OIDC subject was provisioned in acme, never in tnt_dev.
	if !identityExists(t, pool, "acme", subject) {
		t.Fatal("OIDC identity not provisioned in deployment tenant acme")
	}
	if identityExists(t, pool, "tnt_dev", subject) {
		t.Fatal("OIDC identity leaked into tnt_dev")
	}
	// The issued refresh cookie is set (the SPA can complete the session).
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == RefreshCookie {
			found = true
		}
	}
	if !found {
		t.Fatal("no refresh cookie set by the OIDC callback")
	}
}
