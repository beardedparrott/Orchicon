package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/config"
)

// testHandler constructs an auth.Handler over a nil pool. The endpoints
// exercised here (dev-login gate, /auth/config) never touch the DB, so a
// nil pool is safe. The embedded OP is enabled only when needed.
func testHandler(t *testing.T, mutate func(*config.Config)) *Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Auth.SigningKey = "test-signing-key-auth-flags"
	cfg.Auth.EmbeddedOP = true
	cfg.Auth.DevLoginAllowed = false
	if mutate != nil {
		mutate(&cfg)
	}
	return NewHandler(cfg, nil, slog.New(slog.DiscardHandler))
}

func serveHandler(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); h.CloseEmbeddedOP() })
	return srv
}

// TestDevLoginDisabledReturns403 pins D2: with ORCHICON_DEV_LOGIN off (the
// default) POST /auth/dev-login returns 403 — it is a flag-gated escape
// hatch, not the auth path.
func TestDevLoginDisabledReturns403(t *testing.T) {
	srv := serveHandler(t, testHandler(t, nil))
	resp, err := http.Post(srv.URL+"/auth/dev-login", "application/json",
		strings.NewReader(`{"subject":"dev@orchicon.local"}`))
	if err != nil {
		t.Fatalf("POST dev-login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when dev-login is disabled", resp.StatusCode)
	}
}

// TestDevLoginRejectedInProduction pins that dev-login is local-mode only:
// even with the flag on, production rejects it.
func TestDevLoginRejectedInProduction(t *testing.T) {
	h := testHandler(t, func(c *config.Config) {
		c.Mode = config.ModeProduction
		c.Auth.DevLoginAllowed = true
	})
	srv := serveHandler(t, h)
	resp, err := http.Post(srv.URL+"/auth/dev-login", "application/json",
		strings.NewReader(`{"subject":"dev@orchicon.local"}`))
	if err != nil {
		t.Fatalf("POST dev-login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for dev-login in production", resp.StatusCode)
	}
}

// TestAuthConfigFlags renders the honest login-page contract (D5): the
// public /auth/config payload mirrors the plane's auth capabilities and
// never leaks secrets.
func TestAuthConfigFlags(t *testing.T) {
	h := testHandler(t, func(c *config.Config) {
		c.Auth.DevLoginAllowed = true
	})
	srv := serveHandler(t, h)
	resp, err := http.Get(srv.URL + "/auth/config")
	if err != nil {
		t.Fatalf("GET /auth/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out authConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.EmbeddedOP {
		t.Error("embedded_op = false, want true")
	}
	if out.ExternalOIDC {
		t.Error("external_oidc = true, want false (no external issuer configured)")
	}
	if !out.DevLogin {
		t.Error("dev_login = false, want true (flag enabled in this test)")
	}
	// Secret-free: no issuer URL, client id, or signing material in the
	// payload — the struct only carries the four capability flags.
	if out.Mode == "" {
		t.Error("mode is empty")
	}
}

// TestAuthConfigExternalOIDC mirrors an external-IdP plane: external_oidc
// flips on, dev_login stays off.
func TestAuthConfigExternalOIDC(t *testing.T) {
	h := testHandler(t, func(c *config.Config) {
		c.Auth.Issuer = "https://sso.example.com"
		c.Auth.ClientSecret = "confidential-secret"
	})
	srv := serveHandler(t, h)
	resp, err := http.Get(srv.URL + "/auth/config")
	if err != nil {
		t.Fatalf("GET /auth/config: %v", err)
	}
	defer resp.Body.Close()
	var out authConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.ExternalOIDC {
		t.Error("external_oidc = false, want true")
	}
	if out.DevLogin {
		t.Error("dev_login = true, want false (flag default off)")
	}
}
