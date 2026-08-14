package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// testLocalLoginEnv wires a real auth.Handler (embedded OP included) over a
// migrated test database. Guarded by ORCHICON_TEST_DSN like the seed tests.
func testLocalLoginEnv(t *testing.T) (*httptest.Server, *db.Pool, *Handler) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed local-login test")
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
	cfg.Auth.SigningKey = "test-signing-key-for-local-login"
	cfg.Auth.RedirectURL = "http://localhost:8080/auth/callback"
	cfg.Auth.EmbeddedOP = true
	cfg.Auth.DevLoginAllowed = true
	cfg.Auth.OPRedirectURIs = ""

	log := slog.New(slog.DiscardHandler)
	h := NewHandler(cfg, pool, log)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); h.CloseEmbeddedOP() })
	return srv, pool, h
}

// createLocalAccount seeds an identity + local credential directly through
// the data-access layer (the boundary primitive), mirroring what the admin
// SetLocalCredential path does.
func createLocalAccount(t *testing.T, pool *db.Pool, tenantID, subject, username, password string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, subject, username, "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := db.UpsertLocalCredential(ctx, ttx.Tx, tenantID, ident.ID, username, hash); err != nil {
		t.Fatalf("UpsertLocalCredential: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		dtx, err := pool.BeginTenantTx(c, tenantID)
		if err != nil {
			return
		}
		_, _ = dtx.Exec(c, `DELETE FROM identities WHERE id = $1`, ident.ID)
		_ = dtx.Commit(c)
	})
	return ident.ID
}

