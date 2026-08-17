package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// TestSignupHappyPath creates a fresh account through POST /auth/signup and
// asserts the full session contract: identity + credential rows exist, the
// stored hash verifies, the token pair is issued, the refresh cookie is
// HttpOnly, and the account is a plain user (no admin grant).
func TestSignupHappyPath(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	const tenantID = "tnt_dev"
	username := "signup_ok"

	resp, body := postSignup(t, srv.URL, username, "signup-password-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	var out tokenResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.AccessToken == "" || out.TokenType != "Bearer" || out.ExpiresIn == 0 {
		t.Fatalf("missing/odd token response: %+v", out)
	}
	if out.IdentityID == "" || out.TenantID != tenantID {
		t.Fatalf("identity/tenant = %+v, want tenant %q", out, tenantID)
	}
	if out.IsAdmin {
		t.Fatal("a self-signed-up account must not be an admin")
	}

	// The refresh token must land in an HttpOnly cookie.
	var refreshCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == RefreshCookie {
			refreshCookie = c
		}
	}
	if refreshCookie == nil || !refreshCookie.HttpOnly {
		t.Fatal("refresh cookie missing or not HttpOnly")
	}

	// Cleanup (identity delete cascades to the credential).
	cleanupSignupIdentity(t, pool, tenantID, out.IdentityID)
}

func TestSignupPersistsCredentialAndVerifies(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	const tenantID = "tnt_dev"
	username := "signup_persist"

	resp, body := postSignup(t, srv.URL, username, "signup-password-2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	var out tokenResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	t.Cleanup(func() { cleanupSignupIdentity(t, pool, tenantID, out.IdentityID) })

	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	// Credential row exists, bound to the identity, argon2id-hashed.
	cred, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, tenantID, username)
	if err != nil {
		t.Fatalf("GetLocalCredentialByUsername: %v", err)
	}
	if cred.IdentityID != out.IdentityID || cred.Status != "active" {
		t.Fatalf("credential = %+v, want identity %q active", cred, out.IdentityID)
	}
	if !strings.HasPrefix(cred.PasswordHash, "$argon2id$") {
		t.Fatalf("stored hash is not argon2id: %q", cred.PasswordHash)
	}
	if strings.Contains(cred.PasswordHash, "signup-password-2") {
		t.Fatal("plaintext leaked into the stored hash")
	}
	valid, err := VerifyPassword("signup-password-2", cred.PasswordHash)
	if err != nil || !valid {
		t.Fatalf("VerifyPassword = (%v, %v), want (true, nil)", valid, err)
	}

	// Identity row exists with the username as subject.
	ident, err := db.GetIdentity(ctx, ttx.Tx, tenantID, out.IdentityID)
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	if ident.Subject != username || ident.IdentityType != "user" || ident.Status != "active" {
		t.Fatalf("identity = %+v, want subject %q user active", ident, username)
	}
	// No role bindings: a self-signed-up account has zero entitlements.
	ents, isAdmin, err := db.ListIdentityEntitlements(ctx, ttx.Tx, tenantID, out.IdentityID)
	if err != nil {
		t.Fatalf("ListIdentityEntitlements: %v", err)
	}
	if isAdmin || len(ents) != 0 {
		t.Fatalf("signup identity must have no entitlements: isAdmin=%v ents=%v", isAdmin, ents)
	}
}

// TestSignupDuplicateUsername creates an account, then signs up again with
// the same username → 409 (generic message, no extra enumeration).
func TestSignupDuplicateUsername(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	const tenantID = "tnt_dev"
	username := "signup_dup"

	// Seed an account via the boundary primitives (like the admin path).
	createLocalAccount(t, pool, tenantID, username, username, "seed-password", false)

	resp, body := postSignup(t, srv.URL, username, "new-password")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "already exists") {
		t.Fatalf("body = %q, want generic already-exists message", body)
	}
}

// TestSignupIdentitySquattingRejected seeds an identity (no credential) —
// e.g. a BYO-IdP first-login provisioning — and signs up with the same
// subject. The existing identity must NOT get a local password bound to it.
func TestSignupIdentitySquattingRejected(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	const tenantID = "tnt_dev"
	subject := "squat@orchicon.local"

	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, subject, "Someone Else", "user")
	if err != nil {
		t.Fatalf("GetOrCreateIdentity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() { cleanupSignupIdentity(t, pool, tenantID, ident.ID) })

	resp, body := postSignup(t, srv.URL, subject, "password-123")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for existing identity (body %s)", resp.StatusCode, body)
	}
	// The squatting identity must still have no credential bound.
	gtx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin get tx: %v", err)
	}
	defer gtx.Rollback(ctx)
	if _, err := db.GetLocalCredentialByIdentity(ctx, gtx.Tx, tenantID, ident.ID); err == nil {
		t.Fatal("squatting identity gained a local credential")
	}
}

