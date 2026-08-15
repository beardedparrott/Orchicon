package auth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/beardedparrott/orchicon/internal/config"
)

// newLogoutServer builds a live mux server with the auth endpoints registered
// (the same path the control plane serves) and no database: /auth/logout is
// pure cookie-clearing and never touches the resolver/pool. The embedded OP
// and OIDC construction are skipped via the flags NewHandler gates on.
func newLogoutServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Auth.Issuer = "local"
	cfg.Auth.EmbeddedOP = false
	cfg.Auth.SigningKey = "logout-test-signing-key"
	h := NewHandler(cfg, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(h.CloseEmbeddedOP)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestLogoutClearsRefreshCookie pins the contract the frontend relies on:
// POST /auth/logout must emit a Set-Cookie that expires the HttpOnly refresh
// cookie (empty value + negative MaxAge + the same Path/HttpOnly attributes
// it was set with), so a later /auth/refresh has nothing to exchange and the
// signed-out browser stays signed out across reloads.
func TestLogoutClearsRefreshCookie(t *testing.T) {
	srv := newLogoutServer(t)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/auth/logout", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	// Simulate a logged-in browser holding the refresh cookie.
	req.AddCookie(&http.Cookie{Name: RefreshCookie, Value: "still-valid-refresh-jwt"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /auth/logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	var cleared *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == RefreshCookie {
			cleared = c
		}
	}
	if cleared == nil {
		t.Fatal("no orchicon_refresh Set-Cookie on logout")
	}
	if cleared.Value != "" {
		t.Errorf("cookie value = %q, want empty", cleared.Value)
	}
	if cleared.MaxAge >= 0 {
		t.Errorf("cookie MaxAge = %d, want negative (expire immediately)", cleared.MaxAge)
	}
	if !cleared.HttpOnly {
		t.Error("cleared cookie is not HttpOnly")
	}
	if cleared.Path != "/" {
		t.Errorf("cookie Path = %q, want /", cleared.Path)
	}
}

// TestLogoutRequiresPost pins the method gate: only POST is accepted (the
// frontend issues POST; a GET is not a logout).
func TestLogoutRequiresPost(t *testing.T) {
	srv := newLogoutServer(t)
	resp, err := http.Get(srv.URL + "/auth/logout")
	if err != nil {
		t.Fatalf("GET /auth/logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}
