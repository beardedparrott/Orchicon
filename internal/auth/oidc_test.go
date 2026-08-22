package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeOIDCServer builds a minimal OIDC discovery server advertising the
// given code_challenge_methods_supported (nil omits the field entirely).
func fakeOIDCServer(t *testing.T, codeChallengeMethods []string) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		issuer := "http://" + r.Host
		doc := map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/authorize",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/jwks",
			"userinfo_endpoint":                     issuer + "/userinfo",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if codeChallengeMethods != nil {
			doc["code_challenge_methods_supported"] = codeChallengeMethods
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// TestRPNoPKCEWhenNotAdvertised proves the BYO-IdP flow is unchanged when
// the IdP's discovery does not advertise PKCE at all: no code_challenge is
// sent on the wire (capability-gated D6).
func TestRPNoPKCEWhenNotAdvertised(t *testing.T) {
	srv := fakeOIDCServer(t, nil)
	ver := NewOIDCVerifier(srv.URL, "client", "secret", "http://localhost:5173/auth/callback")
	authURL, err := ver.AuthCodeURL(context.Background(), "state-1")
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	q := parsed.Query()
	if q.Get("code_challenge") != "" || q.Get("code_challenge_method") != "" {
		t.Fatalf("no-PKCE IdP must receive no challenge params, got code_challenge=%q method=%q",
			q.Get("code_challenge"), q.Get("code_challenge_method"))
	}
}

// TestRPNoPKCEWhenOnlyPlain proves PKCE is only activated for S256: an IdP
// advertising only "plain" still gets a byte-for-byte unchanged flow.
func TestRPNoPKCEWhenOnlyPlain(t *testing.T) {
	srv := fakeOIDCServer(t, []string{"plain"})
	ver := NewOIDCVerifier(srv.URL, "client", "secret", "http://localhost:5173/auth/callback")
	authURL, err := ver.AuthCodeURL(context.Background(), "state-2")
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	q := parsed.Query()
	if q.Get("code_challenge") != "" || q.Get("code_challenge_method") != "" {
		t.Fatalf("plain-only IdP must receive no challenge params, got code_challenge=%q", q.Get("code_challenge"))
	}
}

// TestRPPKCEWhenS256Advertised proves the capability gate flips on for an
// IdP advertising S256 (the embedded OP advertises it).
func TestRPPKCEWhenS256Advertised(t *testing.T) {
	srv := fakeOIDCServer(t, []string{"S256"})
	ver := NewOIDCVerifier(srv.URL, "client", "secret", "http://localhost:5173/auth/callback")
	authURL, err := ver.AuthCodeURL(context.Background(), "state-3")
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	q := parsed.Query()
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("S256-advertising IdP must receive a code challenge, got code_challenge=%q method=%q",
			q.Get("code_challenge"), q.Get("code_challenge_method"))
	}
}
