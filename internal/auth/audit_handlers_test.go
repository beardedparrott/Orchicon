package auth

// HTTP-endpoint audit tests (design §5 item 3 + D5): the credential-free
// auth paths (local-login, signup, logout, refresh) write one audit row
// each — auth.login (local), auth.signup, auth.logout (refresh),
// auth.refresh — and a failed login writes nothing. These are public paths
// with no identity in the request context, so the actor is resolved after
// credential validation and passed explicitly. Skipped unless
// ORCHICON_TEST_DSN is set:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/auth/ -run TestAuditHTTP -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/beardedparrott/orchicon/internal/db"
)

func auditHTTPCount(t *testing.T, pool *db.Pool, tenantID, action, actorID string) int {
	t.Helper()
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, action, actorID, "", "", "", 100, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	return len(rows)
}

func respURL(t *testing.T, srv *httptest.Server) *url.URL {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	return u
}

// TestAuditHTTPLoginSignupLogoutRefresh drives the real HTTP endpoints
// against a migrated test DB and asserts one audit row per action.
func TestAuditHTTPLoginSignupLogoutRefresh(t *testing.T) {
	srv, pool, _ := testLocalLoginEnv(t)
	const tenantID = "tnt_dev"

	// --- Signup: 1 auth.signup row, actor == the new identity. ---
	username := "audit_http_" + strings.ToLower(db.NewID())
	resp, body := postSignup(t, srv.URL, username, "audit-password-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signup status = %d (body %s)", resp.StatusCode, body)
	}
	var out tokenResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode signup: %v", err)
	}
	t.Cleanup(func() { cleanupSignupIdentity(t, pool, tenantID, out.IdentityID) })
	if n := auditHTTPCount(t, pool, tenantID, "auth.signup", out.IdentityID); n != 1 {
		t.Fatalf("auth.signup rows = %d, want 1", n)
	}

	// --- Local login: 1 auth.login row (auth_method=local). ---
	loginResp, err := http.Post(srv.URL+"/auth/local-login", "application/json",
		strings.NewReader(`{"username":"`+username+`","password":"audit-password-1"}`))
	if err != nil {
		t.Fatalf("POST local-login: %v", err)
	}
	loginBody, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("local-login status = %d (body %s)", loginResp.StatusCode, loginBody)
	}
	if n := auditHTTPCount(t, pool, tenantID, "auth.login", out.IdentityID); n != 1 {
		t.Fatalf("auth.login rows = %d, want 1", n)
	}
	// The login row carries the real auth method.
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "auth.login", out.IdentityID, "", "", "", 10, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if rows[0].AuthMethod != "local" || rows[0].ActorType != "user" {
		t.Fatalf("login actor fields = auth:%s type:%s, want local/user",
			rows[0].AuthMethod, rows[0].ActorType)
	}

	// --- Failed login: no row (no enumeration side-channel). ---
	if _, err := http.Post(srv.URL+"/auth/local-login", "application/json",
		strings.NewReader(`{"username":"`+username+`","password":"wrong-password"}`)); err != nil {
		t.Fatalf("POST failed local-login: %v", err)
	}
	if n := auditHTTPCount(t, pool, tenantID, "auth.login", out.IdentityID); n != 1 {
		t.Fatalf("failed login wrote a row: auth.login rows = %d, want still 1", n)
	}

	// --- Refresh: exchange the refresh cookie → 1 auth.refresh row. ---
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}
	// Seed the jar with the refresh cookie from the login response.
	for _, c := range loginResp.Cookies() {
		if c.Name == RefreshCookie {
			jar.SetCookies(respURL(t, srv), []*http.Cookie{c})
		}
	}
	rresp, err := client.Post(srv.URL+"/auth/refresh", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST refresh: %v", err)
	}
	rbody, _ := io.ReadAll(rresp.Body)
	rresp.Body.Close()
	if rresp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d (body %s)", rresp.StatusCode, rbody)
	}
	if n := auditHTTPCount(t, pool, tenantID, "auth.refresh", out.IdentityID); n != 1 {
		t.Fatalf("auth.refresh rows = %d, want 1", n)
	}

	// --- Logout: POST with the refresh cookie → 1 auth.logout row. ---
	lresp, err := client.Post(srv.URL+"/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("POST logout: %v", err)
	}
	lresp.Body.Close()
	if lresp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", lresp.StatusCode)
	}
	if n := auditHTTPCount(t, pool, tenantID, "auth.logout", out.IdentityID); n != 1 {
		t.Fatalf("auth.logout rows = %d, want 1", n)
	}
}