func TestLocalLoginCorrectCredentials(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	const tenantID = "tnt_dev"
	createLocalAccount(t, pool, tenantID, "local@orchicon.local", "localuser", "correct-password")

	resp, err := http.Post(srv.URL+"/auth/local-login", "application/json",
		strings.NewReader(`{"username":"localuser","password":"correct-password"}`))
	if err != nil {
		t.Fatalf("POST local-login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AccessToken == "" || body.TokenType != "Bearer" {
		t.Fatalf("missing/odd tokens: %+v", body)
	}
	if body.TenantID != tenantID {
		t.Fatalf("tenant = %q, want %q", body.TenantID, tenantID)
	}
	// The refresh token must land in an HttpOnly cookie (the browser cannot
	// read it via JS).
	var setCookies = resp.Cookies()
	var refreshCookie *http.Cookie
	for _, c := range setCookies {
		if c.Name == RefreshCookie {
			refreshCookie = c
		}
	}
	if refreshCookie == nil {
		t.Fatal("no refresh cookie set")
	}
	if !refreshCookie.HttpOnly {
		t.Error("refresh cookie is not HttpOnly")
	}
	// No plaintext in any response.
	if strings.Contains(respBody(t, resp), "correct-password") {
		t.Fatal("login response leaks the plaintext password")
	}
}

func TestLocalLoginGeneric401(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	createLocalAccount(t, pool, "tnt_dev", "enum@orchicon.local", "enumuser", "right-password")

	wrongBody := postLogin(t, srv.URL, `{"username":"enumuser","password":"wrong-password"}`)
	unknownBody := postLogin(t, srv.URL, `{"username":"nosuchuser","password":"whatever"}`)
	// Identical generic rejection: no user-enumeration hint.
	if wrongBody != unknownBody {
		t.Fatalf("responses differ, leaking enumeration: wrong=%q unknown=%q", wrongBody, unknownBody)
	}
	if wrongBody != "invalid credentials" {
		t.Fatalf("body = %q, want generic 'invalid credentials'", wrongBody)
	}
}

func TestLocalLoginValidation(t *testing.T) {
	srv, _, _ := testLocalLoginEnv(t)
	cases := []string{
		`{"username":"","password":"x"}`,    // empty username
		`{"username":"u","password":""}`,     // empty password
		`not-json`,                            // malformed body
	}
	for _, body := range cases {
		resp, err := http.Post(srv.URL+"/auth/local-login", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %q: %v", body, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestLocalLoginDisabledWithoutOP(t *testing.T) {
	// With the embedded OP disabled the local-account surface is dead.
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

	resp, err := http.Post(srv.URL+"/auth/local-login", "application/json",
		strings.NewReader(`{"username":"u","password":"p"}`))
	if err != nil {
		t.Fatalf("POST local-login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the embedded OP is disabled", resp.StatusCode)
	}
}

// TestLocalLoginCompletesOPFlow drives a full OIDC authorization-code flow
// for a local account: /authorize → login bridge → local-login (password) →
// authorize callback → token exchange. This is the end-to-end proof that a
// local account completes the embedded-OP flow through the login bridge.
func TestLocalLoginCompletesOPFlow(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	const tenantID = "tnt_dev"
	createLocalAccount(t, pool, tenantID, "flow@orchicon.local", "flowuser", "flow-password")

	state := "state-local"
	verifier := "012345678901234567890123456789012345678901234567890123"
	challenge := localS256(verifier)

	// 1. Start the authorize request at the embedded OP.
	authz := srv.URL + "/authorize?" + url.Values{
		"client_id":             {"orchicon"},
		"redirect_uri":          {"http://localhost:8080/auth/callback"},
		"response_type":         {"code"},
		"scope":                 {"openid profile email"},
		"state":                 {state},
		"nonce":                 {"nonce-local"},
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

	// 2. The unauthenticated bridge would bounce to the SPA; the SPA form
	//    posts to /auth/local-login with next=<bridge path>.
	loginBody, _ := json.Marshal(localLoginRequest{Username: "flowuser", Password: "flow-password", Next: "/auth/op/login?id=" + id})
	resp, err = http.Post(srv.URL+"/auth/local-login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("POST local-login: %v", err)
	}
	var lr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatalf("decode local-login response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local-login status = %d, want 200", resp.StatusCode)
	}
	if !strings.HasPrefix(lr.Next, "/authorize/callback?id=") {
		t.Fatalf("local-login did not complete the OP request: next=%q", lr.Next)
	}

	// 3. The SPA full-page-loads the server-constructed callback path; the
	//    OP redirects to the relying party with the code.
	resp, err = noRedirectClient().Get(srv.URL + lr.Next)
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
	// The OP redirects the browser to the registered relying-party callback
	// with the authorization code (standard OIDC) — never an attacker host.
	if finalURL.Host != "localhost:8080" || finalURL.Path != "/auth/callback" {
		t.Fatalf("callback redirects to an unexpected host/path: %s", finalLoc)
	}

	// 4. Exchange the code (PKCE) for tokens.
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
		body, _ := io.ReadAll(tresp.Body)
		t.Fatalf("token status = %d: %s", tresp.StatusCode, body)
	}
	var tokens map[string]any
	if err := json.NewDecoder(tresp.Body).Decode(&tokens); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if tokens["access_token"] == "" || tokens["id_token"] == "" {
		t.Fatalf("missing tokens: %v", tokens)
	}

}

func TestSetLocalCredentialService(t *testing.T) {
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
	const tenantID = "tnt_dev"
	svc := NewService(pool, slog.New(slog.DiscardHandler))

	// Seed an identity to bind the credential to.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, "svc@orchicon.local", "Svc User", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		dtx, err := pool.BeginTenantTx(c, tenantID)
		if err != nil {
			return
		}
		_, _ = dtx.Exec(c, `DELETE FROM identities WHERE id = $1`, ident.ID)
		_ = dtx.Commit(c)
	})

	ctx = tenant.WithID(ctx, tenantID)
	call := func(req *apiv1.SetLocalCredentialRequest) (*connect.Response[apiv1.SetLocalCredentialResponse], error) {
		return svc.SetLocalCredential(ctx, connect.NewRequest(req))
	}

	// Valid upsert: response carries username+status, never the hash.
	resp, err := call(&apiv1.SetLocalCredentialRequest{
		IdentityId: ident.ID,
		Username:   "svc-user",
		Password:   "svc-password-123",
	})
	if err != nil {
		t.Fatalf("SetLocalCredential: %v", err)
	}
	if resp.Msg.Username != "svc-user" || resp.Msg.Status != "active" {
		t.Fatalf("unexpected response: %+v", resp.Msg)
	}

	// The stored value is an argon2id hash, not plaintext.
	gtx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin get tx: %v", err)
	}
	row, err := db.GetLocalCredentialByIdentity(ctx, gtx.Tx, tenantID, ident.ID)
	if err != nil {
		t.Fatalf("GetLocalCredentialByIdentity: %v", err)
	}
	if !strings.HasPrefix(row.PasswordHash, "$argon2id$") {
		t.Fatalf("stored hash is not argon2id: %q", row.PasswordHash)
	}
	if strings.Contains(row.PasswordHash, "svc-password-123") {
		t.Fatal("plaintext leaked into the stored hash column")
	}
	if err := gtx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Login through the real handler succeeds with the service-set credential.
	ident2 := row.IdentityID
	if ident2 == "" {
		t.Fatal("no identity bound")
	}

	// Validation: empty password, over-long password, bad username, unknown identity.
	if _, err := call(&apiv1.SetLocalCredentialRequest{IdentityId: ident.ID, Username: "u", Password: ""}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty password: err = %v, want InvalidArgument", err)
	}
	if _, err := call(&apiv1.SetLocalCredentialRequest{IdentityId: ident.ID, Username: "u", Password: strings.Repeat("a", MaxPasswordLen+1)}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("over-long password: err = %v, want InvalidArgument", err)
	}
	if _, err := call(&apiv1.SetLocalCredentialRequest{IdentityId: ident.ID, Username: "BAD USER", Password: "pw"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad username: err = %v, want InvalidArgument", err)
	}
	if _, err := call(&apiv1.SetLocalCredentialRequest{IdentityId: "does-not-exist", Username: "u", Password: "pw"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown identity: err = %v, want NotFound", err)
	}

	// Username conflict on another identity → AlreadyExists.
	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	other, _, err := db.GetOrCreateIdentity(ctx, ttx2.Tx, tenantID, "other@orchicon.local", "Other", "user")
	if err != nil {
		t.Fatalf("ensure other identity: %v", err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatalf("commit tx2: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		dtx, err := pool.BeginTenantTx(c, tenantID)
		if err != nil {
			return
		}
		_, _ = dtx.Exec(c, `DELETE FROM identities WHERE id = $1`, other.ID)
		_ = dtx.Commit(c)
	})
	if _, err := call(&apiv1.SetLocalCredentialRequest{IdentityId: other.ID, Username: "svc-user", Password: "pw"}); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("username conflict: err = %v, want AlreadyExists", err)
	}
}

// --- helpers ---------------------------------------------------------------

func postLogin(t *testing.T, base, body string) string {
	t.Helper()
	resp, err := http.Post(base+"/auth/local-login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST local-login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for %s", resp.StatusCode, body)
	}
	raw, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(raw))
}

func respBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func localS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestOpAuthRequestID(t *testing.T) {
	good := map[string]string{
		"/auth/op/login?id=abc123":         "abc123",
		"/authorize/callback?id=xyz":       "xyz",
		"/auth/op/login?id=a%20b":          "a b",
	}
	for in, want := range good {
		got, ok := opAuthRequestID(in)
		if !ok || got != want {
			t.Errorf("opAuthRequestID(%q) = (%q, %v), want (%q, true)", in, got, ok, want)
		}
	}
	bad := []string{
		"", "//evil.example/x", "http://evil.example/x", "/login?next=x",
		"/auth/op/login", "/auth/op/login?id=", "/auth/op/login?id=a&x=y",
		"/\\evil", "https://evil.example/auth/op/login?id=x",
	}
	for _, in := range bad {
		if got, ok := opAuthRequestID(in); ok {
			t.Errorf("opAuthRequestID(%q) = (%q, true), want false", in, got)
		}
	}
}
