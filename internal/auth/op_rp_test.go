package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	authop "github.com/beardedparrott/orchicon/internal/auth/op"
)

const (
	rpTestIdentityID = "01JRPIDENTITY000000000000"
	rpTestRedirect   = "http://localhost:8080/auth/callback"
)

// rpNoRedirectClient does not follow redirects so each 302 Location can be
// inspected.
func rpNoRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// resolveURL resolves a possibly-relative Location against the base.
func resolveURL(base, loc string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.IsAbs() {
		return loc
	}
	bu, _ := url.Parse(base)
	return bu.ResolveReference(u).String()
}

func TestEmbeddedOPVerifyThroughRP(t *testing.T) {
	// Build the embedded OP + login bridge on an httptest server.
	prov, err := authop.NewProvider(authop.Config{
		SigningKey:    "test-signing-key",
		ClientID:      authop.DefaultClientID,
		RedirectURIs:  []string{rpTestRedirect},
		AllowInsecure: true,
		Identity: func(context.Context, string, string) (authop.IdentityClaims, error) {
			return authop.IdentityClaims{Name: "RP Test User", Email: "rp@test.local", Username: "rp"}, nil
		},
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	mux := http.NewServeMux()
	prov.Register(mux)
	authop.RegisterLoginBridge(mux, prov, func(r *http.Request) (string, string, bool) {
		if c, err := r.Cookie(RefreshCookie); err == nil && c.Value == "rp-test-refresh" {
			return "tnt_dev", rpTestIdentityID, true
		}
		return "", "", false
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer prov.Close()

	// The relying party is the existing coreos/go-oidc verifier pointed at
	// the plane origin — zero issuer-specific code.
	ver := NewOIDCVerifier(srv.URL, authop.DefaultClientID, "", rpTestRedirect)
	state := "rp-state-1"

	authURL, err := ver.AuthCodeURL(context.Background(), state)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	if parsed.Query().Get("code_challenge") == "" || parsed.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("RP must send PKCE when the IdP advertises S256 (embedded OP does)")
	}
	if parsed.Query().Get("state") != state {
		t.Fatalf("state = %q, want %q", parsed.Query().Get("state"), state)
	}

	client := rpNoRedirectClient()

	// Step 1: authorize → 302 to the login bridge.
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	loginURL := resolveURL(srv.URL, resp.Header.Get("Location"))
	resp.Body.Close()
	if !strings.Contains(loginURL, "/auth/op/login?id=") {
		t.Fatalf("authorize did not redirect to login bridge: %s", loginURL)
	}

	// Step 2: login bridge with a valid Orchicon session cookie → 302 to
	// the authorize callback.
	req, _ := http.NewRequest(http.MethodGet, loginURL, nil)
	req.AddCookie(&http.Cookie{Name: RefreshCookie, Value: "rp-test-refresh"})
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET login bridge: %v", err)
	}
	cbURL := resolveURL(srv.URL, resp.Header.Get("Location"))
	resp.Body.Close()
	if !strings.Contains(cbURL, "/authorize/callback?id=") {
		t.Fatalf("login bridge did not redirect to authorize callback: %s", cbURL)
	}

	// Step 3: authorize callback → 302 back to the client redirect_uri
	// with an authorization code.
	resp, err = client.Get(cbURL)
	if err != nil {
		t.Fatalf("GET authorize callback: %v", err)
	}
	final, _ := url.Parse(resolveURL(srv.URL, resp.Header.Get("Location")))
	resp.Body.Close()
	code := final.Query().Get("code")
	if code == "" {
		t.Fatalf("authorize callback produced no code: %s", final.String())
	}

	// Step 4: exchange the code through the RP — ID token verified against
	// the OP's /jwks via go-oidc.
	out, err := ver.Exchange(context.Background(), state, code)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if out.Subject != rpTestIdentityID {
		t.Errorf("subject = %q, want identity ULID %q", out.Subject, rpTestIdentityID)
	}
	if out.Email != "rp@test.local" {
		t.Errorf("email = %q, want rp@test.local", out.Email)
	}
	if out.DisplayName != "RP Test User" {
		t.Errorf("display name = %q, want RP Test User", out.DisplayName)
	}
}