// TestSignupValidation pins the boundary validation mirroring the admin
// SetLocalCredential path: bad username charset, empty fields, over-long
// password → 400.
func TestSignupValidation(t *testing.T) {
	srv, _, _ := testLocalLoginEnv(t)
	cases := []string{
		`{"username":"","password":"x"}`,                                                    // empty username
		`{"username":"u","password":""}`,                                                    // empty password
		`{"username":"BAD USER","password":"x"}`,                                            // bad charset
		`{"username":"U","password":"x"}`,                                                   // uppercase not allowed
		`{"username":"ok_user","password":"` + strings.Repeat("a", MaxPasswordLen+1) + `"}`, // over-long password
		`not-json`, // malformed body
	}
	for _, body := range cases {
		resp, err := http.Post(srv.URL+"/auth/signup", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %q: %v", body, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

// TestSignupDisabledWithoutOP asserts the endpoint 404s when the embedded
// OP is disabled (sign-up availability == the embedded IdP being on).
func TestSignupDisabledWithoutOP(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set")
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

	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Auth.SigningKey = "test-signing-key-disabled"
	cfg.Auth.EmbeddedOP = false
	h := NewHandler(cfg, pool, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/auth/signup", "application/json",
		strings.NewReader(`{"username":"u","password":"p"}`))
	if err != nil {
		t.Fatalf("POST signup: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the embedded OP is disabled", resp.StatusCode)
	}
}

// TestSignupCompletesOPFlow drives the full OIDC authorization-code flow for
// a sign-up that happens mid-OP-flow: /authorize → login bridge → signup
// (username+password) → authorize callback → PKCE token exchange. This
// proves sign-up completes the pending authorize request exactly like
// local-login does.
func TestSignupCompletesOPFlow(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	username := "signup_flow_user"

	state := "state-signup"
	verifier := "012345678901234567890123456789012345678901234567890123"
	challenge := localS256(verifier)

	authz := srv.URL + "/authorize?" + url.Values{
		"client_id":             {"orchicon"},
		"redirect_uri":          {"http://localhost:8080/auth/callback"},
		"response_type":         {"code"},
		"scope":                 {"openid profile email"},
		"state":                 {state},
		"nonce":                 {"nonce-signup"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	resp, err := noRedirectClient().Get(authz)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.Contains(loc, "/auth/op/login?id=") {
		t.Fatalf("authorize did not redirect to the login bridge: status=%d loc=%s", resp.StatusCode, loc)
	}
	id := strings.TrimPrefix(loc, "/auth/op/login?id=")

	signupBody, _ := json.Marshal(signupRequest{Username: username, Password: "flow-password", Next: "/auth/op/login?id=" + id})
	sresp, body := postSignupRaw(t, srv.URL, signupBody)
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("signup status = %d, want 200 (body %s)", sresp.StatusCode, body)
	}
	var out tokenResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode signup response: %v", err)
	}
	if !strings.HasPrefix(out.Next, "/authorize/callback?id=") {
		t.Fatalf("signup did not complete the OP request: next=%q", out.Next)
	}
	t.Cleanup(func() { cleanupSignupIdentity(t, pool, "tnt_dev", out.IdentityID) })

	// Full-page-load the server-constructed callback path; the OP redirects
	// to the relying party with the code.
	resp, err = noRedirectClient().Get(srv.URL + out.Next)
	if err != nil {
		t.Fatalf("GET authorize callback: %v", err)
	}
	finalLoc := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize callback status = %d, want 302", resp.StatusCode)
	}
	finalURL, err := url.Parse(finalLoc)
	if err != nil {
		t.Fatalf("parse final redirect: %v", err)
	}
	code := finalURL.Query().Get("code")
	if code == "" || finalURL.Query().Get("state") != state {
		t.Fatalf("no code/state in final redirect: %s", finalLoc)
	}
	if finalURL.Host != "localhost:8080" || finalURL.Path != "/auth/callback" {
		t.Fatalf("callback redirects to an unexpected host/path: %s", finalLoc)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", "orchicon")
	form.Set("code", code)
	form.Set("redirect_uri", "http://localhost:8080/auth/callback")
	form.Set("code_verifier", verifier)
	tresp, err := http.PostForm(srv.URL+"/token", form)
	if err != nil {
		t.Fatalf("POST token: %v", err)
	}
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(tresp.Body)
		t.Fatalf("token status = %d: %s", tresp.StatusCode, raw)
	}
	var tokens map[string]any
	if err := json.NewDecoder(tresp.Body).Decode(&tokens); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if tokens["access_token"] == "" || tokens["id_token"] == "" {
		t.Fatalf("missing tokens: %v", tokens)
	}
}

// TestSignupConcurrentSameUsername fires N concurrent signups for one
// username; exactly one succeeds and every other gets 409. This pins both
// insert races (identity insert + credential upsert) mapping to 409.
func TestSignupConcurrentSameUsername(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	username := "signup_race"

	const n = 6
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/auth/signup", "application/json",
				strings.NewReader(`{"username":"`+username+`","password":"race-password"}`))
			if err != nil {
				results[i] = -1
				return
			}
			defer resp.Body.Close()
			results[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	var okCount, conflictCount int
	for _, code := range results {
		switch code {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if okCount != 1 {
		t.Fatalf("okCount = %d, want exactly 1 (results %v)", okCount, results)
	}
	if okCount+conflictCount != n {
		t.Fatalf("lost responses: results %v", results)
	}

	// Clean up the winning identity so a re-run against the same test DB
	// starts clean (the race leaves exactly one account behind).
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	cred, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, "tnt_dev", username)
	if err != nil {
		t.Fatalf("GetLocalCredentialByUsername after race: %v", err)
	}
	_ = ttx.Rollback(ctx)
	cleanupSignupIdentity(t, pool, "tnt_dev", cred.IdentityID)
}

// --- helpers ---------------------------------------------------------------

func postSignup(t *testing.T, base, username, password string) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(signupRequest{Username: username, Password: password})
	return postSignupRaw(t, base, body)
}

func postSignupRaw(t *testing.T, base string, body []byte) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(base+"/auth/signup", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST signup: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, strings.TrimSpace(string(raw))
}

func cleanupSignupIdentity(t *testing.T, pool *db.Pool, tenantID, identityID string) {
	t.Helper()
	if identityID == "" {
		return
	}
	ctx := context.Background()
	dtx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	_, _ = dtx.Exec(ctx, `DELETE FROM identities WHERE id = $1`, identityID)
	_ = dtx.Commit(ctx)
}
